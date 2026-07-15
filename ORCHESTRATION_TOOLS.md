# Orchestration Tools: poll_run / verify_run / log grep / run_workers_batch

**Date**: 2026-07-15
**Status**: Implemented, tests passing, verified end-to-end against the real API
**Tools**: 34 → 37 (3 new orchestration tools + grep parameter on an existing tool)

## Context

A full-store acceptance run (117 public CoreClaw workers, results in `D:\Coreclaw_Work\all-workers\reports\`) exposed four pain points where the MCP server lagged behind raw API calls. The upstream pagination bug was already fixed in commit `0d22581`; this change closes the remaining gaps so MCP matches raw API for acceptance/diagnosis workflows and is safer (the judgment is codified, not reimplemented per caller).

| Pain point | Solution |
|---|---|
| Slow workers (LinkedIn/YouTube/glassdoor 60-285s) exceed a single MCP call; `run_worker` is fire-and-forget so callers poll `get_worker_run` manually | `poll_run` — polls to terminal state with a timeout |
| PASS judgment (count>0 + first row has real payload, not an error record) is reimplemented by every caller; a CAPTCHA/403 row was misjudged as PASS | `verify_run` — codifies the judgment, distinguishes ERROR_RECORD |
| Failed-run diagnosis needs the full log + local grep; logs often have only 4 system lines with the traceback buried | `get_worker_run_log` gains an in-process `grep` parameter |
| Accepting 117 workers one MCP call at a time is slow | `run_workers_batch` — runs many + returns per-item summary |

## Architecture: CustomHandler field

`v2ToolSpec` gains a field whose signature mirrors the existing `Handler(client)`:

```go
CustomHandler func(client *CoreClawClient) server.ToolHandlerFunc
```

`Handler` dispatches at the top: non-nil `CustomHandler` wins, otherwise the default transparent passthrough handler runs. All 34 existing tools leave `CustomHandler` nil → zero regression (enforced by `TestCustomHandlerNilFallsBackToDefault`).

The default handler's parameter-parsing loop was extracted into `resolveV2Request(request) (path, query, body, hasBody, err)` so custom handlers reuse one code path instead of duplicating path/query/body logic.

Custom-handler tools still set `Method`/`Path`/`Auth`/`Params` so `Tool()` generates the MCP schema and annotations correctly. `poll_run` and `verify_run` use synthetic paths (`/api/v2/worker-runs/{runId}/poll`, `.../verify`) so the registry's no-duplicate-endpoint check passes; the handler issues the real `get_worker_run` / `list_worker_run_results` calls itself. `run_workers_batch` uses a synthetic `/api/v2/workers/batch/runs` path. Annotations are derived from `Method` and the `run_` prefix as before, so they come out right automatically.

## poll_run (`poll_run.go`)

Polls `GET /api/v2/worker-runs/{runId}` until terminal (succeeded/failed/aborted) or timeout.

- Params: `run_id` (required), `timeout_seconds` (default 300, 1–900), `poll_interval_seconds` (default 5, 1–60), `limit` (result preview, default 10, 0–100).
- `context.WithTimeout(ctx, timeoutSeconds)` bounds the total poll. Each `doGetAuth` failure first checks `pollCtx.Err()`: if the context expired, return the `timed_out` result instead of a tool error (so a slow upstream doesn't surface as a hard failure).
- On `succeeded` with `limit>0`, pre-fetches `/result?offset=0&limit=N` (offset=0 is aligned → single upstream request) and returns `result_count` + first-row sample field names.
- Return: `{run_id, status, err_msg, poll_count, elapsed_ms, terminal, [timed_out], [result_count], [sample_fields]}`. On timeout: `terminal:false, timed_out:true` + a "call poll_run again" message.

Shared helpers in `poll_run.go` (reused by the other three tools): `jsonResult`, `readIntParamDefault`, `isTerminalStatus`, `parseRunStatus`, `runStatusPath`, `fetchResultPreview`, `pollTimeoutResult`, `pollUntilTerminal`.

## verify_run (`verify_run.go`)

Returns a structured verdict without the caller inspecting result rows.

- Params: `run_id` (required), `limit` (rows to sniff, default 5, 1–20).
- Flow: `get_worker_run` → parse status. Non-terminal → `RUNNING`. failed/aborted → `FAILED` + best-effort `err_lines` (via shared `fetchErrLines`). succeeded → fetch `/result?offset=0&limit=N`, decode `count` + first row, then `judgeFirstRow`.
- Verdict enum: `PASS` / `NO_DATA` / `FAILED` / `ERROR_RECORD` / `RUNNING` / `SUBMIT_FAIL`.

### judgeFirstRow — the false-PASS guard

```go
diagnosticKeys = {__coreclaw_data_id__, error, error_code, err, err_code, status, status_code, message, msg, input, input_url}
weakFields     = {url}  // real-looking, but cannot alone satisfy PASS
```

- Skip diagnostic keys and empty values.
- If the first row has zero real fields → `ERROR_RECORD` ("only diagnostic/error fields").
- If only weak fields (e.g. only `url`) → `ERROR_RECORD` ("no substantive payload").
- Otherwise → `PASS`.

This is exactly the standard the acceptance run used manually. A CAPTCHA/403 row that populates the list with `{error, status, error_code}` is judged `ERROR_RECORD`, not `PASS` — the trap that caused a false-PASS during cross-checking.

## get_worker_run_log + grep (`log_grep.go`)

Extends the existing tool (backward compatible). When `grep` is unset, returns the raw upstream payload verbatim.

- Params added: `grep` (pipe-separated keywords, case-insensitive), `context_lines` (default 2, 0–20), `max_matches` (default 50, 1–500).
- `extractLogLines` flattens `data.list[].content` (splitting on newlines). `grepLogLines` compiles `(?i)+pattern`, matches with `context_lines` before/after, de-duplicates overlapping regions, caps at `max_matches`. Invalid pattern → falls back to `defaultErrGrep` (`Error|raise|Exception|Traceback|BANNED|403|429|CAPTCHA|blocked|forbidden`).
- `fetchErrLines` (shared) pulls the log and returns default-error-matched lines; used by `verify_run` for `err_lines`.

## run_workers_batch (`run_workers_batch.go`)

Runs multiple workers in one call, returns a per-item summary.

- Params: `items` (JSON array of `{worker_id, input_json, version?}`, required, max 50), `concurrency` (default 1, 1–10), `timeout_seconds` (per-item, default 180), `poll_interval_seconds` (default 5), `verify` (default true), `skip_run_ids` (optional).
- Each item: `POST /workers/{id}/runs` with `is_async:true` + `wrapWorkerCustomInput` (reused from `v2_tools.go:375` to match the saved-task input contract) → extract `run_slug` → `pollUntilTerminal` (per-item `WithTimeout`) → optional `verifyRunByID`.
- Concurrency via a buffered `chan struct{}` semaphore; shared parent `ctx` for HTTP-cancellation propagation.
- `skip_run_ids` is best-effort: re-submitting an ad-hoc run starts a NEW run rather than returning the old one, so for exact resume callers should omit completed items from `items` instead. Documented in the tool description.
- Default `concurrency=1` (serial) avoids hammering the platform / tripping rate-limit code 13000.
- Return: `{total, counts:{verdict:count}, results:[{worker_id, run_slug, status, verdict, err_msg, real_field_count}]}` in input order.

## Tests (`custom_handlers_test.go`)

httptest mock upstream, following `pagination_test.go` style (reuses `mustV2ToolSpec` / `extractText` / `WithAPIKey`):

- `TestCustomHandlerNilFallsBackToDefault` — all 34 passthrough tools have `CustomHandler==nil`; the 4 custom tools have it set.
- `TestJudgeFirstRow` — table-driven: real payload / only diagnostics / only url / url+real / empty values.
- `TestVerifyRunPASS/NoData/Failed/ErrorRecord/Running` — the four-scenario guard. `ErrorRecord` is the false-PASS guard.
- `TestPollRunReachesSucceeded` — running→running→succeeded sequence, asserts result_count.
- `TestPollRunTimeout` — always running, asserts `timed_out` within the timeout.
- `TestGetWorkerRunLogNoGrepPassthrough` — without grep, raw data passthrough.
- `TestGetWorkerRunLogGrepFilters` / `TestGrepLogLinesCaseInsensitive` / `TestGrepLogLinesInvalidPatternFallsBack`.
- `TestRunWorkersBatchRejectsOversize` (51 items) / `TestRunWorkersBatchRejectsMissingWorkerID`.

## Hardcoded 34 → 37 updated

`v2_tools_test.go` (4 count assertions + workflow-order slice), `v2_tools.go` (`v2ToolWorkflowOrder`), `MCP_TOOL_SPEC.md` (Coverage Contract + new Orchestration stage in Workflow Order + Verification counts + Custom Handlers section), `README.md` / `README.zh-CN.md` (counts + tool lists), `scripts/verify-real-api.ps1` (`Assert-ToolCount`), `main.go` (`WriteTimeout` 10→15 min to accommodate poll_run's up-to-900s window + batch per-item polling).

## End-to-end verification (real API, REST shim)

- `verify_run` on a succeeded YouTube Channel run → `PASS, real_field_count=17, sample_fields=[handle, banner_img, subscribers, ...]`.
- `verify_run` on a succeeded-but-empty google-maps run → `NO_DATA`.
- `verify_run` on a failed glassdoor run → `FAILED` + `err_lines` including `AttributeError: 'NoneType' object has no attribute 'get'`.
- `get_worker_run_log` with `grep=Traceback|Error|AttributeError` on that glassdoor run → located the root cause at `main.py:234`: `item.get("error")` when `item` is `None`. (This is the single real code bug from the acceptance report; the MCP tool surfaced the exact line without source access.)

`go test ./...` / `go vet ./...` / `go build ./...` / `gofmt -l .` all clean. `-race` requires cgo and is run in CI on Ubuntu (no local gcc).

## Files

New: `poll_run.go`, `verify_run.go`, `log_grep.go`, `run_workers_batch.go`, `custom_handlers_test.go`.
Modified: `v2_tools.go`, `main.go`, `v2_tools_test.go`, `MCP_TOOL_SPEC.md`, `README.md`, `README.zh-CN.md`, `scripts/verify-real-api.ps1`.
Unchanged: `server.go`, `rest.go`, `client.go`, `types.go`, `exported-api-docs/` (upstream OpenAPI truth stays at 34 operations).
