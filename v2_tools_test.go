package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestV2PublicToolRegistryMatchesOpenAPIScope(t *testing.T) {
	specs := v2ToolSpecs()
	if len(specs) != 37 {
		t.Fatalf("expected 37 public v2 tools, got %d", len(specs))
	}

	seenNames := map[string]bool{}
	seenEndpoints := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" {
			t.Fatal("tool name must not be empty")
		}
		if seenNames[spec.Name] {
			t.Fatalf("duplicate tool name: %s", spec.Name)
		}
		seenNames[spec.Name] = true

		key := spec.Method + " " + spec.Path
		if seenEndpoints[key] {
			t.Fatalf("duplicate endpoint: %s", key)
		}
		seenEndpoints[key] = true

		if spec.Path == "/api/v2/workers/{workerId}/internal" {
			t.Fatalf("internal worker endpoint must not be exposed")
		}
		if spec.Path == "/api/v2/workers/{workerId}/versions" {
			t.Fatalf("create worker version endpoint must not be exposed")
		}
		if spec.Path == "/api/v2/workers/{workerId}/versions/{version}" {
			t.Fatalf("update worker version endpoint must not be exposed")
		}
		if spec.Method == "" || spec.Path == "" {
			t.Fatalf("tool %s must have method and path", spec.Name)
		}
		if spec.Description == "" {
			t.Fatalf("tool %s must have a bilingual description", spec.Name)
		}
	}

	required := []string{
		"list_proxy_regions",
		"list_store_workers",
		"get_account_info",
		"run_worker",
		"list_worker_runs",
		"get_worker_run",
		"list_worker_run_results",
		"export_worker_run_results",
		"run_worker_task",
		"rerun_worker_last_run",
	}
	for _, name := range required {
		if !seenNames[name] {
			t.Fatalf("expected public v2 tool %q to be registered", name)
		}
	}
}

func TestV2ToolWorkflowOrder(t *testing.T) {
	expected := []string{
		"list_proxy_regions",
		"list_store_workers",
		"list_workers",
		"get_worker",
		"get_worker_input_schema",
		"list_worker_tasks",
		"get_worker_task",
		"get_worker_task_input",
		"get_account_info",
		"create_worker_task",
		"update_worker_task",
		"update_worker_task_input",
		"run_worker",
		"run_worker_task",
		"run_workers_batch",
		"list_worker_runs",
		"get_last_worker_run",
		"get_worker_run",
		"poll_run",
		"verify_run",
		"get_worker_last_run",
		"list_last_worker_run_results",
		"export_last_worker_run_results",
		"get_last_worker_run_log",
		"list_worker_run_results",
		"export_worker_run_results",
		"get_worker_run_log",
		"list_worker_last_run_results",
		"export_worker_last_run_results",
		"get_worker_last_run_log",
		"rerun_last_worker_run",
		"rerun_worker_run",
		"rerun_worker_last_run",
		"abort_last_worker_run",
		"abort_worker_run",
		"abort_worker_last_run",
		"delete_worker_task",
	}

	specs := v2ToolSpecs()
	if len(specs) != len(expected) {
		t.Fatalf("expected %d workflow-ordered tools, got %d", len(expected), len(specs))
	}
	for i, spec := range specs {
		if spec.Name != expected[i] {
			t.Fatalf("tool order mismatch at %d: expected %s, got %s", i, expected[i], spec.Name)
		}
	}
}

func TestV2ToolsExposeExplicitMCPAnnotations(t *testing.T) {
	for _, spec := range v2ToolSpecs() {
		tool := spec.Tool()
		if tool.Annotations.Title == "" {
			t.Fatalf("tool %s must expose a human-readable title annotation", spec.Name)
		}

		expectReadOnly := spec.Method == http.MethodGet
		expectDestructive := spec.Method == http.MethodPost || spec.Method == http.MethodDelete
		expectIdempotent := spec.Method == http.MethodGet || spec.Method == http.MethodPut || spec.Method == http.MethodDelete || strings.Contains(spec.Name, "abort")
		expectOpenWorld := strings.HasPrefix(spec.Name, "run_") || strings.HasPrefix(spec.Name, "rerun_")

		assertBoolPtr(t, spec.Name, "readOnlyHint", tool.Annotations.ReadOnlyHint, expectReadOnly)
		assertBoolPtr(t, spec.Name, "destructiveHint", tool.Annotations.DestructiveHint, expectDestructive)
		assertBoolPtr(t, spec.Name, "idempotentHint", tool.Annotations.IdempotentHint, expectIdempotent)
		assertBoolPtr(t, spec.Name, "openWorldHint", tool.Annotations.OpenWorldHint, expectOpenWorld)
	}
}

// TestV2ListToolsCarryListKey asserts every GET tool that exposes offset+limit
// pagination also sets ListKey, so the transparent pagination-compensation
// layer is wired for it. A list tool missing ListKey would silently return
// wrong rows for unaligned offsets (the upstream pagination bug).
func TestV2ListToolsCarryListKey(t *testing.T) {
	expected := map[string]string{
		"list_store_workers":           "scraper",
		"list_workers":                 "scraper",
		"list_worker_runs":             "list",
		"list_worker_tasks":            "list",
		"list_worker_run_results":      "list",
		"list_last_worker_run_results": "list",
		"list_worker_last_run_results": "list",
	}
	specs := v2ToolSpecs()
	for _, spec := range specs {
		want, isList := expected[spec.Name]
		if isList {
			if spec.ListKey == "" {
				t.Fatalf("list tool %s must set ListKey for pagination compensation", spec.Name)
			}
			if spec.ListKey != want {
				t.Fatalf("list tool %s has ListKey %q, want %q", spec.Name, spec.ListKey, want)
			}
			continue
		}
		// Non-list tools must not set ListKey.
		if spec.ListKey != "" {
			t.Fatalf("non-list tool %s must not set ListKey (got %q)", spec.Name, spec.ListKey)
		}
	}
}

func TestV2ToolUsesGETPathQueryAndBearerAuth(t *testing.T) {
	var got struct {
		Method        string
		Path          string
		EscapedPath   string
		Query         url.Values
		Authorization string
		APIKeyHeader  string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Method = r.Method
		got.Path = r.URL.Path
		got.EscapedPath = r.URL.EscapedPath()
		got.Query = r.URL.Query()
		got.Authorization = r.Header.Get("Authorization")
		got.APIKeyHeader = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"ok":true}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_worker_run_results")
	// Use an aligned (offset, limit) so the pagination-compensation layer stays
	// inactive and this test asserts raw path/query/bearer pass-through.
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "user-token"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"run_id": "run/abc 123",
				"offset": 10,
				"limit":  10,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	if got.Method != http.MethodGet {
		t.Fatalf("expected GET, got %s", got.Method)
	}
	if got.EscapedPath != "/api/v2/worker-runs/run%2Fabc%20123/result" {
		t.Fatalf("unexpected escaped path: %s (decoded path: %s)", got.EscapedPath, got.Path)
	}
	if got.Query.Get("offset") != "10" || got.Query.Get("limit") != "10" {
		t.Fatalf("unexpected query: %s", got.Query.Encode())
	}
	if got.Authorization != "Bearer user-token" {
		t.Fatalf("expected bearer auth from context key, got %q", got.Authorization)
	}
	if got.APIKeyHeader != "" {
		t.Fatalf("did not expect legacy api-key header to be forwarded upstream, got %q", got.APIKeyHeader)
	}
}

func TestV2ToolUsesPOSTJSONBody(t *testing.T) {
	var got struct {
		Method string
		Path   string
		Body   map[string]any
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Method = r.Method
		got.Path = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got.Body); err != nil {
			t.Fatalf("unmarshal body %q: %v", string(body), err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"run_slug":"run_123"}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("fallback-token", upstream.URL)
	spec := mustV2ToolSpec(t, "run_worker")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_id":  "owner~demo-worker",
				"version":    "latest",
				"input_json": `{"keyword":"coffee","limit":10}`,
				"is_async":   false,
				"offset":     2,
				"limit":      5,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	if got.Method != http.MethodPost {
		t.Fatalf("expected POST, got %s", got.Method)
	}
	if got.Path != "/api/v2/workers/owner~demo-worker/runs" {
		t.Fatalf("unexpected path: %s", got.Path)
	}
	if got.Body["version"] != "latest" {
		t.Fatalf("expected version latest, got %#v", got.Body["version"])
	}
	if got.Body["is_async"] != false {
		t.Fatalf("expected is_async=false, got %#v", got.Body["is_async"])
	}
	if got.Body["offset"] != float64(2) || got.Body["limit"] != float64(5) {
		t.Fatalf("unexpected sync pagination body: %#v", got.Body)
	}
	input, ok := got.Body["input"].(map[string]any)
	if !ok {
		t.Fatalf("expected input object, got %#v", got.Body["input"])
	}
	parameters, ok := input["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters object, got %#v", input["parameters"])
	}
	custom, ok := parameters["custom"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters.custom object, got %#v", parameters["custom"])
	}
	if custom["keyword"] != "coffee" || custom["limit"] != float64(10) {
		t.Fatalf("unexpected input.parameters.custom object: %#v", custom)
	}
}

func TestV2RunWorkerAcceptsRawInputJSON(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal body %q: %v", string(body), err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"run_slug":"run_123"}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("fallback-token", upstream.URL)
	spec := mustV2ToolSpec(t, "run_worker")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_id":      "demo-worker",
				"raw_input_json": `{"parameters":{"system":{"proxy_region":"US"},"custom":{"keyword":"coffee"}}}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	input, ok := got["input"].(map[string]any)
	if !ok {
		t.Fatalf("expected input object, got %#v", got["input"])
	}
	parameters := input["parameters"].(map[string]any)
	system := parameters["system"].(map[string]any)
	custom := parameters["custom"].(map[string]any)
	if system["proxy_region"] != "US" || custom["keyword"] != "coffee" {
		t.Fatalf("unexpected raw input object: %#v", input)
	}
	if _, ok := got["raw_input_json"]; ok {
		t.Fatalf("raw_input_json must not be forwarded upstream: %#v", got)
	}
}

func TestV2RunWorkerRejectsAmbiguousInputJSON(t *testing.T) {
	client := NewCoreClawClient("token", "http://127.0.0.1:1")
	spec := mustV2ToolSpec(t, "run_worker")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_id":      "demo-worker",
				"input_json":     `{"keyword":"coffee"}`,
				"raw_input_json": `{"parameters":{"custom":{"keyword":"coffee"}}}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler should return tool error, not Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected tool error for ambiguous input_json/raw_input_json, got %+v", result)
	}
}

func TestV2CreateWorkerTaskWrapsInput(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/worker-tasks" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal body %q: %v", string(body), err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"slug":"task_1"}}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("fallback-token", upstream.URL)
	spec := mustV2ToolSpec(t, "create_worker_task")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_id":  "coreclaw~google-search-scraper",
				"title":      "Daily Search",
				"input_json": `{"keyword":"coffee","max_pages":"1"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	input, ok := got["input"].(map[string]any)
	if !ok {
		t.Fatalf("expected input object, got %#v", got["input"])
	}
	parameters, ok := input["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters object, got %#v", input["parameters"])
	}
	custom, ok := parameters["custom"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters.custom object, got %#v", parameters["custom"])
	}
	if custom["keyword"] != "coffee" || custom["max_pages"] != "1" {
		t.Fatalf("unexpected input.parameters.custom object: %#v", custom)
	}
	if _, ok := got["input_json"]; ok {
		t.Fatalf("input_json must not be forwarded upstream: %#v", got)
	}
}

func TestV2UpdateWorkerTaskInputWrapsInput(t *testing.T) {
	var got map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v2/worker-tasks/task_1/input" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal body %q: %v", string(body), err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("fallback-token", upstream.URL)
	spec := mustV2ToolSpec(t, "update_worker_task_input")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_task_id": "task_1",
				"input_json":     `{"keyword":"coffee","max_pages":"1"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	input, ok := got["input"].(map[string]any)
	if !ok {
		t.Fatalf("expected input object, got %#v", got["input"])
	}
	parameters, ok := input["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters object, got %#v", input["parameters"])
	}
	custom, ok := parameters["custom"].(map[string]any)
	if !ok {
		t.Fatalf("expected input.parameters.custom object, got %#v", parameters["custom"])
	}
	if custom["keyword"] != "coffee" {
		t.Fatalf("unexpected input.parameters.custom object: %#v", custom)
	}
}

func TestV2ToolRejectsInvalidInputJSON(t *testing.T) {
	client := NewCoreClawClient("token", "http://127.0.0.1:1")
	spec := mustV2ToolSpec(t, "run_worker")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"worker_id":  "demo-worker",
				"input_json": "not-json",
			},
		},
	})
	if err != nil {
		t.Fatalf("handler should return tool error, not Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected tool error for invalid input_json, got %+v", result)
	}
}

func TestRESTHandlerIncludesAllV2Tools(t *testing.T) {
	tools := restToolHandlers(NewCoreClawClient("token", "http://127.0.0.1:1"))
	if len(tools) != 37 {
		t.Fatalf("expected 37 REST tool handlers, got %d", len(tools))
	}
	if _, ok := tools["get_worker_internal"]; ok {
		t.Fatalf("internal endpoint must not have a REST handler")
	}
	if _, ok := tools["create_worker_version"]; ok {
		t.Fatalf("create worker version endpoint must not have a REST handler")
	}
	if _, ok := tools["update_worker_version"]; ok {
		t.Fatalf("update worker version endpoint must not have a REST handler")
	}
}

func TestMCPServerListsAllV2Tools(t *testing.T) {
	s := NewCoreClawMCPServer(NewCoreClawClient("token", "http://127.0.0.1:1"))
	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("create in-process client: %v", err)
	}
	defer c.Close()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start in-process client: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	initResult, err := c.Initialize(context.Background(), initReq)
	if err != nil {
		t.Fatalf("initialize in-process client: %v", err)
	}
	if initResult.ServerInfo.Title != "CoreClaw MCP Server" {
		t.Fatalf("expected server title annotation, got %q", initResult.ServerInfo.Title)
	}
	if initResult.ServerInfo.WebsiteURL != "https://mcp.coreclaw.com/mcp" {
		t.Fatalf("expected hosted MCP endpoint website URL, got %q", initResult.ServerInfo.WebsiteURL)
	}
	for _, want := range []string{
		"CoreClaw",
		"get_worker_input_schema",
		"run_worker",
		"中文",
		"不要调用",
	} {
		if !strings.Contains(initResult.Instructions, want) {
			t.Fatalf("initialize instructions must contain %q, got:\n%s", want, initResult.Instructions)
		}
	}

	tools, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 37 {
		t.Fatalf("expected 37 listed MCP tools, got %d", len(tools.Tools))
	}

	found := map[string]bool{}
	for i, tool := range tools.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %s missing description", tool.Name)
		}
		if tool.Name != v2ToolSpecs()[i].Name {
			t.Fatalf("MCP tools/list order mismatch at %d: expected %s, got %s", i, v2ToolSpecs()[i].Name, tool.Name)
		}
		if tool.Annotations.Title == "" {
			t.Fatalf("MCP tools/list tool %s missing title annotation", tool.Name)
		}
		assertBoolPtr(t, tool.Name, "readOnlyHint", tool.Annotations.ReadOnlyHint, v2ToolSpecs()[i].Method == http.MethodGet)
	}
	for _, name := range []string{"list_store_workers", "run_worker", "get_worker_input_schema", "list_worker_last_run_results"} {
		if !found[name] {
			t.Fatalf("expected MCP tool %s in tools/list response", name)
		}
	}
}

func TestDocumentationTreatsHostedEndpointAsFirstClassEntry(t *testing.T) {
	readme := mustReadUTF8(t, "README.md")
	assertContains(t, readme, "## Hosted Endpoint")
	assertContains(t, readme, "https://mcp.coreclaw.com/mcp")
	assertBefore(t, readme, "## Hosted Endpoint", "## Build And Test")
	if strings.Contains(readme, "https://your-server.example.com/mcp") {
		t.Fatalf("README.md should use the real hosted endpoint, not a placeholder remote URL")
	}

	readmeZH := mustReadUTF8(t, "README.zh-CN.md")
	assertContains(t, readmeZH, "## 托管入口")
	assertContains(t, readmeZH, "https://mcp.coreclaw.com/mcp")
	assertBefore(t, readmeZH, "## 托管入口", "## 构建和测试")
	if strings.Contains(readmeZH, "https://your-server.example.com/mcp") {
		t.Fatalf("README.zh-CN.md should use the real hosted endpoint, not a placeholder remote URL")
	}

	exampleConfig := mustReadUTF8(t, "codex-mcp.example.json")
	assertContains(t, exampleConfig, "https://mcp.coreclaw.com/mcp")
	if strings.Contains(exampleConfig, "https://your-server.example.com/mcp") {
		t.Fatalf("codex-mcp.example.json should make the hosted endpoint the first-class remote target")
	}
}

func mustV2ToolSpec(t *testing.T, name string) v2ToolSpec {
	t.Helper()
	for _, spec := range v2ToolSpecs() {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("missing v2 tool spec %q", name)
	return v2ToolSpec{}
}

func assertBoolPtr(t *testing.T, toolName, field string, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("tool %s annotation %s must be explicitly set", toolName, field)
	}
	if *got != want {
		t.Fatalf("tool %s annotation %s: expected %t, got %t", toolName, field, want, *got)
	}
}

func mustReadUTF8(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected content to contain %q", needle)
	}
}

func assertBefore(t *testing.T, haystack, earlier, later string) {
	t.Helper()
	earlierIndex := strings.Index(haystack, earlier)
	laterIndex := strings.Index(haystack, later)
	if earlierIndex < 0 {
		t.Fatalf("expected content to contain %q", earlier)
	}
	if laterIndex < 0 {
		t.Fatalf("expected content to contain %q", later)
	}
	if earlierIndex > laterIndex {
		t.Fatalf("expected %q before %q", earlier, later)
	}
}
