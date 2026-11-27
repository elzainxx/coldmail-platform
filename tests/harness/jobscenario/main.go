//go:build harness

// Command jobscenario is the scenario program of the harness scope `jobs` (tests/harness/jobs/stage.sh, Phase 4 chunk B17a): it drives the PLATFORM'S OWN HTTP API with a fresh user, and reads
// what the platform, module 1 and the ledger say afterwards, so that the crash, handover and outage stages can be written in bash around a few facts and a verdict.
//
// It is NOT a test: it adds nothing to the harness's test count (tests/floors-harness.txt), it is compiled by run.sh next to platform-migrate and run INSIDE the private network (compose service
// `jobscenario`). It never mocks a module and never imports internal/jobs or internal/jobengine: the platform is the service process it talks to, the database is read with plain SQL, module 1 is
// asked through its own adapter with the key the platform derives for the user.
//
// Subcommands (flags; one JSON object per line on stdout, a `fact` member names the line; a failure is a non-zero exit with the reason on stderr; a secret and a token are never printed):
//
//	create   --api URL --rows N [--module m1] [--bad K] [--dup D] [--grant G]   a fresh user with credits (through the ledger), a draft of N lines, its preview: prints the user, the job, the fingerprint
//	confirm  --api URL --job J [--key K] [--scheduled-in S] [--expect ok|no-response|refused:<code>]   one confirm of a previewed job (exit 0 only when the answer is the one expected)
//	confirm-retry  the same flags and the same confirm key: the replay after a kill (it must answer 200)
//	setup    create + confirm in one call (the flags of both)
//	wait     --job J --until terminal|running|any:<status>[,<status>] [--timeout S]   polls the database; prints the status history of the job from platform.job_events
//	state    --job J [--expect name=value,name>value,...]   one line with what the database holds of the job (status, rows stamped, results, the reserve, module 1's holding); the conditions are asserted
//	verify   --job J   the invariants of a finished job, each printed as `OK <name>` or failed by name (see verify.go)
//	leader   which of the instances holds the engine's leader lock (pg_stat_activity.application_name)
//
// and, for the senders (send.go, send_verify.go; scope sendjobs, tests/harness/jobs/send-stage.sh): create and setup with --module m2|m3 (a user with an account at the module, module 2's credits or module 3's
// entitlement, the controllable sink told which recipients to reject and how long to hold each message, a campaign with a subject and a body), send-prepare (the modules' dials and the sink, once), move (the
// user's pause, resume or cancel of a job through the API), state of a send job (the module's own tables and the sink; --report adds the module's campaign report) and verify of a send job (--path
// normal|pause|cancel: the invariants with the sink counted per recipient).
//
// and the five of stage J6 (vendor.go; each takes --expect conditions of the syntax of `state`): vendor (module 1's own describe: vendor_fallback.reachable), flag (the platform's stored flag of m1_vendor and
// its history), preview (a job previewed again through the API: the rate, the credits, whether module 1 was read), reserve (what the ledger and the price snapshot of a job hold) and cancel (the user's
// cancel of a scheduled job).
//
// and the two of stage J7 (optout.go; the per-user vendor opt-out): policy (module 1's own results of a job: the policies the rows say they were made under, against the key module 1's describe publishes) and stub (the
// requests module 1's ZeroBounce stand-in has received), with the flags --optout and --vendor-tag of create and setup.
//
// Environment (a missing one fails the command naming it): HARNESS_ADMIN_DATABASE_URL (the harness's superuser connection), JOBS_JWT_SECRET and JOBS_JWT_ISSUER (the platform's PLATFORM_JWT_* of the
// instances: HS256, minted per run), JOBS_M1_KEY_SECRET (the platform's PLATFORM_M1_KEY_SECRET), and, optional, JOBS_JWT_AUDIENCE (authenticated), JOBS_M1_KEY_VERSION (1), JOBS_M1_URL (http://m1:8080).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// the exit codes: 0 is success; a failed check or step is 1; a bad command line 2; a request that got no HTTP answer at all 4; an HTTP answer other than the one expected 5.
const (
	exitFail       = 1
	exitUsage      = 2
	exitNoResponse = 4
	exitRefused    = 5
)

// failure is an error that carries the exit code of the command.
type failure struct {
	code int
	msg  string
}

func (f *failure) Error() string { return f.msg }

func failf(code int, format string, a ...any) error {
	return &failure{code: code, msg: fmt.Sprintf(format, a...)}
}

// stdout is where facts go; a test of this program could swap it.
var stdout io.Writer = os.Stdout

// fact prints one line of JSON: {"fact":"<name>", k:v, ...} with the members in the order given. The values are what encoding/json makes of them.
func fact(name string, kv ...any) {
	var b strings.Builder
	b.WriteString(`{"fact":`)
	b.Write(mustJSON(name))
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(",")
		b.Write(mustJSON(fmt.Sprint(kv[i])))
		b.WriteString(":")
		b.Write(mustJSON(kv[i+1]))
	}
	b.WriteString("}")
	fmt.Fprintln(stdout, b.String())
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic("jobscenario: a fact could not be encoded: " + err.Error()) // only a value this program built can fail to encode: a bug of this file
	}
	return out
}

const usageText = `usage: jobscenario <create|confirm|confirm-retry|setup|wait|state|verify|leader|vendor|flag|preview|reserve|cancel|policy|stub|send-prepare|move> [flags]   (see the header of tests/harness/jobscenario/main.go)`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usageText)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "create":
		err = cmdCreate(rest)
	case "confirm":
		err = cmdConfirm(rest, false)
	case "confirm-retry":
		err = cmdConfirm(rest, true)
	case "setup":
		err = cmdSetup(rest)
	case "wait":
		err = cmdWait(rest)
	case "state":
		err = cmdState(rest)
	case "verify":
		err = cmdVerify(rest)
	case "leader":
		err = cmdLeader(rest)
	case "vendor":
		err = cmdVendor(rest)
	case "flag":
		err = cmdFlag(rest)
	case "preview":
		err = cmdPreview(rest)
	case "reserve":
		err = cmdReserve(rest)
	case "cancel":
		err = cmdCancel(rest)
	case "policy":
		err = cmdPolicy(rest)
	case "stub":
		err = cmdStub(rest)
	case "send-prepare":
		err = cmdSendPrepare(rest)
	case "move":
		err = cmdMove(rest)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, usageText)
		return exitUsage
	default:
		fmt.Fprintf(os.Stderr, "jobscenario: unknown command %q\n%s\n", cmd, usageText)
		return exitUsage
	}
	if err == nil {
		return 0
	}
	code := exitFail
	var f *failure
	if errors.As(err, &f) {
		code = f.code
	}
	fmt.Fprintf(os.Stderr, "jobscenario %s: %s\n", cmd, err.Error())
	return code
}

// flags makes a FlagSet that reports a bad command line as a usage failure instead of exiting.
func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// parse parses args and refuses leftovers.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return failf(exitUsage, "%v", err)
	}
	if fs.NArg() != 0 {
		return failf(exitUsage, "unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// env reads a required variable of the scenario's environment.
func env(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", failf(exitFail, "the environment variable %s is not set (the compose service jobscenario gives it; tests/harness/docker-compose.jobs.yml)", name)
	}
	return v, nil
}

// envOr reads an optional variable.
func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// ctxFor is the context of one command: every command is bounded, so a hung database or API ends the stage instead of the run.
func ctxFor(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
