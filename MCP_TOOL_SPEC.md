# CoreClaw MCP Tool Design Spec

This project exposes CoreClaw OpenAPI v2 as MCP tools. The source of truth is the exported OpenAPI documentation, not hand-written assumptions.

## Coverage Contract

- Total OpenAPI v2 operations: 31
- Public MCP tools: 28
- Excluded operations:
  - `POST /api/v2/workers/{workerId}/versions`
  - `PUT /api/v2/workers/{workerId}/versions/{version}`
  - `GET /api/v2/workers/{workerId}/internal`

Every non-excluded operation must have exactly one MCP tool and one REST shim route at `/mcp/<tool_name>`.

## Naming

Use `snake_case` names and CoreClaw v2 nouns:

- `worker_id`, not `scraper_slug`
- `run_id`, not `run_slug`
- `worker_task_id`, not `task_slug`
- `offset` and `limit`, not `page` and `page_size`

Tool names should mirror the endpoint intent:

- `list_store_workers`
- `get_worker_input_schema`
- `run_worker`
- `list_worker_run_results`
- `export_worker_last_run_results`

## Description Format

Each tool description must contain these sections:

1. One short English function summary with the CoreClaw domain anchor.
2. `WHEN TO USE:` with English and Chinese trigger phrases.
3. `WHEN NOT TO USE:` naming competing or excluded actions.
4. `RETURNS:` with the top-level JSON contract.
5. `WORKFLOW:` with the usual previous/next tool.

Descriptions should help models trigger tools naturally in both English and Chinese. Examples:

- "run the latest CoreClaw worker again"
- "show me the output rows for this run id"
- "查一下这个 worker 的输入 schema"
- "导出上次运行结果"

## Parameters

Parameters must be explicit and stable:

- Path parameters are required.
- Optional pagination parameters include defaults in the description.
- Enum parameters use `mcp.Enum`.
- Complex worker input is accepted as `input_json`, a JSON object string, because many MCP clients handle simple strings more reliably than arbitrary nested objects.

## Return Values

Successful tools return the upstream `data` JSON as tool text. Errors return `mcp.NewToolResultError` with the upstream code/message/request_id when available.

The REST shim returns raw JSON on success and `{"error":"..."}` with a 4xx/5xx status on failure.

## Verification

Required checks:

- `go test ./...`
- `go vet ./...`
- `go build .`
- `scripts/verify-real-api.ps1` with `CORECLAW_API_KEY` set for authenticated checks
- Tool registry test proving 28 exposed tools and the three excluded endpoints absent
- MCP tools/list test proving 28 tools are visible to MCP clients
- REST handler test proving 28 `/mcp/<tool_name>` handlers

`go test -race ./...` is required in CI. On Windows it requires a C compiler; local Windows runs may skip it when `gcc` is unavailable, but GitHub Actions must run it on Ubuntu.
