// Package gateway is a JSON-RPC gateway that keeps upstream RPC URLs (which
// usually embed an API key) inside the process, and limits what each client
// token can spend through it.
//
// Clients call /<token>/<chain>. Each token names the upstream URLs it may
// use, and a call goes to the first enabled one of them for that chain. The
// gateway forwards only the methods the token allows, caps batch sizes,
// enforces a daily budget per token and chain, and never returns an upstream
// URL in a response, status page or log line.
//
// The gateway has no storage of its own: callers give it tokens and URLs
// with SetTokens and SetUpstreams, seed today's usage with Usage().Seed, and
// persist Usage().Drain.
package gateway

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// StatusPath is the per-token status page: GET /<token>/status.
const StatusPath = "status"

// Options configures a Gateway. Zero values take defaults.
type Options struct {
	MaxBodyBytes int64
	Logger       *log.Logger // nil disables logging
	Now          func() time.Time
}

// Gateway is an http.Handler serving /<token>/<chain> and /<token>/status.
type Gateway struct {
	opts   Options
	access atomic.Pointer[access]
	ups    atomic.Pointer[[]*Upstream] // enabled URLs, oldest first
	usage  *Usage
}

// New returns a gateway with no tokens and no chains.
func New(opts Options) *Gateway {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	g := &Gateway{opts: opts, usage: NewUsage()}
	g.access.Store(newAccess(nil))
	g.ups.Store(&[]*Upstream{})
	return g
}

// Usage returns the gateway's usage counters.
func (g *Gateway) Usage() *Usage { return g.usage }

// SetTokens replaces the enabled tokens. Tokens shorter than MinTokenLength
// are ignored.
func (g *Gateway) SetTokens(tokens []Token) { g.access.Store(newAccess(tokens)) }

// SetUpstreams replaces the enabled URLs, oldest first: a token uses the
// first of its URLs for a chain. Only Ready ones are served.
func (g *Gateway) SetUpstreams(ups []*Upstream) {
	cp := slices.Clone(ups)
	g.ups.Store(&cp)
}

// Upstreams returns the enabled URLs, oldest first.
func (g *Gateway) Upstreams() []*Upstream { return slices.Clone(*g.ups.Load()) }

// routeFor returns the upstream tok uses for chain: the first enabled URL
// among tok's that serves it, ready or not, or nil when tok has none.
func (g *Gateway) routeFor(tok *tokenEntry, chain string) *Upstream {
	for _, u := range *g.ups.Load() {
		if tok.urls[u.ID] && u.Chain.Name == chain {
			return u
		}
	}
	return nil
}

func (g *Gateway) day() string { return g.opts.Now().UTC().Format("2006-01-02") }

// scrub removes every upstream's secrets, not only the one that answered: a
// key is often shared by several URLs.
func (g *Gateway) scrub(s string) string {
	for _, u := range *g.ups.Load() {
		s = u.scrub(s)
	}
	return s
}

func (g *Gateway) logf(format string, args ...any) { g.tlogf(nil, format, args...) }

// tlogf logs a line about tok's request. Besides upstream secrets it scrubs
// tok's own secret, which a client could put in a method name.
func (g *Gateway) tlogf(tok *tokenEntry, format string, args ...any) {
	if g.opts.Logger == nil {
		return
	}
	line := g.scrub(fmt.Sprintf(format, args...))
	if tok != nil && tok.secret != "" {
		line = strings.ReplaceAll(line, tok.secret, "[token]")
	}
	g.opts.Logger.Printf("%s", line)
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, http.StatusNotFound, nil, codeInvalid, "not found; call /<token>/<chain>")
		return
	}
	tok := g.access.Load().lookup(parts[0])
	if tok == nil {
		// Log no path segment: a client that swapped them would log its token.
		g.logf("reject: unknown or disabled token")
		writeError(w, http.StatusUnauthorized, nil, codeUnauthorized, "unauthorized")
		return
	}
	g.usage.Touch(tok.id, g.opts.Now())
	if parts[1] == StatusPath {
		g.serveStatus(w, r, tok)
		return
	}
	up := g.routeFor(tok, parts[1])
	if up == nil || !up.Ready {
		writeError(w, http.StatusNotFound, nil, codeInvalid,
			"chain not available to this token; available: "+strings.Join(g.tokenChains(tok), ", "))
		return
	}
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "WebSocket subscriptions are not supported", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "only POST is accepted", http.StatusMethodNotAllowed)
		return
	}
	g.serveRPC(w, r, tok, up)
}

// tokenChains lists the chains tok can call: those whose URL for tok is
// ready.
func (g *Gateway) tokenChains(tok *tokenEntry) []string {
	var out []string
	for _, c := range Chains {
		if u := g.routeFor(tok, c.Name); u != nil && u.Ready {
			out = append(out, c.Name)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func (g *Gateway) serveRPC(w http.ResponseWriter, r *http.Request, tok *tokenEntry, up *Upstream) {
	start := g.opts.Now()
	chainName := up.Chain.Name
	prefix := tok.name + " " + chainName
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.opts.MaxBodyBytes))
	if err != nil {
		g.countReject(tok, up, []request{{}})
		writeError(w, http.StatusRequestEntityTooLarge, nil, codeInvalid, "request body too large")
		return
	}
	reqs, batch, msg, code := parseBody(body, MaxBatch)
	if msg != "" {
		g.tlogf(tok, "%s reject: %s", prefix, msg)
		g.countReject(tok, up, []request{{}})
		writeError(w, http.StatusOK, nil, code, msg)
		return
	}

	// Validate everything before spending budget or touching upstream.
	for _, req := range reqs {
		if msg, code := g.check(tok, up, req); msg != "" {
			g.tlogf(tok, "%s reject %s: %s", prefix, safeMethod(req.Method), msg)
			g.countReject(tok, up, reqs)
			reject(w, http.StatusOK, reqs, batch, code, msg)
			return
		}
	}
	if msg := g.spend(tok, up, usageMethods(tok, up, reqs)); msg != "" {
		g.tlogf(tok, "%s reject %s: %s", prefix, methodList(reqs), msg)
		g.countReject(tok, up, reqs)
		reject(w, http.StatusTooManyRequests, reqs, batch, codeLimit, msg)
		return
	}

	status, respBody, err := up.forward(r.Context(), bytes.TrimSpace(body))
	if err != nil || status != http.StatusOK {
		g.countUpstreamErrors(tok, up, reqs)
	}
	if err != nil {
		g.tlogf(tok, "%s upstream failure for %s: %s", prefix, methodList(reqs), err.Error())
		reject(w, http.StatusBadGateway, reqs, batch, codeUpstream, "upstream request failed")
		return
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(g.scrub(string(respBody))))
	g.tlogf(tok, "%s %s -> %d in %s", prefix, methodList(reqs), status, g.opts.Now().Sub(start).Round(time.Millisecond))
}

// usageMethods names each request for usage rows: its method when allowed
// and shaped like a method name, OtherMethod otherwise. With a "*" pattern a
// client picks any name, so odd ones are not given rows of their own.
func usageMethods(tok *tokenEntry, up *Upstream, reqs []request) []string {
	out := make([]string, len(reqs))
	allowed := tok.methods[up.Chain.Family]
	for i, r := range reqs {
		if allowed.allows(r.Method) && plainMethod(r.Method) {
			out[i] = r.Method
		} else {
			out[i] = OtherMethod
		}
	}
	return out
}

func (g *Gateway) countReject(tok *tokenEntry, up *Upstream, reqs []request) {
	day := g.day()
	for _, m := range usageMethods(tok, up, reqs) {
		g.usage.Add(usageKey(day, tok, up, m), Counts{Rejections: 1})
	}
}

func (g *Gateway) countUpstreamErrors(tok *tokenEntry, up *Upstream, reqs []request) {
	day := g.day()
	for _, m := range usageMethods(tok, up, reqs) {
		g.usage.Add(usageKey(day, tok, up, m), Counts{UpstreamErrors: 1})
	}
}

func usageKey(day string, tok *tokenEntry, up *Upstream, method string) UsageKey {
	return UsageKey{Day: day, TokenID: tok.id, TokenName: tok.name, Chain: up.Chain.Name, URL: up.Source, Method: method}
}

// check returns a rejection message and code, or "" when req may be forwarded.
func (g *Gateway) check(tok *tokenEntry, up *Upstream, req request) (string, int) {
	if !tok.methods[up.Chain.Family].allows(req.Method) {
		return fmt.Sprintf("method %q is not allowed for this token", safeMethod(req.Method)), codeMethod
	}
	return "", 0
}

// spend takes one request per method from the token's daily budget on this
// chain. It returns a rejection message, worded so clients with an adaptive
// log range read it as a reason to back off.
func (g *Gateway) spend(tok *tokenEntry, up *Upstream, methods []string) string {
	keys := make([]UsageKey, len(methods))
	day := g.day()
	for i, m := range methods {
		keys[i] = usageKey(day, tok, up, m)
	}
	if !g.usage.trySpend(day, tok.id, up.Chain.Name, keys, tok.budget) {
		return fmt.Sprintf("daily quota of %d requests used up (resets 00:00 UTC)", tok.budget)
	}
	return ""
}

// plainMethod reports whether m looks like a JSON-RPC method name: 1 to 64
// letters, digits and underscores.
func plainMethod(m string) bool {
	if m == "" || len(m) > 64 {
		return false
	}
	for _, r := range m {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}
