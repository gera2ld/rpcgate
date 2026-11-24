// Package store connects the gateway to PocketBase. PocketBase holds
// the chains' URLs (with the keys), profiles, tokens and daily usage; the
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

	"rpcgate/internal/gateway"
)

//go:embed collections.json
var snapshot []byte

// Collection names.
const (
	URLs     = "urls"
	Profiles = "profiles"
	Tokens   = "tokens"
	Usage    = "usage"
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

	started  atomic.Bool // hooks reload only after Start's own load
	chainMu  sync.Mutex  // serializes chain reloads
	reloads  sync.WaitGroup
	mu       sync.Mutex
	profiles []gateway.Profile
	tokens   int
	urls     []gateway.URLInfo
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
// reloads the gateway when URLs, profiles or tokens change.
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
			s.logf("reload tokens and profiles: %v", err)
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
			if err := s.LoadChains(ctx); err != nil {
				s.logf("reload chains: %v", err)
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
	for _, c := range []string{Tokens, Profiles} {
		s.app.OnRecordAfterCreateSuccess(c).BindFunc(access)
		s.app.OnRecordAfterUpdateSuccess(c).BindFunc(access)
		s.app.OnRecordAfterDeleteSuccess(c).BindFunc(access)
	}
	s.app.OnRecordAfterCreateSuccess(URLs).BindFunc(chains)
	s.app.OnRecordAfterUpdateSuccess(URLs).BindFunc(chains)
	s.app.OnRecordAfterDeleteSuccess(URLs).BindFunc(chains)
}

// WaitReloads blocks until chain reloads started by hooks have finished.
func (s *Store) WaitReloads() { s.reloads.Wait() }

// Start seeds defaults on an empty database, then loads usage, access and
// chains into the gateway.
func (s *Store) Start(ctx context.Context) error {
	if err := s.SeedDefaults(); err != nil {
		return err
	}
	if err := s.LoadUsage(); err != nil {
		return err
	}
	if err := s.LoadAccess(); err != nil {
		return err
	}
	s.started.Store(true)
	if err := s.LoadChains(ctx); err != nil {
		return err
	}
	// A change saved while the chains were checked is picked up here.
	return s.LoadAccess()
}

// SeedDefaults creates a "read-only" profile for every chain when the
// database has no profiles. It never creates a URL or a token.
func (s *Store) SeedDefaults() error {
	np, err := s.app.CountRecords(Profiles)
	if err != nil || np > 0 {
		return err
	}
	pcol, err := s.app.FindCollectionByNameOrId(Profiles)
	if err != nil {
		return err
	}
	p := core.NewRecord(pcol)
	p.Set("name", "read-only")
	p.Set("chains", gateway.ChainNames())
	p.Set("methods", []string{})
	p.Set("daily_budget", 200000)
	p.Set("max_batch", gateway.DefaultMaxBatch)
	if err := s.app.Save(p); err != nil {
		return fmt.Errorf("seed profile: %w", err)
	}
	s.logf("created profile read-only; add the chains' URLs in the admin UI")
	return nil
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

// LoadAccess reloads profiles and enabled tokens into the gateway.
func (s *Store) LoadAccess() error {
	profRecs, err := s.app.FindAllRecords(Profiles)
	if err != nil {
		return err
	}
	profiles := map[string]*gateway.Profile{}
	var list []gateway.Profile
	for _, r := range profRecs {
		p := &gateway.Profile{
			ID:          r.Id,
			Name:        r.GetString("name"),
			DailyBudget: r.GetInt("daily_budget"),
			MaxBatch:    r.GetInt("max_batch"),
		}
		p.Chains = r.GetStringSlice("chains")
		var err error
		if p.Methods, err = jsonList(r, "methods"); err != nil {
			// Deny rather than guess: its tokens stop working until fixed.
			s.logf("profile %s ignored: %v", p.Name, err)
			continue
		}
		profiles[r.Id] = p
		list = append(list, *p)
	}
	tokRecs, err := s.app.FindAllRecords(Tokens, dbx.HashExp{"enabled": true})
	if err != nil {
		return err
	}
	var tokens []gateway.Token
	for _, r := range tokRecs {
		p := profiles[r.GetString("profile")]
		if p == nil {
			s.logf("token %s has no usable profile; it is refused", r.GetString("name"))
			continue
		}
		tokens = append(tokens, gateway.Token{ID: r.Id, Name: r.GetString("name"), Secret: r.GetString("token"), Profile: p})
	}
	s.gw.SetTokens(tokens)
	s.mu.Lock()
	s.profiles, s.tokens = list, len(tokens)
	s.mu.Unlock()
	return nil
}

// Profiles returns the profiles last loaded, for the status table.
func (s *Store) Profiles() []gateway.Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gateway.Profile(nil), s.profiles...)
}

// URLInfo returns what the status table shows of the saved URLs: names,
// chains, and which are enabled and in use. Never the URL.
func (s *Store) URLInfo() []gateway.URLInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gateway.URLInfo(nil), s.urls...)
}

// TokenCount returns how many enabled tokens were loaded.
func (s *Store) TokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}

// LoadChains resolves every chain's upstream from its first enabled URL
// (oldest first), checks them concurrently, and swaps them into the
// gateway. A failing chain is reported and left out rather than stopping
// the others.
func (s *Store) LoadChains(ctx context.Context) error {
	s.chainMu.Lock()
	defer s.chainMu.Unlock()
	var urlRecs []*core.Record
	if err := s.app.RecordQuery(URLs).OrderBy("created ASC", "id ASC").All(&urlRecs); err != nil {
		return err
	}
	inUse := map[string]*gateway.Endpoint{} // chain name → its first enabled URL
	infos := make([]gateway.URLInfo, 0, len(urlRecs))
	for _, r := range urlRecs {
		chain := r.GetString("chain")
		info := gateway.URLInfo{Name: r.GetString("name"), Chain: chain, Enabled: r.GetBool("enabled")}
		if info.Enabled && inUse[chain] == nil && strings.TrimSpace(r.GetString("url")) != "" {
			inUse[chain] = &gateway.Endpoint{Name: info.Name, URL: r.GetString("url")}
			info.InUse = true
		}
		infos = append(infos, info)
	}
	ups := make([]*gateway.Upstream, 0, len(gateway.Chains))
	var wg sync.WaitGroup
	for _, c := range gateway.Chains {
		e := inUse[c.Name]
		if e != nil {
			c.Endpoint = e.Name
		}
		up := gateway.ResolveUpstream(c, e, s.opts.Client, s.opts.UpstreamTimeout)
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
	s.gw.SetChains(ups)
	s.mu.Lock()
	s.urls = infos
	s.mu.Unlock()
	for _, up := range ups {
		switch {
		case up.Ready:
			s.logf("chain %s ready (%s)", up.Chain.Name, up.Source)
		case up.Err != nil:
			s.logf("chain %s not served: %v", up.Chain.Name, up.Err)
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
			Chain: r.GetString("chain"), Method: r.GetString("method")}
		rows[k] = gateway.Counts{Requests: r.GetInt64("requests"), Rejections: r.GetInt64("rejections"),
			UpstreamErrors: r.GetInt64("upstream_errors")}
	}
	s.gw.Usage().Seed(rows)
	return nil
}

// Flush adds the pending usage deltas to their rows in one transaction. On
// failure the deltas go back to be retried at the next flush.
func (s *Store) Flush() error {
	deltas := s.gw.Usage().Drain()
	if len(deltas) == 0 {
		return nil
	}
	err := s.app.RunInTransaction(func(tx core.App) error {
		col, err := tx.FindCollectionByNameOrId(Usage)
		if err != nil {
			return err
		}
		for k, d := range deltas {
			rec, err := tx.FindFirstRecordByFilter(col,
				"day = {:day} && token_id = {:token} && chain = {:chain} && method = {:method}",
				dbx.Params{"day": k.Day, "token": k.TokenID, "chain": k.Chain, "method": k.Method})
			switch {
			case errors.Is(err, sql.ErrNoRows):
				rec = core.NewRecord(col)
				rec.Set("day", k.Day)
				rec.Set("token_id", k.TokenID)
				rec.Set("chain", k.Chain)
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
