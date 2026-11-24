package gateway

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

var pool = common.HexToAddress("0x46Cf1cF8c69595804ba91dFdd8d6b960c9B0a7C4")

func TestForwardsAllowedMethodsThroughEthclient(t *testing.T) {
	up := newUpstream(t)
	client, err := ethclient.Dial(newProxy(t, up.url(), Settings{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := client.ChainID(ctx)
	if err != nil || id.Int64() != 56 {
		t.Fatalf("ChainID = %v, %v", id, err)
	}
	logs, err := client.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: big.NewInt(1000), ToBlock: big.NewInt(5999), Addresses: []common.Address{pool},
	})
	if err != nil || len(logs) != 0 {
		t.Fatalf("FilterLogs = %v, %v", logs, err)
	}
}

func TestRejectsMethodsOutsideTheAllowlist(t *testing.T) {
	up := newUpstream(t)
	proxy := newProxy(t, up.url(), Settings{})
	for _, method := range []string{"eth_sendRawTransaction", "eth_sendTransaction", "eth_accounts", "eth_subscribe",
		"debug_traceTransaction", "admin_peers", "getSlot"} {
		_, body := post(t, proxy, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":[]}`)
		if !strings.Contains(body, "not allowed") {
			t.Errorf("%s: got %s", method, body)
		}
	}
	if n := up.calls.Load(); n != 0 {
		t.Fatalf("upstream received %d calls for rejected methods", n)
	}
}

func TestLogsPassThrough(t *testing.T) {
	up := newUpstream(t)
	proxy := newProxy(t, up.url(), Settings{})
	// No address, a wide range: the gateway leaves eth_getLogs to the
	// provider, so it costs one upstream call and no head lookup.
	_, body := post(t, proxy, `{"jsonrpc":"2.0","id":7,"method":"eth_getLogs","params":[{"fromBlock":"earliest","toBlock":"latest"}]}`)
	if !strings.Contains(body, `"result"`) || up.calls.Load() != 1 {
		t.Fatalf("eth_getLogs: %s, %d upstream calls", body, up.calls.Load())
	}
}

// A client's adaptive log range must see the gateway's quota rejection as
// something to back off from, not a range to shrink.
func TestErrorsClassifyForLogRange(t *testing.T) {
	up := newUpstream(t)
	ctx := context.Background()
	q := ethereum.FilterQuery{FromBlock: big.NewInt(1), ToBlock: big.NewInt(2), Addresses: []common.Address{pool}}

	client, _ := ethclient.Dial(newProxy(t, up.url(), Settings{DailyBudget: 1}))
	if _, err := client.FilterLogs(ctx, q); err != nil {
		t.Fatal(err)
	}
	_, err := client.FilterLogs(ctx, q)
	if err == nil || !strings.Contains(err.Error(), "quota") || isLogLimitError(err) {
		t.Fatalf("budget rejection %v should be a non-log-limit quota error", err)
	}
}

func TestBatchLimitsAndBudgetCountItems(t *testing.T) {
	up := newUpstream(t)
	proxy := newProxy(t, up.url(), Settings{DailyBudget: MaxBatch + 10})
	batch := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_chainId","params":[]}`, i+1)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	if _, body := post(t, proxy, batch(MaxBatch+1)); !strings.Contains(body, "exceeds") {
		t.Fatalf("oversized batch: %s", body)
	}
	if _, body := post(t, proxy, batch(MaxBatch)); strings.Count(body, `"0x38"`) != MaxBatch {
		t.Fatalf("full batch: %s", body)
	}
	status, body := post(t, proxy, batch(MaxBatch)) // 50 + 50 > 60
	if status != http.StatusTooManyRequests || !strings.Contains(body, "quota") || !strings.HasPrefix(body, "[") {
		t.Fatalf("over budget: %d %s", status, body)
	}
	// A rejected item in a batch rejects the whole batch before upstream sees it.
	before := up.calls.Load()
	_, body = post(t, proxy, `[{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]},{"jsonrpc":"2.0","id":2,"method":"eth_sendRawTransaction","params":["0x00"]}]`)
	if !strings.Contains(body, "not allowed") || up.calls.Load() != before {
		t.Fatalf("mixed batch: %s", body)
	}
}

func TestBudgetResetsEachUTCDay(t *testing.T) {
	up := newUpstream(t)
	now := time.Date(2026, 11, 24, 23, 59, 0, 0, time.UTC)
	p := Settings{Chains: []string{"bsc"}, DailyBudget: 1}
	_, srv := newGateway(t, Options{Now: func() time.Time { return now }}, []*Upstream{ready(t, bscChain, up.url())},
		tok("a", "indexer", tokenA, &p))
	proxy := srv.URL + "/" + tokenA + "/bsc"
	req := `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`
	if _, body := post(t, proxy, req); !strings.Contains(body, "0x38") {
		t.Fatal(body)
	}
	if _, body := post(t, proxy, req); !strings.Contains(body, "quota") {
		t.Fatal(body)
	}
	now = now.Add(2 * time.Minute)
	if _, body := post(t, proxy, req); !strings.Contains(body, "0x38") {
		t.Fatalf("after midnight: %s", body)
	}
}

func TestNeverRevealsUpstreamURL(t *testing.T) {
	up := newUpstream(t)
	req := `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`

	// An upstream that echoes its own URL or key in an error.
	up.reply = func([]byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"key ` + secretKey + ` invalid at ` + up.url() + `"}}`
	}
	if _, body := post(t, newProxy(t, up.url(), Settings{}), req); strings.Contains(body, secretKey) || !strings.Contains(body, "[redacted]") {
		t.Fatalf("echoed key not scrubbed: %s", body)
	}

	// A redirect is not followed, and its Location is not passed on.
	up.reply = nil
	redirect := httptest.NewServer(http.RedirectHandler("http://example.invalid/"+secretKey, http.StatusFound))
	defer redirect.Close()
	status, body := post(t, newProxy(t, redirect.URL+"/v2/"+secretKey, Settings{}), req)
	if status != http.StatusBadGateway || strings.Contains(body, secretKey) {
		t.Fatalf("redirect: %d %s", status, body)
	}

	// A transport error (*url.Error carries the URL) is reduced to a generic message.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/v2/" + secretKey
	dead.Close()
	var logged strings.Builder
	p := Settings{Chains: []string{"bsc"}}
	_, srv := newGateway(t, Options{Logger: newLogger(&logged)}, []*Upstream{ready(t, bscChain, deadURL)},
		tok("a", "indexer", tokenA, &p))
	status, body = post(t, srv.URL+"/"+tokenA+"/bsc", req)
	if status != http.StatusBadGateway || strings.Contains(body, secretKey) || strings.Contains(body, dead.URL) {
		t.Fatalf("transport error: %d %s", status, body)
	}
	if strings.Contains(logged.String(), secretKey) || strings.Contains(logged.String(), tokenA) {
		t.Fatalf("log reveals a secret: %s", logged.String())
	}

	// A malformed URL is rejected without being echoed.
	bad := ResolveUpstream(bscChain, endpoint("ftp://"+secretKey), nil, 0)
	if bad.Err == nil || strings.Contains(bad.Err.Error(), secretKey) {
		t.Fatalf("resolve error: %v", bad.Err)
	}
}

func TestRejectsNonPost(t *testing.T) {
	up := newUpstream(t)
	if status, _ := get(t, newProxy(t, up.url(), Settings{})); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", status)
	}
}

func TestResolveUpstream(t *testing.T) {
	const key = "0123456789abcdef"
	up := ResolveUpstream(bscChain, &Endpoint{Name: "ankr", URL: " https://rpc.ankr.com/bsc/" + key + " "}, nil, 0)
	if up.Err != nil || up.url != "https://rpc.ankr.com/bsc/"+key || up.Source != "ankr" {
		t.Fatalf("url: %+v", up)
	}

	// Every URL is used as it is, the multichain one included.
	multi := &Endpoint{Name: "ankr", URL: "https://rpc.ankr.com/multichain/" + key}
	if up := ResolveUpstream(bscChain, multi, nil, 0); up.url != multi.URL {
		t.Fatalf("multichain URL rewritten: %+v", up)
	}

	// No URL: present but unconfigured.
	for _, e := range []*Endpoint{nil, {Name: "ankr"}, {Name: "ankr", URL: "  "}} {
		if up := ResolveUpstream(bscChain, e, nil, 0); up.Configured() || up.Err != nil {
			t.Fatalf("unset %v: %+v", e, up)
		}
	}

	// A malformed URL is rejected by name, never echoed.
	for _, raw := range []string{"rpc.ankr.com/bsc/" + key, "ftp://x/" + key, "https:///" + key} {
		up := ResolveUpstream(bscChain, &Endpoint{Name: "ankr", URL: raw}, nil, 0)
		if up.Err == nil || up.Configured() || strings.Contains(up.Err.Error(), key) || !strings.Contains(up.Err.Error(), "ankr") {
			t.Errorf("%s: %v", raw, up.Err)
		}
	}

	if up := ResolveUpstream(Chain{Name: "status", Family: EVM, ChainID: 1}, endpoint("https://x.example"), nil, 0); up.Err == nil {
		t.Fatal(`chain named "status" accepted`)
	}
}

func TestSecretPartsSparesRouteWords(t *testing.T) {
	u := ResolveUpstream(solanaChain, &Endpoint{Name: "ankr", URL: "https://rpc.ankr.com/solana/0123456789abcdef"}, nil, 0)
	got := u.scrub(`{"program":"solana multichain arbitrum","key":"0123456789abcdef"}`)
	if got != `{"program":"solana multichain arbitrum","key":"[redacted]"}` {
		t.Fatalf("scrub = %s", got)
	}
}

func TestCheckChainID(t *testing.T) {
	up := newUpstream(t) // answers 0x38
	ctx := context.Background()
	u := ResolveUpstream(bscChain, endpoint(up.url()), nil, 0)
	if err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	arb := ResolveUpstream(Chain{Name: "arbitrum", Family: EVM, ChainID: 42161}, endpoint(up.url()), nil, 0)
	if err := arb.Check(ctx); err == nil || !strings.Contains(err.Error(), "chain 56, expected 42161") {
		t.Fatalf("mismatch: %v", err)
	}

	up.reply = func([]byte) (int, string) { return http.StatusUnauthorized, "bad key " + secretKey }
	if err := u.Check(ctx); err == nil || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("unauthorized: %v", err)
	}
}

func TestWriteStatus(t *testing.T) {
	const key = "0123456789abcdef"
	up := func(id, name, rawURL string, c Chain) *Upstream {
		return ResolveUpstream(c, &Endpoint{ID: id, Name: name, URL: rawURL}, nil, 0)
	}
	bscC := Chain{Name: "bsc", Family: EVM, ChainID: 56}
	fe := up("1", "ankr-fe", "https://rpc.ankr.com/bsc/"+key, bscC)
	fe.Ready = true
	be := up("2", "ankr-be", "https://rpc.ankr.com/bsc/be"+key, bscC)
	be.Ready = true
	arb := up("4", "ankr-be-arb", "https://rpc.ankr.com/arbitrum/be"+key, Chain{Name: "arbitrum", Family: EVM, ChainID: 42161})
	arb.Err = errors.New("upstream is chain 56, expected 42161")
	sol := up("5", "node", "https://sol.example/"+key, Chain{Name: "solana", Family: Solana, GenesisHash: solanaGenesis})
	sol.Ready = true
	g := New(Options{})
	g.SetUpstreams([]*Upstream{fe, be, arb, sol})
	g.SetTokens([]Token{
		{ID: "a", Name: "frontend", Secret: tokenA, URLs: []string{"1", "3", "5"}},
		{ID: "b", Name: "backend", Secret: tokenB, URLs: []string{"2", "4"}, DailyBudget: 100, Methods: []string{"eth_*"}},
		{ID: "c", Name: "idle", Secret: "idle0123456789abcdefghijkl"},
	})
	var b strings.Builder
	g.WriteStatus(&b, "0.0.0.0:8545", []URLInfo{
		{ID: "1", Name: "ankr-fe", Chain: "bsc", Enabled: true}, {ID: "2", Name: "ankr-be", Chain: "bsc", Enabled: true},
		{ID: "3", Name: "old", Chain: "bsc"}, {ID: "4", Name: "ankr-be-arb", Chain: "arbitrum", Enabled: true},
		{ID: "5", Name: "node", Chain: "solana", Enabled: true},
	})
	out := b.String()
	for _, want := range []string{
		"rpcgate on http://0.0.0.0:8545: proxy at /<token>/<chain>, usage at /dashboard, admin UI at /_/",
		"  bsc       ankr-fe      56", "ready", "  bsc       old          56", "disabled",
		"  arbitrum  ankr-be-arb  42161", "failed", "ankr-be-arb (arbitrum): upstream is chain 56, expected 42161",
		"frontend  200000/day  defaults  bsc (ankr-fe), solana (node)",
		"backend   100/day     eth_*     bsc (ankr-be), arbitrum (ankr-be-arb, failed)",
		"idle      200000/day  defaults  (none; select its URLs in the admin UI)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, key) || strings.Contains(out, "sol.example") || strings.Contains(out, "rpc.ankr.com") || strings.Contains(out, tokenA) {
		t.Fatalf("status reveals a secret:\n%s", out)
	}
}
