package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestV2PublicToolRegistryMatchesOpenAPIScope(t *testing.T) {
	specs := v2ToolSpecs()
	if len(specs) != 28 {
		t.Fatalf("expected 28 public v2 tools, got %d", len(specs))
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
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "user-token"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{
				"run_id": "run/abc 123",
				"offset": 7,
				"limit":  33,
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
	if got.Query.Get("offset") != "7" || got.Query.Get("limit") != "33" {
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
	if len(tools) != 28 {
		t.Fatalf("expected 28 REST tool handlers, got %d", len(tools))
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
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("initialize in-process client: %v", err)
	}

	tools, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 28 {
		t.Fatalf("expected 28 listed MCP tools, got %d", len(tools.Tools))
	}

	found := map[string]bool{}
	for _, tool := range tools.Tools {
		found[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %s missing description", tool.Name)
		}
	}
	for _, name := range []string{"list_store_workers", "run_worker", "get_worker_input_schema", "list_worker_last_run_results"} {
		if !found[name] {
			t.Fatalf("expected MCP tool %s in tools/list response", name)
		}
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
