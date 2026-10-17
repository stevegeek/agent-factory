package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAPIStaleTokensDontLockOutOthers(t *testing.T) {
	h := newAPIHarness(t)
	good := h.mint("example-agent", "read", "sandbox")
	old := h.mint("example-agent-2", "read", "sandbox")
	var store map[string]any
	data, _ := os.ReadFile(h.store)
	json.Unmarshal(data, &store)
	for _, r := range store["tokens"].([]any) {
		if rec := r.(map[string]any); rec["name"] == "example-agent-2" {
			rec["expires"] = "2020-01-01T00:00:00+00:00"
		}
	}
	data, _ = json.Marshal(store)
	os.WriteFile(h.store, data, 0o600)
	far := header("X-Forwarded-For", "100.64.0.66")
	for i := 0; i <= apiFailLimit; i++ {
		if w := h.call("GET", "/api/v1/me", old, nil, far); w.Code != 401 {
			t.Fatalf("an expired token, call %d: %d", i, w.Code)
		}
	}
	// on this host every caller is loopback: a revoked token retried locks out only itself
	for i := 0; i < apiFailLimit; i++ {
		h.call("GET", "/api/v1/me", "fct_revoked", nil)
	}
	if w := h.call("GET", "/api/v1/me", "fct_revoked", nil); w.Code != 429 {
		t.Errorf("the bad token itself waits: %d", w.Code)
	}
	if w := h.call("GET", "/api/v1/me", good, nil); w.Code != 200 {
		t.Errorf("another local token still gets in: %d", w.Code)
	}
}
