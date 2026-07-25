package services

import "testing"

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
