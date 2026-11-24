// Command rpcgate is a JSON-RPC gateway for EVM and Solana chains. Clients
// call http://<host>:<port>/<token>/<chain> with a revocable per-client token
// and never see the provider URLs or keys.
//
// It is a PocketBase app, and one port serves both: the proxy at
// /<token>/<chain> and /<token>/status, and PocketBase's admin UI (/_/) and
// API (/api/) for the chains' URLs, profiles, tokens and
// usage. RPC URLs are set there, not in the environment. PocketBase's
// backup API is switched off, because a backup holds the provider keys.
//
// Environment:
//
//	RPCGATE_LISTEN          address for everything (default 127.0.0.1:8545)
//	RPCGATE_DATA_DIR        PocketBase data directory (default ./pb_data)
//	RPCGATE_QUIET           set to 1 to stop logging each request
//
// With no arguments it runs `serve --http=$RPCGATE_LISTEN`. Other PocketBase
// commands work as usual, for example
// `rpcgate superuser upsert you@example.com <password>`.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/core"

	"rpcgate/internal/gateway"
	"rpcgate/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// route sends proxy paths to gw and everything else to pb, and refuses
// PocketBase's backup API: a backup is a copy of the database, provider keys
// included. A proxy path is /<token>/<chain> or /<token>/status: two
// segments, the first neither "api" nor "_".
func route(gw, pb http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/backups" || strings.HasPrefix(p, "/api/backups/") {
			http.Error(w, "backups are disabled in rpcgate: they contain the provider keys", http.StatusForbidden)
			return
		}
		first, rest, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/")
		if ok && first != "" && first != "api" && first != "_" && rest != "" && !strings.Contains(rest, "/") {
			gw.ServeHTTP(w, r)
			return
		}
		pb.ServeHTTP(w, r)
	})
}

func run() error {
	listen := envOr("RPCGATE_LISTEN", "127.0.0.1:8545")
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("RPCGATE_LISTEN: %w", err)
	}
	quiet := envOr("RPCGATE_QUIET", "") == "1"

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  envOr("RPCGATE_DATA_DIR", "./pb_data"),
		HideStartBanner: true,
	})
	if len(os.Args) == 1 {
		app.RootCmd.SetArgs([]string{"serve", "--http=" + listen})
	} else {
		// The status table should name the address actually used.
		for i, a := range os.Args {
			if v, ok := strings.CutPrefix(a, "--http="); ok {
				listen = v
			} else if a == "--http" && i+1 < len(os.Args) {
				listen = os.Args[i+1]
			}
		}
	}

	logger := log.New(os.Stderr, "rpcgate ", log.LstdFlags)
	gwOpts := gateway.Options{}
	if !quiet {
		gwOpts.Logger = logger
	}
	gw := gateway.New(gwOpts)
	st := store.New(app, gw, store.Options{Logger: logger})
	st.Bind()

	var (
		stopRun context.CancelFunc
		runDone = make(chan struct{})
	)
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		if err := st.Start(context.Background()); err != nil {
			return err
		}
		// PocketBase builds its handler and listener here; it starts
		// serving only after this hook returns, with the wrapped handler.
		if err := se.Next(); err != nil {
			return err
		}
		se.Server.Handler = route(gw, se.Server.Handler)
		gateway.WriteStatus(os.Stderr, listen, gw.Chains(), st.URLInfo(), st.Profiles(), st.TokenCount())

		var ctx context.Context
		ctx, stopRun = context.WithCancel(context.Background())
		go func() {
			defer close(runDone)
			st.Run(ctx)
		}()
		return nil
	})
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if stopRun != nil {
			stopRun() // final usage flush, while the database is still open
			<-runDone
		}
		return e.Next()
	})

	return app.Start()
}
