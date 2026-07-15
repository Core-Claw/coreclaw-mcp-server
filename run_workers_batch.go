package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// run_workers_batch: run multiple workers in one call and return a per-item
// summary (run_slug, status, verdict). Serial by default; optional bounded
// concurrency. Each item is an ad-hoc async run_worker. The handler polls each
// run to a terminal state and optionally verifies the result.
//
// MCP has no streaming return, so the tool runs all items and returns a summary
// array once. items is capped at 50 so a single call stays within the HTTP
// WriteTimeout. skip_run_ids is best-effort: re-submitting an ad-hoc run starts
// a NEW run rather than returning the old one, so for exact resume callers
// should omit completed items from `items` instead.

const batchMaxItems = 50

// batchItem is one entry of the items array.
type batchItem struct {
	workerID string
	input    any // already-decoded custom payload (wrapped via wrapWorkerCustomInput)
	version  string
}

// parseBatchItems validates and parses the raw items array.
func parseBatchItems(raw []any) ([]batchItem, error) {
	out := make([]batchItem, 0, len(raw))
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("items[%d] must be an object", i)
		}
		wid, _ := m["worker_id"].(string)
		if wid == "" {
			return nil, fmt.Errorf("items[%d].worker_id is required", i)
		}
		it := batchItem{workerID: wid}
		if v, ok := m["input_json"]; ok {
			it.input = v
		}
		if v, ok := m["version"].(string); ok {
			it.version = v
		}
		out = append(out, it)
	}
	return out, nil
}

// extractRunSlug pulls run_slug (or slug) from a run-worker submit payload.
func extractRunSlug(data json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	if s, ok := m["run_slug"].(string); ok && s != "" {
		return s
	}
	if s, ok := m["slug"].(string); ok && s != "" {
		return s
	}
	return ""
}

// runOneBatchItem submits one worker run, polls it to terminal, and optionally
// verifies the result. Returns the per-item summary map.
func runOneBatchItem(ctx context.Context, client *CoreClawClient, it batchItem, timeoutSec, intervalSec int, doVerify bool, skipSet map[string]bool) map[string]any {
	out := map[string]any{"worker_id": it.workerID}

	body := map[string]any{"is_async": true}
	if it.input != nil {
		body["input"] = wrapWorkerCustomInput(it.input)
	} else {
		body["input"] = wrapWorkerCustomInput(map[string]any{})
	}
	if it.version != "" {
		body["version"] = it.version
	}
	submitPath := "/api/v2/workers/" + url.PathEscape(it.workerID) + "/runs"
	submitData, err := client.doPost(ctx, submitPath, body)
	if err != nil {
		out["status"] = "submit_failed"
		out["verdict"] = verdictSubmitFail
		out["err_msg"] = err.Error()
		return out
	}
	runSlug := extractRunSlug(submitData)
	out["run_slug"] = runSlug

	if runSlug != "" && skipSet[runSlug] {
		out["status"] = "skipped"
		out["verdict"] = "SKIPPED"
		return out
	}

	status, errMsg := pollUntilTerminal(ctx, client, runSlug, timeoutSec, intervalSec)
	out["status"] = status
	out["err_msg"] = errMsg

	switch {
	case !isTerminalStatus(status):
		out["verdict"] = verdictRunning
	case status != "succeeded":
		out["verdict"] = verdictFailed
	case doVerify:
		v := verifyRunByID(ctx, client, runSlug, 5)
		out["verdict"] = v.verdict
		out["real_field_count"] = v.realFieldCount
	default:
		out["verdict"] = verdictPass
	}
	return out
}

// runWorkersBatchHandler is the MCP handler for run_workers_batch.
func runWorkersBatchHandler(client *CoreClawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		itemsRaw, ok, err := readV2Param(request, v2ParamSpec{Name: "items", Location: v2BodyParam, Type: v2JSONParam, Required: true})
		if err != nil || !ok {
			return mcp.NewToolResultError("items is required (JSON array of {worker_id, input_json, version?})"), nil
		}
		items, ok := itemsRaw.([]any)
		if !ok || len(items) == 0 {
			return mcp.NewToolResultError("items must be a non-empty JSON array"), nil
		}
		if len(items) > batchMaxItems {
			return mcp.NewToolResultError(fmt.Sprintf("items length capped at %d per batch call", batchMaxItems)), nil
		}
		parsed, perr := parseBatchItems(items)
		if perr != nil {
			return mcp.NewToolResultError(perr.Error()), nil
		}

		concurrency := readIntParamDefault(request, "concurrency", 1)
		if concurrency < 1 {
			concurrency = 1
		}
		if concurrency > 10 {
			concurrency = 10
		}
		timeoutSec := readIntParamDefault(request, "timeout_seconds", 180)
		intervalSec := readIntParamDefault(request, "poll_interval_seconds", 5)
		verifyRaw, _, _ := readV2Param(request, v2ParamSpec{Name: "verify", Location: v2BodyParam, Type: v2BoolParam, Default: true})
		doVerify, _ := verifyRaw.(bool)

		skipSet := map[string]bool{}
		if skipRaw, ok, _ := readV2Param(request, v2ParamSpec{Name: "skip_run_ids", Location: v2BodyParam, Type: v2JSONParam}); ok {
			if arr, ok := skipRaw.([]any); ok {
				for _, s := range arr {
					if str, ok := s.(string); ok {
						skipSet[str] = true
					}
				}
			}
		}

		results := make([]map[string]any, len(parsed))
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i, it := range parsed {
			i, it := i, it
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i] = runOneBatchItem(ctx, client, it, timeoutSec, intervalSec, doVerify, skipSet)
			}()
		}
		wg.Wait()

		counts := map[string]int{}
		for _, r := range results {
			if v, ok := r["verdict"].(string); ok {
				counts[v]++
			}
		}
		return jsonResult(map[string]any{
			"total":   len(results),
			"counts":  counts,
			"results": results,
		})
	}
}
