//go:build harness

// Package harness holds the platform's real-infrastructure integration tests. Every file in it carries the build tag
// `harness`, so `go test ./...` never compiles them, never needs a database, and can never skip into a green.
//
// The tests run in the harness environment (tests/harness/run.sh, the `harness` workflow): one real PostgreSQL, the
// platform migrated into the live database `platform`, and the migrator built as a real binary. They are given that
// environment through five variables, and a missing one FAILS the test naming it; nothing here skips:
//
//	HARNESS_ADMIN_DATABASE_URL       superuser connection to the live database `platform` (read-only use)
//	HARNESS_ADMIN_DATABASE_HOST_URL  superuser connection to the maintenance database `postgres` (creates and drops scratch databases)
//	HARNESS_PLATFORM_APP_PASSWORD    the password the live service logs in with as platform_app
//	HARNESS_MIGRATE_BIN              path of the built platform-migrate binary
//	HARNESS_REPO_ROOT                the repository root (the migrations are at $HARNESS_REPO_ROOT/db/migrations)
//
// Tests that need a clean database create their own scratch database `platform_h_<random>` and drop it when they end,
// pass or fail, and run the migrator against it as a real process. Roles are shared by every database in the cluster and
// the live service logs in as platform_app, so no test changes that role's password to any value but
// HARNESS_PLATFORM_APP_PASSWORD, drops the roles on purpose, or alters the live database. The tests named TestLive_*
// only read it.
package harness
