package store

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	_ "ganigeorgiev.com/pocketbase/pocketbase/migrations" // system collections, as pocketbase.New loads them

	"rpcgate/internal/gateway"
)

const key = "sk_live_abcdef123456"

// fakeRPC answers like BSC (chain 56) and Solana mainnet, behind a key path.
func fakeRPC(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+key {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		result := `"0x"`
		switch req.Method {
		case "eth_chainId":
			result = `"0x38"`
		case "getGenesisHash":
			result = `"` + gateway.SolanaMainnetGenesis + `"`
		case "getSlot":
			result = "7"
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+result+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	app *core.BaseApp
	gw  *gateway.Gateway
	st  *Store
	srv *httptest.Server
	log *bytes.Buffer
}

// start bootstraps a PocketBase app in dir and starts a store on it. A
// non-empty url is saved, enabled, for every default chain.
func start(t *testing.T, dir, url string) *harness {
	t.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir})
	gw := gateway.New(gateway.Options{})
	h := &harness{app: app, gw: gw, log: &bytes.Buffer{}}
	h.st = New(app, gw, Options{CheckTimeout: 5 * time.Second, Logger: log.New(h.log, "", 0)})
	h.st.Bind()
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(gw)
	t.Cleanup(func() {
		h.st.WaitReloads()
		h.srv.Close()
		_ = app.ResetBootstrapState()
	})
	if url != "" {
		for _, c := range []string{"bsc", "arbitrum", "solana"} {
			h.addURL(t, c, "test", url, true)
		}
	}
	return h
}

// addURL saves a URL for a chain and waits for the chains to reload.
func (h *harness) addURL(t *testing.T, chain, name, url string, enabled bool) *core.Record {
	t.Helper()
	col, err := h.app.FindCollectionByNameOrId(URLs)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("chain", chain)
	r.Set("name", name)
	r.Set("url", url)
	r.Set("enabled", enabled)
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	h.st.WaitReloads()
	return r
}

// save saves r and waits for the chains to reload.
func (h *harness) save(t *testing.T, r *core.Record) {
	t.Helper()
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	h.st.WaitReloads()
}

// chains maps each chain to its oldest enabled URL's upstream, which is the
// one a token with every URL uses.
func (h *harness) chains() map[string]*gateway.Upstream {
	ups := map[string]*gateway.Upstream{}
	for _, u := range h.gw.Upstreams() {
		if ups[u.Chain.Name] == nil {
			ups[u.Chain.Name] = u
		}
	}
	return ups
}

// allURLs returns the IDs of every saved URL.
func (h *harness) allURLs(t *testing.T) []string {
	t.Helper()
	recs, err := h.app.FindAllRecords(URLs)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.Id
	}
	return ids
}

func (h *harness) post(t *testing.T, token, chain, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/"+token+"/"+chain, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) record(t *testing.T, col, filter string, params dbx.Params) *core.Record {
	t.Helper()
	r, err := h.app.FindFirstRecordByFilter(col, filter, params)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// createToken saves a token record using the given URLs. An empty secret
// lets PocketBase generate one.
func (h *harness) createToken(t *testing.T, name, secret string, urls []string) *core.Record {
	t.Helper()
	col, err := h.app.FindCollectionByNameOrId(Tokens)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("token", secret)
	r.Set("enabled", true)
	r.Set("urls", urls)
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// rpcURL is fakeRPC's URL with the key. It has no {chain}, so every chain
// gets the same endpoint, and arbitrum fails its check (it answers 56).
func rpcURL(rpc *httptest.Server) string { return rpc.URL + "/" + key }

const chainID = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`

func TestSchemaSnapshotIsCurrent(t *testing.T) {
	want, err := MarshalSnapshot(Collections())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, snapshot) {
		t.Fatal("collections.json is stale; run `just schema`")
	}

	h := start(t, t.TempDir(), "")
	for _, name := range []string{URLs, Tokens, Usage} {
		c, err := h.app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatal(err)
		}
		// Superusers only: no public or user access to any of it.
		if c.ListRule != nil || c.ViewRule != nil || c.CreateRule != nil || c.UpdateRule != nil || c.DeleteRule != nil {
			t.Errorf("%s has an API rule", name)
		}
	}
	// A field added in the admin UI is removed on the next start.
	c, _ := h.app.FindCollectionByNameOrId(Tokens)
	c.Fields.Add(&core.TextField{Name: "ui_added"})
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := ApplySchema(h.app); err != nil {
		t.Fatal(err)
	}
	c, _ = h.app.FindCollectionByNameOrId(Tokens)
	if c.Fields.GetByName("ui_added") != nil {
		t.Fatal("snapshot did not remove an undeclared field")
	}
}

func TestLoadAndHooks(t *testing.T) {
	rpc := fakeRPC(t)
	h := start(t, t.TempDir(), "")

	// An empty database serves nothing and creates nothing.
	if len(h.gw.Upstreams()) != 0 || len(h.st.URLInfo()) != 0 {
		t.Fatalf("upstreams before a URL: %+v", h.gw.Upstreams())
	}
	for _, c := range []string{"bsc", "arbitrum", "solana", "ankr"} {
		h.addURL(t, c, "fake", rpcURL(rpc), true)
	}
	ups := h.chains()
	if len(ups) != 4 || !ups["ankr"].Ready || !ups["bsc"].Ready || !ups["solana"].Ready || ups["arbitrum"].Ready ||
		!strings.Contains(ups["arbitrum"].Err.Error(), "expected 42161") {
		t.Fatalf("upstreams: %+v", ups)
	}
	for _, c := range []string{Tokens, Usage} {
		if n, _ := h.app.CountRecords(c); n != 0 {
			t.Fatalf("start created %d %s", n, c)
		}
	}

	// Creating a token in PocketBase makes it usable at once, with a
	// generated secret.
	tok := h.createToken(t, "indexer", "", h.allURLs(t))
	secret := tok.GetString("token")
	if len(secret) != 40 {
		t.Fatalf("generated token %q", secret)
	}
	if status, body := h.post(t, secret, "bsc", chainID); status != 200 || !strings.Contains(body, "0x38") {
		t.Fatalf("new token: %d %s", status, body)
	}
	if _, body := h.post(t, secret, "solana", `{"jsonrpc":"2.0","id":1,"method":"getSlot"}`); !strings.Contains(body, `"result":7`) {
		t.Fatalf("solana: %s", body)
	}
	if status, _ := h.post(t, secret, "arbitrum", chainID); status != http.StatusNotFound {
		t.Fatalf("failed chain served: %d", status)
	}

	// A token's edits apply at once.
	tok.Set("methods", []string{"eth_sendRawTransaction"})
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	if _, body := h.post(t, secret, "bsc", `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0x"]}`); !strings.Contains(body, `"result"`) {
		t.Fatalf("after enabling a method: %s", body)
	}
	tok.Set("methods", "not a list")
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusUnauthorized {
		t.Fatalf("token with broken methods still works: %d", status)
	}
	tok.Set("methods", []string{})
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	// Its URLs decide its chains.
	sol := h.record(t, URLs, "chain = 'solana'", nil)
	tok.Set("urls", []string{sol.Id})
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusNotFound {
		t.Fatalf("bsc without a bsc URL: %d", status)
	}
	tok.Set("urls", h.allURLs(t))
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}

	// Revoking, rotating and deleting.
	tok.Set("enabled", false)
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusUnauthorized {
		t.Fatalf("disabled token: %d", status)
	}
	rotated := "rotated0123456789abcdefghijk"
	tok.Set("enabled", true)
	tok.Set("token", rotated)
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusUnauthorized {
		t.Fatalf("old secret after rotation: %d", status)
	}
	if status, _ := h.post(t, rotated, "bsc", chainID); status != 200 {
		t.Fatalf("rotated secret: %d", status)
	}
	if err := h.app.Delete(tok); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.post(t, rotated, "bsc", chainID); status != http.StatusUnauthorized {
		t.Fatalf("deleted token: %d", status)
	}

	// Disabling a chain's only URL takes the chain out after the reload.
	tok = h.createToken(t, "again", "again0123456789abcdefghijklm", h.allURLs(t))
	bscURL := h.record(t, URLs, "chain = 'bsc'", nil)
	bscURL.Set("enabled", false)
	h.save(t, bscURL)
	if status, _ := h.post(t, "again0123456789abcdefghijklm", "bsc", chainID); status != http.StatusNotFound {
		t.Fatalf("chain without an enabled URL: %d", status)
	}
	if strings.Contains(h.log.String(), key) || strings.Contains(h.log.String(), rpc.URL) {
		t.Fatalf("store log reveals an upstream:\n%s", h.log.String())
	}
}

func TestUsageFlushAndBudgetReload(t *testing.T) {
	rpc := fakeRPC(t)
	dir := t.TempDir()
	h := start(t, dir, rpcURL(rpc))
	const secret = "budget0123456789abcdefghijk"
	tok := h.createToken(t, "indexer", secret, h.allURLs(t))
	tok.Set("daily_budget", 3)
	if err := h.app.Save(tok); err != nil {
		t.Fatal(err)
	}

	h.post(t, secret, "bsc", chainID)
	h.post(t, secret, "bsc", `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":[]}`) // rejected
	if err := h.st.Flush(); err != nil {
		t.Fatal(err)
	}
	h.post(t, secret, "bsc", "["+chainID+","+chainID+"]")
	if err := h.st.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Flush(); err != nil { // nothing pending: no change
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	rows, err := h.app.FindAllRecords(Usage, dbx.HashExp{"day": day})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][3]int{}
	for _, r := range rows {
		if r.GetString("token_name") != "indexer" {
			t.Errorf("row token_name = %q", r.GetString("token_name"))
		}
		if r.GetString("url") != "test" {
			t.Errorf("row url = %q", r.GetString("url"))
		}
		got[r.GetString("chain")+"/"+r.GetString("method")] = [3]int{r.GetInt("requests"), r.GetInt("rejections"), r.GetInt("upstream_errors")}
	}
	want := map[string][3]int{"bsc/eth_chainId": {3, 0, 0}, "bsc/" + gateway.OtherMethod: {0, 1, 0}}
	if len(got) != len(want) || got["bsc/eth_chainId"] != want["bsc/eth_chainId"] || got["bsc/"+gateway.OtherMethod] != want["bsc/"+gateway.OtherMethod] {
		t.Fatalf("usage rows = %v", got)
	}
	if status, body := h.post(t, secret, "bsc", chainID); status != http.StatusTooManyRequests || !strings.Contains(body, "quota") {
		t.Fatalf("over budget: %d %s", status, body)
	}
	if err := h.st.Flush(); err != nil {
		t.Fatal(err)
	}

	// Restart on the same data: today's spending is seeded, so the budget
	// still holds; status shows it too.
	h.srv.Close()
	h.st.WaitReloads()
	if err := h.app.ResetBootstrapState(); err != nil {
		t.Fatal(err)
	}
	h2 := start(t, dir, "") // the URL was saved in the database
	if status, body := h2.post(t, secret, "bsc", chainID); status != http.StatusTooManyRequests || !strings.Contains(body, "quota") {
		t.Fatalf("budget after restart: %d %s", status, body)
	}
	if _, body := h2.post(t, secret, "solana", `{"jsonrpc":"2.0","id":1,"method":"getSlot"}`); !strings.Contains(body, `"result":7`) {
		t.Fatalf("other chain after restart: %s", body)
	}
	resp, err := http.Get(h2.srv.URL + "/" + secret + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st gateway.TokenStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	// Only the chains with a URL are listed: bsc and solana.
	if st.Token != "indexer" || len(st.Chains) != 2 || st.Chains[0].Chain != "bsc" || st.Chains[0].UsedToday != 3 || st.Chains[0].Remaining != 0 || st.Chains[0].Via != "test" ||
		st.Chains[0].URL != h2.srv.URL+"/"+secret+"/bsc" {
		t.Fatalf("status after restart: %+v", st)
	}
}

func TestURLSwitching(t *testing.T) {
	rpc := fakeRPC(t)
	h := start(t, t.TempDir(), "")
	const secret = "switch0123456789abcdefghijk"
	ankr := h.addURL(t, "bsc", "ankr", rpc.URL+"/wrong-key", true) // 404s: configured, not ready
	node := h.addURL(t, "bsc", "node", rpcURL(rpc), false)
	h.createToken(t, "indexer", secret, []string{ankr.Id, node.Id})
	if got := h.chains()["bsc"]; got.Source != "ankr" || got.Ready {
		t.Fatalf("first URL: %+v", got)
	}
	// Its first enabled URL failed: the chain is unavailable, not skipped.
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusNotFound {
		t.Fatalf("failed URL skipped: %d", status)
	}

	// Both enabled: the older one (ankr) still wins.
	node.Set("enabled", true)
	h.save(t, node)
	if got := h.chains()["bsc"].Source; got != "ankr" {
		t.Fatalf("both enabled: %q", got)
	}
	info := h.st.URLInfo()
	if len(info) != 2 || info[0].Name != "ankr" || info[1].Name != "node" || info[1].Chain != "bsc" || info[1].ID != node.Id {
		t.Fatalf("url info: %+v", info)
	}

	// Disabling ankr switches the token to node at once.
	ankr.Set("enabled", false)
	h.save(t, ankr)
	if got := h.chains()["bsc"]; !got.Ready || got.Source != "node" {
		t.Fatalf("after disabling ankr: %+v", got)
	}
	if status, body := h.post(t, secret, "bsc", chainID); status != 200 || !strings.Contains(body, "0x38") {
		t.Fatalf("bsc through node: %d %s", status, body)
	}

	// Deleting the URL in use takes the chain out, even though tokens use it.
	if err := h.app.Delete(node); err != nil {
		t.Fatal(err)
	}
	h.st.WaitReloads()
	if h.chains()["bsc"] != nil {
		t.Fatal("bsc still served after its only enabled URL was deleted")
	}
	if status, _ := h.post(t, secret, "bsc", chainID); status != http.StatusNotFound {
		t.Fatalf("after delete: %d", status)
	}
}

// A URL holds the key: the API never returns it, superusers included, and
// saving a URL record with the url field empty keeps the old one.
func TestURLIsWriteOnly(t *testing.T) {
	const secretURL = "http://127.0.0.1:1/write-only-key-0123456789"
	h := start(t, t.TempDir(), "")
	rec := h.addURL(t, "bsc", "ankr", secretURL, true)

	r, err := apis.NewRouter(h.app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	sucol, err := h.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	su := core.NewRecord(sucol)
	su.SetEmail("admin@example.com")
	su.SetPassword("password-0123456789")
	if err := h.app.Save(su); err != nil {
		t.Fatal(err)
	}
	auth, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, api.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, path := range []string{
		"/api/collections/urls/records",
		"/api/collections/urls/records/" + rec.Id,
	} {
		status, body := do(http.MethodGet, path, "")
		if status != 200 || strings.Contains(body, "write-only-key") || strings.Contains(body, `"url"`) {
			t.Fatalf("GET %s: %d %s", path, status, body)
		}
	}

	// An edit that sends the URL empty, as the admin UI does, keeps it.
	if status, body := do(http.MethodPatch, "/api/collections/urls/records/"+rec.Id, `{"name":"ankr2","url":""}`); status != 200 || strings.Contains(body, "write-only-key") {
		t.Fatalf("PATCH: %d %s", status, body)
	}
	if got := h.record(t, URLs, "id = {:id}", dbx.Params{"id": rec.Id}).GetString("url"); got != secretURL {
		t.Fatal("an empty URL in an edit wiped the saved one")
	}
	h.st.WaitReloads()
	if got := h.chains()["bsc"]; !got.Configured() || got.Source != "ankr2" {
		t.Fatalf("after rename: %+v", got)
	}
}

// last_used moves forward at each flush, without a record save: the token's
// updated time stays, and an older time never replaces a newer one.
func TestTokenLastUsed(t *testing.T) {
	rpc := fakeRPC(t)
	h := start(t, t.TempDir(), rpcURL(rpc))
	const secret = "lastused0123456789abcdefghij"
	tok := h.createToken(t, "indexer", secret, h.allURLs(t))
	idle := h.createToken(t, "idle", "idle0123456789abcdefghijklmn", h.allURLs(t))
	updated := tok.GetString("updated")

	before := time.Now().UTC().Add(-time.Second)
	h.post(t, secret, "bsc", chainID)
	if err := h.st.Flush(); err != nil {
		t.Fatal(err)
	}
	tok = h.record(t, Tokens, "id = {:id}", dbx.Params{"id": tok.Id})
	got := tok.GetDateTime("last_used").Time()
	if got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("last_used = %v", got)
	}
	if tok.GetString("updated") != updated {
		t.Errorf("updated moved: %s -> %s", updated, tok.GetString("updated"))
	}
	if idle = h.record(t, Tokens, "id = {:id}", dbx.Params{"id": idle.Id}); !idle.GetDateTime("last_used").IsZero() {
		t.Errorf("idle token has last_used %v", idle.GetDateTime("last_used"))
	}

	h.gw.Usage().Touch(tok.Id, got.Add(-time.Hour))
	if err := h.st.Flush(); err != nil {
		t.Fatal(err)
	}
	tok = h.record(t, Tokens, "id = {:id}", dbx.Params{"id": tok.Id})
	if !tok.GetDateTime("last_used").Time().Equal(got) {
		t.Errorf("an older time replaced last_used: %v", tok.GetDateTime("last_used"))
	}
	// A rejected call is still a use.
	h.post(t, secret, "bsc", `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":[]}`)
	if len(h.gw.Usage().DrainLastUsed()) != 1 {
		t.Error("a rejected call did not count as a use")
	}
}
