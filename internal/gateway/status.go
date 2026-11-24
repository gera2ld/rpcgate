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
	ID      string
	Name    string
	Chain   string
	Enabled bool
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

// WriteStatus prints each saved URL's state, then each token's chains and
// the URL each one uses, by name. It never prints a URL or a token.
func (g *Gateway) WriteStatus(w io.Writer, listen string, urls []URLInfo) {
	fmt.Fprintf(w, "rpcgate on http://%s: proxy at /<token>/<chain>, usage at /dashboard, admin UI at /_/\n\n", listen)
	byID := map[string]*Upstream{}
	for _, u := range g.Upstreams() {
		byID[u.ID] = u
	}

	fmt.Fprintln(w, "URLs (a token uses the first enabled one of its URLs for each chain):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  CHAIN\tNAME\tEXPECTED\tSTATUS")
	var notes []string
	for _, info := range urls {
		expect, status := "-", "disabled"
		if c, ok := ChainByName(info.Chain); ok {
			expect = expected(c)
		}
		if up := byID[info.ID]; up != nil {
			status = "ready"
			if !up.Ready {
				status = "failed"
				if up.Err != nil {
					notes = append(notes, fmt.Sprintf("  %s (%s): %v", info.Name, info.Chain, up.Err))
				}
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", info.Chain, info.Name, expect, status)
	}
	_ = tw.Flush()
	if len(urls) == 0 {
		fmt.Fprintln(w, "  (none; add them in the admin UI under urls)")
	}
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}

	fmt.Fprintln(w, "\nTokens (budgets are per chain per day):")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TOKEN\tBUDGET\tMETHODS\tCHAINS")
	toks := g.access.Load().list
	for _, tok := range toks {
		methods := "defaults"
		if len(tok.patterns) > 0 {
			methods = strings.Join(tok.patterns, ",")
		}
		var chains []string
		for _, c := range Chains {
			up := g.routeFor(tok, c.Name)
			switch {
			case up == nil:
			case up.Ready:
				chains = append(chains, fmt.Sprintf("%s (%s)", c.Name, up.Source))
			default:
				chains = append(chains, fmt.Sprintf("%s (%s, failed)", c.Name, up.Source))
			}
		}
		chainList := strings.Join(chains, ", ")
		if chainList == "" {
			chainList = "(none; select its URLs in the admin UI)"
		}
		budget := "unlimited"
		if tok.budget > 0 {
			budget = fmt.Sprintf("%d/day", tok.budget)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", tok.name, budget, methods, chainList)
	}
	_ = tw.Flush()
	if len(toks) == 0 {
		fmt.Fprintln(w, "  (no enabled tokens)")
	}
	fmt.Fprintf(w, "\nToken status: GET http://%s/<token>/%s\n\n", listen, StatusPath)
}

// TokenStatus is the body of GET /<token>/status: only what that token can
// use, and its usage today.
type TokenStatus struct {
	Token  string        `json:"token"` // the token's name, not its secret
	Day    string        `json:"day"`
	Limits StatusLimits  `json:"limits"`
	Chains []ChainStatus `json:"chains"`
	Usage  []MethodUsage `json:"usage"`
}

// StatusLimits are the token's limits.
type StatusLimits struct {
	DailyBudget int `json:"daily_budget"` // per chain per UTC day; 0 = unlimited
	MaxBatch    int `json:"max_batch"`
}

// ChainStatus is one chain the token may call and the gateway serves.
type ChainStatus struct {
	Chain     string   `json:"chain"`
	Family    Family   `json:"family"`
	URL       string   `json:"url"` // the token's own URL for this chain
	Via       string   `json:"via"` // the name of the upstream URL it uses
	UsedToday int64    `json:"used_today"`
	Remaining *int64   `json:"remaining_today,omitempty"` // absent when unlimited
	Methods   []string `json:"methods"`
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request, tok *tokenEntry) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET for status", http.StatusMethodNotAllowed)
		return
	}
	day := g.day()
	st := TokenStatus{
		Token: tok.name, Day: day,
		Limits: StatusLimits{tok.budget, MaxBatch},
		Chains: []ChainStatus{},
		Usage:  g.usage.Today(day, tok.id),
	}
	if st.Usage == nil {
		st.Usage = []MethodUsage{}
	}
	base := requestOrigin(r) + "/" + url.PathEscape(tok.secret) + "/"
	for _, c := range Chains {
		up := g.routeFor(tok, c.Name)
		if up == nil || !up.Ready {
			continue // only chains a call would reach
		}
		methods := methodsFor(tok.patterns, c.Family)
		sort.Strings(methods)
		used := g.usage.Spent(day, tok.id, c.Name)
		st.Chains = append(st.Chains, ChainStatus{Chain: c.Name, Family: c.Family, URL: base + c.Name, Via: up.Source,
			UsedToday: used, Remaining: remaining(tok.budget, used), Methods: methods})
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

func remaining(budget int, used int64) *int64 {
	if budget <= 0 {
		return nil
	}
	r := max(int64(budget)-used, 0)
	return &r
}
