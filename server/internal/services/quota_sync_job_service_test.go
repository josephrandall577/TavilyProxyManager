package services

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"tavily-proxy/server/internal/db"
)

func TestQuotaSyncJobService_RunsInBackgroundAndReportsProgress(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":{"usage":1,"limit":1000}}`))
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

	keys := NewKeyService(database)
	proxy := NewTavilyProxy(upstream.URL, 5*time.Second, keys, nil, nil, logger)
	sync := NewQuotaSyncService(keys, proxy, logger)
	jobs := NewQuotaSyncJobService(keys, sync)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := keys.Create(ctx, fmt.Sprintf("tvly-test-%d", i), "test", 1000); err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
	}

	started, alreadyRunning, err := jobs.Start(0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if alreadyRunning {
		t.Fatalf("unexpected alreadyRunning=true")
	}
	if started.Status != "running" {
		t.Fatalf("unexpected status: got %q want %q", started.Status, "running")
	}
	if started.Total != 3 {
		t.Fatalf("unexpected total: got %d want %d", started.Total, 3)
	}

	started2, alreadyRunning2, err := jobs.Start(0)
	if err != nil {
		t.Fatalf("start again: %v", err)
	}
	if !alreadyRunning2 {
		t.Fatalf("expected alreadyRunning=true")
	}
	if started2.ID != started.ID {
		t.Fatalf("expected same job id: got %q want %q", started2.ID, started.ID)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := jobs.Get()
		if got.Status != "running" {
			if got.Status != "completed" {
				t.Fatalf("unexpected final status: %q", got.Status)
			}
			if got.Completed != got.Total {
				t.Fatalf("incomplete job: completed=%d total=%d", got.Completed, got.Total)
			}
			if got.Succeeded != got.Total {
				t.Fatalf("unexpected succeeded: got %d want %d", got.Succeeded, got.Total)
			}
			if got.Failed != 0 {
				t.Fatalf("unexpected failed: got %d want %d", got.Failed, 0)
			}
			if got.StartedAt == nil {
				t.Fatalf("missing started_at")
			}
			if got.EndedAt == nil {
				t.Fatalf("missing ended_at")
			}
			for _, item := range got.Items {
				if item.Status != "ok" {
					t.Fatalf("unexpected item status: %q", item.Status)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for job to complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
