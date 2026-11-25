package authtest_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/auth/authtest"
)

const sub = "0b9e1c1e-5d3a-4c53-9f6e-2a1d7c4b8e90"

var t0 = time.Unix(1_000_000_000, 0).UTC()

// both runs the body once per key source, each with its clock at t0.
func both(t *testing.T, body func(t *testing.T, m *authtest.Minter, v *auth.Verifier)) {
	t.Helper()
	for _, mk := range []struct {
		name string
		new  func(testing.TB) *authtest.Minter
	}{{"HS256", authtest.NewHS256}, {"ES256", authtest.NewES256}} {
		t.Run(mk.name, func(t *testing.T) {
			m := mk.new(t)
			m.Now = func() time.Time { return t0 }
			body(t, m, m.Verifier(t))
		})
	}
}

func TestMinter_ALegalTokenVerifiesForBothKeySources(t *testing.T) {
	both(t, func(t *testing.T, m *authtest.Minter, v *auth.Verifier) {
		c, err := v.Verify(context.Background(), m.Token(sub))
		if err != nil {
			t.Fatalf("a legal token was refused: %v", err)
		}
		if c.Subject != sub || c.Issuer != authtest.Issuer || c.Role != "authenticated" {
			t.Fatalf("claims = %+v", c)
		}
	})
}

// Every token an attacker could make is refused by the REAL verifier, for the reason it is made for, and each has its legal twin (the same claims signed properly) accepted: the minter is
// not refusing everything.
func TestMinter_EveryAttackIsRefusedForItsOwnReasonAndHasALegalTwin(t *testing.T) {
	both(t, func(t *testing.T, m *authtest.Minter, v *auth.Verifier) {
		good := m.Good(sub)
		for _, c := range []struct {
			name   string
			attack string
			twin   string
			want   error
		}{
			{"expired", m.Mint(good.With("exp", t0.Unix()-3600)), m.Mint(good.With("exp", t0.Unix()+3600)), auth.ErrExpired},
			{"not yet valid", m.Mint(good.With("nbf", t0.Unix()+3600)), m.Mint(good.With("nbf", t0.Unix()-3600)), auth.ErrNotYetValid},
			{"wrong audience", m.Mint(good.With("aud", "somebody-else")), m.Mint(good), auth.ErrAudience},
			{"wrong issuer", m.Mint(good.With("iss", "https://evil.example.test/auth/v1")), m.Mint(good), auth.ErrIssuer},
			{"forged signature", m.Forged(good), m.Mint(good), auth.ErrSignature},
			{"alg none", m.Unsigned(good), m.Mint(good), auth.ErrAlgorithm},
			{"tampered payload", m.Tampered(m.Mint(good), good.With("sub", "7d4c2a90-3e51-4b8f-a6d2-91c0e5f3b7a4")), m.Mint(good), auth.ErrSignature},
		} {
			if _, err := v.Verify(context.Background(), c.attack); !errors.Is(err, c.want) {
				t.Errorf("%s: the attack was answered with %v, want %v", c.name, err, c.want)
			}
			if _, err := v.Verify(context.Background(), c.twin); err != nil {
				t.Errorf("%s: its legal twin was refused: %v", c.name, err)
			}
		}
	})
}

func TestMinter_TheNowHookMovesTheClockOfMinterAndVerifierTogether(t *testing.T) {
	both(t, func(t *testing.T, m *authtest.Minter, v *auth.Verifier) {
		tok := m.Token(sub)
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("before the clock moves: %v", err)
		}
		m.Now = func() time.Time { return t0.Add(2 * time.Hour) } // an hour past exp, far beyond the leeway
		if _, err := v.Verify(context.Background(), tok); !errors.Is(err, auth.ErrExpired) {
			t.Fatalf("after the clock moved two hours: %v, want ErrExpired (the verifier does not read the Minter's clock)", err)
		}
		// the same minter now mints tokens that are good at the new time
		if _, err := v.Verify(context.Background(), m.Token(sub)); err != nil {
			t.Fatalf("a token minted at the new time was refused: %v", err)
		}
	})
}

func TestMinter_KeysAreMadeAtRunTimeAndNeverShared(t *testing.T) {
	for _, mk := range []func(testing.TB) *authtest.Minter{authtest.NewHS256, authtest.NewES256} {
		a, b := mk(t), mk(t)
		a.Now, b.Now = func() time.Time { return t0 }, func() time.Time { return t0 }
		va, vb := a.Verifier(t), b.Verifier(t)
		if _, err := va.Verify(context.Background(), a.Token(sub)); err != nil {
			t.Fatalf("%s: a minter's own token was refused by its own verifier: %v", a.Alg(), err)
		}
		// the control: another minter's token (the same claims, another key) is refused: two minters never share a key
		if _, err := vb.Verify(context.Background(), a.Token(sub)); err == nil {
			t.Fatalf("%s: a token of one minter was accepted by another minter's verifier: the keys are not made per minter", a.Alg())
		}
	}
}

// ---- production never links it -------------------------------------------------------------------------------

const authtestPath = "github.com/coldmail-platform/platform/internal/auth/authtest"

// goDeps is `go list -deps` of the package, one import path per line.
func goDeps(t *testing.T, pkg string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	var deps []string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			deps = append(deps, l)
		}
	}
	return deps
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestAuthtest_IsNotLinkedIntoTheService(t *testing.T) {
	const service = "github.com/coldmail-platform/platform/cmd/platform"
	deps := goDeps(t, service)
	// the instrument must have looked at something: the service links the platform's own packages (the job API among them, once built) and the standard library
	if len(deps) < 100 || !has(deps, "github.com/coldmail-platform/platform/internal/auth") || !has(deps, "github.com/coldmail-platform/platform/internal/httpapi") {
		t.Fatalf("`go list -deps %s` listed %d packages and not the platform's own: the instrument matched nothing", service, len(deps))
	}
	if has(deps, authtestPath) {
		t.Fatalf("the service links %s: test support is in the production binary", authtestPath)
	}
	t.Logf("go list -deps %s: %d packages, none of them authtest", service, len(deps))

	// the control: the same instrument DOES find the package where it is a dependency (the package itself)
	if own := goDeps(t, authtestPath); !has(own, authtestPath) {
		t.Fatalf("`go list -deps %s` does not list the package itself: the instrument cannot see authtest at all", authtestPath)
	}
}

// importsAuthtest says whether Go source imports the package.
func importsAuthtest(src string) bool { return strings.Contains(src, `"`+authtestPath+`"`) }

func TestAuthtest_NoNonTestFileImportsIt(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	own, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	files, hits := 0, []string{}
	for _, dir := range []string{"cmd", "internal"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				if abs, _ := filepath.Abs(path); abs == own {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files++
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if importsAuthtest(string(b)) {
				hits = append(hits, path)
			}
			return nil
		})
	}
	if files < 50 {
		t.Fatalf("the scan found %d non-test Go files under cmd/ and internal/: the instrument matched (almost) nothing", files)
	}
	if len(hits) != 0 {
		t.Fatalf("non-test files import test support: %v", hits)
	}
	t.Logf("scanned %d non-test Go files; none imports authtest (test files are not scanned: they are what it is for)", files)

	// the control: a file that does import it is found by the same predicate
	if !importsAuthtest("import (\n\t\"" + authtestPath + "\"\n)") {
		t.Fatal("the scan's predicate does not recognise an import of authtest")
	}
	if importsAuthtest("import \"github.com/coldmail-platform/platform/internal/auth\"") {
		t.Fatal("the scan's predicate matches the auth package itself")
	}
}
