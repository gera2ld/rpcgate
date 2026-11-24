package gateway

import (
	"crypto/sha256"
	"slices"
	"strings"
)

// Family is a chain's JSON-RPC dialect.
type Family string

const (
	EVM    Family = "evm"
	Solana Family = "solana"
	// Ankr is Ankr's Advanced API (https://rpc.ankr.com/multichain/<key>):
	// ankr_* methods that name their chains in their params.
	Ankr Family = "ankr"
)

// Families lists every chain family.
var Families = []Family{EVM, Solana, Ankr}

// DefaultEVMMethods are read-only. Anything that signs, sends, subscribes, or
// administers is absent; a profile's methods replace them.
var DefaultEVMMethods = []string{
	"eth_chainId",
	"net_version",
	"eth_blockNumber",
	"eth_getBlockByNumber",
	"eth_getBlockByHash",
	"eth_getCode",
	"eth_call",
	"eth_getLogs",
	"eth_getBalance",
	"eth_getStorageAt",
	"eth_getTransactionCount",
	"eth_getTransactionByHash",
	"eth_getTransactionReceipt",
}

// DefaultSolanaMethods are read-only. getProgramAccounts (expensive) and
// sendTransaction (a write) stay off unless a profile's methods allow them.
var DefaultSolanaMethods = []string{
	"getSlot",
	"getBlockHeight",
	"getBlock",
	"getTransaction",
	"getSignaturesForAddress",
	"getAccountInfo",
	"getMultipleAccounts",
	"getBalance",
	"getTokenAccountsByOwner",
	"getLatestBlockhash",
	"getHealth",
	"getVersion",
	"getGenesisHash",
}

// DefaultAnkrMethods are Ankr's Advanced API query methods, all read-only.
var DefaultAnkrMethods = []string{
	"ankr_getAccountBalance",
	"ankr_getCurrencies",
	"ankr_getTokenPrice",
	"ankr_getTokenPriceHistory",
	"ankr_getTokenHolders",
	"ankr_getTokenHoldersCount",
	"ankr_getTokenTransfers",
	"ankr_getNFTsByOwner",
	"ankr_getNFTMetadata",
	"ankr_getNFTHolders",
	"ankr_getNftTransfers",
	"ankr_getBlockchainStats",
	"ankr_getBlocks",
	"ankr_getLogs",
	"ankr_getTransactionsByHash",
	"ankr_getTransactionsByAddress",
	"ankr_getInteractions",
}

// DefaultMethods returns a copy of the family's default allowlist.
func DefaultMethods(f Family) []string {
	switch f {
	case EVM:
		return slices.Clone(DefaultEVMMethods)
	case Solana:
		return slices.Clone(DefaultSolanaMethods)
	case Ankr:
		return slices.Clone(DefaultAnkrMethods)
	}
	return nil
}

// Profile is a set of limits shared by tokens. Zero numeric limits take the
// defaults in Normalize, except DailyBudget, where 0 means unlimited.
type Profile struct {
	ID   string
	Name string
	// Chains are the chain names a token with this profile may call.
	Chains []string
	// Methods are the method patterns a token may call, replacing the
	// family defaults when non-empty. A pattern is a method name, where *
	// matches any run of characters: "eth_*", or "*" for every method.
	Methods []string

	DailyBudget int // per token and chain per UTC day; 0 = unlimited
	MaxBatch    int
}

// Profile defaults.
const (
	DefaultMaxBatch = 50
)

// Normalize fills zero limits with defaults.
func (p Profile) Normalize() Profile {
	if p.MaxBatch <= 0 {
		p.MaxBatch = DefaultMaxBatch
	}
	if p.DailyBudget < 0 {
		p.DailyBudget = 0
	}
	return p
}

// AllowsChain reports whether the chain is in the profile.
func (p *Profile) AllowsChain(name string) bool { return slices.Contains(p.Chains, name) }

// MethodsFor returns the method patterns the profile allows on a chain of
// family f: its own, or the family defaults. Names meant for another family
// are harmless: they never match.
func (p *Profile) MethodsFor(f Family) []string {
	if len(p.Methods) > 0 {
		return slices.Clone(p.Methods)
	}
	return DefaultMethods(f)
}

// methodSet is a list of method patterns.
type methodSet []string

func (s methodSet) allows(method string) bool {
	for _, p := range s {
		if matchMethod(p, method) {
			return true
		}
	}
	return false
}

// matchMethod reports whether method matches pattern, where * matches any
// run of characters, the empty one included.
func matchMethod(pattern, method string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == method
	}
	rest, ok := strings.CutPrefix(method, parts[0])
	if !ok {
		return false
	}
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return len(rest) >= len(last) && strings.HasSuffix(rest, last)
}

// Token is a client credential. Name identifies it in usage and logs; the
// secret itself is kept only as a hash.
type Token struct {
	ID      string
	Name    string
	Secret  string // only read by NewAccess, then dropped
	Profile *Profile
}

// MinTokenLength is the shortest secret the gateway accepts.
const MinTokenLength = 24

// access is an immutable snapshot of tokens, keyed by the SHA-256 of their
// secret so a lookup's timing says nothing about the secret's prefix.
type access struct {
	tokens map[[32]byte]*tokenEntry
}

type tokenEntry struct {
	id, name string
	secret   string // only to scrub it from log lines
	profile  Profile
	methods  map[Family]methodSet
}

func newAccess(tokens []Token) *access {
	a := &access{tokens: make(map[[32]byte]*tokenEntry, len(tokens))}
	for _, t := range tokens {
		if len(t.Secret) < MinTokenLength || t.Profile == nil {
			continue
		}
		p := t.Profile.Normalize()
		e := &tokenEntry{id: t.ID, name: t.Name, secret: t.Secret, profile: p, methods: map[Family]methodSet{}}
		for _, f := range Families {
			e.methods[f] = p.MethodsFor(f)
		}
		a.tokens[sha256.Sum256([]byte(t.Secret))] = e
	}
	return a
}

func (a *access) lookup(secret string) *tokenEntry {
	if a == nil || len(secret) < MinTokenLength {
		return nil
	}
	return a.tokens[sha256.Sum256([]byte(secret))]
}
