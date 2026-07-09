# CoreClaw MCP Tool Design Spec

This project exposes CoreClaw OpenAPI v2 as MCP tools. The source of truth is the exported OpenAPI documentation, not hand-written assumptions.

## Coverage Contract

- Total OpenAPI v2 operations: 34
- Public MCP tools: 34
- Excluded operations:
  - `POST /api/v2/workers/{workerId}/versions`
  - `PUT /api/v2/workers/{workerId}/versions/{version}`
  - `GET /api/v2/workers/{workerId}/internal`

Every non-excluded operation must have exactly one MCP tool and one REST shim route at `/mcp/<tool_name>`.

## Client Entry Contract

The first-class hosted endpoint is:

```text
https://mcp.coreclaw.com/mcp
```

Documentation and example MCP client configs must present the hosted endpoint before local stdio or local HTTP usage. Local transports remain development and fallback paths.

The MCP `initialize` response must include:

- `serverInfo.title`: `CoreClaw MCP Server`
- `serverInfo.websiteUrl`: `https://mcp.coreclaw.com/mcp`
- `instructions`: bilingual workflow guidance covering worker discovery, schema inspection, execution, polling/status, results/export/logs, rerun, abort, auth headers, and excluded internal APIs

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

## Workflow Order

Register and expose tools in the order a model should normally use them:

1. Discovery and preflight: proxy regions, public store workers, private workers, worker detail, worker input schema, saved tasks, account info.
2. Execution: ad-hoc worker runs and saved task runs.
3. Run lookup: list runs, last run, specific run, worker-specific last run.
4. Output retrieval: result rows, export links, and logs for last/specific/worker-specific runs.
5. Repeat and control: rerun tools, then abort tools.

This order is part of the MCP surface and must be tested through `tools/list`, not only through internal slices.

## Tool Annotations

Every tool must expose explicit MCP annotations:

- `title`: a human-readable title derived from the tool name.
- `readOnlyHint`: `true` for `GET` tools, `false` for tools that change run state.
- `destructiveHint`: `true` for `POST` tools in this API because they start, repeat, or stop CoreClaw runs; `false` for `GET` tools.
- `idempotentHint`: `true` for `GET` tools and abort controls, `false` for run/rerun tools.
- `openWorldHint`: `true` for run/rerun tools that execute CoreClaw workers and may interact with external sites; `false` for CoreClaw-only metadata, status, results, export, log, and abort tools.

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
- `run_worker`, `create_worker_task`, and `update_worker_task_input` treat `input_json` as the Worker's business/custom fields and send it upstream as `input.parameters.custom`, matching CoreClaw saved task payloads observed in the v2 docs and real API. Sending `input_json` unwrapped makes a created task un-runnable (backend rejects required custom fields). Advanced callers can send a complete upstream `input` object through `raw_input_json` (run_worker only); `input_json` and `raw_input_json` are mutually exclusive.

## Return Values

Successful tools return the upstream `data` JSON as tool text. Errors return `mcp.NewToolResultError` with the upstream code/message/request_id when available.

The REST shim returns raw JSON on success and `{"error":"..."}` with a 4xx/5xx status on failure.

## Verification

Required checks:

- `go test ./...`
- `go vet ./...`
- `go build .`
- `scripts/verify-real-api.ps1` with `CORECLAW_API_KEY` set for authenticated checks
- `scripts/verify-e2e-run.ps1` with `CORECLAW_API_KEY` set for a real MCP `tools/call` run, polling, logs, results, and export validation
- Tool registry test proving 34 exposed tools and the three excluded endpoints absent
- MCP tools/list test proving 34 tools are visible to MCP clients
- REST handler test proving 34 `/mcp/<tool_name>` handlers
- Initialize test proving server instructions and hosted endpoint metadata are visible to MCP clients
- Annotation test proving every MCP tool has explicit behavior hints
- Workflow order test proving `tools/list` follows the intended discovery-to-control order

`go test -race ./...` is required in CI. On Windows it requires a C compiler; local Windows runs may skip it when `gcc` is unavailable, but GitHub Actions must run it on Ubuntu.
