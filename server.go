package main

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewCoreClawMCPServer creates an MCP server with all public CoreClaw v2 tools registered.
func NewCoreClawMCPServer(client *CoreClawClient) *server.MCPServer {
	s := server.NewMCPServer(
		"coreclaw-mcp-server",
		version,
		server.WithTitle("CoreClaw MCP Server"),
		server.WithDescription("MCP server for CoreClaw OpenAPI v2 worker discovery, execution, task CRUD, run monitoring, result export, and logs."),
		server.WithWebsiteURL("https://mcp.coreclaw.com/mcp"),
		server.WithInstructions(serverInstructions()),
		server.WithToolFilter(orderToolsForCoreClawWorkflow),
		server.WithToolCapabilities(true),
	)

	for _, spec := range v2ToolSpecs() {
		s.AddTool(spec.Tool(), spec.Handler(client))
	}

	return s
}

func orderToolsForCoreClawWorkflow(_ context.Context, tools []mcp.Tool) []mcp.Tool {
	rank := make(map[string]int, len(tools))
	for i, name := range v2ToolWorkflowOrder() {
		rank[name] = i
	}

	ordered := append([]mcp.Tool(nil), tools...)
	sort.SliceStable(ordered, func(i, j int) bool {
		leftRank, leftKnown := rank[ordered[i].Name]
		rightRank, rightKnown := rank[ordered[j].Name]
		switch {
		case leftKnown && rightKnown:
			return leftRank < rightRank
		case leftKnown:
			return true
		case rightKnown:
			return false
		default:
			return ordered[i].Name < ordered[j].Name
		}
	})
	return ordered
}

func serverInstructions() string {
	return `CoreClaw MCP Server exposes the public CoreClaw OpenAPI v2 workflow. Use it for CoreClaw worker discovery, input schema inspection, execution, run monitoring, results, exports, logs, reruns, and aborts.

Recommended workflow:
1. Discover workers with list_store_workers for public marketplace workers or list_workers for the authenticated user's workers. Use list_proxy_regions when a worker input asks for a proxy region.
2. Inspect get_worker and get_worker_input_schema before run_worker. For saved presets, use list_worker_tasks, create_worker_task, get_worker_task, update_worker_task, or delete_worker_task before run_worker_task.
3. Start work with run_worker or run_worker_task. Prefer async runs unless the user explicitly needs a small synchronous result.
4. Poll or inspect status with get_worker_run, get_last_worker_run, or get_worker_last_run.
5. Read output with list_worker_run_results, list_last_worker_run_results, or list_worker_last_run_results. Use export_* tools for CSV/JSON download links and get_*_log tools for debugging.
6. Use rerun_* only when the user asks to retry/repeat a previous run. Use abort_* only when the user asks to stop/cancel an active run.

Auth: MCP callers may provide api-key, X-API-Key, or Authorization: Bearer <token>; this server forwards CoreClaw auth upstream as Authorization: Bearer <token>.

Do not call excluded internal APIs through this MCP server: worker version create/update and worker internal detail are intentionally unavailable.

中文：当用户要查找 CoreClaw worker、查看输入 schema、运行 worker、运行任务、查询运行状态、查看结果、导出结果、查看日志、重跑或停止任务时使用这些工具。通常先 list_store_workers/list_workers，再 get_worker_input_schema，然后 run_worker 或 run_worker_task，随后 get_worker_run 并读取结果。不要调用被排除的内部版本接口或 internal 详情接口。`
}
