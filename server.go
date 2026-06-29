package main

import "github.com/mark3labs/mcp-go/server"

// NewCoreClawMCPServer creates an MCP server with all public CoreClaw v2 tools registered.
func NewCoreClawMCPServer(client *CoreClawClient) *server.MCPServer {
	s := server.NewMCPServer(
		"coreclaw-mcp-server",
		version,
		server.WithToolCapabilities(true),
	)

	for _, spec := range v2ToolSpecs() {
		s.AddTool(spec.Tool(), spec.Handler(client))
	}

	return s
}
