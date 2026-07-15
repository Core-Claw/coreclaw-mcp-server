package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestCustomHandlerNilFallsBackToDefault proves the architecture change is
// zero-regression for the existing passthrough tools: every tool except the
// four custom-handler tools has CustomHandler == nil.
func TestCustomHandlerNilFallsBackToDefault(t *testing.T) {
	custom := map[string]bool{
		"poll_run":           true,
		"verify_run":         true,
		"run_workers_batch":  true,
		"get_worker_run_log": true,
	}
	for _, spec := range v2ToolSpecs() {
		if custom[spec.Name] {
			if spec.CustomHandler == nil {
				t.Fatalf("tool %s must set CustomHandler", spec.Name)
			}
			continue
		}
		if spec.CustomHandler != nil {
			t.Fatalf("passthrough tool %s must not set CustomHandler", spec.Name)
		}
	}
}

// --- judgeFirstRow unit tests ---

func TestJudgeFirstRow(t *testing.T) {
	cases := []struct {
		name        string
		row         map[string]any
		wantVerdict string
		wantReal    int
	}{
		{"real payload", map[string]any{"title": "x", "price": 1.0, "__coreclaw_data_id__": "id"}, verdictPass, 2},
		{"only diagnostics", map[string]any{"error": "captcha", "status": "failed", "__coreclaw_data_id__": "id"}, verdictErrorRecord, 0},
		{"only url weak", map[string]any{"url": "https://x"}, verdictErrorRecord, 1},
		{"url plus real", map[string]any{"url": "https://x", "title": "y"}, verdictPass, 2},
		{"empty values ignored", map[string]any{"title": "", "price": 0, "name": "n"}, verdictPass, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, real, _, _ := judgeFirstRow(c.row)
			if v != c.wantVerdict {
				t.Fatalf("verdict: got %s, want %s", v, c.wantVerdict)
			}
			if real != c.wantReal {
				t.Fatalf("realFieldCount: got %d, want %d", real, c.wantReal)
			}
		})
	}
}

// --- verify_run scenarios ---

func runVerifyScenario(t *testing.T, runStatus string, resultPayload string) string {
	t.Helper()
	var statusHits, resultHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/result"):
			resultHits.Add(1)
			_, _ = w.Write([]byte(resultPayload))
		default:
			statusHits.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"` + runStatus + `","err_msg":"some err"}}`))
		}
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "verify_run")
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "tok"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"run_id": "r1"}},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success, got %+v", result)
	}
	return extractText(result)
}

func TestVerifyRunPASS(t *testing.T) {
	txt := runVerifyScenario(t, "succeeded", `{"code":0,"data":{"count":2,"list":[{"title":"Apple Watch","price":299,"__coreclaw_data_id__":"x"}]}}`)
	var out map[string]any
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		t.Fatal(err)
	}
	if out["verdict"] != verdictPass {
		t.Fatalf("expected PASS, got %v", out["verdict"])
	}
}

func TestVerifyRunNoData(t *testing.T) {
	txt := runVerifyScenario(t, "succeeded", `{"code":0,"data":{"count":0,"list":[]}}`)
	var out map[string]any
	json.Unmarshal([]byte(txt), &out)
	if out["verdict"] != verdictNoData {
		t.Fatalf("expected NO_DATA, got %v", out["verdict"])
	}
}

func TestVerifyRunFailed(t *testing.T) {
	txt := runVerifyScenario(t, "failed", `{}`)
	var out map[string]any
	json.Unmarshal([]byte(txt), &out)
	if out["verdict"] != verdictFailed {
		t.Fatalf("expected FAILED, got %v", out["verdict"])
	}
}

// TestVerifyRunErrorRecord is the false-PASS guard: a CAPTCHA row populates the
// list but carries only diagnostic fields. Must be ERROR_RECORD, not PASS.
func TestVerifyRunErrorRecord(t *testing.T) {
	txt := runVerifyScenario(t, "succeeded", `{"code":0,"data":{"count":1,"list":[{"error":"Forbidden","error_code":"403","status":"failed","__coreclaw_data_id__":"x"}]}}`)
	var out map[string]any
	json.Unmarshal([]byte(txt), &out)
	if out["verdict"] != verdictErrorRecord {
		t.Fatalf("expected ERROR_RECORD, got %v", out["verdict"])
	}
}

func TestVerifyRunRunning(t *testing.T) {
	txt := runVerifyScenario(t, "running", `{}`)
	var out map[string]any
	json.Unmarshal([]byte(txt), &out)
	if out["verdict"] != verdictRunning {
		t.Fatalf("expected RUNNING, got %v", out["verdict"])
	}
}

// --- poll_run ---

func TestPollRunReachesSucceeded(t *testing.T) {
	var statusHits atomic.Int32
	states := []string{"running", "running", "succeeded"}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/result") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"count":2,"list":[{"title":"x","price":1,"__coreclaw_data_id__":"i"}]}}`))
			return
		}
		idx := int(statusHits.Add(1)) - 1
		st := "running"
		if idx < len(states) {
			st = states[idx]
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"` + st + `"}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "poll_run")
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "tok"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"run_id": "r1", "poll_interval_seconds": 0, "timeout_seconds": 5, "limit": 5}},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var out map[string]any
	if e := json.Unmarshal([]byte(extractText(result)), &out); e != nil {
		t.Fatalf("unmarshal %q: %v", extractText(result), e)
	}
	if out["status"] != "succeeded" {
		t.Fatalf("expected succeeded, got %v (raw: %s)", out["status"], extractText(result))
	}
	if out["terminal"] != true {
		t.Fatalf("expected terminal true, got %v", out["terminal"])
	}
	if out["result_count"] != float64(2) {
		t.Fatalf("expected result_count 2, got %v", out["result_count"])
	}
}

func TestPollRunTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"running"}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "poll_run")
	start := time.Now()
	result, _ := spec.Handler(client)(WithAPIKey(context.Background(), "tok"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"run_id": "r1", "poll_interval_seconds": 0, "timeout_seconds": 1}},
	})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("poll_run did not respect timeout")
	}
	var out map[string]any
	if e := json.Unmarshal([]byte(extractText(result)), &out); e != nil {
		t.Fatalf("unmarshal %q: %v", extractText(result), e)
	}
	if out["timed_out"] != true {
		t.Fatalf("expected timed_out true, got %v (raw: %s)", out["timed_out"], extractText(result))
	}
}

// --- get_worker_run_log grep ---

func TestGetWorkerRunLogNoGrepPassthrough(t *testing.T) {
	// client.doGetAuth unwraps the upstream envelope and returns `data`.
	// Without grep, the handler returns that data payload verbatim.
	wantData := `{"list":[{"content":"line A\nline B"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":` + wantData + `}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "get_worker_run_log")
	result, _ := spec.Handler(client)(WithAPIKey(context.Background(), "tok"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"run_id": "r1"}},
	})
	if extractText(result) != wantData {
		t.Fatalf("expected raw data passthrough, got %s", extractText(result))
	}
}

func TestGetWorkerRunLogGrepFilters(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"list":[{"content":"normal line\nTraceback (most recent call last)\n  File x\nmore normal"}]}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "get_worker_run_log")
	result, _ := spec.Handler(client)(WithAPIKey(context.Background(), "tok"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"run_id": "r1", "grep": "Traceback", "context_lines": 1}},
	})
	var out map[string]any
	json.Unmarshal([]byte(extractText(result)), &out)
	if out["matched_count"] == nil || out["matched_count"] == float64(0) {
		t.Fatalf("expected matched_count>0, got %v", out["matched_count"])
	}
}

func TestGrepLogLinesCaseInsensitive(t *testing.T) {
	lines := []string{"normal", "ERROR: something broke", "normal"}
	matched := grepLogLines(lines, "error", 0, 10)
	if len(matched) == 0 {
		t.Fatalf("expected case-insensitive match")
	}
}

func TestGrepLogLinesInvalidPatternFallsBack(t *testing.T) {
	lines := []string{"Error: x"}
	matched := grepLogLines(lines, "[invalid(", 0, 10)
	if len(matched) == 0 {
		t.Fatalf("expected fallback to default pattern")
	}
}

// --- run_workers_batch ---

func TestRunWorkersBatchRejectsOversize(t *testing.T) {
	client := NewCoreClawClient("", "http://unused")
	spec := mustV2ToolSpec(t, "run_workers_batch")
	items := make([]map[string]any, 51)
	for i := range items {
		items[i] = map[string]any{"worker_id": "w"}
	}
	raw, _ := json.Marshal(items)
	result, _ := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"items": string(raw)}},
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for >50 items, got %+v", result)
	}
}

func TestRunWorkersBatchRejectsMissingWorkerID(t *testing.T) {
	client := NewCoreClawClient("", "http://unused")
	spec := mustV2ToolSpec(t, "run_workers_batch")
	result, _ := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"items": `[{"input_json":{}}]`}},
	})
	if result == nil || !result.IsError {
		t.Fatalf("expected error for missing worker_id")
	}
}
