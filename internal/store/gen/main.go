// Command gen writes the collections snapshot from store.Collections. Run
// `just schema` after editing the definitions, and commit both. The snapshot,
// not this program, is what rpcgate applies at startup.
package main

import (
	"fmt"
	"os"

	"rpcgate/internal/store"
)

func main() {
	out, err := store.MarshalSnapshot(store.Collections())
	if err == nil {
		err = os.WriteFile(os.Args[1], out, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
