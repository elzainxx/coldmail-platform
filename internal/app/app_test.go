package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
)

// These tests drive app.Run the way the process does: environment in, a real listener out. The database
// is never mocked. Where a test needs "a database that is not there" it points the real pgx driver at a
// closed port, and where it needs "a database that hangs" it points it at a listener that accepts and
// never answers. Real-Postgres behaviour belongs to the harness that arrives in a later phase.

const canary = "CANARY-app-pw-5521"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if raw == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

func find(lines []map[string]any, msg string) map[string]any {
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	return nil
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func merge(a map[string]string, more map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// closedPortDSN returns a connection string for a port nothing listens on (the port is reserved and
// released, so it is closed at the moment of use).
func closedPortDSN(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "postgres://platform_app:" + canary + "@" + addr + "/platform?sslmode=disable"
}

// blackHole is a TCP listener that accepts connections and never answers: a database that hangs. It counts
// what it accepted, which is proof the pool dialled the address in PLATFORM_DATABASE_URL.
func blackHole(t *testing.T) (dsn string, accepted *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted = new(atomic.Int32)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return "postgres://platform_app:" + canary + "@" + ln.Addr().String() + "/platform?sslmode=disable", accepted
}

// base is the environment of a service that binds a free local port.
func base(dsn string) map[string]string {
	return map[string]string{
		config.VarDatabaseURL:        dsn,
		config.VarHTTPAddr:           "127.0.0.1:0",
		config.VarReadyzTimeoutMS:    "500",
		config.VarShutdownTimeoutSec: "5",
	}
}

type running struct {
	addr   string
	cancel context.CancelFunc
	done   <-chan error
	logs   *syncBuffer
}

// startApp runs app.Run and waits for the line that reports the address it bound.
func startApp(t *testing.T, env map[string]string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, envOf(env), logs) }()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("Run returned before it was listening: %v\nlogs:\n%s", err, logs.String())
		default:
		}
		if l := find(logs.lines(t), "http server listening"); l != nil {
			return &running{addr: l["addr"].(string), cancel: cancel, done: done, logs: logs}
		}
	}
	t.Fatalf("Run never reported its listening address\nlogs:\n%s", logs.String())
	return nil
}

func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return within 10s of the cancel\nlogs:\n%s", r.logs.String())
		return nil
	}
}

func fetch(t *testing.T, url string) (int, map[string]any, string) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, string(b)
}

// ---- configuration errors ---------------------------------------------------------------------------------

func TestRun_AMissingDatabaseURLIsAConfigErrorNamingTheVariable(t *testing.T) {
	logs := &syncBuffer{}
	err := Run(context.Background(), envOf(map[string]string{config.VarHTTPAddr: "127.0.0.1:0"}), logs)

	var ce *config.Error
	if !errors.As(err, &ce) {
		t.Fatalf("Run = %v, want a *config.Error", err)
	}
	if !strings.Contains(err.Error(), config.VarDatabaseURL) || !strings.Contains(err.Error(), "required") {
		t.Errorf("the error does not name the missing variable and why: %v", err)
	}
	if got := ExitCode(err); got != ExitConfig {
		t.Errorf("ExitCode = %d, want %d", got, ExitConfig)
	}
	if logs.String() != "" {
		t.Errorf("a process that could not read its configuration wrote logs: %s", logs.String())
	}

	// Paired control: a DIFFERENT invalid variable is named instead, and the missing one is not.
	err = Run(context.Background(), envOf(map[string]string{config.VarDatabaseURL: closedPortDSN(t), config.VarLogLevel: "loud"}), logs)
	if err == nil || !strings.Contains(err.Error(), config.VarLogLevel) || strings.Contains(err.Error(), config.VarDatabaseURL) {
		t.Errorf("an invalid %s was reported as: %v", config.VarLogLevel, err)
	}
}

func TestRun_AnUnparseableDatabaseURLNeverEchoesTheCredential(t *testing.T) {
	// A shape pgx v5.13 leaks (the spaced keyword form) and one it redacts itself.
	dsns := map[string]string{
		"spaced keyword password": "host=h password = " + canary + " dbname=db sslmode=bogusmode",
		"url userinfo":            "postgres://u:" + canary + "@h:5432/db?sslmode=bogusmode",
	}
	for name, dsn := range dsns {
		t.Run(name, func(t *testing.T) {
			logs := &syncBuffer{}
			err := Run(context.Background(), envOf(map[string]string{config.VarDatabaseURL: dsn}), logs)

			var ce *config.Error
			if !errors.As(err, &ce) {
				t.Fatalf("Run = %v, want a *config.Error naming %s", err, config.VarDatabaseURL)
			}
			if !strings.Contains(err.Error(), config.VarDatabaseURL) {
				t.Errorf("the error does not name the variable: %v", err)
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), dsn) {
				t.Errorf("the credential or the whole DSN reached the returned error: %v", err)
			}
			if strings.Contains(logs.String(), canary) {
				t.Errorf("the credential reached the log: %s", logs.String())
			}
			if ExitCode(err) != ExitConfig {
				t.Errorf("ExitCode = %d, want %d", ExitCode(err), ExitConfig)
			}
		})
	}

	// Paired control: the raw driver error for the leaky shape DOES carry the credential, so the absence
	// above is the scrub's doing and not an artefact of the shape.
	if _, raw := pgxpool.ParseConfig(dsns["spaced keyword password"]); raw == nil || !strings.Contains(raw.Error(), canary) {
		t.Fatalf("control failed: the raw pgx error does not contain the credential: %v", raw)
	}
}

func TestExitCode_MapsErrorsToStatuses(t *testing.T) {
	cfgErr := &config.Error{Problems: []config.Problem{{Name: config.VarEnv, Reason: "x"}}}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"config error", cfgErr, 2},
		{"wrapped config error", fmt.Errorf("start: %w", cfgErr), 2},
		{"any other error", errors.New("listen tcp: address already in use"), 1},
		{"a cancelled context is not a clean drain", context.Canceled, 1},
	}
	for _, tc := range cases {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
	if ExitOK == ExitFailure || ExitFailure == ExitConfig || ExitOK == ExitConfig {
		t.Fatal("the three exit codes must be distinct")
	}
}

func TestRun_AFailureToListenExitsWithFailureNotConfig(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	logs := &syncBuffer{}
	got := make(chan error, 1)
	env := merge(base(closedPortDSN(t)), map[string]string{config.VarHTTPAddr: busy.Addr().String()})
	go func() { got <- Run(context.Background(), envOf(env), logs) }()
	select {
	case err := <-got:
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("Run on a busy address returned %v, want a listen error", err)
		}
		if ExitCode(err) != ExitFailure {
			t.Errorf("ExitCode = %d, want %d (a runtime failure, not a configuration error)", ExitCode(err), ExitFailure)
		}
		if strings.Contains(err.Error(), canary) || strings.Contains(logs.String(), canary) {
			t.Error("the credential reached the error or the log")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run on a busy address did not return")
	}
}

// ---- the service, over a real listener --------------------------------------------------------------------

func TestRun_AnUnreachableDatabaseLeavesTheProcessAliveButNotReady(t *testing.T) {
	r := startApp(t, base(closedPortDSN(t)))

	hStatus, hBody, _ := fetch(t, "http://"+r.addr+"/healthz")
	rStatus, rBody, rRaw := fetch(t, "http://"+r.addr+"/readyz")

	if hStatus != http.StatusOK || hBody["status"] != "ok" {
		t.Errorf("healthz = %d %v, want 200 ok: the process is alive", hStatus, hBody)
	}
	if rStatus != http.StatusServiceUnavailable || rBody["status"] != "not_ready" || rBody["db"] != false {
		t.Errorf("readyz = %d %v, want 503 not_ready db:false", rStatus, rBody)
	}
	if checks, _ := rBody["checks"].(map[string]any); checks["db"] != false {
		t.Errorf("checks = %v, want db:false", rBody["checks"])
	}
	// Paired control: the two answers differ ON THE SAME SERVER at the same time, so "503" is the
	// readiness verdict and not a server that is down.
	if hStatus == rStatus {
		t.Error("liveness and readiness gave the same status: the probe is not distinguishing alive from ready")
	}
	if strings.Contains(rRaw, canary) {
		t.Errorf("the credential reached the readyz body: %s", rRaw)
	}

	// The failure was logged with a reason and without the credential.
	if l := find(r.logs.lines(t), "readiness check failed: database ping"); l == nil || l["reason"] == nil {
		t.Errorf("no readiness failure line with a reason: %s", r.logs.String())
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("clean drain returned %v", err)
	}
	if strings.Contains(r.logs.String(), canary) || strings.Contains(r.logs.String(), "platform_app") {
		t.Errorf("the credential or the login reached the log: %s", r.logs.String())
	}
	if ExitCode(nil) != ExitOK {
		t.Error("a clean drain must exit 0")
	}
	if c, err := net.DialTimeout("tcp", r.addr, 2*time.Second); err == nil {
		_ = c.Close()
		t.Error("the address still accepts connections after Run returned")
	}
}

func TestRun_TheDatabaseURLAndTheReadyzTimeoutAreConsumed(t *testing.T) {
	dsn, accepted := blackHole(t)
	env := base(dsn)
	env[config.VarReadyzTimeoutMS] = "300"
	r := startApp(t, env)

	t0 := time.Now()
	status, body, _ := fetch(t, "http://"+r.addr+"/readyz")
	took := time.Since(t0)

	if status != http.StatusServiceUnavailable || body["db"] != false {
		t.Fatalf("readyz against a hanging database = %d %v, want 503 db:false", status, body)
	}
	// The 300 ms dial is what produced the 503: it waited about that long (the database hung) and well
	// under the 2000 ms default, so PLATFORM_READYZ_TIMEOUT_MS reached the handler.
	if took < 250*time.Millisecond || took > 1500*time.Millisecond {
		t.Errorf("readyz took %v, want about 300ms (default would be 2s)", took)
	}
	// The pool really dialled the address in PLATFORM_DATABASE_URL.
	if accepted.Load() == 0 {
		t.Error("the hanging database saw no connection: PLATFORM_DATABASE_URL did not reach the pool")
	}

	// Shutdown must not be held up by the connect that is still hanging.
	t0 = time.Now()
	if err := r.stop(t); err != nil {
		t.Fatalf("drain with a hanging database connect returned %v", err)
	}
	if d := time.Since(t0); d > 4*time.Second {
		t.Errorf("shutdown took %v with a hanging database connect", d)
	}
}

func TestRun_StartupAndShutdownLinesAndTheLogLevel(t *testing.T) {
	Version = "test-1.2.3"
	t.Cleanup(func() { Version = "dev" })

	r := startApp(t, merge(base(closedPortDSN(t)), map[string]string{config.VarEnv: "staging"}))
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	lines := r.logs.lines(t)

	start := find(lines, "platform starting")
	if start == nil {
		t.Fatalf("no startup line: %s", r.logs.String())
	}
	if start["version"] != "test-1.2.3" || start["env"] != "staging" || start["addr"] != "127.0.0.1:0" {
		t.Errorf("startup line = %v, want version, env and the configured addr", start)
	}
	if stop := find(lines, "platform stopped"); stop == nil || stop["reason"] != "clean drain" {
		t.Errorf("shutdown line = %v", stop)
	}
	// Not the Config struct, and not a secret.
	for _, l := range lines {
		for k := range l {
			if strings.Contains(k, "database") || strings.Contains(k, "config") {
				t.Errorf("a log line carries %q: %v", k, l)
			}
		}
	}
	if strings.Contains(r.logs.String(), canary) {
		t.Errorf("the credential reached the log: %s", r.logs.String())
	}

	// Paired control for the LOG_LEVEL dial: at "warn" the same service writes no info lines, but it does
	// write the warning a failed readiness probe produces (so the quiet is the level's doing, not a logger
	// that writes nothing). A fixed port is needed because the listening line is itself an info line.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixed := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quietLogs := &syncBuffer{}
	done := make(chan error, 1)
	quietEnv := merge(base(closedPortDSN(t)), map[string]string{config.VarLogLevel: "warn", config.VarHTTPAddr: fixed})
	go func() { done <- Run(ctx, envOf(quietEnv), quietLogs) }()
	up := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !up; time.Sleep(10 * time.Millisecond) {
		if resp, err := http.Get("http://" + fixed + "/healthz"); err == nil {
			_ = resp.Body.Close()
			up = true
		}
	}
	if !up {
		t.Fatal("the quiet service never came up")
	}
	fetch(t, "http://"+fixed+"/readyz")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := quietLogs.String()
	if strings.Contains(got, `"level":"INFO"`) {
		t.Errorf("PLATFORM_LOG_LEVEL=warn still wrote info lines: %s", got)
	}
	if !strings.Contains(got, `"level":"WARN"`) {
		t.Errorf("control failed: the warn-level run wrote no warning at all: %q", got)
	}
}

// ---- every dial has a consumer ----------------------------------------------------------------------------

func TestServerConfig_TheServerDialsReachTheServer(t *testing.T) {
	custom, err := config.Load(envOf(map[string]string{
		config.VarDatabaseURL:          "postgres://platform_app:x@h/db",
		config.VarHTTPAddr:             "127.0.0.1:9191",
		config.VarShutdownTimeoutSec:   "41",
		config.VarReadHeaderTimeoutSec: "13",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := serverConfig(custom)
	if got.Addr != "127.0.0.1:9191" || got.ShutdownTimeout != 41*time.Second || got.ReadHeaderTimeout != 13*time.Second {
		t.Fatalf("serverConfig = %+v, want the configured address and timeouts", got)
	}

	// Paired control: the defaults map to different values, so the assertion above is not satisfied by a
	// serverConfig that ignores its input.
	def, err := config.Load(envOf(map[string]string{config.VarDatabaseURL: "postgres://platform_app:x@h/db"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := serverConfig(def); d.Addr != ":8080" || d.ShutdownTimeout != 30*time.Second || d.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("control failed: default serverConfig = %+v", d)
	}
}
