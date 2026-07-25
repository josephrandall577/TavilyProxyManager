package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"tavily-proxy/server/internal/db"
)

func TestBuildCacheKeyIncludesAllRequestFields(t *testing.T) {
	cache := &CacheService{}

	first, query := cache.BuildCacheKey([]byte(`{"query":"go","include_answer":false,"max_results":5}`))
	second, _ := cache.BuildCacheKey([]byte(`{"max_results":5,"include_answer":false,"query":"go"}`))
	different, _ := cache.BuildCacheKey([]byte(`{"query":"go","include_answer":true,"max_results":5}`))

	if query != "go" {
		t.Fatalf("query = %q, want go", query)
	}
	if first != second {
		t.Fatal("equivalent JSON objects produced different cache keys")
	}
	if first == different {
		t.Fatal("response-changing field did not affect cache key")
	}
}

func TestCacheStoreUpsertsExistingEntry(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	cache := NewCacheService(database)
	ctx := context.Background()

	if err := cache.Store(ctx, "key", "first", `{}`, `{"value":1}`, 200, time.Hour); err != nil {
		t.Fatalf("first store: %v", err)
	}
	if err := cache.Store(ctx, "key", "second", `{}`, `{"value":2}`, 201, time.Hour); err != nil {
		t.Fatalf("second store: %v", err)
	}

	entry, found, err := cache.Lookup(ctx, "key")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !found || entry.Query != "second" || entry.ResponseBody != `{"value":2}` || entry.StatusCode != 201 {
		t.Fatalf("unexpected cached entry: %+v", entry)
	}
}
