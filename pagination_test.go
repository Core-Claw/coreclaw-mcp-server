package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// upstreamStorePage implements the REAL CoreClaw behaviour derived from the
// grid map: a request for (offset, limit) returns rows
// [floor(offset/limit)*limit, floor(offset/limit)*limit + limit), capped at
// the 117-item dataset. This is the bug the compensation layer works around.
func upstreamStorePage(offset, limit int) string {
	pageStart := (offset / limit) * limit
	pageEnd := pageStart + limit
	return storeListPage(pageStart, pageEnd)
}

func newBuggyStoreUpstream(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		offset := atoiOr(r.URL.Query().Get("offset"), 0)
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

// TestPaginationNeedsCompensation verifies the detector matches the upstream
// rule: a single aligned request suffices iff offset is a multiple of limit.
func TestPaginationNeedsCompensation(t *testing.T) {
	cases := []struct {
		offset, limit int
		need          bool
	}{
		{0, 20, false},    // aligned page 0
		{0, 100, false},   // aligned first page
		{20, 20, false},   // aligned
		{100, 100, false}, // aligned
		{80, 100, true},   // 80%100 != 0 → BUG region (user's report)
		{20, 100, true},   // 20%100 != 0
		{50, 100, true},   // 50%100 != 0
		{50, 20, true},    // 50%20=10 → would return [40,60)
		{95, 10, true},    // 95%10=5 → would return [90,100)
		{90, 10, false},   // aligned
	}
	for _, c := range cases {
		got := paginationNeedsCompensation(c.offset, c.limit)
		if got != c.need {
			t.Errorf("paginationNeedsCompensation(%d,%d)=%v, want %v", c.offset, c.limit, got, c.need)
		}
	}
}

// TestListStoreWorkersCompensatesPagination is the regression test for the
// user-reported bug: list_store_workers(limit=100, offset=80) used to return
// the same first 100 rows as offset=0. With compensation the MCP tool must
// return rows [80, 117) (the real page).
func TestListStoreWorkersCompensatesPagination(t *testing.T) {
	var hits atomic.Int32
	upstream := newBuggyStoreUpstream(t, &hits)
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"offset": 80, "limit": 100},
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatalf("expected success result, got %+v", result)
	}

	var data struct {
		Scraper []struct {
			Slug string `json:"slug"`
		} `json:"scraper"`
	}
	if err := json.Unmarshal([]byte(extractText(result)), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Expect rows 80..116 inclusive (37 items), NOT rows 0..99.
	if len(data.Scraper) != 37 {
		t.Fatalf("expected 37 items for offset=80 limit=100 over 117 total, got %d", len(data.Scraper))
	}
	if data.Scraper[0].Slug != "slug-80" {
		t.Fatalf("expected first slug slug-80, got %s", data.Scraper[0].Slug)
	}
	if data.Scraper[len(data.Scraper)-1].Slug != "slug-116" {
		t.Fatalf("expected last slug slug-116, got %s", data.Scraper[len(data.Scraper)-1].Slug)
	}

	// Compensation walks aligned pages: (0,100) then (100,100). Two upstream
	// calls, never a single buggy (80,100).
	if n := hits.Load(); n != 2 {
		t.Fatalf("expected exactly 2 upstream sub-requests, got %d", n)
	}
}

// TestListStoreWorkersAlignedRequestsBypassCompensation ensures aligned
// (offset,limit) combos take the single-request path with no stitching.
func TestListStoreWorkersAlignedRequestsBypassCompensation(t *testing.T) {
	cases := []struct {
		offset, limit, wantCount, wantHits int
		wantFirst                          string
	}{
		{0, 20, 20, 1, "slug-0"},     // first small aligned page
		{0, 100, 100, 1, "slug-0"},   // first full page (aligned)
		{100, 20, 17, 1, "slug-100"}, // aligned (5th page of 20)
		{100, 100, 17, 1, "slug-100"},
		{20, 20, 20, 1, "slug-20"}, // aligned
		{80, 10, 10, 1, "slug-80"}, // aligned (8th page of 10)
	}
	for _, c := range cases {
		t.Run(itoa(c.offset)+"_"+itoa(c.limit), func(t *testing.T) {
			var hits atomic.Int32
			upstream := newBuggyStoreUpstream(t, &hits)
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
			if data.Scraper[0].Slug != c.wantFirst {
				t.Fatalf("offset=%d limit=%d: expected first %s, got %s", c.offset, c.limit, c.wantFirst, data.Scraper[0].Slug)
			}
			if n := hits.Load(); int(n) != c.wantHits {
				t.Fatalf("offset=%d limit=%d: expected %d upstream hits, got %d", c.offset, c.limit, c.wantHits, n)
			}
		})
	}
}

// TestListWorkerRunsCompensatesPagination checks the "list"-keyed auth path
// (worker-runs) compensates identically and forwards the bearer token on every
// sub-request.
func TestListWorkerRunsCompensatesPagination(t *testing.T) {
	const total = 1566
	var hits atomic.Int32
	var seenAuth atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer user-token" {
			seenAuth.Add(1)
		}
		offset := atoiOr(r.URL.Query().Get("offset"), 0)
		limit := atoiOr(r.URL.Query().Get("limit"), 20)
		w.Header().Set("Content-Type", "application/json")
		// Simulate the same upstream bug for the list envelope.
		pageStart := (offset / limit) * limit
		_, _ = io.WriteString(w, runsListPage(pageStart, pageStart+limit, total))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_worker_runs")
	result, err := spec.Handler(client)(WithAPIKey(context.Background(), "user-token"), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"offset": 20, "limit": 100},
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
	// [20,120) = 100 rows. base=0, lead=20 → collect 120, drop 20 → 100.
	if len(data.List) != 100 {
		t.Fatalf("expected 100 merged rows, got %d", len(data.List))
	}
	if data.List[0].Slug != "slug-20" {
		t.Fatalf("expected first slug slug-20, got %s", data.List[0].Slug)
	}
	if data.List[99].Slug != "slug-119" {
		t.Fatalf("expected last slug slug-119, got %s", data.List[99].Slug)
	}
	if data.Count != total {
		t.Fatalf("expected total count %d preserved, got %d", total, data.Count)
	}
	// base=0 → page (0,100) + (100,100) = 2 hits, both authed.
	if n := hits.Load(); n != 2 {
		t.Fatalf("expected 2 upstream hits, got %d", n)
	}
	if n := seenAuth.Load(); n != 2 {
		t.Fatalf("expected bearer auth on both sub-requests, got %d/2", n)
	}
}

// TestCompensatePaginationPreservesKeywordFilter ensures non-pagination query
// params (e.g. keyword) are forwarded on every sub-request.
func TestCompensatePaginationPreservesKeywordFilter(t *testing.T) {
	type req struct {
		offset, limit int
		keyword       string
	}
	var mu sync.Mutex
	var got []req
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, req{
			offset:  atoiOr(r.URL.Query().Get("offset"), 0),
			limit:   atoiOr(r.URL.Query().Get("limit"), 20),
			keyword: r.URL.Query().Get("keyword"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Return a full page so the compensation loop walks a second aligned
		// page (we only assert query forwarding here, not row contents).
		_, _ = io.WriteString(w, storeListPage(0, 100))
	}))
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	_, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: map[string]any{"offset": 30, "limit": 100, "keyword": "amazon"},
		},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sub-requests, got %d", len(got))
	}
	for i, r := range got {
		if r.keyword != "amazon" {
			t.Errorf("sub-request %d: expected keyword=amazon, got %q", i, r.keyword)
		}
	}
	// Aligned pages: base=0 → (0,100) + (100,100).
	want := []req{{0, 100, "amazon"}, {100, 100, "amazon"}}
	for i := range got {
		if got[i].offset != want[i].offset || got[i].limit != want[i].limit {
			t.Errorf("sub-request %d: expected offset=%d limit=%d, got offset=%d limit=%d", i, want[i].offset, want[i].limit, got[i].offset, got[i].limit)
		}
	}
}

// TestListStoreWorkersCompensatesSmallLimit covers limit<100 misalignment:
// offset=50, limit=20 would naively return [40,60); compensation returns [50,70).
func TestListStoreWorkersCompensatesSmallLimit(t *testing.T) {
	var hits atomic.Int32
	upstream := newBuggyStoreUpstream(t, &hits)
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"offset": 50, "limit": 20}},
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
	if len(data.Scraper) != 20 {
		t.Fatalf("expected 20 items, got %d", len(data.Scraper))
	}
	if data.Scraper[0].Slug != "slug-50" {
		t.Fatalf("expected first slug slug-50, got %s", data.Scraper[0].Slug)
	}
	if data.Scraper[19].Slug != "slug-69" {
		t.Fatalf("expected last slug slug-69, got %s", data.Scraper[19].Slug)
	}
	// base=40, lead=10 → need 30 rows → pages (40,20)+(60,20) = 2 hits.
	if n := hits.Load(); n != 2 {
		t.Fatalf("expected 2 sub-requests, got %d", n)
	}
}

// TestCompensatePaginationShortTail covers a window that runs past the dataset
// end: offset=95, limit=100 over 117 total → rows [95,117) = 22.
func TestCompensatePaginationShortTail(t *testing.T) {
	var hits atomic.Int32
	upstream := newBuggyStoreUpstream(t, &hits)
	defer upstream.Close()

	client := NewCoreClawClient("", upstream.URL)
	spec := mustV2ToolSpec(t, "list_store_workers")
	result, err := spec.Handler(client)(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"offset": 95, "limit": 100}},
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
	if len(data.Scraper) != 22 {
		t.Fatalf("expected 22 items (tail), got %d", len(data.Scraper))
	}
	if data.Scraper[0].Slug != "slug-95" {
		t.Fatalf("expected first slug slug-95, got %s", data.Scraper[0].Slug)
	}
	if data.Scraper[21].Slug != "slug-116" {
		t.Fatalf("expected last slug slug-116, got %s", data.Scraper[21].Slug)
	}
}

// TestListStoreWorkersDefaultPaginationUsesSingleRequest verifies the default
// (offset=0, limit=20) path still issues exactly one request.
func TestListStoreWorkersDefaultPaginationUsesSingleRequest(t *testing.T) {
	var hits atomic.Int32
	upstream := newBuggyStoreUpstream(t, &hits)
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
