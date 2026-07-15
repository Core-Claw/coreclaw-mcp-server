package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// get_worker_run_log grep: extends the existing get_worker_run_log tool with
// optional in-process filtering. The upstream log endpoint returns
// data.list[].content and does not support server-side grep, so filtering is
// done here. When grep is unset, the handler returns the raw upstream payload
// unchanged (backward compatible).

// defaultErrGrep is the keyword pattern used when a caller asks for error lines
// without specifying a pattern (e.g. verify_run's err_lines extraction).
const defaultErrGrep = "Error|raise|Exception|Traceback|BANNED|403|429|CAPTCHA|blocked|forbidden"

// extractLogLines flattens a log-endpoint payload into a slice of text lines.
// Each log entry's `content` may itself contain newlines; they are split.
func extractLogLines(data json.RawMessage) []string {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	rawList, ok := m["list"].([]any)
	if !ok {
		return nil
	}
	var lines []string
	for _, e := range rawList {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		c, ok := entry["content"].(string)
		if !ok || c == "" {
			continue
		}
		lines = append(lines, strings.Split(c, "\n")...)
	}
	return lines
}

// grepLogLines returns matching regions from lines. Each region is the matched
// line plus contextLines before/after (clamped), de-duplicated when regions
// overlap. Caps roughly at maxMatches regions. pattern is compiled
// case-insensitive; an invalid pattern falls back to defaultErrGrep.
func grepLogLines(lines []string, pattern string, contextLines, maxMatches int) []map[string]any {
	if contextLines < 0 {
		contextLines = 0
	}
	if maxMatches < 1 {
		maxMatches = 1
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil || strings.TrimSpace(pattern) == "" {
		re = regexp.MustCompile("(?i)" + defaultErrGrep)
	}
	var out []map[string]any
	covered := make(map[int]bool)
	for i, ln := range lines {
		if !re.MatchString(ln) {
			continue
		}
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		end := i + contextLines
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := start; j <= end; j++ {
			if j < 0 || covered[j] {
				continue
			}
			covered[j] = true
			out = append(out, map[string]any{
				"text":       lines[j],
				"line_index": j,
				"match":      j == i,
			})
		}
		if len(out) >= maxMatches*2 {
			break
		}
	}
	return out
}

// fetchErrLines pulls the run log and extracts lines matching default error
// markers. Best-effort: returns nil on any failure. Used by verify_run.
func fetchErrLines(ctx context.Context, client *CoreClawClient, runID string) []string {
	data, err := client.doGetAuth(ctx, runStatusPath(runID)+"/log", nil)
	if err != nil {
		return nil
	}
	lines := extractLogLines(data)
	matched := grepLogLines(lines, defaultErrGrep, 1, 20)
	out := make([]string, 0, len(matched))
	for _, m := range matched {
		if t, ok := m["text"].(string); ok {
			out = append(out, t)
		}
	}
	return out
}

// getWorkerRunLogHandler is the MCP handler for get_worker_run_log with
// optional grep filtering. When grep is unset it returns the raw upstream log.
func getWorkerRunLogHandler(client *CoreClawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		runIDRaw, ok, err := readV2Param(request, runIDPathParam())
		if err != nil || !ok {
			return mcp.NewToolResultError("run_id is required"), nil
		}
		runID, ok := runIDRaw.(string)
		if !ok {
			return mcp.NewToolResultError("run_id must be a string"), nil
		}
		grepRaw, _, _ := readV2Param(request, v2ParamSpec{Name: "grep", Location: v2QueryParam, Type: v2StringParam})
		grep, _ := grepRaw.(string)
		contextLines := readIntParamDefault(request, "context_lines", 2)
		maxMatches := readIntParamDefault(request, "max_matches", 50)

		data, gerr := client.doGetAuth(ctx, runStatusPath(runID)+"/log", nil)
		if gerr != nil {
			return mcp.NewToolResultError(gerr.Error()), nil
		}
		if strings.TrimSpace(grep) == "" {
			// Backward compatible: return raw upstream payload.
			return mcp.NewToolResultText(string(data)), nil
		}
		lines := extractLogLines(data)
		matched := grepLogLines(lines, grep, contextLines, maxMatches)
		return jsonResult(map[string]any{
			"run_id":        runID,
			"total_lines":   len(lines),
			"matched_count": len(matched),
			"lines":         matched,
		})
	}
}
