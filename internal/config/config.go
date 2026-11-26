// Package config loads the platform's runtime configuration from the environment.
//
// CONFIG, NOT CODE. Every dial the process reads is declared once, in Vars(): name, default, whether it
// is required, whether it is a secret, and what it means. Load reads exactly those variables and nothing
// else; Docs/config-reference.md is checked against Vars() by a test in both directions, so the table an
// operator reads cannot drift from the code that reads it. A dial without a consumer is a bug: the tests
// in this package fail when a Var is not read by Load, when changing it changes nothing in Config, or
// when the documentation disagrees.
//
// Phase 0 reads process-wiring values only (address, credentials, timeouts of the HTTP server). Phase 2
// adds the process wiring of module 3's entitlement flow (where module 3's control is, the credential for
// its admin route, and how often the two loops run); the entitlement rules themselves live in the
// database. Phase 3 adds the module client's dials, the module 1 key secret and four optional dials (where
// modules 1 and 2 are, and the service tokens of modules 2 and 3) that the adapters read. Tunables that govern product behaviour belong in the database's config table when it exists,
// not here.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/logging"
)

// The environment variables the platform reads. Exported so callers name them instead of repeating a
// string literal.
const (
	VarHTTPAddr             = "PLATFORM_HTTP_ADDR"
	VarDatabaseURL          = "PLATFORM_DATABASE_URL"
	VarLogLevel             = "PLATFORM_LOG_LEVEL"
	VarEnv                  = "PLATFORM_ENV"
	VarShutdownTimeoutSec   = "PLATFORM_SHUTDOWN_TIMEOUT_SEC"
	VarReadyzTimeoutMS      = "PLATFORM_READYZ_TIMEOUT_MS"
	VarReadHeaderTimeoutSec = "PLATFORM_READ_HEADER_TIMEOUT_SEC"

	// Module 3's entitlement flow (internal/entitlements, internal/m3admin).
	VarM3URL                        = "PLATFORM_M3_URL"
	VarM3AdminToken                 = "PLATFORM_M3_ADMIN_TOKEN"
	VarEntitlementExpireIntervalSec = "PLATFORM_ENTITLEMENT_EXPIRE_INTERVAL_SEC"
	VarM3ReconcileIntervalSec       = "PLATFORM_M3_RECONCILE_INTERVAL_SEC"
	VarM3ReverifyIntervalSec        = "PLATFORM_M3_REVERIFY_INTERVAL_SEC"
	VarM3RequestTimeoutSec          = "PLATFORM_M3_REQUEST_TIMEOUT_SEC"

	// The money store (internal/money): how long one operation waits for another on the same credits (Phase 2 close-out D8).
	VarMoneyLockTimeoutMS = "PLATFORM_MONEY_LOCK_TIMEOUT_MS"

	// The calls to the three modules (internal/modclient, Phase 3 B1): how long one request may take, how many extra attempts an idempotent request gets, and the first wait between attempts.
	VarModuleTimeoutMS      = "PLATFORM_MODULE_TIMEOUT_MS"
	VarModuleRetries        = "PLATFORM_MODULE_RETRIES"
	VarModuleRetryBackoffMS = "PLATFORM_MODULE_RETRY_BACKOFF_MS"

	// The secret every user's module 1 caller key is derived from (internal/tenant, Phase 3 B2).
	VarM1KeySecret = "PLATFORM_M1_KEY_SECRET"
	// The version of the keys the platform presents to module 1 (internal/tenant, Phase 3 close-out A4): the number in the derivation text, raised to rotate the secret.
	VarM1KeyVersion = "PLATFORM_M1_KEY_VERSION"
	// Whether module 1's vendor fallback is switched on (internal/app syncReachability -> internal/money SetUnitReachable, Phase 4 B3): the platform's mirror of module 1's own TM_EXTERNAL_* switches,
	// which decides what a module 1 job reserves (1 credit per row with it off, 2 with it on).
	VarM1VendorReachable = "PLATFORM_M1_VENDOR_REACHABLE"

	// Where the three modules are and the credentials of the platform's own calls to them (the adapters internal/adapters/m1, m2 and m3, Phase 3 B3 to B5). All four are optional: an adapter
	// whose dial is empty refuses with a typed error, the service still starts. Module 1 has no service token: it is called with each user's derived caller key (VarM1KeySecret).
	VarM1URL          = "PLATFORM_M1_URL"
	VarM2URL          = "PLATFORM_M2_URL"
	VarM2ServiceToken = "PLATFORM_M2_SERVICE_TOKEN"
	VarM3ServiceToken = "PLATFORM_M3_SERVICE_TOKEN"

	// How the job API verifies the Supabase access tokens it is shown (internal/auth, Phase 4 B14a): the HS256 secret and/or the JWKS URL of the asymmetric keys, and the rules a token must meet.
	VarJWTSecret    = "PLATFORM_JWT_SECRET"
	VarJWKSURL      = "PLATFORM_JWKS_URL"
	VarJWTIssuer    = "PLATFORM_JWT_ISSUER"
	VarJWTAudience  = "PLATFORM_JWT_AUDIENCE"
	VarJWTLeewaySec = "PLATFORM_JWT_LEEWAY_SEC"

	// The limits of a job and of the job API (internal/app builds the job store and the API from them, Phase 4 B14b): the longest list one job holds, how long a confirmed or scheduled job waits for the
	// engine before it expires, and the largest page of results the API serves.
	VarJobMaxRows          = "PLATFORM_JOB_MAX_ROWS"
	VarJobMaxStartDelayMin = "PLATFORM_JOB_MAX_START_DELAY_MIN"
	VarResultsPageMax      = "PLATFORM_RESULTS_PAGE_MAX"

	// The job engine (internal/jobengine, built and run by internal/app, Phase 4 B7 to B9): whether it runs in this instance, how often it looks, how long it leaves a running job alone between two
	// observations, how long a running job may make no progress before it is called stale (one dial per module), how many rows one submit call offers a module, how many run calls to modules 2 and 3
	// are in flight at once, and how long one job's step may take.
	VarJobEngineEnabled     = "PLATFORM_JOB_ENGINE_ENABLED"
	VarJobTickMS            = "PLATFORM_JOB_TICK_MS"
	VarJobSettleIntervalSec = "PLATFORM_JOB_SETTLE_INTERVAL_SEC"
	VarJobStaleAfterMinM1   = "PLATFORM_JOB_STALE_AFTER_MIN_M1"
	VarJobStaleAfterMinM2   = "PLATFORM_JOB_STALE_AFTER_MIN_M2"
	VarJobStaleAfterMinM3   = "PLATFORM_JOB_STALE_AFTER_MIN_M3"
	VarJobSubmitBatch       = "PLATFORM_JOB_SUBMIT_BATCH"
	VarJobSubmitConcurrency = "PLATFORM_JOB_SUBMIT_CONCURRENCY"
	VarJobStepTimeoutSec    = "PLATFORM_JOB_STEP_TIMEOUT_SEC"

	// Which instance this is, when more than one platform instance runs (Phase 2 close-out D9): the name of the session that holds the leader lock of the loops.
	VarInstanceID = "PLATFORM_INSTANCE_ID"
)

// MinM1KeySecretBytes is the shortest PLATFORM_M1_KEY_SECRET accepted (and the shortest internal/tenant derives a key from). 32 bytes is the size of the HMAC-SHA256 output the key is made of:
// a shorter secret would make every user's key easier to find than the key it protects is long.
const MinM1KeySecretBytes = 32

// The versions PLATFORM_M1_KEY_VERSION may take. The version is written into the text every module 1 key is derived from (internal/tenant), so two versions of one user are two different keys and
// the secret can be rotated while the old keys still work. 999 is far more rotations than a deployment will ever make, and keeps the derivation text short and unambiguous.
const (
	MinM1KeyVersion = 1
	MaxM1KeyVersion = 999
)

// MinJWTSecretBytes is the shortest PLATFORM_JWT_SECRET accepted (internal/auth refuses the same: auth.MinHMACSecretBytes, pinned equal by a test there). 32 bytes is the size of the HMAC-SHA256
// output the token signature is made of.
const MinJWTSecretBytes = 32

// The bounds of the token-verification dials: the largest leeway the clocks of the issuer and of this process may be allowed to differ by (five minutes; a token is valid for an hour, so more would
// be a window, not a tolerance), the longest a configured issuer or audience may be, and the audience the access token of a signed-in Supabase user carries.
const (
	maxJWTLeewaySec = 300
	maxJWTClaimText = 512
	jwtAudienceDef  = "authenticated"
)

// The bounds of the job dials. A job holds at most 100,000 rows (module 1's own limit for one job, and the store's default: internal/jobs.DefaultMaxRows, pinned equal by a test); a start may be
// waited for at most a day; a page of results is at most 1,000 (the page the CSV download reads, internal/jobs.ResultsPageSize, pinned equal by a test).
const (
	maxJobRows          = 100000
	maxJobStartDelayMin = 1440
	maxResultsPage      = 1000
)

// The bounds of the job engine's dials. A pass faster than a tenth of a second would be a busy loop against the database and the modules; slower than a minute would make a job wait a minute for
// every step. An observation of a running job at most once an hour, a week of silence before a stale event, 5000 rows to a submit call and 64 run calls in flight are each far past what a module
// can take (module 1's own request holds 100,000 rows, and the engine's batch for it is the job); a step shorter than five seconds could not finish a healthy call.
const (
	minJobTickMS         = 100
	maxJobTickMS         = 60000
	maxJobSettleSec      = 3600
	maxJobStaleMin       = 10080
	maxJobSubmitBatch    = 5000
	maxJobSubmitConc     = 64
	minJobStepTimeoutSec = 5
	maxJobStepTimeoutSec = 3600
)

// Upper bounds on the integer dials. They are sanity guards, not tuning: they keep a typo (an extra digit,
// a value in the wrong unit) from becoming a one-day shutdown, and keep the multiplication into a
// time.Duration far from overflow.
const (
	maxTimeoutSec = 3600
	maxReadyzMS   = 60000

	maxLoopIntervalSec = 3600  // the expiry and reconcile loops
	minReverifySec     = 10    // a re-check more often than this would be the reconcile loop's own job
	maxReverifySec     = 86400 // one day
	maxM3RequestSec    = 120

	// The wait of a money operation for the lock of a balance. Below the floor a healthy operation that merely queued behind another would fail; above the ceiling a stuck holder
	// is no better than no timeout (ten minutes). 0 is not an option at all: to PostgreSQL it means "wait for ever".
	minMoneyLockMS = 100
	maxMoneyLockMS = 600000

	// The module client's dials. A request below half a second would fail a healthy module that is merely busy (every request costs a bcrypt verify and a database round trip); above five minutes a
	// stalled module is no better than no timeout. More than ten extra attempts, or a first wait above thirty seconds, is a retry storm against a module that is already struggling.
	minModuleTimeoutMS = 500
	maxModuleTimeoutMS = 300000
	maxModuleRetries   = 10
	maxModuleBackoffMS = 30000

	// The name of an instance is the application_name of a database session. PostgreSQL cuts that setting at 63 bytes and replaces every byte that is not printable ASCII, so a longer or
	// wider name would be shown differently from what the operator wrote; it is refused instead.
	maxInstanceIDBytes = 63

	// instanceIDDefault is the documented default of VarInstanceID. It stands for "the host name": Load reads nothing but getenv, so the host name is resolved by the service (internal/entitlements
	// ResolveInstanceID) and Config.InstanceID stays empty for it.
	instanceIDDefault = "(hostname)"

	// unsetMarker is the documented default of every optional dial that has no usable default: VarM1KeySecret, VarM1URL, VarM2URL, VarM2ServiceToken and VarM3ServiceToken. It stands for "not
	// configured" and is NOT a value (a built-in key or address would be one everybody knows): unset leaves the Config field empty, the platform still starts, and the consumer refuses with a typed
	// error until it is set (internal/tenant: ErrNoM1KeySecret; the adapters: their FromConfig).
	unsetMarker = "(unset)"
)

// Var describes one environment variable. Vars() is the single source of truth for every dial.
type Var struct {
	Name     string
	Default  string // "" for a variable with no default (a required one)
	Doc      string // the text of the Meaning column in Docs/config-reference.md, verbatim
	Required bool
	Secret   bool
}

// Vars returns every dial the platform reads, in documentation order. The slice is a fresh copy.
func Vars() []Var {
	return []Var{
		{Name: VarHTTPAddr, Default: ":8080",
			Doc: "TCP listen address of the HTTP server, as host:port. Port 0 picks a free port."},
		{Name: VarDatabaseURL, Required: true, Secret: true,
			Doc: "PostgreSQL connection string of the least-privileged `platform_app` login. Never printed or logged."},
		{Name: VarLogLevel, Default: "info",
			Doc: "Minimum log level: one of " + strings.Join(logging.LevelNames(), ", ") + "."},
		{Name: VarEnv, Default: "development",
			Doc: "Free-form label for the deployment (for example development or production), reported in the startup log line."},
		{Name: VarShutdownTimeoutSec, Default: "30",
			Doc: fmt.Sprintf("Seconds in-flight requests get to finish after SIGINT or SIGTERM before the remaining connections are cut. Integer, 1 to %d.", maxTimeoutSec)},
		{Name: VarReadyzTimeoutMS, Default: "2000",
			Doc: fmt.Sprintf("Milliseconds the readiness probe waits for the database ping before answering not ready. Integer, 1 to %d.", maxReadyzMS)},
		{Name: VarReadHeaderTimeoutSec, Default: "10",
			Doc: fmt.Sprintf("Seconds a client has to send its request headers before the connection is closed. Integer, 1 to %d.", maxTimeoutSec)},
		{Name: VarM3URL, Required: true,
			Doc: "Base URL of module 3's control service (http or https, with a host and no credentials), for example http://m3:8080. The platform suspends and resumes users through its admin route."},
		{Name: VarM3AdminToken, Required: true, Secret: true,
			Doc: "Module 3's operator (admin) key, sent as a bearer token on its admin route. A different credential from the service token. Never printed or logged."},
		{Name: VarEntitlementExpireIntervalSec, Default: "30",
			Doc: fmt.Sprintf("Seconds between passes of the expiry loop, which ends every M3 entitlement whose enabled-until date has passed by the database's clock. Integer, 1 to %d.", maxLoopIntervalSec)},
		{Name: VarM3ReconcileIntervalSec, Default: "5",
			Doc: fmt.Sprintf("Seconds between passes of the reconcile loop, which delivers each wanted suspend or resume to module 3 and records what module 3 answered. Integer, 1 to %d.", maxLoopIntervalSec)},
		{Name: VarM3ReverifyIntervalSec, Default: "300",
			Doc: fmt.Sprintf("Seconds after which a user already in the wanted state is checked against module 3 again (the same suspend or resume is sent once more; a changed answer means module 3 had drifted). Integer, %d to %d.", minReverifySec, maxReverifySec)},
		{Name: VarM3RequestTimeoutSec, Default: "10",
			Doc: fmt.Sprintf("Seconds one request to module 3's admin route may take before it counts as failed and is retried later. Integer, 1 to %d.", maxM3RequestSec)},
		{Name: VarMoneyLockTimeoutMS, Default: "10000",
			Doc: fmt.Sprintf("Milliseconds a money operation (grant, adjustment, reserve, settle, release) waits for another operation on the same user's credits of the same module before it gives up with a lock-timeout error and writes nothing. Integer, %d to %d.", minMoneyLockMS, maxMoneyLockMS)},
		{Name: VarModuleTimeoutMS, Default: "15000",
			Doc: fmt.Sprintf("Milliseconds one request to a module (module 1, 2 or 3) may take, counted per attempt and including the time to read the response body, before it counts as a timeout. A request that sets its own timeout (module 3's account creation and bulk add run for tens of seconds) overrides it. Integer, %d to %d.", minModuleTimeoutMS, maxModuleTimeoutMS)},
		{Name: VarModuleRetries, Default: "2",
			Doc: fmt.Sprintf("Extra attempts a request to a module gets after a failure, 0 for none. Only a request marked idempotent (a read, or a write the module deduplicates by the platform's own key) is ever retried, and only after the module was unreachable, timed out or answered 429; a refusal, a not found, a module fault (500) or an answer that breaks the contract is never retried, and neither is a write that is not idempotent. Integer, 0 to %d.", maxModuleRetries)},
		{Name: VarModuleRetryBackoffMS, Default: "250",
			Doc: fmt.Sprintf("Milliseconds before the first retry of a request to a module; each further retry waits twice as long as the one before, up to 30 seconds, with no random jitter. A 429 that names a Retry-After waits the larger of the two (never more than the request timeout). 0 retries at once. Integer, 0 to %d.", maxModuleBackoffMS)},
		{Name: VarM1KeySecret, Default: unsetMarker, Secret: true,
			Doc: fmt.Sprintf("The secret each user's module 1 caller key is derived from: the key is the unpadded base64url of the HMAC-SHA256, under this secret, of the text m1-caller, v and the key version (PLATFORM_M1_KEY_VERSION) and the user's id, each part set off by a vertical bar; module 1 keeps only the SHA-256 of each key. At least %d bytes (64 random hexadecimal characters are plenty). The default (unset) means no module 1 key can be derived and no tenant scope built; the service still starts. Changing it changes every user's key: rotate it with a new key version (see PLATFORM_M1_KEY_VERSION). Never printed or logged.", MinM1KeySecretBytes)},
		{Name: VarM1KeyVersion, Default: "1",
			Doc: fmt.Sprintf("The version of the module 1 caller keys the platform derives and presents: the key of a user is the HMAC-SHA256, under PLATFORM_M1_KEY_SECRET, of the text m1-caller, v and this number, and the user's id, each part set off by a vertical bar. Integer, %d to %d. Raise it (with a new PLATFORM_M1_KEY_SECRET) to rotate: provision the new version's rows first (the old keys keep working beside them), switch this dial and the secret, then retire the old version; the steps are in Docs/config-reference.md. Changing it without provisioning the new rows makes every module 1 call answer 401.", MinM1KeyVersion, MaxM1KeyVersion)},
		{Name: VarM1VendorReachable, Default: "false",
			Doc: "Whether module 1's vendor fallback (ZeroBounce) is switched on, as the platform is told: `true` or `false` and nothing else (any other value stops the process at start-up with exit status 2). It is the platform's mirror of module 1's own `TM_EXTERNAL_*` fallback switches: module 1 decides whether it calls the vendor, the platform decides what a module 1 job reserves, and the owner switches BOTH together (the pair is listed in OWNER-ACTIONS.md of the parent repository). With it off (the default) a module 1 job reserves 1 credit per row, and a vendor answer that arrives all the same is never charged: it is flagged and its credits stay in the reserve until the release. With it on, a module 1 job reserves 2 credits per row (the dearest unit, `m1_vendor`). Switching it never changes the reserve of a job already confirmed: a job's prices are frozen at its reserve. Every platform instance runs a loop that writes the value into `platform.unit_costs.reachable` when it starts and every 60 seconds after (every 5 seconds until the first success), so all instances must be given the same value. Consumed by `internal/app` (`syncReachability`, through `internal/money.Store.SetUnitReachable`)."},
		{Name: VarM1URL, Default: unsetMarker,
			Doc: "Base URL of module 1's control plane (http or https, with a host and no credentials, query or fragment), for example http://m1:8080. Module 1 is called with each user's derived caller key (see PLATFORM_M1_KEY_SECRET), not with a service token. The default (unset) means no module 1 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m1.FromConfig`."},
		{Name: VarM2URL, Default: unsetMarker,
			Doc: "Base URL of module 2's platform API (http or https, with a host and no credentials, query or fragment), for example http://m2:8080. The default (unset) means no module 2 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m2.FromConfig`."},
		{Name: VarM2ServiceToken, Default: unsetMarker, Secret: true,
			Doc: "Module 2's service token (its Domain A bearer), sent on every platform call to module 2. Printable ASCII with no spaces. The default (unset) means no module 2 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m2.FromConfig`. Never printed or logged."},
		{Name: VarM3ServiceToken, Default: unsetMarker, Secret: true,
			Doc: "Module 3's Domain A service token, sent as the bearer on the platform's user-facing calls to module 3. NOT the operator key PLATFORM_M3_ADMIN_TOKEN, which stays a different credential. Printable ASCII with no spaces. The default (unset) means no user-facing module 3 call can be made and its adapter refuses with a typed error; the service still starts. Consumed by `internal/adapters/m3.FromConfig`. Never printed or logged."},
		{Name: VarJWTSecret, Default: unsetMarker, Secret: true,
			Doc: fmt.Sprintf("The HS256 secret of the Supabase project (its \"legacy JWT secret\"), used as the raw bytes of the string, with which the job API verifies the access tokens users present. At least %d bytes of printable ASCII with no spaces. The default (unset) means no HS256 token is accepted; the service still starts, and the job API is not served until this or PLATFORM_JWKS_URL is set. Consumed by `internal/auth.FromConfig`. Never printed or logged.", MinJWTSecretBytes)},
		{Name: VarJWKSURL, Default: unsetMarker,
			Doc: "Where the Supabase project publishes the public keys its access tokens are signed with (a project with asymmetric signing keys): `https://<project-ref>.supabase.co/auth/v1/.well-known/jwks.json`. An http or https URL with a host and no credentials, query or fragment; `internal/auth` fetches it only over https, at most every 30 seconds, with a 10 second timeout, no proxy and no redirect, and refuses it at start-up when it is http. Keys are used for ES256 (P-256) and RS256 (at least 2048 bits) only. The default (unset) means no asymmetric token is accepted; the service still starts, and the job API is not served until this or PLATFORM_JWT_SECRET is set. Consumed by `internal/auth.FromConfig`."},
		{Name: VarJWTIssuer, Default: unsetMarker,
			Doc: "The issuer (iss) every access token must carry, exactly, for example `https://<project-ref>.supabase.co/auth/v1`. Printable ASCII with no spaces, at most 512 bytes. The default (unset) means the issuer is not checked: a token is judged by its signature, audience and times alone. Set it in production. Consumed by `internal/auth.FromConfig`."},
		{Name: VarJWTAudience, Default: jwtAudienceDef,
			Doc: "The audience (aud) every access token must include; Supabase gives a signed-in user's token authenticated. Printable ASCII with no spaces, at most 512 bytes. Consumed by `internal/auth.FromConfig`."},
		{Name: VarJWTLeewaySec, Default: "30",
			Doc: fmt.Sprintf("Seconds of clock difference between the token's issuer and this process that exp, nbf and iat are judged with: a token is expired at exp plus this, and not yet valid before nbf minus this. Integer, 0 to %d. Consumed by `internal/auth.FromConfig`.", maxJWTLeewaySec)},
		{Name: VarJobMaxRows, Default: "100000",
			Doc: fmt.Sprintf("The most rows one job may hold. A longer list is refused when the job is created, with the limit named: the job API answers 413 `too_many_rows`, and a CSV body is cut off at the limit while it is read. Integer, 1 to %d (100000 is module 1's own limit for one job). Consumed by `internal/app` (`jobs.WithMaxRows`, on the one job store) and, through the store's `MaxRows`, by `internal/api`.", maxJobRows)},
		{Name: VarJobMaxStartDelayMin, Default: "30",
			Doc: fmt.Sprintf("Minutes after its start that a confirmed or scheduled job may wait for the engine to start it before it expires and its credits are released. The start deadline is the scheduled start (the moment of the confirm for a job with none) plus this, added by the database's clock when the job is confirmed; a job confirmed before the dial was changed keeps the deadline it was given. Integer, 1 to %d. Consumed by `internal/app` (`jobs.WithMaxStartDelay`, on the one job store).", maxJobStartDelayMin)},
		{Name: VarResultsPageMax, Default: "1000",
			Doc: fmt.Sprintf("The most results one page of the job API's `GET /v1/jobs/{id}/results` may hold: the largest `limit` it accepts (a larger one is refused with 400 `validation_failed`); without a `limit` a page is 100, or this when it is smaller. The CSV download is not paged by it: it streams every result, 1000 at a time. Integer, 1 to %d. Consumed by `internal/app` (`api.New`, `Deps.ResultsPageMax`).", maxResultsPage)},
		{Name: VarJobEngineEnabled, Default: "true",
			Doc: "Whether this instance runs the job engine: `true` or `false` and nothing else (any other value stops the process at start-up with exit status 2). With `true` (the default) the engine runs in the instance that holds the leader lock, beside the entitlement loops and under the same lock: it starts confirmed and due jobs in their module, reads what the module reports, records and charges every finished item, completes jobs and gives back the credits a job did not use. With `false` the engine does not start in this instance and the start-up log says so once; a job then stays where it is. Give every instance the same value: a job waits for the engine of whichever instance leads, so one instance with the engine off can win the lock and hold every job. The engine needs the module URLs and tokens (`PLATFORM_M1_URL` with `PLATFORM_M1_KEY_SECRET`, `PLATFORM_M2_URL` with `PLATFORM_M2_SERVICE_TOKEN`, `PLATFORM_M3_URL` with `PLATFORM_M3_SERVICE_TOKEN`): a job of a module that is not configured waits and goes stale, it is never failed for it. Consumed by `internal/app` (it builds and runs `internal/jobengine`, or does not)."},
		{Name: VarJobTickMS, Default: "1500",
			Doc: fmt.Sprintf("Milliseconds between two passes of the job engine, each of which looks at every job that has something to do. Integer, %d to %d. Consumed by `internal/app` (`jobengine.Config.Tick`).", minJobTickMS, maxJobTickMS)},
		{Name: VarJobSettleIntervalSec, Default: "10",
			Doc: fmt.Sprintf("Seconds the engine leaves a running job alone between two observations of its module, unless the last observation showed finished items it has not recorded yet or said the job was complete (the next pass then looks again). Integer, 1 to %d. Consumed by `internal/app` (`jobengine.Config.SettleEvery`).", maxJobSettleSec)},
		{Name: VarJobStaleAfterMinM1, Default: "30",
			Doc: fmt.Sprintf("Minutes a running or cancelling module 1 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to %d (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 1).", maxJobStaleMin)},
		{Name: VarJobStaleAfterMinM2, Default: "15",
			Doc: fmt.Sprintf("Minutes a running or cancelling module 2 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to %d (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 2).", maxJobStaleMin)},
		{Name: VarJobStaleAfterMinM3, Default: "15",
			Doc: fmt.Sprintf("Minutes a running or cancelling module 3 job may make no progress (no row handed over, no result recorded) before the engine writes a `stale` event for it, and again at most once per this long. The job is not touched: a stale job stays as it is and is never failed or expired for being slow; an administrator decides. Integer, 1 to %d (one week). Consumed by `internal/app` (`jobengine.Config.StaleAfter`, for module 3).", maxJobStaleMin)},
		{Name: VarJobSubmitBatch, Default: "500",
			Doc: fmt.Sprintf("How many rows one submit call of the engine offers module 2 or module 3 (one run call each; a module that takes fewer is asked for the rest at the next pass). Module 1 takes a job's whole list in one request and ignores this dial. Integer, 1 to %d. Consumed by `internal/app` (`jobengine.Config.SubmitBatch`).", maxJobSubmitBatch)},
		{Name: VarJobSubmitConcurrency, Default: "8",
			Doc: fmt.Sprintf("How many run calls to module 2 or module 3 one submit batch has in flight at once; the first row of a batch always goes alone, and the batch stops at the first answer that says the module is busy or cannot answer. Integer, 1 to %d. Consumed by `internal/app` (`jobengine.RegistryDeps.SubmitConcurrency`).", maxJobSubmitConc)},
		{Name: VarJobStepTimeoutSec, Default: "120",
			Doc: fmt.Sprintf("The most seconds the engine spends on ONE job in one pass: a module that hangs costs a job this much, then the engine goes on to the next job and tries this one again at the next pass. Keep it above `PLATFORM_MODULE_TIMEOUT_MS` (module 1's bulk submit is given at least two minutes). Integer, %d to %d. Consumed by `internal/app` (`jobengine.Config.StepTimeout`).", minJobStepTimeoutSec, maxJobStepTimeoutSec)},
		{Name: VarInstanceID, Default: instanceIDDefault,
			Doc: fmt.Sprintf("Name of this platform instance, 1 to %d printable ASCII characters. With more than one instance running, only the one holding the leader lock runs the expiry and reconcile loops; the name is the application_name of the database session that holds it (pg_stat_activity) and appears in the loop log lines. Each instance needs its own. The default (hostname) means this machine's host name.", maxInstanceIDBytes)},
	}
}

// Config is the resolved configuration. It prints without its secret: see String, GoString, LogValue and
// the Secret type.
type Config struct {
	HTTPAddr          string
	DatabaseURL       Secret
	LogLevel          string // normalised to lower case
	Env               string
	ShutdownTimeout   time.Duration
	ReadyzTimeout     time.Duration
	ReadHeaderTimeout time.Duration

	M3URL                     string
	M3AdminToken              Secret
	EntitlementExpireInterval time.Duration
	M3ReconcileInterval       time.Duration
	M3ReverifyInterval        time.Duration
	M3RequestTimeout          time.Duration

	MoneyLockTimeout time.Duration // how long a money operation waits for the lock of a balance (internal/money WithLockTimeout)

	ModuleTimeout      time.Duration // how long one request to a module may take, per attempt (internal/modclient FromConfig)
	ModuleRetries      int           // extra attempts of an idempotent request to a module (internal/modclient FromConfig)
	ModuleRetryBackoff time.Duration // the first wait between attempts; it doubles (internal/modclient FromConfig)

	M1KeyVersion int    // the version of the module 1 keys derived and presented (internal/tenant WithM1KeyVersion, Scope, ProvisionM1Caller)
	M1KeySecret  Secret // the secret every user's module 1 caller key is derived from; "" means not configured (internal/tenant NewResolver, DeriveM1Key)

	M1VendorReachable bool // whether module 1's vendor fallback is on: what internal/app writes into platform.unit_costs.reachable for m1_vendor (syncReachability); false means a module 1 job reserves 1 credit per row

	M1URL          string // base URL of module 1's control plane; "" means not configured (internal/adapters/m1 FromConfig)
	M2URL          string // base URL of module 2's platform API; "" means not configured (internal/adapters/m2 FromConfig)
	M2ServiceToken Secret // module 2's service token (Domain A bearer); "" means not configured (internal/adapters/m2 FromConfig)
	M3ServiceToken Secret // module 3's Domain A service token, not the admin token M3AdminToken; "" means not configured (internal/adapters/m3 FromConfig)

	JWTSecret   Secret        // the HS256 secret the job API verifies access tokens with; "" means not configured (internal/auth FromConfig)
	JWKSURL     string        // where the asymmetric keys of the access tokens are published; "" means not configured (internal/auth FromConfig)
	JWTIssuer   string        // the iss every access token must carry; "" means not checked (internal/auth FromConfig)
	JWTAudience string        // the aud every access token must include (internal/auth FromConfig)
	JWTLeeway   time.Duration // the clock skew exp, nbf and iat are judged with (internal/auth FromConfig)

	JobMaxRows       int           // the longest list one job holds (internal/app: jobs.WithMaxRows)
	JobMaxStartDelay time.Duration // how long after its start a confirmed or scheduled job waits for the engine before it expires (internal/app: jobs.WithMaxStartDelay)
	ResultsPageMax   int           // the largest page of results the API serves (internal/app: api.New, Deps.ResultsPageMax)

	JobEngineEnabled     bool          // whether this instance runs the job engine (internal/app: builds and runs internal/jobengine, or logs that it does not)
	JobTick              time.Duration // the wait between two passes of the engine (internal/app: jobengine.Config.Tick)
	JobSettleInterval    time.Duration // the least wait between two observations of a running job (internal/app: jobengine.Config.SettleEvery)
	JobStaleAfterM1      time.Duration // how long a module 1 job may make no progress before its stale event (internal/app: jobengine.Config.StaleAfter)
	JobStaleAfterM2      time.Duration // the same for module 2
	JobStaleAfterM3      time.Duration // the same for module 3
	JobSubmitBatch       int           // the rows one submit call offers module 2 or 3 (internal/app: jobengine.Config.SubmitBatch)
	JobSubmitConcurrency int           // the run calls to module 2 or 3 in flight at once in one batch (internal/app: jobengine.RegistryDeps.SubmitConcurrency)
	JobStepTimeout       time.Duration // the bound of one job's step (internal/app: jobengine.Config.StepTimeout)

	InstanceID string // this instance's name; "" means the default, the host name, which the service resolves (internal/entitlements ResolveInstanceID)
}

// Problem is one invalid or missing variable. Reason is a fixed sentence: it never contains a value, so
// an error built from Problems cannot leak one.
type Problem struct {
	Name   string
	Reason string
}

// Error is the error Load returns: every problem found, not just the first.
type Error struct{ Problems []Problem }

func (e *Error) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Name + ": " + p.Reason
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}

// Load resolves the configuration from getenv. It never reads the process environment itself, so tests
// and callers control exactly what it sees. An unset or blank variable takes its default; a required
// variable with no value is a problem. All problems are collected and returned together as *Error, and
// none of them contains a value.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("config: Load needs a getenv function")
	}
	var (
		c        Config
		problems []Problem
	)
	fail := func(name, reason string) { problems = append(problems, Problem{Name: name, Reason: reason}) }
	intIn := func(name, raw string, lo, hi int) (int, bool) {
		n, err := strconv.Atoi(raw)
		if err != nil {
			fail(name, "must be an integer")
			return 0, false
		}
		if n < lo || n > hi {
			fail(name, fmt.Sprintf("must be between %d and %d", lo, hi))
			return 0, false
		}
		return n, true
	}

	for _, v := range Vars() {
		raw := strings.TrimSpace(getenv(v.Name))
		if raw == "" {
			if v.Required {
				fail(v.Name, "is required but not set")
				continue
			}
			raw = v.Default
		}
		switch v.Name {
		case VarHTTPAddr:
			if !validListenAddr(raw) {
				fail(v.Name, "must be a listen address as host:port, for example :8080")
				continue
			}
			c.HTTPAddr = raw
		case VarDatabaseURL:
			c.DatabaseURL = Secret(raw)
		case VarLogLevel:
			if _, err := logging.ParseLevel(raw); err != nil {
				fail(v.Name, err.Error())
				continue
			}
			c.LogLevel = strings.ToLower(raw)
		case VarEnv:
			c.Env = raw
		case VarShutdownTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxTimeoutSec); ok {
				c.ShutdownTimeout = time.Duration(n) * time.Second
			}
		case VarReadyzTimeoutMS:
			if n, ok := intIn(v.Name, raw, 1, maxReadyzMS); ok {
				c.ReadyzTimeout = time.Duration(n) * time.Millisecond
			}
		case VarReadHeaderTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxTimeoutSec); ok {
				c.ReadHeaderTimeout = time.Duration(n) * time.Second
			}
		case VarM3URL:
			if !validModuleURL(raw) {
				fail(v.Name, "must be an http or https URL with a host and no credentials, for example http://m3:8080")
				continue
			}
			c.M3URL = raw
		case VarM3AdminToken:
			if !validBearerToken(raw) {
				fail(v.Name, "must be printable ASCII with no spaces (it is sent as a bearer token)")
				continue
			}
			c.M3AdminToken = Secret(raw)
		case VarEntitlementExpireIntervalSec:
			if n, ok := intIn(v.Name, raw, 1, maxLoopIntervalSec); ok {
				c.EntitlementExpireInterval = time.Duration(n) * time.Second
			}
		case VarM3ReconcileIntervalSec:
			if n, ok := intIn(v.Name, raw, 1, maxLoopIntervalSec); ok {
				c.M3ReconcileInterval = time.Duration(n) * time.Second
			}
		case VarM3ReverifyIntervalSec:
			if n, ok := intIn(v.Name, raw, minReverifySec, maxReverifySec); ok {
				c.M3ReverifyInterval = time.Duration(n) * time.Second
			}
		case VarM3RequestTimeoutSec:
			if n, ok := intIn(v.Name, raw, 1, maxM3RequestSec); ok {
				c.M3RequestTimeout = time.Duration(n) * time.Second
			}
		case VarMoneyLockTimeoutMS:
			if n, ok := intIn(v.Name, raw, minMoneyLockMS, maxMoneyLockMS); ok {
				c.MoneyLockTimeout = time.Duration(n) * time.Millisecond
			}
		case VarModuleTimeoutMS:
			if n, ok := intIn(v.Name, raw, minModuleTimeoutMS, maxModuleTimeoutMS); ok {
				c.ModuleTimeout = time.Duration(n) * time.Millisecond
			}
		case VarModuleRetries:
			if n, ok := intIn(v.Name, raw, 0, maxModuleRetries); ok {
				c.ModuleRetries = n
			}
		case VarModuleRetryBackoffMS:
			if n, ok := intIn(v.Name, raw, 0, maxModuleBackoffMS); ok {
				c.ModuleRetryBackoff = time.Duration(n) * time.Millisecond
			}
		case VarM1KeyVersion:
			if n, ok := intIn(v.Name, raw, MinM1KeyVersion, MaxM1KeyVersion); ok {
				c.M1KeyVersion = n
			}
		case VarM1KeySecret:
			if raw != v.Default { // the default stands for "not configured", which leaves the field empty (see unsetMarker)
				if len(raw) < MinM1KeySecretBytes {
					fail(v.Name, fmt.Sprintf("must be at least %d bytes", MinM1KeySecretBytes))
					continue
				}
				c.M1KeySecret = Secret(raw)
			}
		case VarM1VendorReachable:
			switch raw { // exactly these two spellings: "TRUE", "1", "yes" and "on" are refused, because a switch that silently reads as off is a customer reserved at the wrong rate
			case "true":
				c.M1VendorReachable = true
			case "false":
				c.M1VendorReachable = false
			default:
				fail(v.Name, "must be true or false")
			}
		case VarM1URL:
			if raw != v.Default { // the default stands for "not configured", which leaves the field empty (see unsetMarker)
				if !validModuleURL(raw) {
					fail(v.Name, "must be an http or https URL with a host and no credentials, for example http://m1:8080")
					continue
				}
				c.M1URL = raw
			}
		case VarM2URL:
			if raw != v.Default {
				if !validModuleURL(raw) {
					fail(v.Name, "must be an http or https URL with a host and no credentials, for example http://m2:8080")
					continue
				}
				c.M2URL = raw
			}
		case VarM2ServiceToken:
			if raw != v.Default {
				if !validBearerToken(raw) {
					fail(v.Name, "must be printable ASCII with no spaces (it is sent as a bearer token)")
					continue
				}
				c.M2ServiceToken = Secret(raw)
			}
		case VarM3ServiceToken:
			if raw != v.Default {
				if !validBearerToken(raw) {
					fail(v.Name, "must be printable ASCII with no spaces (it is sent as a bearer token)")
					continue
				}
				c.M3ServiceToken = Secret(raw)
			}
		case VarJWTSecret:
			if raw != v.Default { // the default stands for "not configured", which leaves the field empty (see unsetMarker)
				if len(raw) < MinJWTSecretBytes {
					fail(v.Name, fmt.Sprintf("must be at least %d bytes", MinJWTSecretBytes))
					continue
				}
				if !validClaimText(raw, len(raw)) {
					fail(v.Name, "must be printable ASCII with no spaces (the secret is used as the raw bytes of the string)")
					continue
				}
				c.JWTSecret = Secret(raw)
			}
		case VarJWKSURL:
			if raw != v.Default {
				if !validModuleURL(raw) {
					fail(v.Name, "must be an http or https URL with a host and no credentials, query or fragment, for example the project's /auth/v1/.well-known/jwks.json")
					continue
				}
				c.JWKSURL = raw
			}
		case VarJWTIssuer:
			if raw != v.Default {
				if !validClaimText(raw, maxJWTClaimText) {
					fail(v.Name, fmt.Sprintf("must be printable ASCII with no spaces, at most %d bytes", maxJWTClaimText))
					continue
				}
				c.JWTIssuer = raw
			}
		case VarJWTAudience:
			if !validClaimText(raw, maxJWTClaimText) {
				fail(v.Name, fmt.Sprintf("must be printable ASCII with no spaces, at most %d bytes", maxJWTClaimText))
				continue
			}
			c.JWTAudience = raw
		case VarJWTLeewaySec:
			if n, ok := intIn(v.Name, raw, 0, maxJWTLeewaySec); ok {
				c.JWTLeeway = time.Duration(n) * time.Second
			}
		case VarJobMaxRows:
			if n, ok := intIn(v.Name, raw, 1, maxJobRows); ok {
				c.JobMaxRows = n
			}
		case VarJobMaxStartDelayMin:
			if n, ok := intIn(v.Name, raw, 1, maxJobStartDelayMin); ok {
				c.JobMaxStartDelay = time.Duration(n) * time.Minute
			}
		case VarResultsPageMax:
			if n, ok := intIn(v.Name, raw, 1, maxResultsPage); ok {
				c.ResultsPageMax = n
			}
		case VarJobEngineEnabled:
			switch raw { // exactly these two spellings: "TRUE", "1", "yes" and "off" are refused, because a switch that silently reads the other way is a platform whose jobs never move, or an engine nobody meant to run
			case "true":
				c.JobEngineEnabled = true
			case "false":
				c.JobEngineEnabled = false
			default:
				fail(v.Name, "must be true or false")
			}
		case VarJobTickMS:
			if n, ok := intIn(v.Name, raw, minJobTickMS, maxJobTickMS); ok {
				c.JobTick = time.Duration(n) * time.Millisecond
			}
		case VarJobSettleIntervalSec:
			if n, ok := intIn(v.Name, raw, 1, maxJobSettleSec); ok {
				c.JobSettleInterval = time.Duration(n) * time.Second
			}
		case VarJobStaleAfterMinM1:
			if n, ok := intIn(v.Name, raw, 1, maxJobStaleMin); ok {
				c.JobStaleAfterM1 = time.Duration(n) * time.Minute
			}
		case VarJobStaleAfterMinM2:
			if n, ok := intIn(v.Name, raw, 1, maxJobStaleMin); ok {
				c.JobStaleAfterM2 = time.Duration(n) * time.Minute
			}
		case VarJobStaleAfterMinM3:
			if n, ok := intIn(v.Name, raw, 1, maxJobStaleMin); ok {
				c.JobStaleAfterM3 = time.Duration(n) * time.Minute
			}
		case VarJobSubmitBatch:
			if n, ok := intIn(v.Name, raw, 1, maxJobSubmitBatch); ok {
				c.JobSubmitBatch = n
			}
		case VarJobSubmitConcurrency:
			if n, ok := intIn(v.Name, raw, 1, maxJobSubmitConc); ok {
				c.JobSubmitConcurrency = n
			}
		case VarJobStepTimeoutSec:
			if n, ok := intIn(v.Name, raw, minJobStepTimeoutSec, maxJobStepTimeoutSec); ok {
				c.JobStepTimeout = time.Duration(n) * time.Second
			}
		case VarInstanceID:
			if raw != v.Default { // the default stands for the host name, which Load does not read (see instanceIDDefault)
				if !validInstanceID(raw) {
					fail(v.Name, fmt.Sprintf("must be 1 to %d printable ASCII characters (it is the application_name of a database session)", maxInstanceIDBytes))
					continue
				}
				c.InstanceID = raw
			}
		default:
			// A Var that Load has no case for would be read and then dropped: a dial with no consumer.
			fail(v.Name, "is declared in Vars() but Load does not consume it (programming error)")
		}
	}
	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	return c, nil
}

// validListenAddr accepts host:port with a numeric port in 0..65535 (":8080", "127.0.0.1:0").
func validListenAddr(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n <= 65535
}

// validModuleURL accepts an absolute http or https URL with a host, no credentials, no query and no fragment
// (the base URL a module client appends its routes to: PLATFORM_M1_URL, PLATFORM_M2_URL and PLATFORM_M3_URL
// share this one rule). A URL with credentials is refused rather than accepted: these variables are not
// secrets, so a credential in one would be printed.
func validModuleURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Hostname() != ""
}

// validInstanceID accepts 1 to maxInstanceIDBytes bytes, each printable ASCII (0x20 to 0x7e): what PostgreSQL keeps of an application_name as it was written. The same rule is written out in
// internal/entitlements (ValidInstanceID) for callers that do not come through Load; a test in each pins the same cases.
func validInstanceID(s string) bool {
	if len(s) < 1 || len(s) > maxInstanceIDBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validClaimText accepts 1 to limit bytes of printable ASCII without spaces: what an issuer, an audience and an HMAC secret are (internal/auth applies the same rule to the first two). A
// stray line break or a pasted non-ASCII character would make the value differ from the one the issuer signs and names, and fails at start-up with the variable named.
func validClaimText(s string, limit int) bool {
	if len(s) < 1 || len(s) > limit {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// validBearerToken accepts printable ASCII without spaces: the characters an HTTP bearer credential can carry
// (module 3's client refuses anything else, so a stray newline or a pasted non-ASCII character fails at
// start-up with the variable named, not at the first request).
func validBearerToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return s != ""
}

// secretState is what a Config says about an optional secret (the module 1 key secret, the module 2 and 3 service tokens): the redaction marker when it is set, "(unset)" when it is not.
// Either way, never the value.
func secretState(s Secret) string {
	if s == "" {
		return unsetMarker
	}
	return s.String()
}

// urlState is what a Config says about an optional base URL: the URL when it is set (it carries no credentials, see validModuleURL), "(unset)" when it is not.
func urlState(u string) string {
	if u == "" {
		return unsetMarker
	}
	return u
}

// attrs is the one place Config is rendered; String, GoString and LogValue all derive from it, so they
// cannot disagree about what is shown. The database URL appears only as the redaction marker.
func (c Config) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", c.DatabaseURL.String()),
		slog.String("log_level", c.LogLevel),
		slog.String("env", c.Env),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Duration("readyz_timeout", c.ReadyzTimeout),
		slog.Duration("read_header_timeout", c.ReadHeaderTimeout),
		slog.String("m3_url", c.M3URL),
		slog.String("m3_admin_token", c.M3AdminToken.String()),
		slog.Duration("entitlement_expire_interval", c.EntitlementExpireInterval),
		slog.Duration("m3_reconcile_interval", c.M3ReconcileInterval),
		slog.Duration("m3_reverify_interval", c.M3ReverifyInterval),
		slog.Duration("m3_request_timeout", c.M3RequestTimeout),
		slog.Duration("money_lock_timeout", c.MoneyLockTimeout),
		slog.Duration("module_timeout", c.ModuleTimeout),
		slog.Int("module_retries", c.ModuleRetries),
		slog.Duration("module_retry_backoff", c.ModuleRetryBackoff),
		slog.String("m1_key_secret", secretState(c.M1KeySecret)),
		slog.Int("m1_key_version", c.M1KeyVersion),
		slog.Bool("m1_vendor_reachable", c.M1VendorReachable),
		slog.String("m1_url", urlState(c.M1URL)),
		slog.String("m2_url", urlState(c.M2URL)),
		slog.String("m2_service_token", secretState(c.M2ServiceToken)),
		slog.String("m3_service_token", secretState(c.M3ServiceToken)),
		slog.String("jwt_secret", secretState(c.JWTSecret)),
		slog.String("jwks_url", urlState(c.JWKSURL)),
		slog.String("jwt_issuer", urlState(c.JWTIssuer)), // the issuer is a URL in practice and holds no credential: it is not a secret
		slog.String("jwt_audience", c.JWTAudience),
		slog.Duration("jwt_leeway", c.JWTLeeway),
		slog.Int("job_max_rows", c.JobMaxRows),
		slog.Duration("job_max_start_delay", c.JobMaxStartDelay),
		slog.Int("results_page_max", c.ResultsPageMax),
		slog.Bool("job_engine_enabled", c.JobEngineEnabled),
		slog.Duration("job_tick", c.JobTick),
		slog.Duration("job_settle_interval", c.JobSettleInterval),
		slog.Duration("job_stale_after_m1", c.JobStaleAfterM1),
		slog.Duration("job_stale_after_m2", c.JobStaleAfterM2),
		slog.Duration("job_stale_after_m3", c.JobStaleAfterM3),
		slog.Int("job_submit_batch", c.JobSubmitBatch),
		slog.Int("job_submit_concurrency", c.JobSubmitConcurrency),
		slog.Duration("job_step_timeout", c.JobStepTimeout),
		slog.String("instance_id", c.InstanceID),
	}
}

// LogValue implements slog.LogValuer, so logging a Config (or a pointer to one) can never reveal the URL.
func (c Config) LogValue() slog.Value { return slog.GroupValue(c.attrs()...) }

// String renders the Config without its secret.
func (c Config) String() string {
	attrs := c.attrs()
	parts := make([]string, len(attrs))
	for i, a := range attrs {
		parts[i] = a.Key + "=" + a.Value.String()
	}
	return "config{" + strings.Join(parts, " ") + "}"
}

// GoString is what %#v prints; it is the same redacted rendering.
func (c Config) GoString() string { return c.String() }
