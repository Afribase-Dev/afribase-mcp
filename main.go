// Command afribase-mcp is a standalone bridge that speaks the Model Context
// Protocol and translates every tool call into a request against Afribase's
// public REST API - the same one the dashboard and the CLI use.
//
// It is deliberately single-tenant: one process, one access token, started
// the same way the CLI already expects (AFRIBASE_ACCESS_TOKEN,
// AFRIBASE_API_URL). Afribase's own hosted connector at mcp.afribase.dev is
// multi-tenant with its own OAuth 2.1 authorization server; this is the
// self-hostable shape instead - point an assistant at a bridge you run
// yourself, using your own token, with no internal package to link against.
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	token := os.Getenv("AFRIBASE_ACCESS_TOKEN")
	if token == "" {
		log.Fatal("AFRIBASE_ACCESS_TOKEN is not set - get one with `afribase login` (the Afribase CLI) or from the dashboard")
	}

	apiURL := os.Getenv("AFRIBASE_API_URL")
	if apiURL == "" {
		apiURL = "https://api.useafribase.app"
	}

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = "8090"
	}
	addr = ":" + addr

	bridge := &Bridge{api: newClient(apiURL, token), publicURL: os.Getenv("MCP_PUBLIC_URL")}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", bridge.handleMCP)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("afribase-mcp listening on %s, proxying to %s", addr, apiURL)
	log.Fatal(http.ListenAndServe(addr, mux))
}
