package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tavily-proxy/server/internal/db"
	"tavily-proxy/server/internal/services"
)

func TestLatestTavilyInputSchemas(t *testing.T) {
	t.Parallel()

	search := schemaProperties(t, tavilySearchInputSchema)
	for _, name := range []string{"exact_match", "timeout", "safe_search"} {
		if _, ok := search[name]; !ok {
			t.Errorf("search schema missing %q", name)
		}
	}

	extract := schemaProperties(t, tavilyExtractInputSchema)
	for _, name := range []string{"query", "chunks_per_source", "timeout"} {
		if _, ok := extract[name]; !ok {
			t.Errorf("extract schema missing %q", name)
		}
	}
	for _, name := range []string{"include_image_descriptions", "include_domains", "exclude_domains", "country"} {
		if _, ok := extract[name]; ok {
			t.Errorf("extract schema contains unsupported %q", name)
		}
	}
	urls := mustStructuredMap(t, extract["urls"])
	if _, ok := urls["oneOf"]; !ok {
		t.Error("extract urls must accept either one URL or an array")
	}

	for schemaName, schema := range map[string]map[string]any{"crawl": tavilyCrawlInputSchema, "map": tavilyMapInputSchema} {
		properties := schemaProperties(t, schema)
		if _, ok := properties["timeout"]; !ok {
			t.Errorf("%s schema missing timeout", schemaName)
		}
		breadth := mustStructuredMap(t, properties["max_breadth"])
		if breadth["maximum"] != 500 {
			t.Errorf("%s max_breadth maximum = %v, want 500", schemaName, breadth["maximum"])
		}
	}
	if _, ok := schemaProperties(t, tavilyCrawlInputSchema)["chunks_per_source"]; !ok {
		t.Error("crawl schema missing chunks_per_source")
	}
	for _, name := range []string{"include_images", "extract_depth", "format", "include_favicon"} {
		if _, ok := schemaProperties(t, tavilyMapInputSchema)[name]; ok {
			t.Errorf("map schema contains unsupported %q", name)
		}
	}
	if _, ok := schemaProperties(t, tavilyResearchInputSchema)["stream"]; ok {
		t.Error("official research schema must not expose stream")
	}
}

func TestTavilyResearchToolReturnsCompletedReport(t *testing.T) {
	t.Parallel()

	var createCalls, statusCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tvly-pool" {
			t.Errorf("unexpected authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/research":
			atomic.AddInt32(&createCalls, 1)
			body, _ := io.ReadAll(r.Body)
			if !bytes.Contains(body, []byte(`"input":"latest Tavily API"`)) {
				t.Errorf("unexpected research body: %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request_id":"research-1","status":"pending"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/research/research-1":
			atomic.AddInt32(&statusCalls, 1)
			_, _ = w.Write([]byte(`{"request_id":"research-1","status":"completed","content":"completed report"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	session := connectResearchMCPTest(t, upstream.URL)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	wantNames := map[string]bool{
		"tavily_search": true, "tavily_extract": true, "tavily_crawl": true,
		"tavily_map": true, "tavily_research": true, "tavily_usage": true,
	}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if !wantNames[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
	}
	if len(names) != len(wantNames) {
		t.Fatalf("tool names = %v, want %v", names, wantNames)
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      "tavily_research",
		Arguments: map[string]any{"input": "latest Tavily API"},
	})
	if err != nil || result.IsError {
		t.Fatalf("research: result=%+v err=%v", result, err)
	}
	if got := mustText(t, result); got != "completed report" {
		t.Fatalf("research result = %q, want completed report", got)
	}
	if createCalls != 1 || statusCalls != 1 {
		t.Fatalf("unexpected upstream calls: create=%d status=%d", createCalls, statusCalls)
	}
}

func TestTavilyResearchToolConsumesRequiredStream(t *testing.T) {
	t.Parallel()

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":{"error_code":"research_stream_required"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"streamed \"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"report\"}}]}\n\n")
		_, _ = io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	t.Cleanup(upstream.Close)

	session := connectResearchMCPTest(t, upstream.URL)
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      "tavily_research",
		Arguments: map[string]any{"input": "stream this", "model": "mini"},
	})
	if err != nil || result.IsError {
		t.Fatalf("research: result=%+v err=%v", result, err)
	}
	if got := mustText(t, result); got != "streamed report" {
		t.Fatalf("research result = %q, want streamed report", got)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

func TestTavilyUsage_ReturnsAggregatedStatsWithoutUpstreamCall(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	keys := services.NewKeyService(database)
	keyA, err := keys.Create(ctx, "tvly-pool-a", "a", 1000)
	if err != nil {
		t.Fatalf("create key a: %v", err)
	}
	keyB, err := keys.Create(ctx, "tvly-pool-b", "b", 500)
	if err != nil {
		t.Fatalf("create key b: %v", err)
	}
	if err := keys.SetUsage(ctx, keyA.ID, 250, nil); err != nil {
		t.Fatalf("set usage for key a: %v", err)
	}
	if err := keys.SetUsage(ctx, keyB.ID, 100, nil); err != nil {
		t.Fatalf("set usage for key b: %v", err)
	}

	stats := services.NewStatsService(database)
	expected, err := stats.Get(ctx)
	if err != nil {
		t.Fatalf("stats get: %v", err)
	}

	var upstreamCalls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":{"usage":0,"limit":0}}`))
	}))
	t.Cleanup(upstream.Close)

	proxy := services.NewTavilyProxy(upstream.URL, 3*time.Second, keys, nil, nil, logger)
	handler := NewHandler(Dependencies{
		MasterKey:  master,
		Proxy:      proxy,
		Stats:      stats,
		Stateless:  true,
		SessionTTL: time.Minute,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	session := connectMCPClient(t, server.URL, master.Get())
	defer session.Close()

	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: "tavily_usage"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error result: %+v", res)
	}

	payload := mustStructuredMap(t, res.StructuredContent)
	key := mustStructuredMap(t, payload["key"])
	usage := asInt64(t, key["usage"])
	limit := asInt64(t, key["limit"])

	if usage != expected.TotalUsed {
		t.Fatalf("unexpected usage: got %d want %d", usage, expected.TotalUsed)
	}
	if limit != expected.TotalQuota {
		t.Fatalf("unexpected limit: got %d want %d", limit, expected.TotalQuota)
	}
	if limit-usage != expected.TotalRemaining {
		t.Fatalf("unexpected remaining: got %d want %d", limit-usage, expected.TotalRemaining)
	}

	textPayload := mustStructuredMap(t, mustTextJSON(t, res))
	textKey := mustStructuredMap(t, textPayload["key"])
	if asInt64(t, textKey["usage"]) != usage || asInt64(t, textKey["limit"]) != limit {
		t.Fatalf("content text does not match structured content")
	}

	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		t.Fatalf("unexpected upstream calls for tavily_usage: %d", got)
	}
}

func TestTavilyUsage_ReturnsErrorWhenStatsUnavailable(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	handler := NewHandler(Dependencies{
		MasterKey:  master,
		Stateless:  true,
		SessionTTL: time.Minute,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	session := connectMCPClient(t, server.URL, master.Get())
	defer session.Close()

	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: "tavily_usage"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result when stats unavailable")
	}

	payload := mustStructuredMap(t, res.StructuredContent)
	if payload["error"] != "stats service unavailable" {
		t.Fatalf("unexpected error payload: %+v", payload)
	}
}

func TestMCPHandler_RejectsUnauthorizedRequest(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	handler := NewHandler(Dependencies{
		MasterKey:  master,
		Stateless:  true,
		SessionTTL: time.Minute,
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: got %d want %d", w.Code, http.StatusUnauthorized)
	}
}

type authRoundTripper struct {
	base  http.RoundTripper
	token string
}

func connectResearchMCPTest(t *testing.T, upstreamURL string) *mcp.ClientSession {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}
	keys := services.NewKeyService(database)
	if _, err := keys.Create(ctx, "tvly-pool", "", 1); err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := NewHandler(Dependencies{
		MasterKey:  master,
		Proxy:      services.NewTavilyProxy(upstreamURL, 3*time.Second, keys, nil, nil, logger),
		Stateless:  true,
		SessionTTL: time.Minute,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	session := connectMCPClient(t, server.URL, master.Get())
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func (t *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := t.base
	if transport == nil {
		transport = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return transport.RoundTrip(clone)
}

func connectMCPClient(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()

	connectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-client",
		Version: "0.0.1",
	}, nil)
	session, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &authRoundTripper{token: token}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect mcp client: %v", err)
	}
	return session
}

func mustTextJSON(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()

	text := mustText(t, result)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("text content is not json: %v (text=%q)", err, text)
	}
	return out
}

func mustText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("missing content")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type: %T", result.Content[0])
	}
	return text.Text
}

func mustStructuredMap(t *testing.T, v any) map[string]any {
	t.Helper()
	out, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("unexpected structured type: %T", v)
	}
	return out
}

func schemaProperties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	return mustStructuredMap(t, schema["properties"])
}

func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch x := v.(type) {
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			t.Fatalf("invalid json number %q: %v", x, err)
		}
		return n
	default:
		t.Fatalf("unexpected numeric type: %T", v)
		return 0
	}
}
