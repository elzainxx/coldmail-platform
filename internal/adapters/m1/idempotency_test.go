package m1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/modclient"
)

// idemModule is a stand-in for module 1's idempotent bulk create (api-contract.md, "Idempotent bulk create"), small enough to read in one go and faithful to the rules the adapter
// depends on: the key is scoped to the authenticated caller (the bearer token), a repeat of the same request returns the original job with the same body and the response header
// `Idempotent-Replayed: true`, the same key with another request is a 409 `idempotency_key_reused` that creates nothing, and a request that carries no key makes a job of its own every
// time. It can also misbehave the way a network does: answer the first attempts with a 503 before creating anything, or create the job and then hang up without answering (the case the
// idempotency key exists for). It counts every attempt and remembers the key of each.
type idemModule struct {
	srv *httptest.Server

	mu       sync.Mutex
	jobs     []idemJob
	byKey    map[string]int // caller + "|" + key -> index into jobs
	attempts int
	keys     []string // the Idempotency-Key header of every attempt, in order ("" for none)
	bodies   []string

	// beforeCreate: the statuses the first attempts are answered with, creating nothing (a 503 of a module that is not ready, say).
	beforeCreate []int
	// dropAfterCreate: how many of the attempts that CREATE a job are answered by hanging up instead, after the job exists.
	dropAfterCreate int
}

type idemJob struct {
	id          string
	fingerprint string
	answer      string
}

func newIdemModule(t testing.TB) *idemModule {
	t.Helper()
	m := &idemModule{byKey: map[string]int{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// countsOf is what module 1's 202 says about a request body: total rows, distinct addresses enqueued, repeats as duplicates (the test addresses are well formed, so none is malformed).
func countsOf(items []Item) SubmitCounts {
	seen := map[string]bool{}
	c := SubmitCounts{Total: len(items)}
	for _, it := range items {
		a := strings.ToLower(strings.TrimSpace(it.Address))
		if seen[a] {
			c.Duplicate++
		} else {
			seen[a] = true
			c.Enqueued++
		}
	}
	return c
}

func (m *idemModule) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	key := r.Header.Get("Idempotency-Key")
	caller := r.Header.Get("Authorization")
	m.mu.Lock()
	m.attempts++
	m.keys = append(m.keys, key)
	m.bodies = append(m.bodies, string(b))
	attempt := m.attempts
	reply := func(status int, answer string, replayed bool) {
		w.Header().Set("Content-Type", "application/json")
		if replayed {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}
	defer m.mu.Unlock()
	if attempt <= len(m.beforeCreate) {
		reply(m.beforeCreate[attempt-1], fixtureErr("unavailable"), false)
		return
	}
	sum := sha256.Sum256(b)
	fp := hex.EncodeToString(sum[:])
	if key != "" {
		if i, ok := m.byKey[caller+"|"+key]; ok {
			if m.jobs[i].fingerprint != fp {
				reply(http.StatusConflict, `{"error":{"code":"idempotency_key_reused","message":"This Idempotency-Key was already used for a different request.","details":[{"field":"Idempotency-Key","issue":"conflict"}],"request_id":"00000000-0000-7000-8000-0000000000c1"}}`, false)
				return
			}
			reply(http.StatusAccepted, m.jobs[i].answer, true)
			return
		}
	}
	var req submitBody
	if err := json.Unmarshal(b, &req); err != nil {
		reply(http.StatusBadRequest, fixtureErr("validation_failed"), false)
		return
	}
	c := countsOf(req.Items)
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", len(m.jobs)+1)
	answer := fmt.Sprintf(`{"job_id":%q,"status":"accepted","counts":{"total":%d,"enqueued":%d,"duplicate":%d,"malformed":%d},"malformed_rows":[],"malformed_rows_truncated":false,"processing_paused":false}`,
		id, c.Total, c.Enqueued, c.Duplicate, c.Malformed)
	m.jobs = append(m.jobs, idemJob{id: id, fingerprint: fp, answer: answer})
	if key != "" {
		m.byKey[caller+"|"+key] = len(m.jobs) - 1
	}
	if m.dropAfterCreate > 0 {
		m.dropAfterCreate--
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close() // the job exists and the caller was never told
		}
		return
	}
	reply(http.StatusAccepted, answer, false)
}

func (m *idemModule) jobCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.jobs)
}

func (m *idemModule) attemptKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.keys...)
}

func (m *idemModule) attemptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts
}

// adapter is an adapter for this stand-in with the given dials and its own caller key.
func (m *idemModule) adapter(t testing.TB, opts modclient.Options) *Adapter {
	t.Helper()
	return m.adapterAs(t, newToken(t), opts)
}

func (m *idemModule) adapterAs(t testing.TB, caller string, opts modclient.Options) *Adapter {
	t.Helper()
	if opts.Sleep == nil {
		opts.Sleep = noWait
	}
	a, err := New(m.srv.URL, config.Secret(caller), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// otherPlan is a plan whose request differs from threeRowPlan's (one more distinct address).
func otherPlan() Plan {
	return Prepare([]Input{{"row-0", "jane@acmecorp.com"}, {"row-1", "bob@acmecorp.com"}, {"row-2", "Jane@AcmeCorp.com"}, {"row-3", "cyd@acmecorp.com"}})
}
