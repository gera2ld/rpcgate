package gateway

// SolanaMainnetGenesis is Solana mainnet's genesis hash, which is public.
const SolanaMainnetGenesis = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

// Chains are the chains rpcgate serves, in the order the admin UI lists
// them. Their facts never change, so they live here rather than in the
// database. To add one, add a line, run `just schema`, and rebuild: the
// admin UI's chain pickers come from this list.
var Chains = []Chain{
	{Name: "bsc", Family: EVM, ChainID: 56},
	{Name: "arbitrum", Family: EVM, ChainID: 42161},
	{Name: "base", Family: EVM, ChainID: 8453},
	{Name: "polygon", Family: EVM, ChainID: 137},
	{Name: "avalanche", Family: EVM, ChainID: 43114}, // C-Chain
	{Name: "solana", Family: Solana, GenesisHash: SolanaMainnetGenesis},
	{Name: "ankr", Family: Ankr}, // Ankr's Advanced API
	// Helius's mainnet RPC, for its own methods; solana can use any provider.
	{Name: "helius", Family: Helius, GenesisHash: SolanaMainnetGenesis},
}

// ChainNames lists the names of Chains.
func ChainNames() []string {
	names := make([]string, len(Chains))
	for i, c := range Chains {
		names[i] = c.Name
	}
	return names
}
