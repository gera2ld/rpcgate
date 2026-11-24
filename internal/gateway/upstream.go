package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Chain is a chain the gateway can serve, at /<token>/<Name>.
type Chain struct {
	// Name is the chain's name in client URLs, such as bsc.
	Name   string
	Family Family
	// ChainID is the expected eth_chainId (EVM).
	ChainID uint64
	// GenesisHash is the expected getGenesisHash result (Solana).
	GenesisHash string
	// Endpoint names the URL serving this chain, such as ankr; "" means none.
	Endpoint string
}

// Endpoint is one saved upstream URL, used as it is. URL holds the key and
// is the secret;
// Name (such as ankr) is what logs and status show instead.
type Endpoint struct {
	Name string
	URL  string
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidName reports whether s can be a chain's name in client URLs.
// "status" is reserved for the token status page.
func ValidName(s string) bool { return namePattern.MatchString(s) && s != StatusPath }

// Validate returns why c cannot be served, without any URL in it.
func (c Chain) Validate() error {
	switch {
	case !ValidName(c.Name):
		return errors.New("name must be lowercase letters, digits and dashes, and not \"status\"")
	case !slices.Contains(Families, c.Family):
		return errors.New("family must be evm, solana or ankr")
	case c.Family == EVM && c.ChainID == 0:
		return errors.New("evm chain needs an expected chain ID")
	case c.Family == Solana && strings.TrimSpace(c.GenesisHash) == "":
		return errors.New("solana chain needs an expected genesis hash")
	}
	return nil
}

// Upstream is one chain's upstream. Source names the URL in use ("" when
// none); status output and errors use Source, never the URL. Err is why the
// chain is not served, and Ready reports that it passed its check.
type Upstream struct {
	Chain  Chain
	Source string
	Ready  bool
	Err    error

	url     string
	secrets []string
	client  *http.Client
	timeout time.Duration
}

// ResolveUpstream builds chain c's upstream from endpoint e (nil when the
// chain has none). Errors name the endpoint, never its URL.
func ResolveUpstream(c Chain, e *Endpoint, client *http.Client, timeout time.Duration) *Upstream {
	up := &Upstream{Chain: c, timeout: timeout}
	if timeout <= 0 {
		up.timeout = 30 * time.Second
	}
	if client == nil {
		client = &http.Client{}
	}
	// Never follow redirects: a redirect could carry the key to another host,
	// and its Location could echo the URL back to the caller.
	cl := *client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	up.client = &cl

	if err := c.Validate(); err != nil {
		up.Err = err
		return up
	}
	if e == nil || strings.TrimSpace(e.URL) == "" {
		return up // not configured
	}
	up.Source = e.Name
	u, err := url.Parse(strings.TrimSpace(e.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Do not echo the value: it is the secret.
		up.Err = fmt.Errorf("URL %s must be http:// or https://", e.Name)
		return up
	}
	up.url = u.String()
	up.secrets = secretParts(u)
	return up
}

// Configured reports whether the upstream has a usable URL.
func (u *Upstream) Configured() bool { return u.url != "" }

// secretParts lists the substrings of u that may carry a key, longest first
// in practice, so they can be scrubbed from anything returned to callers.
func secretParts(u *url.URL) []string {
	var parts []string
	add := func(s string) {
		if len(s) >= 6 {
			parts = append(parts, s)
		}
	}
	add(u.String())
	add(u.Host + u.RequestURI())
	if u.User != nil {
		add(u.User.String())
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
	}
	add(u.RawQuery)
	for _, values := range u.Query() {
		for _, v := range values {
			add(v)
		}
	}
	for _, seg := range strings.Split(u.Path, "/") {
		// A short all-lowercase word (multichain, solana, arbitrum, v2) is a
		// route, not a key; scrubbing it would mangle ordinary responses.
		if !isPlainWord(seg) {
			add(seg)
		}
	}
	return parts
}

func isPlainWord(s string) bool {
	if len(s) > 20 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

func (u *Upstream) scrub(s string) string {
	for _, secret := range u.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

func (u *Upstream) forward(ctx context.Context, body []byte) (int, []byte, error) {
	if u.url == "" {
		return 0, nil, errors.New("chain has no upstream")
	}
	ctx, cancel := context.WithTimeout(ctx, u.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("building upstream request failed")
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	resp, err := u.client.Do(req)
	if err != nil {
		// *url.Error includes the full URL; drop it.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return 0, nil, uerr.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, nil, fmt.Errorf("upstream answered with redirect %d", resp.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, errors.New("reading upstream response failed")
	}
	return resp.StatusCode, out, nil
}

// call sends one request and returns its "result", or an error with no URL.
func (u *Upstream) call(ctx context.Context, method string) (json.RawMessage, error) {
	status, body, err := u.forward(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":[]}`))
	if err != nil {
		return nil, fmt.Errorf("upstream did not answer %s: %s", method, u.scrub(err.Error()))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("upstream answered %s with HTTP %d", method, status)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Result) == 0 || string(resp.Result) == nullID {
		return nil, fmt.Errorf("upstream gave no %s result", method)
	}
	return resp.Result, nil
}

// Check confirms the upstream serves the expected chain: eth_chainId on EVM,
// getGenesisHash on Solana. It spends no token's budget. It catches a URL
// for the wrong chain, or a bad key. Ankr's Advanced API serves many chains
// and has no free call to check, so it passes once it has a URL.
func (u *Upstream) Check(ctx context.Context) error {
	if u.Err != nil {
		return u.Err
	}
	if !u.Configured() {
		return errors.New("no upstream configured")
	}
	switch u.Chain.Family {
	case EVM:
		res, err := u.call(ctx, "eth_chainId")
		if err != nil {
			return err
		}
		var hex string
		if json.Unmarshal(res, &hex) != nil {
			return errors.New("upstream gave an unreadable chain ID")
		}
		got, err := strconv.ParseUint(strings.TrimPrefix(hex, "0x"), 16, 64)
		if err != nil {
			return errors.New("upstream gave an unreadable chain ID")
		}
		if got != u.Chain.ChainID {
			return fmt.Errorf("upstream is chain %d, expected %d", got, u.Chain.ChainID)
		}
	case Ankr:
		return nil
	case Solana:
		res, err := u.call(ctx, "getGenesisHash")
		if err != nil {
			return err
		}
		var got string
		if json.Unmarshal(res, &got) != nil || got == "" {
			return errors.New("upstream gave an unreadable genesis hash")
		}
		if got != strings.TrimSpace(u.Chain.GenesisHash) {
			// Genesis hashes are public; showing the mismatch helps.
			return fmt.Errorf("upstream genesis hash is %s, expected %s", safeMethod(got), u.Chain.GenesisHash)
		}
	default:
		return errors.New("unknown chain family")
	}
	return nil
}
