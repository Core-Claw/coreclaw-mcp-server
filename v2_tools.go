package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type v2ParamLocation string
type v2ParamType string

const (
	v2PathParam  v2ParamLocation = "path"
	v2QueryParam v2ParamLocation = "query"
	v2BodyParam  v2ParamLocation = "body"

	v2StringParam v2ParamType = "string"
	v2NumberParam v2ParamType = "number"
	v2BoolParam   v2ParamType = "boolean"
	v2JSONParam   v2ParamType = "json"
)

type v2ParamSpec struct {
	Name        string
	Location    v2ParamLocation
	Type        v2ParamType
	Required    bool
	Description string
	Default     any
	Enum        []string
	Min         *float64
	Max         *float64
}

type v2ToolSpec struct {
	Name        string
	Method      string
	Path        string
	Auth        bool
	Description string
	Params      []v2ParamSpec
}

func (s v2ToolSpec) Tool() mcp.Tool {
	opts := []mcp.ToolOption{
		mcp.WithDescription(s.Description),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithTitleAnnotation(s.titleAnnotation()),
		mcp.WithReadOnlyHintAnnotation(s.readOnlyHint()),
		mcp.WithDestructiveHintAnnotation(s.destructiveHint()),
		mcp.WithIdempotentHintAnnotation(s.idempotentHint()),
		mcp.WithOpenWorldHintAnnotation(s.openWorldHint()),
	}
	for _, p := range s.Params {
		opts = append(opts, p.toolOption())
	}
	return mcp.NewTool(s.Name, opts...)
}

func (s v2ToolSpec) titleAnnotation() string {
	parts := strings.Split(s.Name, "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func (s v2ToolSpec) readOnlyHint() bool {
	return s.Method == http.MethodGet
}

func (s v2ToolSpec) destructiveHint() bool {
	return s.Method == http.MethodPost || s.Method == http.MethodDelete
}

func (s v2ToolSpec) idempotentHint() bool {
	return s.Method == http.MethodGet || s.Method == http.MethodPut || s.Method == http.MethodDelete || strings.Contains(s.Name, "abort")
}

func (s v2ToolSpec) openWorldHint() bool {
	return strings.HasPrefix(s.Name, "run_") || strings.HasPrefix(s.Name, "rerun_")
}

func (s v2ToolSpec) Handler(client *CoreClawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		path := s.Path
		query := url.Values{}
		body := map[string]any{}
		hasBody := false

		for _, p := range s.Params {
			value, ok, err := readV2Param(request, p)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if !ok {
				continue
			}

			switch p.Location {
			case v2PathParam:
				text, ok := value.(string)
				if !ok {
					return mcp.NewToolResultError(fmt.Sprintf("%s must be a string path parameter", p.Name)), nil
				}
				path = strings.ReplaceAll(path, "{"+toOpenAPIPathParamName(p.Name)+"}", url.PathEscape(text))
			case v2QueryParam:
				query.Set(p.Name, formatQueryValue(value))
			case v2BodyParam:
				body[toOpenAPIBodyParamName(p.Name)] = value
				hasBody = true
			}
		}

		var data json.RawMessage
		var err error
		switch s.Method {
		case http.MethodGet:
			if s.Auth {
				data, err = client.doGetAuth(ctx, path, query)
			} else {
				data, err = client.doGet(ctx, path, query)
			}
		case http.MethodPost:
			var bodyArg any
			if hasBody {
				preparedBody, err := s.prepareBody(body)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				bodyArg = preparedBody
			} else {
				bodyArg = map[string]any{}
			}
			data, err = client.doPost(ctx, path, bodyArg)
		case http.MethodPut:
			var bodyArg any
			if hasBody {
				preparedBody, err := s.prepareBody(body)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				bodyArg = preparedBody
			} else {
				bodyArg = map[string]any{}
			}
			data, err = client.doPut(ctx, path, bodyArg)
		case http.MethodDelete:
			data, err = client.doDelete(ctx, path)
		default:
			return mcp.NewToolResultError("unsupported CoreClaw API method: " + s.Method), nil
		}
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	}
}

func (s v2ToolSpec) prepareBody(body map[string]any) (map[string]any, error) {
	// run_worker, create_worker_task, and update_worker_task_input all accept
	// the caller's business fields as input_json and must send them upstream as
	// input.parameters.custom to match CoreClaw's saved task payload contract.
	// Sending input_json unwrapped makes the saved task un-runnable (backend
	// rejects it with "Keyword is required" for required custom fields).
	wrapsInput := s.Name == "run_worker" ||
		s.Name == "create_worker_task" ||
		s.Name == "update_worker_task_input"
	if !wrapsInput {
		return body, nil
	}

	input, hasInput := body["input"]
	rawInput, hasRawInput := body["raw_input_json"]
	if hasInput && hasRawInput {
		return nil, fmt.Errorf("use either input_json or raw_input_json, not both")
	}
	if hasRawInput {
		body["input"] = rawInput
		delete(body, "raw_input_json")
		return body, nil
	}
	if hasInput {
		body["input"] = wrapWorkerCustomInput(input)
	}
	return body, nil
}

func wrapWorkerCustomInput(input any) any {
	if inputMap, ok := input.(map[string]any); ok {
		if _, hasParameters := inputMap["parameters"]; hasParameters {
			return input
		}
	}
	return map[string]any{
		"parameters": map[string]any{
			"custom": input,
		},
	}
}

func (p v2ParamSpec) toolOption() mcp.ToolOption {
	opts := []mcp.PropertyOption{mcp.Description(p.Description)}
	if p.Required {
		opts = append(opts, mcp.Required())
	}
	if len(p.Enum) > 0 {
		opts = append(opts, mcp.Enum(p.Enum...))
	}
	if p.Min != nil {
		opts = append(opts, mcp.Min(*p.Min))
	}
	if p.Max != nil {
		opts = append(opts, mcp.Max(*p.Max))
	}
	switch v := p.Default.(type) {
	case string:
		opts = append(opts, mcp.DefaultString(v))
	case int:
		opts = append(opts, mcp.DefaultNumber(float64(v)))
	case bool:
		opts = append(opts, mcp.DefaultBool(v))
	}

	switch p.Type {
	case v2NumberParam:
		return mcp.WithNumber(p.Name, opts...)
	case v2BoolParam:
		return mcp.WithBoolean(p.Name, opts...)
	case v2JSONParam:
		return mcp.WithString(p.Name, opts...)
	default:
		return mcp.WithString(p.Name, opts...)
	}
}

func readV2Param(request mcp.CallToolRequest, p v2ParamSpec) (any, bool, error) {
	args := request.GetArguments()
	raw, exists := args[p.Name]
	if !exists || raw == nil || raw == "" {
		if p.Default != nil {
			return p.Default, true, nil
		}
		if p.Required {
			return nil, false, fmt.Errorf("missing required argument %s", p.Name)
		}
		return nil, false, nil
	}

	switch p.Type {
	case v2NumberParam:
		switch v := raw.(type) {
		case int:
			return v, true, nil
		case int64:
			return v, true, nil
		case float64:
			return int(v), true, nil
		case string:
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, false, fmt.Errorf("%s must be a number", p.Name)
			}
			return n, true, nil
		default:
			return nil, false, fmt.Errorf("%s must be a number", p.Name)
		}
	case v2BoolParam:
		switch v := raw.(type) {
		case bool:
			return v, true, nil
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, false, fmt.Errorf("%s must be true or false", p.Name)
			}
			return b, true, nil
		default:
			return nil, false, fmt.Errorf("%s must be true or false", p.Name)
		}
	case v2JSONParam:
		switch v := raw.(type) {
		case string:
			var decoded any
			if err := json.Unmarshal([]byte(v), &decoded); err != nil {
				return nil, false, fmt.Errorf("%s must be a valid JSON string: %w", p.Name, err)
			}
			return decoded, true, nil
		case map[string]any, []any:
			return v, true, nil
		default:
			return nil, false, fmt.Errorf("%s must be a JSON object, array, or JSON string", p.Name)
		}
	default:
		text, ok := raw.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s must be a string", p.Name)
		}
		if len(p.Enum) > 0 && !containsString(p.Enum, text) {
			return nil, false, fmt.Errorf("%s must be one of: %s", p.Name, strings.Join(p.Enum, ", "))
		}
		return text, true, nil
	}
}

func formatQueryValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprint(v)
	}
}

func toOpenAPIPathParamName(name string) string {
	switch name {
	case "worker_id":
		return "workerId"
	case "run_id":
		return "runId"
	case "worker_task_id":
		return "workerTaskId"
	case "input_json":
		return "input"
	default:
		return name
	}
}

func toOpenAPIBodyParamName(name string) string {
	if name == "input_json" {
		return "input"
	}
	return name
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func float64Ptr(v float64) *float64 {
	return &v
}

func publicDescription(action, when, returns, workflow string) string {
	return action + "\n\n" +
		"WHEN TO USE: " + when + " 中文触发: 当用户要在 CoreClaw 中查询、运行、重跑、停止、导出或查看对应 worker/run/task 数据时使用。\n\n" +
		"WHEN NOT TO USE: Do not use public web search or code search for private CoreClaw platform data. Do not call excluded internal worker-version or internal-detail APIs.\n\n" +
		"RETURNS: " + returns + "\n\n" +
		"WORKFLOW: " + workflow
}

func workerIDParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "worker_id",
		Location:    v2PathParam,
		Type:        v2StringParam,
		Required:    true,
		Description: "Worker slug or owner path. Example: \"demo-worker\" or \"owner~demo-worker\". Obtain from list_store_workers or list_workers.",
	}
}

func runIDPathParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "run_id",
		Location:    v2PathParam,
		Type:        v2StringParam,
		Required:    true,
		Description: "Worker run identifier. Example: \"01KKDXV2G26BT7NH4ZQR2R4NPZ\". Obtain from run_worker, list_worker_runs, or get_last_worker_run.",
	}
}

func workerTaskIDParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "worker_task_id",
		Location:    v2PathParam,
		Type:        v2StringParam,
		Required:    true,
		Description: "Saved worker task slug. Example: \"task_daily_demo\". Obtain from list_worker_tasks.",
	}
}

func offsetParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "offset",
		Location:    v2QueryParam,
		Type:        v2NumberParam,
		Description: "Result offset. Example: 0. Must be 0 or greater. (default: 0)",
		Default:     0,
		Min:         float64Ptr(0),
	}
}

func limitParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "limit",
		Location:    v2QueryParam,
		Type:        v2NumberParam,
		Description: "Result limit. Example: 20. Must be 1-100. (default: 20)",
		Default:     20,
		Min:         float64Ptr(1),
		Max:         float64Ptr(100),
	}
}

func bodyOffsetParam() v2ParamSpec {
	p := offsetParam()
	p.Location = v2BodyParam
	return p
}

func bodyLimitParam() v2ParamSpec {
	p := limitParam()
	p.Location = v2BodyParam
	return p
}

func keywordParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "keyword",
		Location:    v2QueryParam,
		Type:        v2StringParam,
		Description: "Search keyword for title, slug, or path. Example: \"amazon\". Leave empty to list all matching records. (optional)",
	}
}

func formatParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "format",
		Location:    v2QueryParam,
		Type:        v2StringParam,
		Description: "Export format. Allowed: csv, json. (default: csv)",
		Default:     "csv",
		Enum:        []string{"csv", "json"},
	}
}

func filterKeysParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "filter_keys",
		Location:    v2QueryParam,
		Type:        v2StringParam,
		Description: "Comma-separated field keys to include. Example: \"title,price,url\". Leave empty to export all fields. (optional)",
	}
}

func callbackParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "callback_url",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Description: "Callback URL for asynchronous status updates. Example: \"https://client.example.com/openapi/callback\". (optional)",
	}
}

func isAsyncParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "is_async",
		Location:    v2BodyParam,
		Type:        v2BoolParam,
		Description: "Whether CoreClaw should run asynchronously. Example: true. Use false only for small synchronous runs. (default: true)",
		Default:     true,
	}
}

func runBodyParams() []v2ParamSpec {
	return []v2ParamSpec{callbackParam(), isAsyncParam(), bodyOffsetParam(), bodyLimitParam()}
}

// --- worker-task CRUD body params ---

func taskTitleParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "title",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Required:    true,
		Description: "Task title. Example: \"Daily Amazon Price Check\".",
	}
}

func taskDescriptionParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "description",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Description: "Task description. (optional)",
	}
}

func taskVersionParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "version",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Description: "Worker version. Defaults to current worker version. Example: \"latest\" or \"1.0.1\". (optional)",
	}
}

func taskWorkerIDBodyParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "worker_id",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Required:    true,
		Description: "Worker slug or owner path. Example: \"demo-worker\" or \"owner~demo-worker\". Obtain from list_store_workers or list_workers.",
	}
}

func taskInputJSONBodyParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "input_json",
		Location:    v2BodyParam,
		Type:        v2JSONParam,
		Required:    true,
		Description: "Task input parameters as a JSON object string. Example: {\"keyword\":\"coffee\",\"limit\":10}. Schema comes from get_worker_input_schema.",
	}
}

func taskScheduleTypeParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_type",
		Location:    v2BodyParam,
		Type:        v2NumberParam,
		Description: "Schedule type: 1=daily, 2=weekly, 3=monthly. (optional)",
	}
}

func taskScheduleTimeParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_time",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Description: "Schedule time in HH:mm format. Example: \"09:00\". (optional)",
	}
}

func taskScheduleWeekdayParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_weekday",
		Location:    v2BodyParam,
		Type:        v2NumberParam,
		Description: "Day of week for weekly schedules: 0-6, 0=Sunday. (optional)",
	}
}

func taskScheduleDayParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_day",
		Location:    v2BodyParam,
		Type:        v2NumberParam,
		Description: "Day of month for monthly schedule. (optional)",
	}
}

func taskScheduleOnceDateParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_once_date",
		Location:    v2BodyParam,
		Type:        v2StringParam,
		Description: "Once schedule date in YYYY-MM-DD format. Example: \"2026-12-25\". (optional)",
	}
}

func taskScheduleEnabledParam() v2ParamSpec {
	return v2ParamSpec{
		Name:        "schedule_enabled",
		Location:    v2BodyParam,
		Type:        v2NumberParam,
		Description: "Schedule switch: 0 disabled, 1 enabled. (optional)",
	}
}

func v2ToolSpecs() []v2ToolSpec {
	return orderV2ToolSpecs([]v2ToolSpec{
		{Name: "list_proxy_regions", Method: http.MethodGet, Path: "/api/v2/proxy/region", Description: publicDescription("List CoreClaw proxy regions in English or Chinese.", "Use when the user needs proxy country or region codes before running a worker, such as US, JP, DE, or Chinese localized names.", "JSON with a list of proxy regions and region codes.", "Call before run_worker when the worker input schema asks for proxy_region."), Params: []v2ParamSpec{{Name: "language", Location: v2QueryParam, Type: v2StringParam, Description: "Region display language. Example: \"en\" or \"zh\". (default: en)", Default: "en"}}},
		{Name: "list_store_workers", Method: http.MethodGet, Path: "/api/v2/store", Description: publicDescription("Search the public CoreClaw worker marketplace for ready-to-run workers.", "Use when the user wants to find, discover, browse, or search CoreClaw scrapers/workers by keyword or site name.", "JSON with matching store workers, including slug, path, title, username, and description.", "Usually first step. Follow with get_worker_input_schema or get_worker before run_worker."), Params: []v2ParamSpec{offsetParam(), limitParam(), keywordParam()}},
		{Name: "get_account_info", Method: http.MethodGet, Path: "/api/v2/users/account", Auth: true, Description: publicDescription("Get the current user's CoreClaw account balance and traffic quota.", "Use when the user asks for balance, remaining traffic, quota, billing state, or whether they can run jobs.", "JSON with balance and balance_expiration_at.", "Terminal call or preflight before run_worker.")},
		{Name: "list_worker_runs", Method: http.MethodGet, Path: "/api/v2/worker-runs", Auth: true, Description: publicDescription("List the current user's CoreClaw worker runs.", "Use when the user wants run history, recent jobs, or to find a run_id by worker or status.", "JSON with count, list, offset/page data, run slug, worker info, status, usage, traffic, and timestamps.", "Follow with get_worker_run, list_worker_run_results, export_worker_run_results, rerun_worker_run, or abort_worker_run."), Params: []v2ParamSpec{offsetParam(), limitParam(), {Name: "worker_id", Location: v2QueryParam, Type: v2StringParam, Description: "Filter by worker slug or owner path. Example: \"demo-worker\" or \"owner~demo-worker\". (optional)"}, {Name: "status", Location: v2QueryParam, Type: v2StringParam, Description: "Filter by run status. Allowed: ready, running, succeeded, failed, aborting. (optional)", Enum: []string{"ready", "running", "succeeded", "failed", "aborting"}}}},
		{Name: "get_last_worker_run", Method: http.MethodGet, Path: "/api/v2/worker-runs/last", Auth: true, Description: publicDescription("Get the current user's most recent CoreClaw worker run.", "Use when the user says last run, latest job, most recent scrape, or asks what just happened.", "JSON with the latest run's slug, status, worker, version, timestamps, usage, traffic, and result count.", "Follow with list_last_worker_run_results, export_last_worker_run_results, get_last_worker_run_log, rerun_last_worker_run, or abort_last_worker_run.")},
		{Name: "abort_last_worker_run", Method: http.MethodPost, Path: "/api/v2/worker-runs/last/abort", Auth: true, Description: publicDescription("Abort the current user's most recent CoreClaw worker run.", "Use when the user wants to stop or cancel the last running job.", "JSON success envelope data, often null.", "Call after get_last_worker_run confirms the last run is still active."), Params: []v2ParamSpec{}},
		{Name: "export_last_worker_run_results", Method: http.MethodGet, Path: "/api/v2/worker-runs/last/export", Auth: true, Description: publicDescription("Export the current user's most recent CoreClaw run results.", "Use when the user asks to download/export the latest run as CSV or JSON.", "JSON with a temporary download_url.", "Call after get_last_worker_run shows status succeeded."), Params: []v2ParamSpec{formatParam(), filterKeysParam()}},
		{Name: "get_last_worker_run_log", Method: http.MethodGet, Path: "/api/v2/worker-runs/last/log", Auth: true, Description: publicDescription("Get logs for the current user's most recent CoreClaw worker run.", "Use when debugging why the latest run failed, stalled, or produced unexpected output.", "JSON with recent run log data.", "Call after get_last_worker_run, especially for failed or running states.")},
		{Name: "rerun_last_worker_run", Method: http.MethodPost, Path: "/api/v2/worker-runs/last/rerun", Auth: true, Description: publicDescription("Rerun the current user's most recent CoreClaw worker run with the same saved inputs.", "Use when the user says rerun last, retry latest, or do the previous scrape again.", "JSON with a new run_slug or synchronous result fields.", "Follow with get_last_worker_run or list_last_worker_run_results depending on is_async."), Params: runBodyParams()},
		{Name: "list_last_worker_run_results", Method: http.MethodGet, Path: "/api/v2/worker-runs/last/result", Auth: true, Description: publicDescription("List paginated results from the current user's most recent CoreClaw run.", "Use when the user wants to preview, inspect, or page through latest run output.", "JSON with result rows and pagination metadata.", "Call after get_last_worker_run shows status succeeded; use export_last_worker_run_results for large output."), Params: []v2ParamSpec{offsetParam(), limitParam()}},
		{Name: "get_worker_run", Method: http.MethodGet, Path: "/api/v2/worker-runs/{runId}", Auth: true, Description: publicDescription("Get detail for a specific CoreClaw worker run by run_id.", "Use when the user gives a run id or wants status/cost/detail for a specific run.", "JSON with run status, worker, version, timestamps, usage, traffic, error, and result count.", "Follow with results, logs, export, rerun, or abort tools for the same run_id."), Params: []v2ParamSpec{runIDPathParam()}},
		{Name: "abort_worker_run", Method: http.MethodPost, Path: "/api/v2/worker-runs/{runId}/abort", Auth: true, Description: publicDescription("Abort a specific CoreClaw worker run by run_id.", "Use when the user wants to cancel a known running run.", "JSON success envelope data, often null.", "Call after get_worker_run confirms status is ready or running."), Params: []v2ParamSpec{runIDPathParam()}},
		{Name: "get_worker_run_log", Method: http.MethodGet, Path: "/api/v2/worker-runs/{runId}/log", Auth: true, Description: publicDescription("Get logs for a specific CoreClaw worker run.", "Use to debug a known run id, especially failed, stalled, or suspicious runs.", "JSON with log data for the run.", "Call after get_worker_run when status or output needs explanation."), Params: []v2ParamSpec{runIDPathParam()}},
		{Name: "rerun_worker_run", Method: http.MethodPost, Path: "/api/v2/worker-runs/{runId}/rerun", Auth: true, Description: publicDescription("Rerun a specific CoreClaw worker run with the same saved inputs.", "Use when the user wants to retry or repeat a known run id.", "JSON with a new run_slug or synchronous result fields.", "Follow with get_worker_run or list_worker_run_results for the new run."), Params: append([]v2ParamSpec{runIDPathParam()}, runBodyParams()...)},
		{Name: "list_worker_run_results", Method: http.MethodGet, Path: "/api/v2/worker-runs/{runId}/result", Auth: true, Description: publicDescription("List paginated results for a specific CoreClaw worker run.", "Use when the user wants records/output rows from a known run id.", "JSON with result rows and pagination metadata.", "Call after get_worker_run shows status succeeded; use export_worker_run_results for large output."), Params: []v2ParamSpec{runIDPathParam(), offsetParam(), limitParam()}},
		{Name: "export_worker_run_results", Method: http.MethodGet, Path: "/api/v2/worker-runs/{runId}/result/export", Auth: true, Description: publicDescription("Export result data for a specific CoreClaw worker run.", "Use when the user asks to download or save output from a known run as CSV or JSON.", "JSON with a temporary download_url.", "Call after get_worker_run shows status succeeded."), Params: []v2ParamSpec{runIDPathParam(), formatParam(), filterKeysParam()}},
		// --- worker-task CRUD ---
		{Name: "create_worker_task", Method: http.MethodPost, Path: "/api/v2/worker-tasks", Auth: true, Description: publicDescription("Create a new saved CoreClaw worker task with input and optional schedule.", "Use when the user wants to save a worker configuration as a reusable, scheduled task.", "JSON with the created task details including slug.", "Follow with run_worker_task using the returned worker_task_id."), Params: []v2ParamSpec{taskWorkerIDBodyParam(), taskTitleParam(), taskInputJSONBodyParam(), taskDescriptionParam(), taskVersionParam(), taskScheduleTypeParam(), taskScheduleTimeParam(), taskScheduleWeekdayParam(), taskScheduleDayParam(), taskScheduleOnceDateParam(), taskScheduleEnabledParam()}},
		{Name: "get_worker_task", Method: http.MethodGet, Path: "/api/v2/worker-tasks/{workerTaskId}", Auth: true, Description: publicDescription("Get detail for a specific saved CoreClaw worker task.", "Use when the user wants to inspect a saved task's configuration, schedule, or input.", "JSON with task details including title, description, worker_id, input, schedule, and slug.", "Follow with update_worker_task, update_worker_task_input, run_worker_task, or delete_worker_task."), Params: []v2ParamSpec{workerTaskIDParam()}},
		{Name: "update_worker_task", Method: http.MethodPut, Path: "/api/v2/worker-tasks/{workerTaskId}", Auth: true, Description: publicDescription("Update a saved CoreClaw worker task's metadata and schedule.", "Use when the user wants to change a task's title, description, or schedule settings.", "JSON success envelope data, often null.", "Call after get_worker_task to confirm current settings. Use update_worker_task_input to update the task's input payload separately."), Params: []v2ParamSpec{workerTaskIDParam(), taskTitleParam(), taskDescriptionParam(), taskScheduleTypeParam(), taskScheduleTimeParam(), taskScheduleWeekdayParam(), taskScheduleDayParam(), taskScheduleOnceDateParam(), taskScheduleEnabledParam()}},
		{Name: "delete_worker_task", Method: http.MethodDelete, Path: "/api/v2/worker-tasks/{workerTaskId}", Auth: true, Description: publicDescription("Delete a saved CoreClaw worker task.", "Use when the user wants to permanently remove a saved task.", "JSON success envelope data, often null.", "Call after list_worker_tasks or get_worker_task confirms the task exists."), Params: []v2ParamSpec{workerTaskIDParam()}},
		{Name: "get_worker_task_input", Method: http.MethodGet, Path: "/api/v2/worker-tasks/{workerTaskId}/input", Auth: true, Description: publicDescription("Get the input payload for a saved CoreClaw worker task.", "Use when the user wants to inspect or copy a task's saved input parameters.", "JSON with the task's input object and optional version field.", "Follow with update_worker_task_input or run_worker_task."), Params: []v2ParamSpec{workerTaskIDParam()}},
		{Name: "update_worker_task_input", Method: http.MethodPut, Path: "/api/v2/worker-tasks/{workerTaskId}/input", Auth: true, Description: publicDescription("Update the input payload for a saved CoreClaw worker task.", "Use when the user wants to change a task's saved input parameters without modifying its title/schedule.", "JSON success envelope data, often null.", "Call after get_worker_task_input to confirm the current input. Then use run_worker_task to execute with the new input."), Params: []v2ParamSpec{workerTaskIDParam(), taskInputJSONBodyParam(), taskVersionParam()}},

		{Name: "list_worker_tasks", Method: http.MethodGet, Path: "/api/v2/worker-tasks", Auth: true, Description: publicDescription("List saved CoreClaw worker tasks for the current user.", "Use when the user wants saved tasks, scheduled presets, configured jobs, or task ids.", "JSON list of saved worker tasks.", "Follow with run_worker_task using worker_task_id."), Params: []v2ParamSpec{offsetParam(), limitParam(), {Name: "worker_id", Location: v2QueryParam, Type: v2StringParam, Description: "Filter by worker slug or owner path. Example: \"demo-worker\". (optional)"}, keywordParam()}},
		{Name: "run_worker_task", Method: http.MethodPost, Path: "/api/v2/worker-tasks/{workerTaskId}/runs", Auth: true, Description: publicDescription("Run a saved CoreClaw worker task.", "Use when the user wants to execute a configured task rather than supply ad-hoc worker input.", "JSON with run_slug or synchronous result fields.", "Follow with get_worker_run or get_last_worker_run, then result/export tools."), Params: append([]v2ParamSpec{workerTaskIDParam()}, runBodyParams()...)},
		{Name: "list_workers", Method: http.MethodGet, Path: "/api/v2/workers", Auth: true, Description: publicDescription("List CoreClaw workers owned by the current user.", "Use when the user wants their private/current-user workers, not the public marketplace.", "JSON with worker slug, path, title, username, and description.", "Follow with get_worker, get_worker_input_schema, run_worker, or worker-specific last-run tools."), Params: []v2ParamSpec{offsetParam(), limitParam(), keywordParam()}},
		{Name: "get_worker", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}", Auth: true, Description: publicDescription("Get detail for a CoreClaw worker.", "Use before running a worker to inspect version, README, and parameters.", "JSON with worker name, username, version, readme, and parameters.", "Follow with get_worker_input_schema and then run_worker."), Params: []v2ParamSpec{workerIDParam()}},
		{Name: "get_worker_input_schema", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}/input-schema", Description: publicDescription("Get the public input JSON schema for a CoreClaw worker.", "Use when the user wants to know required input fields or before composing run_worker input_json.", "JSON with input_schema.", "Call before run_worker so input_json matches the worker schema."), Params: []v2ParamSpec{workerIDParam()}},		{Name: "run_worker", Method: http.MethodPost, Path: "/api/v2/workers/{workerId}/runs", Auth: true, Description: publicDescription("Run a CoreClaw worker with an ad-hoc JSON input payload.", "Use when the user wants to start, execute, scrape, crawl, or run a worker with specific input.", "JSON with run_slug for async runs or synchronous result fields for sync runs.", "Call get_worker_input_schema first, then run_worker, then get_worker_run or get_last_worker_run, then results/export/log tools."), Params: append([]v2ParamSpec{workerIDParam(), {Name: "version", Location: v2BodyParam, Type: v2StringParam, Description: "Worker script version. Example: \"latest\" or \"1.0.1\". Obtain from get_worker; default is backend latest. (optional)"}, {Name: "input_json", Location: v2BodyParam, Type: v2JSONParam, Description: "Worker business input payload as a JSON object string. Example: {\"keyword\":\"coffee\",\"limit\":10}. The MCP server sends it as input.parameters.custom, matching CoreClaw saved task payloads. Schema comes from get_worker_input_schema. Marked optional because the schema does not force it, but almost every worker requires input fields to run — omit only when the worker has no business fields. (optional)"}, {Name: "raw_input_json", Location: v2BodyParam, Type: v2JSONParam, Description: "Advanced escape hatch: full CoreClaw input object to send as input without wrapping. Example: {\"parameters\":{\"system\":{\"proxy_region\":\"US\"},\"custom\":{\"keyword\":\"coffee\"}}}. Do not combine with input_json. (optional)"}}, runBodyParams()...)},
		{Name: "get_worker_last_run", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}/runs/last", Auth: true, Description: publicDescription("Get the most recent run for a specific CoreClaw worker.", "Use when the user asks for the last run of a specific worker.", "JSON with last run details for that worker.", "Follow with worker-specific last result/export/log/rerun/abort tools."), Params: []v2ParamSpec{workerIDParam()}},
		{Name: "abort_worker_last_run", Method: http.MethodPost, Path: "/api/v2/workers/{workerId}/runs/last/abort", Auth: true, Description: publicDescription("Abort the most recent run for a specific CoreClaw worker.", "Use when the user wants to cancel the latest active run of a known worker.", "JSON success envelope data, often null.", "Call after get_worker_last_run confirms the run is active."), Params: []v2ParamSpec{workerIDParam()}},
		{Name: "export_worker_last_run_results", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}/runs/last/export", Auth: true, Description: publicDescription("Export results from the most recent run of a specific CoreClaw worker.", "Use when the user asks to download/export the latest output for a known worker.", "JSON with a temporary download_url.", "Call after get_worker_last_run shows status succeeded."), Params: []v2ParamSpec{workerIDParam(), formatParam(), filterKeysParam()}},
		{Name: "get_worker_last_run_log", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}/runs/last/log", Auth: true, Description: publicDescription("Get logs for the most recent run of a specific CoreClaw worker.", "Use when debugging the latest run for a specific worker.", "JSON with log data.", "Call after get_worker_last_run when status or output needs explanation."), Params: []v2ParamSpec{workerIDParam()}},
		{Name: "rerun_worker_last_run", Method: http.MethodPost, Path: "/api/v2/workers/{workerId}/runs/last/rerun", Auth: true, Description: publicDescription("Rerun the most recent run for a specific CoreClaw worker.", "Use when the user asks to retry or repeat the latest run for a known worker.", "JSON with a new run_slug or synchronous result fields.", "Follow with get_worker_last_run or list_worker_last_run_results."), Params: append([]v2ParamSpec{workerIDParam()}, runBodyParams()...)},
		{Name: "list_worker_last_run_results", Method: http.MethodGet, Path: "/api/v2/workers/{workerId}/runs/last/result", Auth: true, Description: publicDescription("List paginated results from the most recent run of a specific CoreClaw worker.", "Use when the user wants latest output rows for a known worker.", "JSON with result rows and pagination metadata.", "Call after get_worker_last_run shows status succeeded; use export_worker_last_run_results for large output."), Params: []v2ParamSpec{workerIDParam(), offsetParam(), limitParam()}},
	})
}

func orderV2ToolSpecs(specs []v2ToolSpec) []v2ToolSpec {
	byName := make(map[string]v2ToolSpec, len(specs))
	for _, spec := range specs {
		byName[spec.Name] = spec
	}

	order := v2ToolWorkflowOrder()
	ordered := make([]v2ToolSpec, 0, len(order))
	for _, name := range order {
		spec, ok := byName[name]
		if !ok {
			panic("missing v2 tool spec in workflow order: " + name)
		}
		ordered = append(ordered, spec)
		delete(byName, name)
	}
	for name := range byName {
		panic("v2 tool spec missing from workflow order: " + name)
	}
	return ordered
}

func v2ToolWorkflowOrder() []string {
	return []string{
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
		"list_worker_runs",
		"get_last_worker_run",
		"get_worker_run",
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
}
