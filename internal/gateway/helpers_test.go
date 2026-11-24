package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	secretKey = "sk_live_abcdef123456"
	// tokenA and tokenB are client tokens; tests also check they never
	// appear in logs or status.
	tokenA = "tokA0123456789abcdefghijklmnop"
	tokenB = "tokB0123456789abcdefghijklmnop"

	solanaGenesis = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
)

// upstream is a fake RPC at a URL whose path holds a key. For EVM it answers
// eth_blockNumber with 0x2710 (10,000) and eth_chainId with 0x38 (56); for
// Solana, getGenesisHash with mainnet's hash. It counts requests.
type upstream struct {
	srv   *httptest.Server
	calls atomic.Int64
	reply func(body []byte) (int, string)
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		if r.URL.Path != "/v2/"+secretKey {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if u.reply != nil {
			status, out := u.reply(body)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, out)
			return
		}
		_, _ = io.WriteString(w, answer(body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) url() string { return u.srv.URL + "/v2/" + secretKey }

func answer(body []byte) string {
	one := func(r request) string {
		id := string(r.ID)
		if id == "" {
			id = "null"
		}
		result := `"0x"`
		switch r.Method {
		case "eth_blockNumber":
			result = `"0x2710"`
		case "eth_chainId":
			result = `"0x38"`
		case "eth_getLogs":
			result = `[]`
		case "getGenesisHash":
			result = `"` + solanaGenesis + `"`
		case "getSlot":
			result = `123`
		}
		return `{"jsonrpc":"2.0","id":` + id + `,"result":` + result + `}`
	}
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var reqs []request
		_ = json.Unmarshal(body, &reqs)
		parts := make([]string, len(reqs))
		for i, r := range reqs {
			parts[i] = one(r)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	var r request
	_ = json.Unmarshal(body, &r)
	return one(r)
}

var (
	bscChain    = Chain{Name: "bsc", Family: EVM, ChainID: 56}
	solanaChain = Chain{Name: "solana", Family: Solana, GenesisHash: solanaGenesis}
)

// endpoint is a test URL entry for rawURL.
func endpoint(rawURL string) *Endpoint { return &Endpoint{Name: "test", URL: rawURL} }

// ready resolves c to rawURL and marks it ready without a check.
func ready(t *testing.T, c Chain, rawURL string) *Upstream {
	t.Helper()
	up := ResolveUpstream(c, endpoint(rawURL), nil, 0)
	if up.Err != nil {
		t.Fatal(up.Err)
	}
	up.Ready = true
	return up
}

// Settings is a test shorthand for a token's settings: Chains become the
// token's URLs, as every test upstream's ID is its chain's name.
type Settings struct {
	Name        string
	Chains      []string
	Methods     []string
	DailyBudget int
}

func tok(id, name, secret string, p *Settings) Token {
	return Token{ID: id, Name: name, Secret: secret, URLs: p.Chains, Methods: p.Methods, DailyBudget: p.DailyBudget}
}

// newGateway serves g over HTTP. Upstreams without an ID get their chain's
// name as one.
func newGateway(t *testing.T, opts Options, ups []*Upstream, tokens ...Token) (*Gateway, *httptest.Server) {
	t.Helper()
	g := New(opts)
	for _, u := range ups {
		if u.ID == "" {
			u.ID = u.Chain.Name
		}
	}
	g.SetUpstreams(ups)
	g.SetTokens(tokens)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

// newProxy is a single-chain proxy: tokenA on bsc with settings p.
// It returns the URL to call.
func newProxy(t *testing.T, upURL string, p Settings) string {
	t.Helper()
	if p.Chains == nil {
		p.Chains = []string{"bsc"}
	}
	_, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, upURL)},
		tok("a", "indexer", tokenA, &p))
	return srv.URL + "/" + tokenA + "/bsc"
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func newLogger(w io.Writer) *log.Logger { return log.New(w, "", 0) }

// isLogLimitError is a typical client's adaptive-log-range classifier, so the
// gateway's errors keep classifying the way such clients expect.
func isLogLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, phrase := range []string{"rate limit", "rate-limit", "too many requests", "429", "capacity", "quota"} {
		if strings.Contains(message, phrase) {
			return false
		}
	}
	var rpcErr interface{ ErrorCode() int }
	if errors.As(err, &rpcErr) && rpcErr.ErrorCode() == -32005 {
		return true
	}
	for _, phrase := range []string{"block range", "range too large", "range is too large", "range limit",
		"exceed", "too many", "more than", "limit", "response size", "too large", "timeout", "timed out"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}
