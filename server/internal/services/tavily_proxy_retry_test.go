package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tavily-proxy/server/internal/db"
)

func newTavilyProxyTestDeps(t *testing.T, upstreamURL string) (context.Context, *KeyService, *TavilyProxy) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	keys := NewKeyService(database)
	proxy := NewTavilyProxy(upstreamURL, 5*time.Second, keys, nil, nil, logger)
	return context.Background(), keys, proxy
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.TrimPrefix(auth, prefix)
}

func TestTavilyProxy_TooManyRequestsFallsBackWithoutExhaustingKey(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch bearerToken(r) {
		case "tvly-429":
			firstCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit","request_id":"req-429"}`))
		case "tvly-ok":
			secondCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"request_id":"req-ok"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)

	first, err := keys.Create(ctx, "tvly-429", "first", 2000)
	if err != nil {
		t.Fatalf("create first key: %v", err)
	}
	_, err = keys.Create(ctx, "tvly-ok", "second", 1000)
	if err != nil {
		t.Fatalf("create second key: %v", err)
	}

	resp, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/search", ClientIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: got %d want %d", resp.StatusCode, http.StatusOK)
	}

	gotFirst, err := keys.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first key: %v", err)
	}
	if gotFirst.UsedQuota == gotFirst.TotalQuota {
		t.Fatalf("first key marked exhausted unexpectedly: used=%d total=%d", gotFirst.UsedQuota, gotFirst.TotalQuota)
	}
	if !gotFirst.IsActive || gotFirst.IsInvalid {
		t.Fatalf("first key state changed unexpectedly: active=%v invalid=%v", gotFirst.IsActive, gotFirst.IsInvalid)
	}

	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("unexpected upstream calls: first=%d second=%d", firstCalls, secondCalls)
	}
}

func TestTavilyProxy_AllTooManyRequestsReturns429AndKeepsKeysAvailable(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		switch bearerToken(r) {
		case "tvly-429-a":
			_, _ = w.Write([]byte(`{"error":"rate_limit","request_id":"req-429-a"}`))
		case "tvly-429-b":
			_, _ = w.Write([]byte(`{"error":"rate_limit","request_id":"req-429-b"}`))
		default:
			_, _ = w.Write([]byte(`{"error":"rate_limit","request_id":"req-429-unknown"}`))
		}
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)

	first, err := keys.Create(ctx, "tvly-429-a", "first", 2000)
	if err != nil {
		t.Fatalf("create first key: %v", err)
	}
	second, err := keys.Create(ctx, "tvly-429-b", "second", 1000)
	if err != nil {
		t.Fatalf("create second key: %v", err)
	}

	resp, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/search", ClientIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: got %d want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if !strings.Contains(string(resp.Body), "req-429-b") {
		t.Fatalf("unexpected response body: %s", string(resp.Body))
	}

	gotFirst, err := keys.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first key: %v", err)
	}
	gotSecond, err := keys.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second key: %v", err)
	}
	if gotFirst.UsedQuota == gotFirst.TotalQuota || gotSecond.UsedQuota == gotSecond.TotalQuota {
		t.Fatalf("429 should not exhaust keys: first=%d/%d second=%d/%d", gotFirst.UsedQuota, gotFirst.TotalQuota, gotSecond.UsedQuota, gotSecond.TotalQuota)
	}
	if !gotFirst.IsActive || gotFirst.IsInvalid || !gotSecond.IsActive || gotSecond.IsInvalid {
		t.Fatalf("key states changed unexpectedly: first(active=%v invalid=%v) second(active=%v invalid=%v)", gotFirst.IsActive, gotFirst.IsInvalid, gotSecond.IsActive, gotSecond.IsInvalid)
	}

	candidates, err := keys.Candidates(ctx)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("unexpected candidate count: got %d want %d", len(candidates), 2)
	}
}

func TestTavilyProxy_TransportFailureReturnsUpstreamErrorWithinTotalBudget(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)
	proxy.client.Timeout = 50 * time.Millisecond
	for _, key := range []string{"tvly-timeout-a", "tvly-timeout-b"} {
		if _, err := keys.Create(ctx, key, "", 1000); err != nil {
			t.Fatalf("create key: %v", err)
		}
	}

	_, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/usage"})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if errors.Is(err, ErrNoAvailableKeys) {
		t.Fatalf("transport error reported as empty key pool: %v", err)
	}
}

func TestTavilyProxy_MixedFailuresPreferTooManyRequests(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch bearerToken(r) {
		case "tvly-429":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit","request_id":"req-429"}`))
		case "tvly-401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"unexpected"}`))
		}
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)

	first, err := keys.Create(ctx, "tvly-429", "first", 2000)
	if err != nil {
		t.Fatalf("create first key: %v", err)
	}
	second, err := keys.Create(ctx, "tvly-401", "second", 1000)
	if err != nil {
		t.Fatalf("create second key: %v", err)
	}

	resp, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/search", ClientIP: "127.0.0.1"})
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unexpected status: got %d want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if !strings.Contains(string(resp.Body), "req-429") {
		t.Fatalf("unexpected response body: %s", string(resp.Body))
	}

	gotFirst, err := keys.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first key: %v", err)
	}
	if gotFirst.UsedQuota == gotFirst.TotalQuota {
		t.Fatalf("first key marked exhausted unexpectedly: used=%d total=%d", gotFirst.UsedQuota, gotFirst.TotalQuota)
	}

	gotSecond, err := keys.Get(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second key: %v", err)
	}
	if gotSecond.IsActive || !gotSecond.IsInvalid {
		t.Fatalf("second key should be marked invalid: active=%v invalid=%v", gotSecond.IsActive, gotSecond.IsInvalid)
	}
}

func TestTavilyProxy_TracksReportedCreditsForAnySuccessfulStatus(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"request_id":"research-1","usage":{"credits":3}}`))
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)
	key, err := keys.Create(ctx, "tvly-research", "", 1000)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	resp, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodPost, Path: "/research", Body: []byte(`{"input":"test"}`)})
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	got, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if got.UsedQuota != 3 {
		t.Fatalf("used quota = %d, want 3", got.UsedQuota)
	}
}

func TestTavilyProxy_StreamErrorStillAccountsAcceptedRequest(t *testing.T) {
	t.Parallel()

	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: accepted\n\n"))
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)
	key, err := keys.Create(ctx, "tvly-stream", "", 1000)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := keys.Create(ctx, "tvly-unused", "", 500); err != nil {
		t.Fatalf("create fallback key: %v", err)
	}

	streamErr := errors.New("client disconnected")
	resp, err := proxy.Do(ctx, ProxyRequest{
		Method: http.MethodPost,
		Path:   "/research",
		Stream: func(ProxyStreamResponse) error { return streamErr },
	})
	if !errors.Is(err, streamErr) {
		t.Fatalf("proxy error = %v, want %v", err, streamErr)
	}
	if !resp.Streamed {
		t.Fatal("response was not marked streamed")
	}
	got, err := keys.Get(ctx, key.ID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if got.UsedQuota != 1 {
		t.Fatalf("used quota = %d, want 1", got.UsedQuota)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestTavilyProxy_ResearchCreateTransportErrorDoesNotFailOver(t *testing.T) {
	t.Parallel()

	ctx, keys, proxy := newTavilyProxyTestDeps(t, "https://example.invalid")
	for _, key := range []string{"tvly-first", "tvly-second"} {
		if _, err := keys.Create(ctx, key, "", 1000); err != nil {
			t.Fatalf("create key: %v", err)
		}
	}

	var calls int
	proxy.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("connection reset after upload")
	})

	_, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodPost, Path: "/research", Body: []byte(`{"input":"test"}`)})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestTavilyProxy_ResearchStatusRetriesNotFoundWithNextKey(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch bearerToken(r) {
		case "tvly-first":
			firstCalls++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"not found"}`))
		case "tvly-second":
			secondCalls++
			_, _ = w.Write([]byte(`{"request_id":"research-1","status":"completed"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(upstream.Close)

	ctx, keys, proxy := newTavilyProxyTestDeps(t, upstream.URL)
	if _, err := keys.Create(ctx, "tvly-first", "", 2000); err != nil {
		t.Fatalf("create first key: %v", err)
	}
	if _, err := keys.Create(ctx, "tvly-second", "", 1000); err != nil {
		t.Fatalf("create second key: %v", err)
	}

	resp, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/research/research-1"})
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("unexpected calls: first=%d second=%d", firstCalls, secondCalls)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
