package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

var version = "2.0.0"

func main() {
	transport := flag.String("transport", "stdio", "Transport mode: stdio or http")
	port := flag.Int("port", 3000, "HTTP server port (only for http transport)")
	baseURL := flag.String("base-url", "https://openapi.coreclaw.com", "CoreClaw API base URL")
	flag.Parse()

	// stdio 模式下通常从 env 读 key（单用户）；HTTP 模式下忽略这个，按每个请求头的 api-key 走。
	apiKey := os.Getenv("CORECLAW_API_KEY")
	client := NewCoreClawClient(apiKey, *baseURL)
	s := NewCoreClawMCPServer(client)

	switch *transport {
	case "stdio":
		if err := server.ServeStdio(s); err != nil {
			log.Fatal(err)
		}
	case "http":
		addr := fmt.Sprintf(":%d", *port)
		httpServer := server.NewStreamableHTTPServer(s,
			server.WithHTTPContextFunc(apiKeyFromHeader),
		)
		// Coze HTTP plugins call individual tools at /mcp/<tool_name>; the
		// MCP streamable endpoint itself stays at exact path /mcp.
		mux := http.NewServeMux()
		mux.Handle("/mcp", httpServer)
		mux.Handle("/mcp/", newRESTHandler(client))
		log.Printf("CoreClaw MCP Server v%s listening on %s (MCP at /mcp, REST shim at /mcp/<tool>)", version, addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("Unknown transport: %s (use stdio or http)", *transport)
	}
}

// apiKeyFromHeader extracts the caller's CoreClaw api-key from the HTTP request
// and attaches it to the context that will be passed to MCP tool handlers.
// Accepted header names (case-insensitive): "api-key" (primary) or "X-API-Key".
func apiKeyFromHeader(ctx context.Context, r *http.Request) context.Context {
	key := r.Header.Get("api-key")
	if key == "" {
		key = r.Header.Get("X-API-Key")
	}
	if key == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			key = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	if key == "" {
		return ctx
	}
	return WithAPIKey(ctx, key)
}
