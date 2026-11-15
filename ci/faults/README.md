# Fault proofs

A **fault proof** shows that a gate can go red, and that it goes red for the reason it names. A gate that has never been seen red is not known to work.

Phase 0 did this with one throwaway git branch per gate. From Phase 1 a fault is a **patch** applied to a scratch copy, so no fault ever enters the history that gitleaks scans:

| File | What |
|---|---|
| `ci/faults/<phase>/<gate>.patch` | the fault: `git diff --binary` output, paths relative to the root of the thing patched (the platform, or one module) |
| `ci/faults/<phase>/<gate>.expect` | the gate command, the setup it needs, and the message it must print |
| `ci/faults/run-leg.sh <phase> <gate>` | runs one leg (what the `fault-proofs` workflow runs, one matrix leg per `.expect`) |
| `ci/faults/run-leg-selftest.sh` | proves the runner fails, for the reason it names, on every way a leg can be wrong (workflow `gates`) |
| `scripts/check-fault-patches.sh` | the always-on gate against patch rot: every `.expect` parses, every platform patch still applies to `HEAD` (workflow `gates`) |
| `ci/faults-archive/` | the Phase 0 diffs: a record, never run, never looked at by any gate |

## What a leg proves

A leg passes **only if all four hold**; anything else fails with a sentence naming the one that did not.

1. The gate command exits 0 on an **unpatched** pristine copy (the paired control).
2. The patch applies to a pristine copy.
3. The setup steps still succeed on the **patched** copy. If the fault broke the setup, a red would be for the wrong reason, so the leg fails and says so.
4. The gate command exits **non-zero** on the patched copy and its output contains **every** `MESSAGE` line (fixed-string match, against the log the step actually produced, captured with `tee` under `pipefail`).

The last line of a run is `FAULT-PROOF PASS gate=<g> step=<s> exit=<rc> messages=<n>` or `FAULT-PROOF FAIL gate=<g>: <the condition>`. Exit status: 0 pass, 1 fail, 2 usage error.

## The `.expect` format

One `KEY=value` per line. The value is the raw rest of the line (no trimming, no quoting, no expansion). Blank lines and lines whose first non-blank character is `#` are ignored; a trailing CR is stripped. Keys are UPPERCASE. The file is read line by line and **never sourced or eval'ed**. An unknown key is an error.

| Key | Required | Meaning |
|---|---|---|
| `GATE` | yes | must equal the file basename (`foo.expect` has `GATE=foo`); lowercase letters, digits, `_` and `-`, starting with a letter or digit (the phase directory name follows the same rule) |
| `TARGET` | yes | `platform`, or `module:<name>` with `<name>` one of `coldmail-verify`, `coldmail-send-managed`, `coldmail-send-byo` |
| `SETUP` | no | space-separated tokens, run in this order on each side (see below); may be empty |
| `STEP` | yes | a label for the gate step, printed in the banners and in the final PASS line |
| `CMD` | yes | the gate command, run with `bash -c` in `FAULT_ROOT` |
| `MESSAGE` | at least one | a fixed string that must appear in the output of the failing step; repeatable, ALL must appear; an empty `MESSAGE` is an error (it would match anything) |

For `TARGET=platform` the patch is applied to the platform copy. For `TARGET=module:<name>` it is applied to that module's copy, and its paths are relative to **that module's root**.

### Worked example (an illustration: this fault is not stored under `ci/faults/`)

The digest-pin gate must go red when a base image loses its digest.

`ci/faults/p1/image-latest.patch` (made with `git diff --binary` after editing the Dockerfile in a scratch checkout):

```
diff --git a/Dockerfile b/Dockerfile
--- a/Dockerfile
+++ b/Dockerfile
@@ -13 +13 @@
-FROM alpine:3.24@sha256:<digest>
+FROM alpine:latest
```

`ci/faults/p1/image-latest.expect`:

```
# the digest-pin gate must go red when a base image is unpinned
GATE=image-latest
TARGET=platform
SETUP=
STEP=check-image-pins on the real repository
CMD=bash scripts/check-image-pins.sh --min-refs 5 .
MESSAGE=tag is :latest
```

The leg runs the command on the pristine copy (must exit 0), applies the patch to the other copy, runs the command there (must exit non-zero and print `tag is :latest`), and prints
`FAULT-PROOF PASS gate=image-latest step=check-image-pins on the real repository exit=1 messages=1`.

## What the runner exports

To `SETUP` and `CMD`, per side (the control side is the unpatched copy, the fault side the patched one):

| Variable | Value |
|---|---|
| `FAULT_ROOT` | the directory being tested and the working directory of `CMD`: the platform copy for `TARGET=platform`, the module copy for a module target |
| `FAULT_PLATFORM` | the platform copy of THIS side (patched on the fault side when `TARGET=platform`) |
| `FAULT_MODULES` | a directory holding all three modules at the pin, with the faulted one replaced by the patched copy on the fault side. Provided only when `TARGET` is a module or a `SETUP` token needs the modules (`m1-chain`, `modules`); otherwise it is **unset** |
| `FAULT_ADMIN_DSN` | from `FAULT_PG_ADMIN_DSN`, default `postgres://postgres:ci@localhost:5432/postgres` |
| `FAULT_DSN` | a fresh database the runner creates per side, `fp_<gate>_<ctl|flt>_<random>`, dropped at the end. Provided only when a `SETUP` token needs a database (`platform-up`, `m1-chain`, `db`); otherwise it is **unset**, so a `CMD` that reads it under `set -u` fails loudly |

The runner reads these inputs: `FAULT_REPO_ROOT` (the repository to test; default the one the script lives in), `FAULT_MODULES_DIR` (a directory holding the three module checkouts, used as given: their commits are not compared with `tools/modules.lock`), `FAULT_PG_ADMIN_DSN`, and, when `FAULT_MODULES_DIR` is not set, `DEPLOY_KEY_COLDMAIL_VERIFY`, `DEPLOY_KEY_COLDMAIL_SEND_MANAGED`, `DEPLOY_KEY_COLDMAIL_SEND_BYO` (the modules are then fetched with `tools/fetch-modules.sh --lock tools/modules.lock`; a missing key fails the leg and names it).

### Setup tokens

A token is a function in `run-leg.sh`; an unknown token fails the leg (and `check-fault-patches`). Each runs once per side, in the order written.

| Token | What it does |
|---|---|
| `platform-up` | builds `./cmd/platform-migrate` from `FAULT_PLATFORM` (`CGO_ENABLED=0 GOTOOLCHAIN=local go build`) and runs `up` against `FAULT_DSN` with `PLATFORM_MIGRATIONS_DIR=$FAULT_PLATFORM/db/migrations` |
| `m1-chain` | applies every `$FAULT_MODULES/coldmail-verify/control-plane/migrations/NNNN_*.sql` to `FAULT_DSN` in sorted order with `psql -v ON_ERROR_STOP=1`; finding zero files fails |
| `db` | only asks for a fresh `FAULT_DSN` (for a `CMD` that brings its own schema) |
| `modules` | only asks for `FAULT_MODULES` (for a platform-target `CMD` that reads module files) |

The three Postgres tokens create the database on the cluster behind `FAULT_ADMIN_DSN`; roles are cluster-wide, so a migration that creates a role must tolerate it already existing (the platform bootstrap does).

## A fault that needs a token-shaped string

**Never store one**: a stored token-shaped string trips gitleaks, which scans the full history of every branch. The leg generates it. The pattern: the patch adds a harmless marker file, and `CMD` writes the string at run time only when the marker exists, building it from fragments or from `openssl rand`, then runs the scanner. For example (`CMD` is one line):

```
CMD=if [ -f .plant-secret ]; then printf 'api_key = "%s"\n' "$(openssl rand -hex 20)" > planted.txt; fi; <the scanner command>
```

The control side has no marker, so nothing is planted and the scanner must pass; the fault side plants it and the scanner must fail with its message. Check the scanner's own rule before relying on the shape of the string.

## Adding a fault

1. Make the gate fail by hand in a scratch checkout of the repository (or of the module) and save it: `git diff --binary > ci/faults/<phase>/<gate>.patch`. Patch only what the gate reads; keep it small enough that it will still apply after unrelated edits.
2. Write `ci/faults/<phase>/<gate>.expect`. Choose a `MESSAGE` that only the intended failure prints: a `MESSAGE` that any red would print proves nothing.
3. Run `bash ci/faults/run-leg.sh <phase> <gate>` locally (below), then `bash scripts/check-fault-patches.sh`.
4. Commit. The `gates` workflow now keeps the patch applying; dispatch `fault-proofs` (input `phase`) to run every leg on the runner, and record the run URL in the phase spec.

Only the runner counts: a local run is rehearsal.

## Running a leg locally

You need `bash`, `git`, `tar`, and `psql` (when a token needs a database); `go` for `platform-up`. The copies are `git archive HEAD`, so **commit the gate or script changes first**: uncommitted changes are not in the copies (the `.patch` and `.expect` themselves are read from the working tree).

```
IMG="$(grep -o 'postgres:16-alpine@sha256:[0-9a-f]*' .github/workflows/fault-proofs.yml | head -n 1)"
docker run --rm -d --name fp-pg -e POSTGRES_PASSWORD=ci -p 5432:5432 "$IMG"
docker exec fp-pg pg_isready -U postgres        # repeat until it says "accepting connections"

export FAULT_PG_ADMIN_DSN=postgres://postgres:ci@localhost:5432/postgres   # the default; shown for clarity
export FAULT_MODULES_DIR=/path/to/dir-with-the-three-module-checkouts       # only for legs that need the modules
bash ci/faults/run-leg.sh p1 <gate>

docker stop fp-pg
```

A leg with an empty `SETUP` needs no Postgres. To check one `.expect` file without running anything: `bash ci/faults/run-leg.sh --check-expect ci/faults/<phase>/<gate>.expect` (this is the parser `check-fault-patches.sh` uses).

## What this does not check

That a fault turns its gate red is proven only by a dispatch of `fault-proofs`; `check-fault-patches` proves only that each platform patch still applies and each `.expect` parses. Module patches cannot be applied outside a leg (no module checkout) and are listed as NOT CHECKED. A `MESSAGE` may match any line of the gate step's output, not a particular one.
