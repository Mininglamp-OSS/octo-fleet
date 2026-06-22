package runtime

import (
	"testing"
	"time"
)

func TestCcOctoSecretStore_PutGetEvict(t *testing.T) {
	s := newCcOctoSecretStore()
	s.put("task_1", ccOctoSecret{GatewayURL: "https://gw", APIKey: "sk-1"})

	got, ok := s.get("task_1")
	if !ok || got.GatewayURL != "https://gw" || got.APIKey != "sk-1" {
		t.Fatalf("get after put = %+v, %v; want the stored secret", got, ok)
	}

	// re-entrant: a second get still returns it (not fetch-and-burn).
	if _, ok := s.get("task_1"); !ok {
		t.Fatal("second get should still hit (re-entrant within TTL)")
	}

	s.evict("task_1")
	if _, ok := s.get("task_1"); ok {
		t.Fatal("get after evict should miss")
	}
}

func TestCcOctoSecretStore_Expired(t *testing.T) {
	s := newCcOctoSecretStore()
	// inject an already-expired entry via the test-only clock seam.
	s.now = func() time.Time { return time.Unix(1000, 0) }
	s.put("task_x", ccOctoSecret{GatewayURL: "https://gw", APIKey: "sk"})
	s.now = func() time.Time { return time.Unix(1000, 0).Add(ccOctoSecretTTL + time.Second) }
	if _, ok := s.get("task_x"); ok {
		t.Fatal("expired entry should miss")
	}
}

func TestCcOctoSecretStore_Miss(t *testing.T) {
	s := newCcOctoSecretStore()
	if _, ok := s.get("nope"); ok {
		t.Fatal("missing key should not hit")
	}
}
