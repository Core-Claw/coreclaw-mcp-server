# CoreClaw MCP Tool Design Spec

This project exposes CoreClaw OpenAPI v2 as MCP tools. The source of truth is the exported OpenAPI documentation, not hand-written assumptions.

## Coverage Contract

- Total OpenAPI v2 operations: 34
- Public MCP tools: 37
- Excluded operations:
  - `POST /api/v2/workers/{workerId}/versions`
  - `PUT /api/v2/workers/{workerId}/versions/{version}`
  - `GET /api/v2/workers/{workerId}/internal`

Every non-excluded operation must have exactly one MCP tool and one REST shim route at `/mcp/<tool_name>`. The 3 additional tools beyond the 34 OpenAPI operations are orchestration tools with custom handlers (not 1:1 with any single upstream operation): `poll_run` (repeated `get_worker_run` until terminal), `verify_run` (`get_worker_run` + `list_worker_run_results` plus in-process verdict), and `run_workers_batch` (per-item `run_worker` + polling). `get_worker_run_log` remains 1:1 with its OpenAPI operation but adds an in-process `grep` filter parameter; when `grep` is unset it returns the raw upstream payload unchanged.

Three operations are intentionally public (`Auth: false`) and match the upstream OpenAPI `security: []` marking: `GET /api/v2/proxy/region`, `GET /api/v2/store`, and `GET /api/v2/workers/{workerId}/input-schema`. All other operations require a CoreClaw token. Do not mark non-public operations as `Auth: false` to "help" callers — it would let unauthenticated MCP requests reach the tool and fail upstream instead of being rejected at the auth layer.

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
2. Execution: ad-hoc worker runs and saved task runs, plus `run_workers_batch` for bulk execution.
3. Run lookup: list runs, last run, specific run, worker-specific last run.
4. Orchestration: `poll_run` to wait for completion (covers slow workers exceeding a single MCP call), `verify_run` for an acceptance verdict.
5. Output retrieval: result rows, export links, and logs for last/specific/worker-specific runs.
6. Repeat and control: rerun tools, then abort tools.

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

## Custom Handlers

Most tools use the default transparent passthrough handler: parse params, issue one upstream request, return the `data` payload. Tools needing multi-request orchestration or in-process post-processing set a `CustomHandler func(client *CoreClawClient) server.ToolHandlerFunc` on `v2ToolSpec`; when non-nil it overrides the default handler, and when nil the default is used so all passthrough tools are unaffected.

Custom-handler tools still set `Method`/`Path`/`Auth`/`Params` so `Tool()` generates the MCP schema and annotations correctly. `poll_run` and `verify_run` use synthetic `Path` values (`/api/v2/worker-runs/{runId}/poll` and `.../verify`) so the registry's no-duplicate-endpoint check passes; the handler issues the real `get_worker_run`/`list_worker_run_results` calls itself. `run_workers_batch` uses a synthetic `/api/v2/workers/batch/runs` Path.

`verify_run` codifies the "real data" acceptance standard: `code==0` + `run_status==succeeded` + `data.count>0` + the first row carries at least one non-empty non-diagnostic field. Rows whose only populated fields are diagnostic markers (`error`, `status`, `error_code`, `__coreclaw_data_id__`, etc.) or weak fields alone (`url`) are judged `ERROR_RECORD`, not `PASS` — this prevents a common false-PASS trap where a CAPTCHA/403 row populates the list but carries no payload.

## Pagination Compensation

CoreClaw list endpoints interpret `(offset, limit)` as 1-indexed paging (`page_index = floor(offset/limit) + 1`), not as an absolute row offset. A request whose `offset` is not a multiple of `limit` therefore returns the wrong window upstream.

Every paginated GET list tool sets `ListKey` (`scraper` for store/workers, `list` for worker-runs/worker-tasks/results). When `offset % limit != 0`, the handler transparently walks aligned upstream pages and stitches the exact `[offset, offset+limit)` window. Aligned requests (including `offset=0`) issue a single upstream call. `TestV2ListToolsCarryListKey` enforces that list tools set `ListKey` and non-list tools do not.

## Verification

Required checks:

- `go test ./...`
- `go vet ./...`
- `go build .`
- `scripts/verify-real-api.ps1` with `CORECLAW_API_KEY` set for authenticated checks
- `scripts/verify-e2e-run.ps1` with `CORECLAW_API_KEY` set for a real MCP `tools/call` run, polling, logs, results, and export validation
- Tool registry test proving 37 exposed tools and the three excluded endpoints absent
- MCP tools/list test proving 37 tools are visible to MCP clients
- REST handler test proving 37 `/mcp/<tool_name>` handlers
- Initialize test proving server instructions and hosted endpoint metadata are visible to MCP clients
- Annotation test proving every MCP tool has explicit behavior hints
- Workflow order test proving `tools/list` follows the intended discovery-to-control order

`go test -race ./...` is required in CI. On Windows it requires a C compiler; local Windows runs may skip it when `gcc` is unavailable, but GitHub Actions must run it on Ubuntu.
