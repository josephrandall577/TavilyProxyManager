package services

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"tavily-proxy/server/internal/db"
	"tavily-proxy/server/internal/models"
)

func TestTavilyProxy_DefaultLoggingEnabled_CreatesLogs(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

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

	settings := NewSettingsService(database)
	keys := NewKeyService(database)
	logs := NewLogService(database)

	ctx := context.Background()
	if _, err := keys.Create(ctx, "tvly-test", "test", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}

	proxy := NewTavilyProxy(upstream.URL, 5*time.Second, keys, logs, nil, logger).
		WithSettings(settings)

	if _, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/usage", ClientIP: "127.0.0.1"}); err != nil {
		t.Fatalf("proxy request: %v", err)
	}

	var count int64
	if err := database.WithContext(ctx).Model(&models.RequestLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 1 {
		t.Fatalf("unexpected logs count: got %d want %d", count, 1)
	}
}

func TestTavilyProxy_LoggingDisabled_SkipsLogs(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

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

	settings := NewSettingsService(database)
	keys := NewKeyService(database)
	logs := NewLogService(database)

	ctx := context.Background()
	if _, err := keys.Create(ctx, "tvly-test", "test", 1000); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := settings.SetBool(ctx, SettingRequestLoggingEnabled, false); err != nil {
		t.Fatalf("disable logging: %v", err)
	}

	proxy := NewTavilyProxy(upstream.URL, 5*time.Second, keys, logs, nil, logger).
		WithSettings(settings)

	if _, err := proxy.Do(ctx, ProxyRequest{Method: http.MethodGet, Path: "/usage", ClientIP: "127.0.0.1"}); err != nil {
		t.Fatalf("proxy request: %v", err)
	}

	var count int64
	if err := database.WithContext(ctx).Model(&models.RequestLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 0 {
		t.Fatalf("unexpected logs count: got %d want %d", count, 0)
	}
}
