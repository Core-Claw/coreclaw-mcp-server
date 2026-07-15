package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// This file holds shared helpers used by the custom-handler tools
// (poll_run, verify_run, get_worker_run_log grep, run_workers_batch) plus the
// poll_run tool itself. Shared helpers: jsonResult, readIntParamDefault,
// isTerminalStatus, parseRunStatus, fetchResultPreview, pollTimeoutResult,
// pollUntilTerminal, runStatusPath.

// jsonResult marshals v into a ToolResultText. On marshal failure it returns a
// ToolResultError with a nil Go error, matching the existing handler
// convention (errors travel as IsError results, not Go errors).
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError("failed to encode result: " + err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// readIntParamDefault reads an integer param by name from an MCP request,
// falling back to def when absent. Used by custom handlers for control params
// (timeouts, intervals, limits) that are not forwarded upstream.
func readIntParamDefault(request mcp.CallToolRequest, name string, def int) int {
	v, ok, err := readV2Param(request, v2ParamSpec{Name: name, Location: v2QueryParam, Type: v2NumberParam, Default: def})
	if err != nil || !ok {
		return def
	}
	if n, ok := v.(int); ok {
		return n
	}
	if n, ok := v.(int64); ok {
		return int(n)
	}
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return def
}

// isTerminalStatus reports whether a run status is terminal (no further change).
func isTerminalStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "aborted":
		return true
	}
	return false
}

// runStatusPath returns the upstream path for a run's detail endpoint.
func runStatusPath(runID string) string {
	return "/api/v2/worker-runs/" + url.PathEscape(runID)
}

// parseRunStatus extracts status and err_msg from a run-detail `data` payload.
func parseRunStatus(data json.RawMessage) (status, errMsg string) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return "unknown", ""
	}
	if s, ok := m["status"].(string); ok {
		status = s
	}
	if e, ok := m["err_msg"].(string); ok {
		errMsg = e
	}
	return
}

// resultPreview is the distilled preview of a run's result list.
type resultPreview struct {
	count        int
	sampleFields []string
}

// fetchResultPreview fetches /result?offset=0&limit=N for a run and returns
// the total count plus the non-empty field names of the first row. offset=0 is
// aligned so pagination compensation stays a single upstream request.
func fetchResultPreview(ctx context.Context, client *CoreClawClient, runID string, limit int) (resultPreview, error) {
	q := url.Values{}
	q.Set("offset", "0")
	q.Set("limit", strconv.Itoa(limit))
	data, err := client.doGetAuth(ctx, runStatusPath(runID)+"/result", q)
	if err != nil {
		return resultPreview{}, err
	}
	count, firstRow := decodeResultList(data)
	pv := resultPreview{count: count}
	if firstRow != nil {
		for k, v := range firstRow {
			if k == "__coreclaw_data_id__" {
				continue
			}
			if !isEmptyValue(v) {
				pv.sampleFields = append(pv.sampleFields, k)
			}
		}
	}
	return pv, nil
}

// pollTimeoutResult builds the result returned when poll_run exhausts its
// timeout before the run reaches a terminal state.
func pollTimeoutResult(runID, status string, pollCount int, start time.Time) (*mcp.CallToolResult, error) {
	return jsonResult(map[string]any{
		"run_id":     runID,
		"status":     status,
		"terminal":   false,
		"timed_out":  true,
		"poll_count": pollCount,
		"elapsed_ms": time.Since(start).Milliseconds(),
		"message":    "poll timed out before terminal state; call poll_run again or get_worker_run to continue",
	})
}

// pollRunHandler polls a run until terminal or timeout, returning the final
// status and (on success) a result preview.
func pollRunHandler(client *CoreClawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runIDRaw, ok, err := readV2Param(request, runIDPathParam())
		if err != nil || !ok {
			return mcp.NewToolResultError("run_id is required"), nil
		}
		runID, ok := runIDRaw.(string)
		if !ok {
			return mcp.NewToolResultError("run_id must be a string"), nil
		}
		timeoutSec := readIntParamDefault(request, "timeout_seconds", 300)
		intervalSec := readIntParamDefault(request, "poll_interval_seconds", 5)
		previewLimit := readIntParamDefault(request, "limit", 10)

		pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()

		path := runStatusPath(runID)
		start := time.Now()
		var pollCount int
		var lastStatus, lastErrMsg string

		for {
			pollCount++
			if pollCtx.Err() != nil {
				return pollTimeoutResult(runID, lastStatus, pollCount, start)
			}
			data, gerr := client.doGetAuth(pollCtx, path, nil)
			if gerr != nil {
				if pollCtx.Err() != nil {
					return pollTimeoutResult(runID, lastStatus, pollCount, start)
				}
				return mcp.NewToolResultError(fmt.Sprintf("poll failed after %d checks: %s", pollCount, gerr)), nil
			}
			lastStatus, lastErrMsg = parseRunStatus(data)
			if isTerminalStatus(lastStatus) {
				out := map[string]any{
					"run_id":     runID,
					"status":     lastStatus,
					"err_msg":    lastErrMsg,
					"poll_count": pollCount,
					"elapsed_ms": time.Since(start).Milliseconds(),
					"terminal":   true,
				}
				if lastStatus == "succeeded" && previewLimit > 0 {
					if pv, perr := fetchResultPreview(pollCtx, client, runID, previewLimit); perr == nil {
						out["result_count"] = pv.count
						out["sample_fields"] = pv.sampleFields
					}
				}
				return jsonResult(out)
			}
			select {
			case <-pollCtx.Done():
				return pollTimeoutResult(runID, lastStatus, pollCount, start)
			case <-time.After(time.Duration(intervalSec) * time.Second):
			}
		}
	}
}

// pollUntilTerminal polls a run until terminal or timeout. Used by
// run_workers_batch to wait on each item. Returns final status and err_msg.
func pollUntilTerminal(ctx context.Context, client *CoreClawClient, runID string, timeoutSec, intervalSec int) (status, errMsg string) {
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	path := runStatusPath(runID)
	for {
		if pollCtx.Err() != nil {
			return status, errMsg
		}
		data, err := client.doGetAuth(pollCtx, path, nil)
		if err != nil {
			return "unknown", err.Error()
		}
		status, errMsg = parseRunStatus(data)
		if isTerminalStatus(status) {
			return status, errMsg
		}
		select {
		case <-pollCtx.Done():
			return status, errMsg
		case <-time.After(time.Duration(intervalSec) * time.Second):
		}
	}
}
