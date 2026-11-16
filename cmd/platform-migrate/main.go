// Command platform-migrate is the platform's migration runner: up, down, status, set-app-password and
// up-and-set-password, plus the one-time setup command create-first-admin. All of its behaviour lives in internal/migrate;
// this file parses the command line, loads the configuration from the environment and maps the outcome to an exit status:
//
//	0  success
//	1  runtime error (the database, a migration file, a lock timeout; create-first-admin refused because an admin exists,
//	   the Supabase login does not, or the login's platform user is inactive)
//	2  usage or configuration error (an unknown command, a missing or invalid variable or flag, down while forbidden)
//
// No message it prints carries the connection string or the app password.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coldmail-platform/platform/internal/migrate"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// usageError is a command-line problem; like migrate.ConfigError it exits 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// run is main with its inputs injected, so the tests drive the real command in-process.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	command := args[0]
	version := ""
	var firstAdmin migrate.FirstAdminInput
	switch command {
	case "-h", "--help", "help":
		usage(stdout)
		return exitOK
	case migrate.CmdUp, migrate.CmdStatus, migrate.CmdSetAppPassword, migrate.CmdUpAndSetPassword:
		if len(args) != 1 {
			return fail(stderr, &usageError{command + " takes no arguments"}, nil)
		}
	case migrate.CmdDown:
		if len(args) != 2 {
			return fail(stderr, &usageError{"down needs exactly one version, for example: platform-migrate down 0001"}, nil)
		}
		v, err := migrate.NormalizeVersion(args[1])
		if err != nil {
			return fail(stderr, err, nil)
		}
		version = v
	case migrate.CmdCreateFirstAdmin:
		// Every flag is validated here, before the configuration is loaded and long before a connection is opened.
		in, err := migrate.ParseFirstAdminArgs(args[1:])
		if err != nil {
			return fail(stderr, err, nil)
		}
		firstAdmin = in
	default:
		fmt.Fprintf(stderr, "platform-migrate: unknown command %q\n", command)
		usage(stderr)
		return exitUsage
	}

	cfg, err := migrate.LoadConfig(getenv, command)
	if err != nil {
		return fail(stderr, err, nil)
	}
	r := &migrate.Runner{Cfg: cfg, Out: stdout}

	switch command {
	case migrate.CmdUp:
		err = r.Up(ctx)
	case migrate.CmdDown:
		err = r.Down(ctx, version)
	case migrate.CmdStatus:
		err = r.Status(ctx)
	case migrate.CmdSetAppPassword:
		err = r.SetAppPassword(ctx)
	case migrate.CmdUpAndSetPassword:
		// The password was validated in LoadConfig, so a missing one fails before any migration is applied.
		if err = r.Up(ctx); err == nil {
			err = r.SetAppPassword(ctx)
		}
	case migrate.CmdCreateFirstAdmin:
		err = r.CreateFirstAdmin(ctx, firstAdmin)
	}
	if err != nil {
		return fail(stderr, err, cfg.Secrets())
	}
	return exitOK
}

// fail prints one error line and returns its exit status. The text goes through Redact as a last backstop: the
// primary defence is that no error is built from a secret, this catches the one nobody foresaw.
func fail(stderr io.Writer, err error, secrets []string) int {
	var cfgErr *migrate.ConfigError
	var useErr *usageError
	code, label := exitRuntime, "FATAL"
	switch {
	case errors.As(err, &cfgErr), errors.As(err, &useErr), errors.Is(err, migrate.ErrDownForbidden):
		code, label = exitUsage, "config error"
	}
	fmt.Fprintf(stderr, "platform-migrate: %s: %s\n", label, migrate.Redact(err.Error(), secrets...))
	return code
}

func usage(w io.Writer) {
	var b strings.Builder
	b.WriteString(`platform-migrate - the platform's self-tracking migration runner

usage:
  platform-migrate up                    apply every pending platform_NNNN_*.up.sql, in order, one transaction each
  platform-migrate down <NNNN>           roll back exactly one version (needs PLATFORM_MIGRATE_ALLOW_DOWN=true)
  platform-migrate status                show applied against pending (takes no lock)
  platform-migrate set-app-password      set the platform_app login password from PLATFORM_APP_PASSWORD
  platform-migrate up-and-set-password   up, then set-app-password (the password is checked first)
  platform-migrate create-first-admin --auth-user-id <uuid> --display-name <text>
                                         create the FIRST admin from an existing Supabase login (refused once any
                                         admin exists; run it after up; the flags are validated before connecting)
  platform-migrate help

environment (reference: Docs/migrator-reference.md):
`)
	for _, v := range migrate.Vars() {
		def := v.Default
		if def == "" {
			def = "(none)"
		}
		secret := ""
		if v.Secret {
			secret = ", secret"
		}
		fmt.Fprintf(&b, "  %-40s default %s, required: %s%s\n", v.Name, def, v.Required, secret)
	}
	b.WriteString(`
PLATFORM_MIGRATE_DATABASE_URL must be a DIRECT (session-mode) connection: the runner holds a session-level
advisory lock, which a transaction pooler does not keep.

exit status: 0 ok, 1 runtime error, 2 usage or configuration error
`)
	fmt.Fprint(w, b.String())
}
