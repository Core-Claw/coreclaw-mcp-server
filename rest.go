package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newRESTHandler exposes each MCP tool as a plain REST endpoint at
// /mcp/<tool_name>, so platforms whose plugin model is per-tool HTTP with
// per-user header auth (notably Coze, where end users supply their own
// api-key) can call individual tools without speaking MCP JSON-RPC.
//
// The shim reuses the existing MCP tool handlers verbatim — it just
// translates an HTTP POST + JSON body into an mcp.CallToolRequest and
// writes the resulting tool text back as the HTTP response.
func newRESTHandler(client *CoreClawClient) http.Handler {
	tools := restToolHandlers(client)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/mcp/"), "/")
		handler, ok := tools[name]
		if !ok {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx := r.Context()
		if key := r.Header.Get("api-key"); key != "" {
			ctx = WithAPIKey(ctx, key)
		} else if key := r.Header.Get("X-API-Key"); key != "" {
			ctx = WithAPIKey(ctx, key)
		} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			ctx = WithAPIKey(ctx, strings.TrimSpace(auth[len("Bearer "):]))
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeRESTError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
			return
		}
		args := map[string]any{}
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
				writeRESTError(w, http.StatusBadRequest, "request body must be a JSON object: "+err.Error())
				return
			}
		}

		req := mcp.CallToolRequest{Header: r.Header}
		req.Params.Name = name
		req.Params.Arguments = args

		result, err := handler(ctx, req)
		if err != nil {
			writeRESTError(w, http.StatusInternalServerError, err.Error())
			return
		}

		text := extractToolText(result)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if result != nil && result.IsError {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": text})
			return
		}
		if json.Valid([]byte(text)) {
			_, _ = w.Write([]byte(text))
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": text})
		}
	})
}

func restToolHandlers(client *CoreClawClient) map[string]server.ToolHandlerFunc {
	tools := make(map[string]server.ToolHandlerFunc, len(v2ToolSpecs()))
	for _, spec := range v2ToolSpecs() {
		tools[spec.Name] = spec.Handler(client)
	}
	return tools
}

func writeRESTError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

func extractToolText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		if t, ok := c.(mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}
