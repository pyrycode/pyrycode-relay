package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthz_ResponseShape(t *testing.T) {
	t.Parallel()

	startedAt := time.Now().Add(-30 * time.Second)
	h := NewHealthzHandler(NewRegistry(), "test-version", startedAt)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type: got %q, want %q", got, want)
	}
	if got, want := rec.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control: got %q, want %q", got, want)
	}

	body := rec.Body.Bytes()
	if len(body) >= 200 {
		t.Errorf("body length: got %d, want < 200", len(body))
	}

	var resp healthzResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v (body=%q)", err, body)
	}

	if resp.Status != "ok" {
		t.Errorf("status: got %q, want %q", resp.Status, "ok")
	}
	if resp.Version != "test-version" {
		t.Errorf("version: got %q, want %q", resp.Version, "test-version")
	}
	if resp.ConnectedBinaries != 0 {
		t.Errorf("connected_binaries: got %d, want 0", resp.ConnectedBinaries)
	}
	if resp.ConnectedPhones != 0 {
		t.Errorf("connected_phones: got %d, want 0", resp.ConnectedPhones)
	}
	if resp.UptimeSeconds < 30 {
		t.Errorf("uptime_seconds: got %d, want >= 30", resp.UptimeSeconds)
	}

	// Confirm all five JSON keys are present on the wire (well-typed
	// presence — distinguishes "field encoded" from "field omitted via
	// omitempty"). Decode into a generic map so a renamed Go field with the
	// same JSON tag would still pass the typed unmarshal but fail here.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("json.Unmarshal (map): %v", err)
	}
	for _, key := range []string{"status", "version", "connected_binaries", "connected_phones", "uptime_seconds"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q in body %q", key, body)
		}
	}
}

func TestHealthz_TracksRegistryState(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()

	// 2 binaries across 2 server-ids.
	if err := reg.ClaimServer("s1", &fakeConn{id: "b-1"}); err != nil {
		t.Fatalf("ClaimServer s1: %v", err)
	}
	if err := reg.ClaimServer("s2", &fakeConn{id: "b-2"}); err != nil {
		t.Fatalf("ClaimServer s2: %v", err)
	}

	// 5 phones across the 2 server-ids (3 + 2).
	for i, sid := range []string{"s1", "s1", "s1", "s2", "s2"} {
		if err := reg.RegisterPhone(sid, &fakeConn{id: "p-" + string(rune('a'+i))}); err != nil {
			t.Fatalf("RegisterPhone %s: %v", sid, err)
		}
	}

	h := NewHealthzHandler(reg, "v", time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var resp healthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if resp.ConnectedBinaries != 2 {
		t.Errorf("connected_binaries: got %d, want 2", resp.ConnectedBinaries)
	}
	if resp.ConnectedPhones != 5 {
		t.Errorf("connected_phones: got %d, want 5", resp.ConnectedPhones)
	}
}
