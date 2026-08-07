package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// storeListPage builds a CoreClaw-style store list response for items
// [start, end) so tests can assert which page the server actually requested.
func storeListPage(start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > 117 {
		end = 117
	}
	if start >= end {
		end = start // empty page
	}
	items := make([]map[string]any, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, map[string]any{
			"slug":  fmtSlug(i),
			"title": "worker",
		})
	}
	envelope := map[string]any{
		"code":    0,
		"message": "success",
		"data":    map[string]any{"scraper": items},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

func fmtSlug(i int) string {
	return "slug-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}

// runsListPage mirrors the worker-runs envelope (array under "list" + count).
func runsListPage(start, end, total int) string {
	if end > total {
		end = total
	}
	items := make([]map[string]any, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, map[string]any{"slug": fmtSlug(i), "status": "succeeded"})
	}
	envelope := map[string]any{
		"code":    0,
		"message": "success",
		"data":    map[string]any{"count": total, "list": items},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

// upstreamStorePage simulates the real CoreClaw backend with 1-based page
// numbering: a request for (offset, limit) returns page `offset` (1-based),
// i.e. rows [(offset-1)*limit, (offset-1)*limit + limit), capped at the
// 117-item dataset. offset=0 is treated as page 1 (compat).
func upstreamStorePage(offset, limit int) string {
	page := offset
	if page <= 0 {
		page = 1 // offset=0 compat → page 1
	}
	pageStart := (page - 1) * limit
	pageEnd := pageStart + limit
	return storeListPage(pageStart, pageEnd)
}

// newStoreUpstream starts a test server that honours 1-based page numbering.
func newStoreUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		offset := atoiOr(r.URL.Query().Get("offset"), 1)
		limit := atoiOr(r.URL.Query().Get("limit"), 20)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamStorePage(offset, limit))
	}))
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TestListStoreWorkersPassesOffsetThrough asserts the MCP layer passes
// offset/limit straight to the upstream (1-based page number) in a single
// request — no stitching, no multi-page walks.
func TestListStoreWorkersPassesOffsetThrough(t *testing.T) {
	cases := []struct {
		offset, limit, wantCount int
		wantFirst                string
	}{
		{1, 20, 20, "slug-0"},    // page 1
		{2, 20, 20, "slug-20"},   // page 2
		{0, 20, 20, "slug-0"},    // offset=0 compat → page 1
		{6, 20, 17, "slug-100"},  // page 6 (tail)
		{1, 100, 100, "slug-0"},  // page 1, full
		{2, 100, 17, "slug-100"}, // page 2, tail
	}
	for _, c := range cases {
		t.Run(itoa(c.offset)+"_"+itoa(c.limit), func(t *testing.T) {
			var hits atomic.Int32
			upstream := newStoreUpstream(t, &hits)
			defer upstream.Close()

			client := NewCoreClawClient("", upstream.URL)
			spec := mustV2ToolSpec(t, "list_store_workers")
			result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
				Params: mcp.CallToolParams{
					Arguments: map[string]any{"offset": c.offset, "limit": c.limit},
				},
			})
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			var data struct {
				Scraper []struct {
					Slug string `json:"slug"`
				} `json:"scraper"`
			}
			if err := json.Unmarshal([]byte(extractText(result)), &data); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(data.Scraper) != c.wantCount {
				t.Fatalf("offset=%d limit=%d: expected %d items, got %d", c.offset, c.limit, c.wantCount, len(data.Scraper))
			}
			if len(data.Scraper) > 0 && data.Scraper[0].Slug != c.wantFirst {
				t.Fatalf("offset=%d limit=%d: expected first %s, got %s", c.offset, c.limit, c.wantFirst, data.Scraper[0].Slug)
			}
			// Single upstream request — no compensation stitching.
			if n := hits.Load(); n != 1 {
				t.Fatalf("offset=%d limit=%d: expected 1 upstream hit, got %d", c.offset, c.limit, n)
			}
		})
	}
}

// TestListStoreWorkersDefaultPaginationUsesSingleRequest verifies the default
// path (no offset/limit arguments) issues exactly one request and returns
// page 1.
func TestListStoreWorkersDefaultPaginationUsesSingleRequest(t *testing.T) {
	var hits atomic.Int32
	upstream := newStoreUpstream(t, &hits)
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	_, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{}},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("default pagination expected 1 upstream hit, got %d", n)
	}
}

// TestListWorkerRunsPassesOffsetAndBearer checks the "list"-keyed auth path
// (worker-runs) passes offset/limit through in a single request and forwards
// the bearer token.
func TestListWorkerRunsPassesOffsetAndBearer(t *testing.T) {
	const total = 1566
	var hits atomic.Int32
	var seenAuth atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer user-token" {
			seenAuth.Add(1)
		}
		offset := atoiOr(r.URL.Query().Get("offset"), 1)
		limit := atoiOr(r.URL.Query().Get("limit"), 20)
		w.Header().Set("Content-Type", "application/json")
		page := offset
		if page <= 0 {
			page = 1
		}
		pageStart := (page - 1) * limit
		_, _ = io.WriteString(w, runsListPage(pageStart, pageStart+limit, total))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_worker_runs")
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "user-token"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"offset": 2, "limit": 100},
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var data struct {
		List []struct {
			Slug string `json:"slug"`
		} `json:"list"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(extractText(result)), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Page 2 = rows [100, 200) = 100 rows.
	if len(data.List) != 100 {
		t.Fatalf("expected 100 rows for page 2, got %d", len(data.List))
	}
	if data.List[0].Slug != "slug-100" {
		t.Fatalf("expected first slug slug-100, got %s", data.List[0].Slug)
	}
	if data.Count != total {
		t.Fatalf("expected total count %d preserved, got %d", total, data.Count)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("expected 1 upstream hit (no stitching), got %d", n)
	}
	if n := seenAuth.Load(); n != 1 {
		t.Fatalf("expected bearer auth on the request, got %d/1", n)
	}
}

// TestListStoreWorkersPreservesKeywordFilter ensures non-pagination query
// params (e.g. keyword) are forwarded on the single upstream request.
func TestListStoreWorkersPreservesKeywordFilter(t *testing.T) {
	type req struct {
		offset, limit int
		keyword       string
	}
	var got req
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = req{
			offset:  atoiOr(r.URL.Query().Get("offset"), 1),
			limit:   atoiOr(r.URL.Query().Get("limit"), 20),
			keyword: r.URL.Query().Get("keyword"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, storeListPage(0, 20))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	_, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"offset": 1, "limit": 100, "keyword": "amazon"},
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got.keyword != "amazon" {
		t.Errorf("expected keyword=amazon, got %q", got.keyword)
	}
	if got.offset != 1 || got.limit != 100 {
		t.Errorf("expected offset=1 limit=100, got offset=%d limit=%d", got.offset, got.limit)
	}
}

func extractText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	for _, c := range r.Content {
		if t, ok := c.(mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}
