// Package store connects the gateway to PocketBase. PocketBase holds
// the chains' URLs (with the keys), tokens and daily usage; the
// chains themselves are fixed, in gateway.Chains; the gateway never reads it on a request.
// The store loads everything into memory at startup, reloads when a record
// changes, and flushes in-memory usage counts every FlushInterval.
//
// The schema is the embedded collections.json snapshot, applied in full on
// every start: never change it in the admin UI (edits are overwritten, and
// undeclared fields are deleted with their data). Regenerate it with
// `just schema` instead.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	"rpcgate/internal/gateway"
)

//go:embed collections.json
var snapshot []byte

// Collection names.
const (
	URLs   = "urls"
	Tokens = "tokens"
	Usage  = "usage"
)

// ApplySchema imports the snapshot, deleting anything it doesn't declare
// (PocketBase's own system collections excepted).
func ApplySchema(app core.App) error {
	if err := app.ImportCollectionsByMarshaledJSON(snapshot, true); err != nil {
		return fmt.Errorf("apply schema snapshot: %w", err)
	}
	return nil
}

// Options configures a Store.
type Options struct {
	Client          *http.Client
	UpstreamTimeout time.Duration
	CheckTimeout    time.Duration // per chain startup check; default 15s
	FlushInterval   time.Duration // default 15s
	Logger          *log.Logger   // nil disables logging
	Now             func() time.Time
}

// Store keeps a gateway in sync with PocketBase.
type Store struct {
	app  core.App
	gw   *gateway.Gateway
	opts Options

	started atomic.Bool // hooks reload only after Start's own load
	chainMu sync.Mutex  // serializes URL reloads
	reloads sync.WaitGroup
	mu      sync.Mutex
	urls    []gateway.URLInfo
}

// New returns a store for app and gw. Call Bind before the app bootstraps.
func New(app core.App, gw *gateway.Gateway, opts Options) *Store {
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = 15 * time.Second
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 15 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{app: app, gw: gw, opts: opts}
}

func (s *Store) logf(format string, args ...any) {
	if s.opts.Logger != nil {
		s.opts.Logger.Printf(format, args...)
	}
}

// Bind applies the schema on bootstrap, keeps saved URLs write-only, and
// reloads the gateway when URLs or tokens change.
func (s *Store) Bind() {
	s.app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return ApplySchema(e.App)
	})
	access := func(e *core.RecordEvent) error {
		if !s.started.Load() {
			return e.Next()
		}
		if err := s.LoadAccess(); err != nil {
			s.logf("reload tokens: %v", err)
		}
		return e.Next()
	}
	chains := func(e *core.RecordEvent) error {
		if !s.started.Load() {
			return e.Next()
		}
		// Chain checks call upstreams; don't hold up the admin's save.
		s.reloads.Add(1)
		go func() {
			defer s.reloads.Done()
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.CheckTimeout+5*time.Second)
			defer cancel()
			if err := s.LoadUpstreams(ctx); err != nil {
				s.logf("reload URLs: %v", err)
			}
		}()
		return e.Next()
	}
	// A URL holds its key. PocketBase shows hidden fields to
	// superusers; hide it from them too, in every API response and the
	// admin UI, so it can be written but never read back.
	s.app.OnRecordEnrich(URLs).BindFunc(func(e *core.RecordEnrichEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		e.Record.Hide("url")
		return nil
	})
	// The admin UI cannot show the URL, so its edit form sends it empty:
	// keep the saved one rather than wiping it.
	s.app.OnRecordUpdateRequest(URLs).BindFunc(func(e *core.RecordRequestEvent) error {
		if strings.TrimSpace(e.Record.GetString("url")) == "" {
			e.Record.Set("url", e.Record.Original().GetString("url"))
		}
		return e.Next()
	})
	s.app.OnRecordAfterCreateSuccess(Tokens).BindFunc(access)
	s.app.OnRecordAfterUpdateSuccess(Tokens).BindFunc(access)
	s.app.OnRecordAfterDeleteSuccess(Tokens).BindFunc(access)
	s.app.OnRecordAfterCreateSuccess(URLs).BindFunc(chains)
	s.app.OnRecordAfterUpdateSuccess(URLs).BindFunc(chains)
	s.app.OnRecordAfterDeleteSuccess(URLs).BindFunc(chains)
}

// WaitReloads blocks until chain reloads started by hooks have finished.
func (s *Store) WaitReloads() { s.reloads.Wait() }

// Start loads usage, tokens and URLs into the gateway.
func (s *Store) Start(ctx context.Context) error {
	if err := s.LoadUsage(); err != nil {
		return err
	}
	if err := s.LoadAccess(); err != nil {
		return err
	}
	s.started.Store(true)
	if err := s.LoadUpstreams(ctx); err != nil {
		return err
	}
	// A change saved while the URLs were checked is picked up here.
	return s.LoadAccess()
}

func jsonList(r *core.Record, field string) ([]string, error) {
	raw, _ := json.Marshal(r.Get(field))
	if string(raw) == "null" || string(raw) == `""` {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s must be a JSON array of strings", field)
	}
	return out, nil
}

// LoadAccess reloads the enabled tokens into the gateway.
func (s *Store) LoadAccess() error {
	recs, err := s.app.FindAllRecords(Tokens, dbx.HashExp{"enabled": true})
	if err != nil {
		return err
	}
	var tokens []gateway.Token
	for _, r := range recs {
		t := gateway.Token{ID: r.Id, Name: r.GetString("name"), Secret: r.GetString("token"),
			URLs: r.GetStringSlice("urls"), DailyBudget: r.GetInt("daily_budget")}
		if t.Methods, err = jsonList(r, "methods"); err != nil {
			// Deny rather than guess: the token stops working until fixed.
			s.logf("token %s refused: %v", t.Name, err)
			continue
		}
		tokens = append(tokens, t)
	}
	s.gw.SetTokens(tokens)
	return nil
}

// URLInfo returns what the status table shows of the saved URLs: names,
// chains and whether they are enabled. Never the URL.
func (s *Store) URLInfo() []gateway.URLInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gateway.URLInfo(nil), s.urls...)
}

// LoadUpstreams resolves every enabled URL, oldest first, checks them
// concurrently, and swaps them into the gateway. A failing URL is reported
// and not served, and tokens whose URL it is for a chain lose that chain
// until it passes: they never fall back to another token's URL.
func (s *Store) LoadUpstreams(ctx context.Context) error {
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	var recs []*core.Record
	if err := s.app.RecordQuery(URLs).OrderBy("created ASC", "id ASC").All(&recs); err != nil {
		return err
	}
	infos := make([]gateway.URLInfo, 0, len(recs))
	var ups []*gateway.Upstream
	var wg sync.WaitGroup
	for _, r := range recs {
		info := gateway.URLInfo{ID: r.Id, Name: r.GetString("name"), Chain: r.GetString("chain"), Enabled: r.GetBool("enabled")}
		infos = append(infos, info)
		if !info.Enabled {
			continue
		}
		c, ok := gateway.ChainByName(info.Chain)
		if !ok {
			s.logf("URL %s is for unknown chain %q; not served", info.Name, info.Chain)
			continue
		}
		up := gateway.ResolveUpstream(c, &gateway.Endpoint{ID: r.Id, Name: info.Name, URL: r.GetString("url")},
			s.opts.Client, s.opts.UpstreamTimeout)
		ups = append(ups, up)
		if up.Err != nil || !up.Configured() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			check, cancel := context.WithTimeout(ctx, s.opts.CheckTimeout)
			defer cancel()
			if up.Err = up.Check(check); up.Err == nil {
				up.Ready = true
			}
		}()
	}
	wg.Wait()
	s.gw.SetUpstreams(ups)
	s.mu.Lock()
	s.urls = infos
	s.mu.Unlock()
	for _, up := range ups {
		if up.Ready {
			s.logf("URL %s (%s) ready", up.Source, up.Chain.Name)
		} else {
			s.logf("URL %s (%s) not served: %v", up.Source, up.Chain.Name, up.Err)
		}
	}
	return nil
}

func (s *Store) today() string { return s.opts.Now().UTC().Format("2006-01-02") }

// LoadUsage seeds the gateway's counters with today's persisted totals, so
// daily budgets survive restarts.
func (s *Store) LoadUsage() error {
	recs, err := s.app.FindAllRecords(Usage, dbx.HashExp{"day": s.today()})
	if err != nil {
		return err
	}
	rows := make(map[gateway.UsageKey]gateway.Counts, len(recs))
	for _, r := range recs {
		k := gateway.UsageKey{Day: r.GetString("day"), TokenID: r.GetString("token_id"), TokenName: r.GetString("token_name"),
			Chain: r.GetString("chain"), URL: r.GetString("url"), Method: r.GetString("method")}
		rows[k] = gateway.Counts{Requests: r.GetInt64("requests"), Rejections: r.GetInt64("rejections"),
			UpstreamErrors: r.GetInt64("upstream_errors")}
	}
	s.gw.Usage().Seed(rows)
	return nil
}

// Flush adds the pending usage deltas to their rows, and moves tokens'
// last_used forward, in one transaction. On failure both go back to be
// retried at the next flush.
func (s *Store) Flush() error {
	deltas := s.gw.Usage().Drain()
	lastUsed := s.gw.Usage().DrainLastUsed()
	if len(deltas) == 0 && len(lastUsed) == 0 {
		return nil
	}
	err := s.app.RunInTransaction(func(tx core.App) error {
		// A plain UPDATE, not a record save: saving a token would fire the
		// hooks that reload every token, and bump its updated time.
		for id, t := range lastUsed {
			ts, _ := types.ParseDateTime(t.UTC())
			_, err := tx.DB().NewQuery("UPDATE {{" + Tokens + "}} SET [[last_used]] = {:t} WHERE [[id]] = {:id} AND ([[last_used]] = '' OR [[last_used]] < {:t})").
				Bind(dbx.Params{"t": ts.String(), "id": id}).Execute()
			if err != nil {
				return err
			}
		}
		col, err := tx.FindCollectionByNameOrId(Usage)
		if err != nil {
			return err
		}
		for k, d := range deltas {
			rec, err := tx.FindFirstRecordByFilter(col,
				"day = {:day} && token_id = {:token} && chain = {:chain} && url = {:url} && method = {:method}",
				dbx.Params{"day": k.Day, "token": k.TokenID, "chain": k.Chain, "url": k.URL, "method": k.Method})
			switch {
			case errors.Is(err, sql.ErrNoRows):
				rec = core.NewRecord(col)
				rec.Set("day", k.Day)
				rec.Set("token_id", k.TokenID)
				rec.Set("chain", k.Chain)
				rec.Set("url", k.URL)
				rec.Set("method", k.Method)
			case err != nil:
				return err
			}
			rec.Set("token_name", k.TokenName)
			rec.Set("requests", rec.GetInt64("requests")+d.Requests)
			rec.Set("rejections", rec.GetInt64("rejections")+d.Rejections)
			rec.Set("upstream_errors", rec.GetInt64("upstream_errors")+d.UpstreamErrors)
			if err := tx.Save(rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.gw.Usage().Restore(deltas)
		s.gw.Usage().RestoreLastUsed(lastUsed)
		return fmt.Errorf("flush usage: %w", err)
	}
	return nil
}

// Run flushes usage every FlushInterval until ctx ends, then once more.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil {
				s.logf("%v", err)
			}
			return
		case <-t.C:
			if err := s.Flush(); err != nil {
				s.logf("%v", err)
			}
		}
	}
}
