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
	// Helius is Helius's Solana mainnet RPC: the Solana methods plus
	// Helius's own, such as getTransactionsForAddress and the DAS API.
	Helius Family = "helius"
)

// Families lists every chain family.
var Families = []Family{EVM, Solana, Ankr, Helius}

// solanaLike reports whether f speaks Solana JSON-RPC, so its upstream is
// checked by getGenesisHash.
func (f Family) solanaLike() bool { return f == Solana || f == Helius }

// DefaultEVMMethods are read-only. Anything that signs, sends, subscribes, or
// administers is absent; a token's methods replace them.
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
// sendTransaction (a write) stay off unless a token's methods allow them.
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

// DefaultHeliusMethods are Helius's own read-only methods. Its family's
// defaults are these plus DefaultSolanaMethods. getPriorityFeeEstimate only
// reads, though it is for sending.
var DefaultHeliusMethods = []string{
	"getTransactionsForAddress",
	"getAsset",
	"getAssetBatch",
	"getAssetProof",
	"getAssetProofBatch",
	"getAssetsByOwner",
	"getAssetsByAuthority",
	"getAssetsByCreator",
	"getAssetsByGroup",
	"searchAssets",
	"getSignaturesForAsset",
	"getTokenAccounts",
	"getNftEditions",
	"getPriorityFeeEstimate",
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
	case Helius:
		return slices.Concat(DefaultSolanaMethods, DefaultHeliusMethods)
	}
	return nil
}

// MaxBatch is the most items a JSON-RPC batch may hold.
const MaxBatch = 50

// methodsFor returns the method patterns allowed on a chain of family f:
// the token's own, or the family defaults. Names meant for another family
// are harmless: they never match.
func methodsFor(patterns []string, f Family) []string {
	if len(patterns) > 0 {
		return slices.Clone(patterns)
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
// secret itself is kept only as a hash. URLs are the IDs of the upstream URLs
// it may use, which also decide its chains: for each chain, it uses the
// first enabled one of them (oldest first).
type Token struct {
	ID          string
	Name        string
	Secret      string // only read by NewAccess, then dropped
	URLs        []string
	Methods     []string // patterns; empty means the family defaults
	DailyBudget int      // per chain per UTC day; 0 means unlimited
}

// MinTokenLength is the shortest secret the gateway accepts.
const MinTokenLength = 24

// access is an immutable snapshot of tokens, keyed by the SHA-256 of their
// secret so a lookup's timing says nothing about the secret's prefix.
type access struct {
	tokens map[[32]byte]*tokenEntry
	list   []*tokenEntry // in the order given, for the status table
}

type tokenEntry struct {
	id, name string
	secret   string // only to scrub it from log lines
	urls     map[string]bool
	patterns []string
	budget   int
	methods  map[Family]methodSet
}

func newAccess(tokens []Token) *access {
	a := &access{tokens: make(map[[32]byte]*tokenEntry, len(tokens))}
	for _, t := range tokens {
		if len(t.Secret) < MinTokenLength {
			continue
		}
		e := &tokenEntry{id: t.ID, name: t.Name, secret: t.Secret, urls: map[string]bool{},
			patterns: t.Methods, budget: max(t.DailyBudget, 0), methods: map[Family]methodSet{}}
		for _, id := range t.URLs {
			e.urls[id] = true
		}
		for _, f := range Families {
			e.methods[f] = methodsFor(t.Methods, f)
		}
		a.tokens[sha256.Sum256([]byte(t.Secret))] = e
		a.list = append(a.list, e)
	}
	return a
}

func (a *access) lookup(secret string) *tokenEntry {
	if a == nil || len(secret) < MinTokenLength {
		return nil
	}
	return a.tokens[sha256.Sum256([]byte(secret))]
}
