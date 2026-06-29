package main

import "encoding/json"

// CoreClawResponse is the standard response envelope from CoreClaw REST API.
type CoreClawResponse struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
	Details   []string        `json:"details"`
}

// coreClawErrors maps CoreClaw error codes to human-readable MCP error messages.
var coreClawErrors = map[int]string{
	10000: "Internal server error: please retry later",
	11000: "Invalid request parameters: check required fields and JSON body",
	11004: "Resource not found: verify the worker_id, run_id, or task id",
	12001: "Authentication required: check your CoreClaw API token",
	13000: "Rate limited: please wait and retry",
	4000:  "Invalid request parameters: check required fields",
	4010:  "Unauthorized: check your CORECLAW_API_KEY",
	4040:  "Resource not found: verify the slug",
	4290:  "Rate limited: please wait and retry",
	5000:  "Server error: please retry later",
	10001: "User not found or unavailable",
	10002: "User account disabled",
	20001: "Invalid API key",
	20002: "API key expired",
	30001: "Insufficient account balance",
	30002: "Insufficient traffic quota",
	50001: "Scraper not found",
	50002: "Scraper run failed",
	50003: "Scraper version not available",
	60001: "Task not found",
	70001: "Run record does not exist",
	70002: "Abort run failed",
}
