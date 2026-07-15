package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// verify_run: verify a run produced real, usable data and return a structured
// verdict. The judgment distinguishes genuine payload rows from "error records"
// (rows that populate the list but carry only diagnostic fields such as
// error/status), which are a common false-PASS trap.

// Verdict values returned by verify_run.
const (
	verdictPass        = "PASS"         // succeeded + count>0 + first row has real payload
	verdictNoData      = "NO_DATA"      // succeeded but count==0
	verdictFailed      = "FAILED"       // status failed/aborted
	verdictErrorRecord = "ERROR_RECORD" // rows exist but first row is an error record
	verdictRunning     = "RUNNING"      // not yet terminal
	verdictSubmitFail  = "SUBMIT_FAIL"  // run could not be started / not found
)

// diagnosticKeys are fields that carry no worker payload — only run/diagnostic
// metadata. A first row whose only populated fields are in this set is an error
// record, not a successful result.
var diagnosticKeys = map[string]bool{
	"__coreclaw_data_id__": true,
	"error":                true,
	"error_code":           true,
	"err":                  true,
	"err_code":             true,
	"status":               true,
	"status_code":          true,
	"message":              true,
	"msg":                  true,
	"input":                true,
	"input_url":            true,
}

// weakFields are real-looking but cannot alone satisfy PASS (e.g. a url with no
// accompanying content). A first row with only weak fields is ERROR_RECORD.
var weakFields = map[string]bool{
	"url": true,
}

// isEmptyValue reports whether a JSON value is effectively empty.
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case bool:
		return false
	case float64:
		return false
	case int, int64:
		return false
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// judgeFirstRow decides PASS vs ERROR_RECORD for the first result row.
// Returns verdict, realFieldCount, sampleFields (capped at 10), reason.
func judgeFirstRow(row map[string]any) (verdict string, realFieldCount int, sampleFields []string, reason string) {
	var realFields []string
	weakOnly := true
	for k, v := range row {
		if diagnosticKeys[k] {
			continue
		}
		if isEmptyValue(v) {
			continue
		}
		realFields = append(realFields, k)
		if !weakFields[k] {
			weakOnly = false
		}
	}
	realFieldCount = len(realFields)
	if realFieldCount > 0 {
		sampleFields = realFields
		if len(sampleFields) > 10 {
			sampleFields = sampleFields[:10]
		}
	}
	switch {
	case realFieldCount == 0:
		verdict = verdictErrorRecord
		reason = "first row carries only diagnostic/error fields, no worker payload"
	case weakOnly:
		verdict = verdictErrorRecord
		reason = "first row has only weak fields (url), no substantive payload"
	default:
		verdict = verdictPass
	}
	return
}

// decodeResultList extracts data.count and the first row (data.list[0]) from a
// result-endpoint payload. Returns (0, nil) when the list is absent/empty.
func decodeResultList(data json.RawMessage) (count int, firstRow map[string]any) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, nil
	}
	if c, ok := m["count"].(float64); ok {
		count = int(c)
	}
	rawList, ok := m["list"].([]any)
	if !ok || len(rawList) == 0 {
		return count, nil
	}
	if row, ok := rawList[0].(map[string]any); ok {
		return count, row
	}
	return count, nil
}

// verifyRunByID runs the verify judgment on a known run id. Returns verdict,
// realFieldCount, sampleFields. Used by run_workers_batch.
type verifyResult struct {
	verdict        string
	realFieldCount int
	sampleFields   []string
}

func verifyRunByID(ctx context.Context, client *CoreClawClient, runID string, sniffLimit int) verifyResult {
	runData, err := client.doGetAuth(ctx, runStatusPath(runID), nil)
	if err != nil {
		return verifyResult{verdict: verdictSubmitFail}
	}
	status, _ := parseRunStatus(runData)
	if !isTerminalStatus(status) {
		return verifyResult{verdict: verdictRunning}
	}
	if status != "succeeded" {
		return verifyResult{verdict: verdictFailed}
	}
	q := url.Values{}
	q.Set("offset", "0")
	q.Set("limit", strconv.Itoa(sniffLimit))
	resData, err := client.doGetAuth(ctx, runStatusPath(runID)+"/result", q)
	if err != nil {
		return verifyResult{verdict: verdictErrorRecord}
	}
	count, firstRow := decodeResultList(resData)
	if count == 0 || firstRow == nil {
		return verifyResult{verdict: verdictNoData}
	}
	v, realFieldCount, sampleFields, _ := judgeFirstRow(firstRow)
	return verifyResult{verdict: v, realFieldCount: realFieldCount, sampleFields: sampleFields}
}

// verifyRunHandler is the MCP handler for verify_run.
func verifyRunHandler(client *CoreClawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runIDRaw, ok, err := readV2Param(request, runIDPathParam())
		if err != nil || !ok {
			return mcp.NewToolResultError("run_id is required"), nil
		}
		runID, ok := runIDRaw.(string)
		if !ok {
			return mcp.NewToolResultError("run_id must be a string"), nil
		}
		sniffLimit := readIntParamDefault(request, "limit", 5)

		runData, gerr := client.doGetAuth(ctx, runStatusPath(runID), nil)
		if gerr != nil {
			return mcp.NewToolResultError(gerr.Error()), nil
		}
		status, errMsg := parseRunStatus(runData)
		out := map[string]any{"run_id": runID, "status": status, "err_msg": errMsg}

		if !isTerminalStatus(status) {
			out["verdict"] = verdictRunning
			return jsonResult(out)
		}
		if status != "succeeded" {
			out["verdict"] = verdictFailed
			out["err_lines"] = fetchErrLines(ctx, client, runID)
			return jsonResult(out)
		}

		q := url.Values{}
		q.Set("offset", "0")
		q.Set("limit", strconv.Itoa(sniffLimit))
		resData, gerr := client.doGetAuth(ctx, runStatusPath(runID)+"/result", q)
		if gerr != nil {
			out["verdict"] = verdictErrorRecord
			out["verdict_reason"] = "result fetch failed: " + gerr.Error()
			return jsonResult(out)
		}
		count, firstRow := decodeResultList(resData)
		out["count"] = count
		if count == 0 || firstRow == nil {
			out["verdict"] = verdictNoData
			out["real_field_count"] = 0
			return jsonResult(out)
		}
		v, realFieldCount, sampleFields, reason := judgeFirstRow(firstRow)
		out["verdict"] = v
		out["real_field_count"] = realFieldCount
		out["sample_fields"] = sampleFields
		if reason != "" {
			out["verdict_reason"] = reason
		}
		return jsonResult(out)
	}
}
