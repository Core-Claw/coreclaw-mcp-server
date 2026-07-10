package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CoreClawClient wraps HTTP communication with the CoreClaw REST API.
type CoreClawClient struct {
	baseURL string
	// apiKey is a fallback used only when the request context has none
	// (stdio / local-dev mode). Public HTTP deployments should pass each
	// caller's token through the request context.
	apiKey     string
	httpClient *http.Client
}

type apiKeyCtxKey struct{}

// WithAPIKey returns a derived context that carries a per-request CoreClaw token.
func WithAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, strings.TrimSpace(key))
}

// APIKeyFromContext extracts the CoreClaw token from ctx, if any.
func APIKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(apiKeyCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// NewCoreClawClient creates a new CoreClaw API client.
func NewCoreClawClient(apiKey, baseURL string) *CoreClawClient {
	return &CoreClawClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  strings.TrimSpace(apiKey),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// doGet sends a GET request to a public CoreClaw API endpoint.
func (c *CoreClawClient) doGet(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodGet, path, params, nil, false)
}

// doGetAuth sends a GET request to an authenticated CoreClaw API endpoint.
func (c *CoreClawClient) doGetAuth(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodGet, path, params, nil, true)
}

// doPost sends a POST request to an authenticated CoreClaw API endpoint.
func (c *CoreClawClient) doPost(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodPost, path, nil, body, true)
}

// doPut sends a PUT request to an authenticated CoreClaw API endpoint.
func (c *CoreClawClient) doPut(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodPut, path, nil, body, true)
}

// doDelete sends a DELETE request to an authenticated CoreClaw API endpoint.
func (c *CoreClawClient) doDelete(ctx context.Context, path string) (json.RawMessage, error) {
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil, true)
}

func (c *CoreClawClient) doJSON(ctx context.Context, method, path string, params url.Values, body any, authRequired bool) (json.RawMessage, error) {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	var reader io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reader = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authRequired {
		token := APIKeyFromContext(ctx)
		if token == "" {
			token = c.apiKey
		}
		if token == "" {
			return nil, fmt.Errorf("missing CoreClaw API token: send an 'api-key', 'X-API-Key', or 'Authorization: Bearer <token>' header in HTTP mode, or set CORECLAW_API_KEY for stdio mode")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	return c.doRequest(req)
}

// doRequest executes an HTTP request and parses the CoreClaw response envelope.
func (c *CoreClawClient) doRequest(req *http.Request) (json.RawMessage, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CoreClaw API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read CoreClaw API response: %w", err)
	}

	var coreClawResp CoreClawResponse
	if err := json.Unmarshal(respBody, &coreClawResp); err != nil {
		return nil, fmt.Errorf("failed to parse CoreClaw API response (HTTP %d): %s", resp.StatusCode, string(respBody[:min(len(respBody), 200)]))
	}

	if resp.StatusCode >= 400 || coreClawResp.Code != 0 {
		msg := coreClawResp.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if mapped, ok := coreClawErrors[coreClawResp.Code]; coreClawResp.Code != 0 && msg == "" && ok {
			msg = mapped
		}
		if coreClawResp.RequestID != "" {
			msg += " (request_id: " + coreClawResp.RequestID + ")"
		}
		if len(coreClawResp.Details) > 0 {
			msg += " details: " + strings.Join(coreClawResp.Details, "; ")
		}
		if coreClawResp.Code != 0 {
			return nil, fmt.Errorf("[%d] %s", coreClawResp.Code, msg)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	if coreClawResp.Data == nil {
		return json.RawMessage(`null`), nil
	}
	return coreClawResp.Data, nil
}
