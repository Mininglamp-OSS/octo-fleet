package runtime

import (
	"sync"
	"time"
)

// ccOctoSecretTTL bounds how long a one-click cc-octo install secret (LLM
// gateway url + key) lives in fleet memory. Matches pluginUpgradeTimeoutSec
// (600s) so the daemon can re-fetch across an install retry within the task's
// own lifetime, then it expires. The secret is NEVER persisted (no DB column,
// never in upgrade-task metadata / event_log / SSE) — fleet only relays it.
const ccOctoSecretTTL = 10 * time.Minute

type ccOctoSecret struct {
	GatewayURL string
	APIKey     string
	Model      string
}

type ccOctoSecretEntry struct {
	secret   ccOctoSecret
	expireAt time.Time
}

// ccOctoSecretStore is an in-memory, TTL-bounded relay for cc-octo install
// secrets, keyed by upgrade task_id. Re-entrant: get does not delete (an install
// retry needs the secret again); entries leave only by evict (terminal report)
// or TTL expiry. Concurrency-safe.
type ccOctoSecretStore struct {
	mu  sync.Mutex
	m   map[string]ccOctoSecretEntry
	now func() time.Time // clock seam for tests
}

func newCcOctoSecretStore() *ccOctoSecretStore {
	return &ccOctoSecretStore{m: make(map[string]ccOctoSecretEntry), now: time.Now}
}

func (s *ccOctoSecretStore) put(taskID string, secret ccOctoSecret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[taskID] = ccOctoSecretEntry{secret: secret, expireAt: s.now().Add(ccOctoSecretTTL)}
}

func (s *ccOctoSecretStore) get(taskID string) (ccOctoSecret, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[taskID]
	if !ok {
		return ccOctoSecret{}, false
	}
	if s.now().After(e.expireAt) {
		delete(s.m, taskID)
		return ccOctoSecret{}, false
	}
	return e.secret, true
}

func (s *ccOctoSecretStore) evict(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, taskID)
}

// sweepLocked drops expired entries; called on put so the map can't grow
// unbounded from never-fetched (e.g. failed-dispatch) tasks. Caller holds mu.
func (s *ccOctoSecretStore) sweepLocked() {
	now := s.now()
	for k, e := range s.m {
		if now.After(e.expireAt) {
			delete(s.m, k)
		}
	}
}

// startSweeper runs a background ticker that drops expired entries even when no
// further put/get traffic touches the store — so a successfully-installed task's
// key string does not linger in memory past its TTL (it's never persisted, but
// we also don't want it sitting in RAM until process exit). Started by the
// production Runtime constructor, NOT by newCcOctoSecretStore, so unit tests
// that reassign the `now` clock seam don't race a live goroutine.
func (s *ccOctoSecretStore) startSweeper() {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			s.mu.Lock()
			s.sweepLocked()
			s.mu.Unlock()
		}
	}()
}
