package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type healthBody struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

type readyChecks struct {
	DB bool `json:"db"`
}

// readyBody is the readiness envelope. `checks` is the uniform part (the same shape on every module, the
// dependency names live inside it); the top-level `db` repeats the one check so a consumer that reads a
// top-level field keeps working. Both are derived from the same variable, so they cannot disagree.
type readyBody struct {
	Status string      `json:"status"`
	Checks readyChecks `json:"checks"`
	DB     bool        `json:"db"`
	At     time.Time   `json:"at"`
}

// healthz is liveness: the process is up and serving. It runs no check, calls no dependency, and so
// answers 200 for as long as the process can answer anything, whether or not the database is reachable.
func (s *service) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthBody{Status: "ok", At: s.now().UTC()})
}

// readyz is readiness: 200 only when the database ping succeeds within ReadyTimeout AND the service is not
// draining. The two conditions are evaluated independently, so a draining instance with a healthy
// database still says db:true (truthfully) and not_ready (because it is leaving).
func (s *service) readyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dbOK, reason := s.pingDB(ctx)
	if !dbOK {
		// A FIXED message plus a coarse reason. The driver's error text is deliberately not logged here:
		// this handler cannot know which secrets that text embeds (a DSN, a host), and the response never
		// carries it either. The reason distinguishes the cases an operator acts on differently.
		s.log.WarnContext(ctx, "readiness check failed: database ping", "reason", reason)
	}
	ready := dbOK && !s.draining()
	status, code := "not_ready", http.StatusServiceUnavailable
	if ready {
		status, code = "ready", http.StatusOK
	}
	writeJSON(w, code, readyBody{Status: status, Checks: readyChecks{DB: dbOK}, DB: dbOK, At: s.now().UTC()})
}

type pingResult struct {
	err      error
	panicked bool
}

// pingDB pings with a deadline of ReadyTimeout and reports (ok, reason). The ping runs in its own
// goroutine and the handler waits on the deadline rather than on the ping, so a Pinger that ignores its
// context still cannot hold a probe past the timeout; the buffered channel lets that goroutine finish and
// exit whenever the Pinger finally returns. A panicking Pinger is contained in that goroutine (a request
// recover cannot reach it) and reported as a failed ping.
func (s *service) pingDB(ctx context.Context) (ok bool, reason string) {
	pctx, cancel := context.WithTimeout(ctx, s.readyTimeout)
	defer cancel()

	done := make(chan pingResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- pingResult{panicked: true}
			}
		}()
		done <- pingResult{err: s.pinger.Ping(pctx)}
	}()

	select {
	case res := <-done:
		switch {
		case res.panicked:
			return false, "panic"
		case res.err == nil:
			return true, ""
		case errors.Is(res.err, context.DeadlineExceeded):
			return false, "timeout"
		default:
			return false, "error"
		}
	case <-pctx.Done():
		if errors.Is(pctx.Err(), context.DeadlineExceeded) {
			return false, "timeout"
		}
		return false, "canceled"
	}
}
