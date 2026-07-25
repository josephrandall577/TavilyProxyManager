package services

import "testing"

func TestResetAllUsage(t *testing.T) {
	t.Parallel()

	ctx, keys, _ := newTavilyProxyTestDeps(t, "http://127.0.0.1")
	for _, item := range []struct {
		key   string
		usage int
	}{{"tvly-reset-a", 10}, {"tvly-reset-b", 20}} {
		key, err := keys.Create(ctx, item.key, "", 100)
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if err := keys.SetUsage(ctx, key.ID, item.usage, nil); err != nil {
			t.Fatalf("set usage: %v", err)
		}
	}

	if err := keys.ResetAllUsage(ctx); err != nil {
		t.Fatalf("reset usage: %v", err)
	}
	got, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	for _, key := range got {
		if key.UsedQuota != 0 {
			t.Fatalf("key %d used quota = %d, want 0", key.ID, key.UsedQuota)
		}
	}
}
