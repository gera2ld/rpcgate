package store

import (
	"bytes"
	"encoding/json"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"

	"rpcgate/internal/gateway"
)

func num(name, help string) *core.NumberField {
	return &core.NumberField{Name: name, Help: help, OnlyInt: true, Min: types.Pointer(0.0)}
}

func timestamps(c *core.Collection) {
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	c.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
}

// Collections defines the schema. collections.json is generated from it
// (`just schema`) and is what rpcgate applies; a test keeps the two equal.
func Collections() []*core.Collection {
	chainNames := gateway.ChainNames()

	urls := core.NewBaseCollection("urls", "rg_urls")
	urls.Fields.Add(
		&core.SelectField{Name: "chain", Required: true, MaxSelect: 1, Values: chainNames},
		&core.TextField{Name: "name", Required: true, Max: 32, Presentable: true,
			Help: "e.g. ankr. Logs and the status table show this, never the URL."},
		// Hidden, and also hidden from superusers by a hook: write-only.
		&core.TextField{Name: "url", Required: true, Max: 2048, Hidden: true,
			Help: "Write-only: never shown after saving; leave empty when editing to keep it. " +
				"This chain's RPC URL with the key, used as it is, e.g. https://rpc.ankr.com/bsc/<key>. " +
				"For ankr (the Advanced API): https://rpc.ankr.com/multichain/<key>. " +
				"For helius: https://mainnet.helius-rpc.com/?api-key=<key>."},
		&core.BoolField{Name: "enabled", Help: "A chain uses its first enabled URL (oldest first). Untick to switch without deleting."},
	)
	timestamps(urls)

	profiles := core.NewBaseCollection("profiles", "rg_profiles")
	profiles.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 64, Presentable: true},
		&core.SelectField{Name: "chains", MaxSelect: len(chainNames), Values: chainNames,
			Help: "Chains tokens with this profile may call."},
		&core.JSONField{Name: "methods", MaxSize: 64 << 10,
			Help: `JSON array of method patterns; * matches anything, e.g. ["*"] or ["eth_*", "getProgramAccounts"]. Empty: each family's read-only defaults.`},
		num("daily_budget", "Requests per token per chain per UTC day; 0 = unlimited."),
		num("max_batch", "Items per JSON-RPC batch; 0 = 50."),
	)
	timestamps(profiles)
	profiles.AddIndex("idx_profiles_name", true, "name", "")

	tokens := core.NewBaseCollection("tokens", "rg_tokens")
	tokens.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 64, Pattern: `^[A-Za-z0-9._-]+$`, Presentable: true,
			Help: "Who uses it (e.g. indexer); shown in usage and logs."},
		&core.TextField{Name: "token", Required: true, Min: 24, Max: 128, Pattern: `^[A-Za-z0-9_-]+$`,
			AutogeneratePattern: "[a-zA-Z0-9]{40}",
			Help:                "Leave empty to generate. Clients call /<token>/<chain>. Change it to rotate."},
		&core.BoolField{Name: "enabled", Help: "Untick to revoke."},
		&core.RelationField{Name: "profile", Required: true, MaxSelect: 1, CollectionId: profiles.Id},
		&core.DateField{Name: "last_used", Help: "Its latest request, to within 15 seconds. Set by rpcgate."},
	)
	timestamps(tokens)
	tokens.AddIndex("idx_tokens_name", true, "name", "")
	tokens.AddIndex("idx_tokens_token", true, "token", "")

	usage := core.NewBaseCollection("usage", "rg_usage")
	usage.Fields.Add(
		&core.TextField{Name: "day", Required: true, Pattern: `^\d{4}-\d{2}-\d{2}$`, Help: "UTC day."},
		&core.TextField{Name: "token_id", Required: true, Max: 32},
		&core.TextField{Name: "token_name", Max: 64, Presentable: true},
		&core.TextField{Name: "chain", Required: true, Max: 32},
		&core.TextField{Name: "method", Required: true, Max: 128},
		num("requests", "Items forwarded upstream; these spend the daily budget."),
		num("rejections", "Items the gateway refused."),
		num("upstream_errors", "Forwarded items whose upstream call failed."),
	)
	timestamps(usage)
	usage.AddIndex("idx_usage_key", true, "day, token_id, chain, method", "")

	return []*core.Collection{urls, profiles, tokens, usage}
}

// MarshalSnapshot renders collections as collections.json does.
func MarshalSnapshot(cols []*core.Collection) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cols); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
