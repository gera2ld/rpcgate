package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// URLInfo is what the status table shows of a saved URL: never the URL.
type URLInfo struct {
	Name    string
	Chain   string
	Enabled bool
	InUse   bool
}

func expected(c Chain) string {
	switch c.Family {
	case Solana, Helius:
		return c.GenesisHash
	case Ankr:
		return "-"
	}
	return strconv.FormatUint(c.ChainID, 10)
}

// WriteStatus prints which chains are served, by which URL, why any are not,
// the saved URLs by name, and each profile's limits. It never prints a URL
// or a token.
func WriteStatus(w io.Writer, listen string, ups []*Upstream, urls []URLInfo, profiles []Profile, tokens int) {
	fmt.Fprintf(w, "rpcgate on http://%s: proxy at /<token>/<chain>, usage at /dashboard, admin UI at /_/\n\n", listen)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  CHAIN\tFAMILY\tEXPECTED\tSTATUS\tURL")
	var notes []string
	for _, up := range ups {
		status, source := "off", up.Source
		switch {
		case up.Ready:
			status = "ready"
		case up.Source == "" && up.Err == nil:
			source = "add an enabled URL for it"
		default:
			status = "failed"
			if up.Err != nil {
				notes = append(notes, fmt.Sprintf("  %s: %v", up.Chain.Name, up.Err))
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", up.Chain.Name, up.Chain.Family, expected(up.Chain), status, source)
	}
	_ = tw.Flush()
	if len(ups) == 0 {
		fmt.Fprintln(w, "  (no chains; add them in the admin UI)")
	}
	if len(notes) > 0 {
		fmt.Fprintln(w)
		for _, n := range notes {
			fmt.Fprintln(w, n)
		}
	}

	fmt.Fprintln(w, "\nURLs (a chain uses its first enabled one):")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  CHAIN\tNAME\tENABLED\tIN USE")
	for _, u := range urls {
		enabled, inUse := "no", ""
		if u.Enabled {
			enabled = "yes"
		}
		if u.InUse {
			inUse = "yes"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", u.Chain, u.Name, enabled, inUse)
	}
	_ = tw.Flush()
	if len(urls) == 0 {
		fmt.Fprintln(w, "  (none; add them in the admin UI under urls)")
	}

	fmt.Fprintln(w, "\nProfiles (budgets are per token per chain):")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  PROFILE\tCHAINS\tBUDGET\tBATCH\tMETHODS")
	for _, p := range profiles {
		p = p.Normalize()
		budget := "unlimited"
		if p.DailyBudget > 0 {
			budget = fmt.Sprintf("%d/day", p.DailyBudget)
		}
		methods := "defaults"
		if len(p.Methods) > 0 {
			methods = strings.Join(p.Methods, ",")
		}
		chainList := strings.Join(p.Chains, ",")
		if chainList == "" {
			chainList = "(none)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\n", p.Name, chainList, budget, p.MaxBatch, methods)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nEnabled tokens: %d. Token status: GET http://%s/<token>/%s\n\n", tokens, listen, StatusPath)
}

// TokenStatus is the body of GET /<token>/status: only what that token can
// use, and its usage today.
type TokenStatus struct {
	Token   string        `json:"token"` // the token's name, not its secret
	Profile string        `json:"profile"`
	Day     string        `json:"day"`
	Limits  StatusLimits  `json:"limits"`
	Chains  []ChainStatus `json:"chains"`
	Usage   []MethodUsage `json:"usage"`
}

// StatusLimits are the token's profile limits.
type StatusLimits struct {
	DailyBudget int `json:"daily_budget"` // per chain; 0 = unlimited
	MaxBatch    int `json:"max_batch"`
}

// ChainStatus is one chain the token may call and the gateway serves.
type ChainStatus struct {
	Chain     string   `json:"chain"`
	Family    Family   `json:"family"`
	URL       string   `json:"url"` // the token's own URL for this chain
	UsedToday int64    `json:"used_today"`
	Remaining *int64   `json:"remaining_today,omitempty"`
	Methods   []string `json:"methods"`
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request, tok *tokenEntry) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET for status", http.StatusMethodNotAllowed)
		return
	}
	day := g.day()
	p := tok.profile
	st := TokenStatus{
		Token: tok.name, Profile: p.Name, Day: day,
		Limits: StatusLimits{p.DailyBudget, p.MaxBatch},
		Chains: []ChainStatus{},
		Usage:  g.usage.Today(day, tok.id),
	}
	if st.Usage == nil {
		st.Usage = []MethodUsage{}
	}
	base := requestOrigin(r) + "/" + url.PathEscape(tok.secret) + "/"
	chains := *g.chains.Load()
	names := append([]string(nil), p.Chains...)
	sort.Strings(names)
	for _, name := range names {
		up := chains[name]
		if up == nil || !up.Ready {
			continue // only chains a call would reach
		}
		methods := p.MethodsFor(up.Chain.Family)
		sort.Strings(methods)
		cs := ChainStatus{Chain: name, Family: up.Chain.Family, URL: base + name,
			UsedToday: g.usage.Spent(day, tok.id, name), Methods: methods}
		if p.DailyBudget > 0 {
			rem := max(int64(p.DailyBudget)-cs.UsedToday, 0)
			cs.Remaining = &rem
		}
		st.Chains = append(st.Chains, cs)
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
}

// requestOrigin is the scheme and host the client used, as told by a reverse
// proxy's X-Forwarded-Proto when there is one.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ","); p == "http" || p == "https" {
		scheme = p
	}
	return scheme + "://" + r.Host
}
