package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"tavily-proxy/server/internal/db"
	"tavily-proxy/server/internal/services"
)

func TestProxyStreamsEventSourceResponses(t *testing.T) {
	t.Parallel()

	firstEventSent := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		close(firstEventSent)
		<-releaseUpstream
		_, _ = w.Write([]byte("data: done\n\n"))
	}))
	t.Cleanup(upstream.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}
	keys := services.NewKeyService(database)
	if _, err := keys.Create(ctx, "tvly-stream", "", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}
	router := NewRouter(Dependencies{
		MasterKeyService: master,
		TavilyProxy:      services.NewTavilyProxy(upstream.URL, 5*time.Second, keys, nil, nil, logger),
	})
	proxyServer := httptest.NewServer(router)
	t.Cleanup(proxyServer.Close)

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/research", strings.NewReader(`{"input":"test","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+master.Get())
	req.Header.Set("Content-Type", "application/json")
	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		resultCh <- result{resp: resp, err: err}
	}()

	select {
	case <-firstEventSent:
	case <-time.After(time.Second):
		close(releaseUpstream)
		t.Fatal("upstream did not start event stream")
	}
	select {
	case got := <-resultCh:
		if got.err != nil {
			close(releaseUpstream)
			t.Fatalf("proxy request: %v", got.err)
		}
		defer got.resp.Body.Close()
		line, err := bufio.NewReader(got.resp.Body).ReadString('\n')
		close(releaseUpstream)
		if err != nil {
			t.Fatalf("read first event: %v", err)
		}
		if line != "data: first\n" {
			t.Fatalf("unexpected first event: %q", line)
		}
	case <-time.After(time.Second):
		close(releaseUpstream)
		t.Fatal("proxy buffered event stream instead of forwarding headers immediately")
	}
}

func init() {
	gin.SetMode(gin.TestMode)
}

func TestProxy_LegacyBodyAPIKey_TavilyKey_IsUnauthorized(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const directKey = "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"
	const poolKey = "tvly-pool-1234567890abcdef"
	var upstreamCalls int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

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
	if err := master.LoadOrCreate(context.Background()); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	ctx := context.Background()
	keys := services.NewKeyService(database)
	if _, err := keys.Create(ctx, poolKey, "pool", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}
	proxy := services.NewTavilyProxy(upstream.URL, 5*time.Second, keys, nil, nil, logger)

	router := NewRouter(Dependencies{
		MasterKeyService: master,
		TavilyProxy:      proxy,
	})

	payload := []byte(`{"query":"today is 2026-01-26 \r\n test query","max_results":5,"api_key":"` + directKey + `"}`)

	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: got %d want %d (body=%q)", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		t.Fatalf("upstream should not be called, got %d calls", got)
	}
}

func TestProxy_LegacyBodyAPIKey_MasterKey_StripsFieldAndUsesPoolKey(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const poolKey = "tvly-pool-1234567890abcdef"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+poolKey {
			t.Fatalf("unexpected Authorization header: got %q want %q", got, "Bearer "+poolKey)
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("upstream body json: %v (body=%q)", err, string(body))
		}
		if _, ok := m["api_key"]; ok {
			t.Fatalf("api_key should be stripped from upstream body (body=%q)", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"request_id":"test","results":[]}`))
	}))
	t.Cleanup(upstream.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	master := services.NewMasterKeyService(database, logger)
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	keys := services.NewKeyService(database)
	if _, err := keys.Create(ctx, poolKey, "pool", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxy := services.NewTavilyProxy(upstream.URL, 5*time.Second, keys, nil, nil, logger)

	router := NewRouter(Dependencies{
		MasterKeyService: master,
		TavilyProxy:      proxy,
	})

	payload := []byte(`{"query":"hello","max_results":5,"api_key":"` + master.Get() + `"}`)

	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d (body=%q)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestProxy_LegacyQueryAPIKey_MasterKey_StripsParamAndUsesPoolKey(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const poolKey = "tvly-pool-1234567890abcdef"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+poolKey {
			t.Fatalf("unexpected Authorization header: got %q want %q", got, "Bearer "+poolKey)
		}
		if got := r.URL.Query().Get("api_key"); got != "" {
			t.Fatalf("api_key should be stripped from upstream query, got %q", got)
		}
		if got := r.URL.Query().Get("foo"); got != "bar" {
			t.Fatalf("unexpected foo query: got %q want %q", got, "bar")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":{"usage":0,"limit":1000}}`))
	}))
	t.Cleanup(upstream.Close)

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
	ctx := context.Background()
	if err := master.LoadOrCreate(ctx); err != nil {
		t.Fatalf("master key init: %v", err)
	}

	keys := services.NewKeyService(database)
	if _, err := keys.Create(ctx, poolKey, "pool", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}
	proxy := services.NewTavilyProxy(upstream.URL, 5*time.Second, keys, nil, nil, logger)

	router := NewRouter(Dependencies{
		MasterKeyService: master,
		TavilyProxy:      proxy,
	})

	req := httptest.NewRequest(http.MethodGet, "/usage?api_key="+master.Get()+"&foo=bar", nil)
	req.Header.Set("Accept", "*/*")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d (body=%q)", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestProxyRejectsOversizedBodyBeforeAuthentication(t *testing.T) {
	t.Parallel()

	router := NewRouter(Dependencies{})
	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader(make([]byte, maxProxyRequestBody+1)))
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestLegacyQueryCredentialIsRedactedBeforeLogging(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := gin.DefaultWriter
	gin.DefaultWriter = &logs
	t.Cleanup(func() { gin.DefaultWriter = previousWriter })

	router := gin.New()
	router.Use(sanitizeLegacyAPIKeyQuery(), gin.Logger())
	router.GET("/usage", func(c *gin.Context) {
		if got := c.GetString(legacyAPIKeyContextKey); got != "secret" {
			t.Fatalf("credential = %q, want secret", got)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/usage?api_key=secret&foo=bar", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if strings.Contains(logs.String(), "secret") {
		t.Fatalf("access log leaked credential: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "foo=bar") {
		t.Fatalf("sanitized query missing from access log: %s", logs.String())
	}
}
