package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const chainIDCall = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`

func TestTokenAuth(t *testing.T) {
	up := newUpstream(t)
	sol := newUpstream(t)
	p := Profile{Name: "evm-only", Chains: []string{"bsc"}}
	var logged strings.Builder
	g, srv := newGateway(t, Options{Logger: newLogger(&logged)},
		[]*Upstream{ready(t, bscChain, up.url()), ready(t, solanaChain, sol.url())},
		Token{ID: "a", Name: "indexer", Secret: tokenA, Profile: &p},
		Token{ID: "s", Name: "short", Secret: "tooShort123", Profile: &p},
		Token{ID: "n", Name: "noprofile", Secret: tokenB})

	if status, body := post(t, srv.URL+"/"+tokenA+"/bsc", chainIDCall); status != http.StatusOK || !strings.Contains(body, "0x38") {
		t.Fatalf("valid token: %d %s", status, body)
	}

	// Unknown, too short, profile-less and empty tokens all get the same
	// answer, which says nothing about why, and upstream is never called.
	before := up.calls.Load()
	var first string
	for _, tok := range []string{tokenA[:len(tokenA)-1] + "X", "tooShort123", tokenB, strings.Repeat("a", 200)} {
		status, body := post(t, srv.URL+"/"+tok+"/bsc", chainIDCall)
		if status != http.StatusUnauthorized || !strings.Contains(body, `"unauthorized"`) {
			t.Errorf("token %q: %d %s", tok, status, body)
		}
		if first == "" {
			first = body
		} else if body != first {
			t.Errorf("rejections differ: %s vs %s", first, body)
		}
		if status, _ := get(t, srv.URL+"/"+tok+"/status"); status != http.StatusUnauthorized {
			t.Errorf("status with %q: %d", tok, status)
		}
	}
	if up.calls.Load() != before {
		t.Fatal("upstream called for a rejected token")
	}

	// Revoking: a reload without the token locks it out immediately.
	g.SetTokens(nil)
	if status, _ := post(t, srv.URL+"/"+tokenA+"/bsc", chainIDCall); status != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", status)
	}
	g.SetTokens([]Token{{ID: "a", Name: "indexer", Secret: tokenA, Profile: &p}})

	// A chain outside the profile, an unknown chain, and odd paths.
	for _, path := range []string{"/" + tokenA + "/solana", "/" + tokenA + "/nope"} {
		status, body := post(t, srv.URL+path, chainIDCall)
		if status != http.StatusNotFound || !strings.Contains(body, "available: bsc") || strings.Contains(body, "solana") {
			t.Errorf("%s: %d %s", path, status, body)
		}
	}
	for _, path := range []string{"/", "/" + tokenA, "/" + tokenA + "/bsc/extra", "/bsc"} {
		if status, _ := post(t, srv.URL+path, chainIDCall); status != http.StatusNotFound {
			t.Errorf("%s: %d", path, status)
		}
	}
	if sol.calls.Load() != 0 {
		t.Fatal("solana upstream called for a token without solana")
	}
	if strings.Contains(logged.String(), tokenA) || strings.Contains(logged.String(), tokenB) {
		t.Fatalf("log reveals a token:\n%s", logged.String())
	}
}

func TestProfileLimitsArePerTokenAndChain(t *testing.T) {
	up := newUpstream(t)
	up2 := newUpstream(t)
	opbnb := Chain{Name: "opbnb", Family: EVM, ChainID: 56}
	tight := Profile{Name: "tight", Chains: []string{"bsc", "opbnb"}, DailyBudget: 2, MaxBatch: 2}
	loose := Profile{Name: "loose", Chains: []string{"bsc"},
		Methods: []string{"eth_chainId", "eth_getLogs", "eth_blockNumber", "eth_send*"}}
	_, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, up.url()), ready(t, opbnb, up2.url())},
		Token{ID: "a", Name: "tight", Secret: tokenA, Profile: &tight},
		Token{ID: "b", Name: "loose", Secret: tokenB, Profile: &loose})
	a, b := srv.URL+"/"+tokenA+"/bsc", srv.URL+"/"+tokenB+"/bsc"

	three := "[" + strings.Repeat(chainIDCall+",", 2) + chainIDCall + "]"
	if _, body := post(t, a, three); !strings.Contains(body, "batch of 3 exceeds") {
		t.Errorf("tight batch: %s", body)
	}
	if _, body := post(t, b, three); strings.Count(body, "0x38") != 3 {
		t.Errorf("loose batch: %s", body)
	}

	// Methods replaces the defaults, and a pattern allows a write method.
	if _, body := post(t, b, `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`); !strings.Contains(body, "not allowed") {
		t.Errorf("loose eth_call: %s", body)
	}
	if _, body := post(t, b, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x00"]}`); !strings.Contains(body, `"result"`) {
		t.Errorf("loose send: %s", body)
	}
	if _, body := post(t, a, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x00"]}`); !strings.Contains(body, "not allowed") {
		t.Errorf("tight send: %s", body)
	}

	// The budget is per token and per chain: tight spends its 2 on bsc, which
	// leaves opbnb and the other token alone.
	post(t, a, chainIDCall)
	post(t, a, chainIDCall)
	if status, body := post(t, a, chainIDCall); status != http.StatusTooManyRequests || !strings.Contains(body, "quota of 2") {
		t.Errorf("tight over budget: %d %s", status, body)
	}
	if _, body := post(t, srv.URL+"/"+tokenA+"/opbnb", chainIDCall); !strings.Contains(body, "0x38") {
		t.Errorf("tight on opbnb: %s", body)
	}
	if _, body := post(t, b, chainIDCall); !strings.Contains(body, "0x38") {
		t.Errorf("loose after tight's budget ran out: %s", body)
	}
}

// Changing a budget keeps today's count: raising it lets the difference
// through, and lowering it below the count stops the token at once.
func TestBudgetChangeKeepsTodaysCount(t *testing.T) {
	up := newUpstream(t)
	p := Profile{Name: "p", Chains: []string{"bsc"}, DailyBudget: 1}
	g, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, up.url())}, Token{ID: "a", Name: "x", Secret: tokenA, Profile: &p})
	url := srv.URL + "/" + tokenA + "/bsc"
	post(t, url, chainIDCall)
	if _, body := post(t, url, chainIDCall); !strings.Contains(body, "quota of 1") {
		t.Fatalf("not limited: %s", body)
	}
	p2 := Profile{Name: "p", Chains: []string{"bsc"}, DailyBudget: 3}
	g.SetTokens([]Token{{ID: "a", Name: "x", Secret: tokenA, Profile: &p2}})
	for i := range 2 {
		if _, body := post(t, url, chainIDCall); !strings.Contains(body, "0x38") {
			t.Fatalf("call %d after raising the budget: %s", i, body)
		}
	}
	if _, body := post(t, url, chainIDCall); !strings.Contains(body, "quota of 3") {
		t.Fatalf("raised budget not enforced: %s", body)
	}
}

func TestSolanaMethodAllowlist(t *testing.T) {
	up := newUpstream(t)
	def := Profile{Name: "def", Chains: []string{"solana"}}
	extra := Profile{Name: "extra", Chains: []string{"solana"}, Methods: []string{"get*"}}
	_, srv := newGateway(t, Options{}, []*Upstream{ready(t, solanaChain, up.url())},
		Token{ID: "a", Name: "a", Secret: tokenA, Profile: &def},
		Token{ID: "b", Name: "b", Secret: tokenB, Profile: &extra})
	a, b := srv.URL+"/"+tokenA+"/solana", srv.URL+"/"+tokenB+"/solana"

	for _, m := range DefaultSolanaMethods {
		// Solana params are positional, by object, or absent.
		if _, body := post(t, a, `{"jsonrpc":"2.0","id":1,"method":"`+m+`"}`); !strings.Contains(body, `"result"`) {
			t.Errorf("%s: %s", m, body)
		}
	}
	if _, body := post(t, a, `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"finalized"}]}`); !strings.Contains(body, `"result":123`) {
		t.Errorf("getSlot: %s", body)
	}
	before := up.calls.Load()
	for _, m := range []string{"getProgramAccounts", "sendTransaction", "simulateTransaction", "requestAirdrop",
		"eth_chainId", "eth_getLogs", "accountSubscribe"} {
		if _, body := post(t, a, `{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":[]}`); !strings.Contains(body, "not allowed") {
			t.Errorf("%s allowed by default: %s", m, body)
		}
	}
	if up.calls.Load() != before {
		t.Fatal("upstream saw a rejected Solana method")
	}
	if _, body := post(t, b, `{"jsonrpc":"2.0","id":1,"method":"getProgramAccounts","params":["Tok"]}`); !strings.Contains(body, `"result"`) {
		t.Errorf("extra getProgramAccounts: %s", body)
	}
	if _, body := post(t, b, `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["x"]}`); !strings.Contains(body, "not allowed") {
		t.Errorf("sendTransaction without listing it: %s", body)
	}
	// A Solana batch counts items like an EVM one.
	if _, body := post(t, a, `[{"jsonrpc":"2.0","id":1,"method":"getSlot"},{"jsonrpc":"2.0","id":2,"method":"getHealth"}]`); !strings.Contains(body, `"result":123`) {
		t.Errorf("batch: %s", body)
	}
}

func TestAnkrAdvancedAPI(t *testing.T) {
	up := newUpstream(t)
	ankr := Chain{Name: "ankr", Family: Ankr, Endpoint: "ankr"}
	u := ResolveUpstream(ankr, endpoint(up.url()), nil, 0)
	if err := u.Check(context.Background()); err != nil || up.calls.Load() != 0 {
		t.Fatalf("check: %v, %d upstream calls", err, up.calls.Load())
	}
	u.Ready = true
	p := Profile{Name: "def", Chains: []string{"ankr"}}
	_, srv := newGateway(t, Options{}, []*Upstream{u}, Token{ID: "a", Name: "a", Secret: tokenA, Profile: &p})
	url := srv.URL + "/" + tokenA + "/ankr"

	for _, m := range DefaultAnkrMethods {
		if _, body := post(t, url, `{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":{"blockchain":["bsc"]}}`); !strings.Contains(body, `"result"`) {
			t.Errorf("%s: %s", m, body)
		}
	}
	// ankr_getLogs is Ankr's, not eth_getLogs: the EVM range rules don't apply.
	if _, body := post(t, url, `{"jsonrpc":"2.0","id":1,"method":"ankr_getLogs","params":{"blockchain":["bsc"],"fromBlock":"0x1","toBlock":"0x100000"}}`); !strings.Contains(body, `"result"`) {
		t.Errorf("ankr_getLogs: %s", body)
	}
	for _, m := range []string{"eth_chainId", "eth_getLogs", "getSlot", "eth_sendRawTransaction"} {
		if _, body := post(t, url, `{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":[]}`); !strings.Contains(body, "not allowed") {
			t.Errorf("%s allowed on ankr: %s", m, body)
		}
	}
	if err := (Chain{Name: "x", Family: "other"}).Validate(); err == nil {
		t.Fatal("unknown family accepted")
	}
}

func TestSolanaGenesisCheck(t *testing.T) {
	up := newUpstream(t)
	ctx := context.Background()
	u := ResolveUpstream(solanaChain, endpoint(up.url()), nil, 0)
	if err := u.Check(ctx); err != nil {
		t.Fatalf("matching genesis: %v", err)
	}

	devnet := solanaChain
	devnet.Name, devnet.GenesisHash = "solana-devnet", "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	d := ResolveUpstream(devnet, endpoint(up.url()), nil, 0)
	if err := d.Check(ctx); err == nil || !strings.Contains(err.Error(), "expected EtWT") {
		t.Fatalf("mismatch: %v", err)
	}

	up.reply = func([]byte) (int, string) {
		return http.StatusOK, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"key ` + secretKey + `"}}`
	}
	if err := u.Check(ctx); err == nil || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("error result: %v", err)
	}
	up.reply = func([]byte) (int, string) { return http.StatusForbidden, secretKey }
	if err := u.Check(ctx); err == nil || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("forbidden: %v", err)
	}
	if err := (&Upstream{Chain: Chain{Name: "x", Family: Solana}}).Check(ctx); err == nil {
		t.Fatal("check passed with no upstream")
	}
	if err := (Chain{Name: "s", Family: Solana}).Validate(); err == nil {
		t.Fatal("solana chain without genesis hash validated")
	}
}

func TestTokenStatus(t *testing.T) {
	up := newUpstream(t)
	arb := Chain{Name: "arbitrum", Family: EVM, ChainID: 42161}
	// polygon has no URL and base failed its check: neither is listed.
	p := Profile{Name: "mixed", Chains: []string{"bsc", "solana", "polygon", "base"}, DailyBudget: 10, Methods: []string{"eth_chainId", "get*"}}
	other := Profile{Name: "secondprofile", Chains: []string{"arbitrum"}}
	polygon := ResolveUpstream(Chain{Name: "polygon", Family: EVM, ChainID: 137}, nil, nil, 0)
	base := ResolveUpstream(Chain{Name: "base", Family: EVM, ChainID: 8453, Endpoint: "x"}, endpoint(up.url()), nil, 0)
	base.Err = errors.New("upstream is chain 56, expected 8453")
	g, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, up.url()), ready(t, solanaChain, up.url()), ready(t, arb, up.url()), polygon, base},
		Token{ID: "a", Name: "indexer", Secret: tokenA, Profile: &p},
		Token{ID: "b", Name: "secondclient", Secret: tokenB, Profile: &other})

	post(t, srv.URL+"/"+tokenA+"/bsc", chainIDCall)
	post(t, srv.URL+"/"+tokenA+"/bsc", `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":[]}`)
	post(t, srv.URL+"/"+tokenA+"/solana", `{"jsonrpc":"2.0","id":1,"method":"getSlot"}`)
	post(t, srv.URL+"/"+tokenB+"/arbitrum", chainIDCall)

	status, raw := get(t, srv.URL+"/"+tokenA+"/status")
	if status != http.StatusOK {
		t.Fatalf("status: %d %s", status, raw)
	}
	var st TokenStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st.Token != "indexer" || st.Profile != "mixed" || st.Limits.DailyBudget != 10 || len(st.Chains) != 2 {
		t.Fatalf("status: %+v", st)
	}
	bsc, sol := st.Chains[0], st.Chains[1]
	if bsc.Chain != "bsc" || bsc.UsedToday != 1 || *bsc.Remaining != 9 || bsc.URL != srv.URL+"/"+tokenA+"/bsc" {
		t.Errorf("bsc: %+v", bsc)
	}
	if sol.Chain != "solana" || sol.Family != Solana || sol.UsedToday != 1 || sol.URL != srv.URL+"/"+tokenA+"/solana" || !strings.Contains(strings.Join(sol.Methods, ","), "get*") {
		t.Errorf("solana: %+v", sol)
	}
	want := map[string]Counts{"bsc/eth_chainId": {Requests: 1}, "bsc/" + OtherMethod: {Rejections: 1}, "solana/getSlot": {Requests: 1}}
	if len(st.Usage) != len(want) {
		t.Errorf("usage: %+v", st.Usage)
	}
	for _, u := range st.Usage {
		if want[u.Chain+"/"+u.Method] != u.Counts {
			t.Errorf("usage %s/%s = %+v", u.Chain, u.Method, u.Counts)
		}
	}
	for _, leak := range []string{"arbitrum", "polygon", "base", "secondclient", "secondprofile", tokenB, secretKey, up.srv.URL} {
		if strings.Contains(raw, leak) {
			t.Errorf("status shows %q:\n%s", leak, raw)
		}
	}
	if status, _ := post(t, srv.URL+"/"+tokenA+"/status", `{}`); status != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d", status)
	}
	_ = g
}

func TestSecretsNeverInLogs(t *testing.T) {
	up := newUpstream(t)
	up.reply = func(body []byte) (int, string) {
		return http.StatusInternalServerError, "upstream " + secretKey + " broke"
	}
	var logged strings.Builder
	p := Profile{Name: "p", Chains: []string{"bsc"}}
	_, srv := newGateway(t, Options{Logger: newLogger(&logged)}, []*Upstream{ready(t, bscChain, up.url())},
		Token{ID: "a", Name: "indexer", Secret: tokenA, Profile: &p})
	url := srv.URL + "/" + tokenA + "/bsc"
	_, body := post(t, url, chainIDCall)
	post(t, url, `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"address":"0x1"}]}`) // upstream fails
	post(t, url, `{"jsonrpc":"2.0","id":1,"method":"`+secretKey+"\\n"+tokenA+`","params":[]}`)
	post(t, srv.URL+"/bsc/"+tokenA, chainIDCall) // swapped segments
	post(t, srv.URL+"/"+tokenB+"/bsc", chainIDCall)
	out := logged.String()
	if !strings.Contains(out, "indexer bsc") {
		t.Fatalf("log lacks the token name:\n%s", out)
	}
	if strings.Contains(body, secretKey) {
		t.Errorf("response reveals the key: %s", body)
	}
	for _, leak := range []string{secretKey, tokenA, tokenB, up.srv.URL} {
		if strings.Contains(out, leak) {
			t.Errorf("log shows %q:\n%s", leak, out)
		}
	}
	if strings.Count(out, "\n") != 5 {
		t.Errorf("want one line per request:\n%s", out)
	}
}

func TestUsageCountsAndDrain(t *testing.T) {
	up := newUpstream(t)
	p := Profile{Name: "p", Chains: []string{"bsc"}, DailyBudget: 3}
	g, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, up.url())}, Token{ID: "a", Name: "indexer", Secret: tokenA, Profile: &p})
	url := srv.URL + "/" + tokenA + "/bsc"
	post(t, url, "["+chainIDCall+","+chainIDCall+"]")
	post(t, url, `{"jsonrpc":"2.0","id":1,"method":"made_up","params":[]}`)
	up.reply = func([]byte) (int, string) { return http.StatusBadGateway, "" }
	post(t, url, `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	post(t, url, chainIDCall) // over budget

	d := g.Usage().Drain()
	day := g.day()
	k := func(m string) UsageKey {
		return UsageKey{Day: day, TokenID: "a", TokenName: "indexer", Chain: "bsc", Method: m}
	}
	want := map[UsageKey]Counts{
		k("eth_chainId"):     {Requests: 2, Rejections: 1},
		k(OtherMethod):       {Rejections: 1},
		k("eth_blockNumber"): {Requests: 1, UpstreamErrors: 1},
	}
	if len(d) != len(want) {
		t.Fatalf("drain = %+v", d)
	}
	for key, c := range want {
		if d[key] != c {
			t.Errorf("%s = %+v, want %+v", key.Method, d[key], c)
		}
	}
	if len(g.Usage().Drain()) != 0 {
		t.Fatal("drain did not clear")
	}
	// Restore puts deltas back for the next flush without touching totals.
	g.Usage().Restore(d)
	if got := g.Usage().Spent(day, "a", "bsc"); got != 3 {
		t.Fatalf("spent after restore = %d", got)
	}
	if len(g.Usage().Drain()) != 3 {
		t.Fatal("restore lost deltas")
	}
}

func TestMethodPatterns(t *testing.T) {
	for _, c := range []struct {
		pattern, method string
		want            bool
	}{
		{"eth_call", "eth_call", true},
		{"eth_call", "eth_callx", false},
		{"*", "anything", true},
		{"*", "", true},
		{"eth_*", "eth_getLogs", true},
		{"eth_*", "net_version", false},
		{"*Transaction", "sendTransaction", true},
		{"*Transaction", "sendTransactions", false},
		{"get*Accounts", "getProgramAccounts", true},
		{"get*Accounts", "getAccounts", true},
		{"a*a", "a", false},
		{"a*b*c", "abxc", true},
		{"a*b*c", "acb", false},
	} {
		if got := matchMethod(c.pattern, c.method); got != c.want {
			t.Errorf("matchMethod(%q, %q) = %v", c.pattern, c.method, got)
		}
	}
}

func TestStarAllowsEveryMethod(t *testing.T) {
	up := newUpstream(t)
	all := Profile{Name: "all", Chains: []string{"bsc"}, Methods: []string{"*"}}
	g, srv := newGateway(t, Options{}, []*Upstream{ready(t, bscChain, up.url())},
		Token{ID: "a", Name: "a", Secret: tokenA, Profile: &all})
	url := srv.URL + "/" + tokenA + "/bsc"
	for _, m := range []string{"eth_sendRawTransaction", "debug_traceTransaction", "weird name!"} {
		if _, body := post(t, url, `{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":[]}`); !strings.Contains(body, `"result"`) {
			t.Errorf("%s: %s", m, body)
		}
	}
	// Odd names a client makes up are counted, but not under their own name.
	got := map[string]int64{}
	for k, c := range g.Usage().Drain() {
		got[k.Method] += c.Requests
	}
	if got["eth_sendRawTransaction"] != 1 || got[OtherMethod] != 1 || got["weird name!"] != 0 {
		t.Errorf("usage: %v", got)
	}
}

func TestRequestOrigin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://rpc.example/x/status", nil)
	if got := requestOrigin(r); got != "http://rpc.example" {
		t.Errorf("plain: %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := requestOrigin(r); got != "https://rpc.example" {
		t.Errorf("behind a TLS proxy: %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "javascript")
	if got := requestOrigin(r); got != "http://rpc.example" {
		t.Errorf("odd proto: %s", got)
	}
}

// helius is a Solana chain with Helius's own methods on top; solana itself
// does not get them.
func TestHeliusMethods(t *testing.T) {
	up := newUpstream(t)
	key := "heliuskey0123456789"
	helius := ResolveUpstream(Chain{Name: "helius", Family: Helius, GenesisHash: solanaGenesis, Endpoint: "helius"},
		&Endpoint{Name: "helius", URL: up.url() + "?api-key=" + key}, nil, 0)
	if err := helius.Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	helius.Ready = true
	p := Profile{Name: "def", Chains: []string{"helius", "solana"}}
	_, srv := newGateway(t, Options{}, []*Upstream{helius, ready(t, solanaChain, up.url())},
		Token{ID: "a", Name: "a", Secret: tokenA, Profile: &p})
	h, sol := srv.URL+"/"+tokenA+"/helius", srv.URL+"/"+tokenA+"/solana"
	for _, m := range []string{"getTransactionsForAddress", "getAssetsByOwner", "getSlot"} {
		if _, body := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":["x"]}`); !strings.Contains(body, `"result"`) {
			t.Errorf("helius %s: %s", m, body)
		}
	}
	for _, c := range []struct{ url, method string }{{h, "sendTransaction"}, {sol, "getTransactionsForAddress"}} {
		if _, body := post(t, c.url, `{"jsonrpc":"2.0","id":1,"method":"`+c.method+`","params":[]}`); !strings.Contains(body, "not allowed") {
			t.Errorf("%s %s allowed: %s", c.url, c.method, body)
		}
	}
	wrong := ResolveUpstream(Chain{Name: "helius", Family: Helius, GenesisHash: "other", Endpoint: "x"}, endpoint(up.url()), nil, 0)
	if err := wrong.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "genesis hash") {
		t.Errorf("wrong genesis: %v", err)
	}
}
