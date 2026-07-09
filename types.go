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

// coreClawErrors maps CoreClaw API v2 error codes (see error-codes.md) to
// human-readable MCP error messages. These only surface when the backend
// returns an empty message; otherwise the upstream message is used verbatim.
var coreClawErrors = map[int]string{
	10000: "Internal server error: please retry later",
	11000: "Invalid argument: check required fields and JSON body",
	11004: "Not found: verify the worker_id, run_id, or task id",
	12001: "Authentication required: check your CoreClaw API token",
	12002: "Invalid token: check your CoreClaw API key",
	13000: "Rate limited: please wait and retry",
	14000: "Database error: please retry later",
	16000: "Not implemented",
	30001: "Insufficient account balance",
	50001: "Worker not found",
	50002: "Worker run failed",
	50003: "Worker version not available",
	60001: "Task not found",
	70001: "Run record does not exist",
	70002: "Run operation failed",
}
