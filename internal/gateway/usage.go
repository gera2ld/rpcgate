package gateway

import (
	"sort"
	"sync"
	"time"
)

// UsageKey identifies one usage row: a token's calls to one method on one
// chain on one UTC day.
type UsageKey struct {
	Day       string // YYYY-MM-DD, UTC
	TokenID   string
	TokenName string
	Chain     string
	Method    string
}

// Counts are usage totals or deltas.
type Counts struct {
	Requests       int64 `json:"requests"`        // items forwarded upstream; these spend the budget
	Rejections     int64 `json:"rejections"`      // items the gateway refused
	UpstreamErrors int64 `json:"upstream_errors"` // forwarded items whose upstream call failed
}

func (c *Counts) add(d Counts) {
	c.Requests += d.Requests
	c.Rejections += d.Rejections
	c.UpstreamErrors += d.UpstreamErrors
}

// OtherMethod stands in for method names a token may not call, so a client
// sending made-up names cannot create unbounded usage rows.
const OtherMethod = "(other)"

// Usage counts calls in memory. Today's totals back the daily budgets and
// the status page; pending deltas wait for Drain to persist them.
type Usage struct {
	mu      sync.Mutex
	day     string
	totals  map[UsageKey]Counts
	spent   map[budgetKey]int64 // today's requests per token and chain
	pending map[UsageKey]Counts
	// lastUsed is each token's latest request since the last DrainLastUsed.
	lastUsed map[string]time.Time
}

type budgetKey struct{ tokenID, chain string }

// NewUsage returns empty counters.
func NewUsage() *Usage {
	return &Usage{totals: map[UsageKey]Counts{}, spent: map[budgetKey]int64{}, pending: map[UsageKey]Counts{},
		lastUsed: map[string]time.Time{}}
}

// rollLocked starts a new day's totals when day is newer. Pending deltas keep
// their own day, so a flush after midnight still lands on the right row.
func (u *Usage) rollLocked(day string) {
	if day > u.day {
		u.day = day
		u.totals = map[UsageKey]Counts{}
		u.spent = map[budgetKey]int64{}
	}
}

// Seed loads persisted totals, so budgets survive restarts. Rows for other
// days than the newest seen are ignored.
func (u *Usage) Seed(rows map[UsageKey]Counts) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, c := range rows {
		u.rollLocked(k.Day)
		if k.Day != u.day {
			continue
		}
		t := u.totals[k]
		t.add(c)
		u.totals[k] = t
		u.spent[budgetKey{k.TokenID, k.Chain}] += c.Requests
	}
}

func (u *Usage) addLocked(k UsageKey, d Counts) {
	u.rollLocked(k.Day)
	if k.Day == u.day {
		t := u.totals[k]
		t.add(d)
		u.totals[k] = t
		u.spent[budgetKey{k.TokenID, k.Chain}] += d.Requests
	}
	p := u.pending[k]
	p.add(d)
	u.pending[k] = p
}

// Add records d against k.
func (u *Usage) Add(k UsageKey, d Counts) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.addLocked(k, d)
}

// Spent returns the requests a token has made on a chain today.
func (u *Usage) Spent(day, tokenID, chain string) int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollLocked(day)
	if day != u.day {
		return 0
	}
	return u.spent[budgetKey{tokenID, chain}]
}

// trySpend records one request per method if the budget allows them all, and
// reports whether it did. budget 0 is unlimited.
func (u *Usage) trySpend(day string, tok *tokenEntry, chain string, methods []string, budget int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollLocked(day)
	if budget > 0 && u.spent[budgetKey{tok.id, chain}]+int64(len(methods)) > int64(budget) {
		return false
	}
	for _, m := range methods {
		u.addLocked(UsageKey{Day: day, TokenID: tok.id, TokenName: tok.name, Chain: chain, Method: m}, Counts{Requests: 1})
	}
	return true
}

// Drain returns and clears the pending deltas.
func (u *Usage) Drain() map[UsageKey]Counts {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.pending
	u.pending = map[UsageKey]Counts{}
	return out
}

// Restore puts deltas back after a failed flush, without counting them twice
// in today's totals.
func (u *Usage) Restore(deltas map[UsageKey]Counts) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, d := range deltas {
		p := u.pending[k]
		p.add(d)
		u.pending[k] = p
	}
}

// Touch records that a token made a request at t.
func (u *Usage) Touch(tokenID string, t time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if t.After(u.lastUsed[tokenID]) {
		u.lastUsed[tokenID] = t
	}
}

// DrainLastUsed returns and clears each token's latest request time.
func (u *Usage) DrainLastUsed() map[string]time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.lastUsed
	u.lastUsed = map[string]time.Time{}
	return out
}

// RestoreLastUsed puts times back after a failed flush.
func (u *Usage) RestoreLastUsed(times map[string]time.Time) {
	for id, t := range times {
		u.Touch(id, t)
	}
}

// MethodUsage is one method's totals for today.
type MethodUsage struct {
	Chain  string `json:"chain"`
	Method string `json:"method"`
	Counts
}

// Today returns a token's totals for day, sorted by chain then method.
func (u *Usage) Today(day, tokenID string) []MethodUsage {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rollLocked(day)
	var out []MethodUsage
	if day != u.day {
		return out
	}
	for k, c := range u.totals {
		if k.TokenID == tokenID {
			out = append(out, MethodUsage{Chain: k.Chain, Method: k.Method, Counts: c})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chain != out[j].Chain {
			return out[i].Chain < out[j].Chain
		}
		return out[i].Method < out[j].Method
	})
	return out
}
