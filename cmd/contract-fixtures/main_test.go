package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/contractcheck"
	"github.com/coldmail-platform/platform/internal/modclient"
)

func block(t *testing.T, file, route, body string) string {
	t.Helper()
	var buf bytes.Buffer
	rc := modclient.RouteContract{Module: modclient.M2, Route: route, Case: "ok", Fixture: file, Status: 200}
	if err := contractcheck.Record(&buf, rc, 200, []byte(body)); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harness.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestCLIWritesEachBlockAndPrintsItsModuleAndRoute(t *testing.T) {
	log := "harness\tThe harness\t2026-11-17T21:32:01.0030894Z " + strings.ReplaceAll(block(t, "a.json", "GET /v1/a", `{"a":1}`), "\n", "\nharness\tThe harness\t2026-11-17T21:32:01.0030894Z ")
	log = strings.TrimSuffix(log, "harness\tThe harness\t2026-11-17T21:32:01.0030894Z ") + block(t, "b.json", "GET /v1/b", `{"b":2}`)
	out := filepath.Join(t.TempDir(), "contract")
	code, stdout, stderr := runCLI("-log", writeLog(t, log), "-out", out)
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	for _, want := range []string{"m2 GET /v1/a", "m2 GET /v1/b"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout must name %q: %q", want, stdout)
		}
	}
	for _, f := range []string{"a.json", "b.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Fatalf("%s not written: %v", f, err)
		}
	}
}

func TestCLIRefusesToOverwriteADifferentFileUnlessForced(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "a.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := writeLog(t, block(t, "a.json", "GET /v1/a", `{"a":1}`))
	code, _, stderr := runCLI("-log", log, "-out", out)
	if code != 1 || !strings.Contains(stderr, "would receive two different bodies") {
		t.Fatalf("code %d stderr %q, want 1 and the conflict", code, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "a.json")); string(got) != "{}\n" {
		t.Fatal("the file was touched without -force")
	}
	if code, _, stderr := runCLI("-log", log, "-out", out, "-force"); code != 0 {
		t.Fatalf("forced run: %d %q", code, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "a.json")); !strings.Contains(string(got), `"a": 1`) {
		t.Fatalf("-force must overwrite: %q", got)
	}
}

func TestCLIAnIdenticalSecondRunIsUnchangedAndSucceeds(t *testing.T) {
	out := t.TempDir()
	log := writeLog(t, block(t, "a.json", "GET /v1/a", `{"a":1}`))
	if code, _, _ := runCLI("-log", log, "-out", out); code != 0 {
		t.Fatal("first run failed")
	}
	code, stdout, _ := runCLI("-log", log, "-out", out)
	if code != 0 || !strings.HasPrefix(stdout, "unchanged ") {
		t.Fatalf("second run: %d %q", code, stdout)
	}
}

func TestCLIRefusesALogWithoutBlocksAndAMissingLog(t *testing.T) {
	out := t.TempDir()
	code, _, stderr := runCLI("-log", writeLog(t, "just noise\n"), "-out", out)
	if code != 1 || !strings.Contains(stderr, "no recorded fixture block") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("-log", filepath.Join(out, "nothere.log"), "-out", out)
	if code != 1 || !strings.Contains(stderr, "cannot read the log") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatal("nothing may be written for a refused log")
	}
}

func TestCLIUsageErrorsExitTwoAndNameTheUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"-log", "x"}, {"-out", "y"}, {"-log", "x", "-out", "y", "stray"}, {"-nope"}} {
		code, _, stderr := runCLI(args...)
		if code != 2 || stderr == "" {
			t.Fatalf("%v: code %d stderr %q, want 2 and a message", args, code, stderr)
		}
	}
}

func TestCLIRefusesASecretShapedBlockInTheLogAndWritesNothing(t *testing.T) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	tok := hex.EncodeToString(raw)
	log := "==== CONTRACT-FIXTURE BEGIN module=m2 route=GET /v1/a case= file=a.json status=200 ====\n" +
		`{"note":"Bearer ` + tok + `"}` + "\n==== CONTRACT-FIXTURE END ====\n"
	out := t.TempDir()
	code, _, stderr := runCLI("-log", writeLog(t, log), "-out", out)
	if code != 1 || !strings.Contains(stderr, "secret-shaped") || strings.Contains(stderr, tok) {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatal("nothing may be written for a secret-shaped block")
	}
}
