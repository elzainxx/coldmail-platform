#!/usr/bin/env bash
# fetch-modules.sh <dest-directory> — clone the three module repositories into <dest>: over SSH with each repository's own READ-ONLY deploy key when that key is supplied, and
# ANONYMOUSLY over https when it is not (the module repositories are public once the owner publishes them, and then no key is needed to read them).
#
# Why deploy keys (while a module repository is private): a workflow's default token cannot read a sibling repository, and a personal access token would carry the owner's whole
# account. A deploy key belongs to exactly ONE repository and is read-only unless it was created with --allow-write, so the blast radius of a leaked key is one repository,
# read-only. There are three, one per module repository, created with `gh repo deploy-key add` (no --allow-write); each private half is an Actions secret on THIS repository:
#
#   coldmail-verify        <- DEPLOY_KEY_COLDMAIL_VERIFY
#   coldmail-send-managed  <- DEPLOY_KEY_COLDMAIL_SEND_MANAGED
#   coldmail-send-byo      <- DEPLOY_KEY_COLDMAIL_SEND_BYO
#
# The key text arrives in the environment under those names. THERE IS NO SKIP PATH, and exactly one of two routes is taken per repository:
#   * the variable is NON-EMPTY: the deploy-key route, exactly as it always was. A key that is not a private key, unknown to GitHub, revoked, registered on a different repository, or
#     not provably read-only makes this script exit non-zero and say which repository and why. A supplied key that does not work is NEVER replaced by the anonymous route (that would turn
#     a revoked key into a green).
#   * the variable is EMPTY or unset: the repository is cloned from ${FETCH_ANON_BASE:-https://github.com}/<owner>/<repo>.git with no credentials at all (credential helpers off, terminal
#     prompts off). That is a real clone, held to every assertion below that does not concern a key (not shallow, tags fetched, HEAD == the pin under --lock); if it fails (the repository
#     is still private, or gone, or there is no network) this script exits non-zero and says so. FETCH_ANON_BASE exists so a selftest and a workflow control can point the route somewhere
#     that cannot answer.
# A check that silently did not run is a false green, so the caller (platform-check.yml) has no conditional around this step either. Key material is written only to a mode-600
# file in a private temp directory that is removed on every exit, and is never echoed.
#
# Host authenticity: GitHub's published ed25519 host key is pinned below and StrictHostKeyChecking is on, so nothing is trusted on first use. If GitHub ever rotates its
# host key this script fails closed ("Host key verification failed"); the new value is at https://api.github.com/meta (.ssh_keys) — compare its fingerprint with
# https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints before updating the line.
set -u
# --lock FILE: after cloning, check each module out at the exact commit FILE pins (tools/modules.lock) and assert HEAD == the pin. Without --lock the clones are left at
# the default branch's head, which is what platform-check (documentation citations) wants.
LOCK=""; DEST=""
while [ $# -gt 0 ]; do
  case "$1" in
    --lock) LOCK="${2:?--lock needs a file}"; shift 2;;
    -*) echo "fetch-modules: unknown option '$1'" >&2; exit 2;;
    *) DEST="$1"; shift;;
  esac
done
[ -n "$DEST" ] || { echo "usage: fetch-modules.sh [--lock FILE] <dest-directory>" >&2; exit 2; }
OWNER="${FETCH_OWNER:-${GITHUB_REPOSITORY_OWNER:-elzainxx}}"
# SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU  (GitHub, ed25519; same value from api.github.com/meta and ssh-keyscan on 2026-11-14)
GITHUB_HOST_KEY='github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl'

KD="$(mktemp -d)" || { echo "fetch-modules: cannot create a temp directory" >&2; exit 2; }
trap 'rm -rf "$KD"' EXIT
chmod 700 "$KD"
printf '%s\n' "$GITHUB_HOST_KEY" > "$KD/known_hosts"
mkdir -p "$DEST" || exit 2

fail=0
oops() { echo "  FAIL: $*"; echo "::error title=module checkout failed::$*"; fail=$((fail+1)); }

# The lock is read ONCE, here, and validated as a whole: exactly one 40-hex entry for each of the three repositories and nothing else. A stale or duplicated entry is
# red because a lock that names a module nobody builds (or two commits for one) says something false about what was tested.
if [ -n "$LOCK" ]; then
  [ -f "$LOCK" ] || { oops "the lock file $LOCK does not exist"; echo "fetch-modules: RED"; exit 1; }
  for repo in coldmail-verify coldmail-send-managed coldmail-send-byo; do
    n=$(tr -d '\r' < "$LOCK" | awk -v r="$repo" '/^[[:space:]]*#/ || NF==0 {next} $1==r {c++} END{print c+0}')
    [ "$n" = 1 ] || oops "$LOCK has $n entries for $repo, want exactly 1"
  done
  others=$(tr -d '\r' < "$LOCK" | awk '/^[[:space:]]*#/ || NF==0 {next} $1!="coldmail-verify" && $1!="coldmail-send-managed" && $1!="coldmail-send-byo" {print $1}')
  [ -z "$others" ] || oops "$LOCK names a repository that is not one of the three modules: $(printf '%s' "$others" | tr '\n' ' ')"
  bad=$(tr -d '\r' < "$LOCK" | awk '/^[[:space:]]*#/ || NF==0 {next} $2 !~ /^[0-9a-f]{40}$/ {print $1}')
  [ -z "$bad" ] || oops "$LOCK: the pin for $(printf '%s' "$bad" | tr '\n' ' ') is not a full 40-hex commit id"
  [ "$fail" -eq 0 ] || { echo "fetch-modules: the lock file is unusable — RED"; exit 1; }
fi

GITOPT=()   # extra git options for the route in use: none on the deploy-key route; credentials off on the anonymous one
export GIT_TERMINAL_PROMPT=0   # never wait for a username on a runner or a workstation: an unreadable repository fails at once
for pair in coldmail-verify:DEPLOY_KEY_COLDMAIL_VERIFY coldmail-send-managed:DEPLOY_KEY_COLDMAIL_SEND_MANAGED coldmail-send-byo:DEPLOY_KEY_COLDMAIL_SEND_BYO; do
  repo="${pair%%:*}"; var="${pair##*:}"; key="${!var:-}"
  if [ -z "$key" ]; then
    # ANONYMOUS ROUTE: no deploy key was supplied, so the repository must be readable by anyone (public). `credential.helper=` empties the helper list, so not even a logged-in
    # workstation's stored credentials are used: what this proves is "anyone can clone it".
    unset GIT_SSH_COMMAND; GITOPT=(-c credential.helper=)
    rm -rf "${DEST:?}/$repo"
    if ! out="$(git ${GITOPT[@]+"${GITOPT[@]}"} clone -q "${FETCH_ANON_BASE:-https://github.com}/${OWNER}/${repo}.git" "$DEST/$repo" 2>&1)"; then
      oops "$repo: no deploy key ($var is empty) and could not clone anonymously from ${FETCH_ANON_BASE:-https://github.com}/${OWNER}/${repo}.git — the repository is still private or is gone, or there is no network ($(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160))"
      rm -rf "${DEST:?}/$repo"
      continue
    fi
    route="anonymous clone, no deploy key ($var empty)"
  else
    GITOPT=()
    kf="$KD/$repo.key"; (umask 077; printf '%s\n' "$key" > "$kf"); chmod 600 "$kf"
    if ! ssh-keygen -y -f "$kf" >/dev/null 2>&1; then
      oops "$repo: the value of $var is not a usable private key"
      continue
    fi
    export GIT_SSH_COMMAND="ssh -F /dev/null -i $kf -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$KD/known_hosts"
    rm -rf "${DEST:?}/$repo"
    if ! out="$(git clone -q "git@github.com:${OWNER}/${repo}.git" "$DEST/$repo" 2>&1)"; then
      oops "$repo: could not clone over SSH with $var — wrong, revoked, or another repository's deploy key, or the repository is gone ($(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160))"
      rm -rf "${DEST:?}/$repo"
      continue
    fi
    # The key must be READ-ONLY. `push --dry-run` asks GitHub whether this key may write, and sends nothing: a read-only deploy key is refused with "marked as read only".
    # Anything else — success (a writable key!), "Repository not found", a refused connection — is red, because the property is then unproven. FETCH_PROBE_URL exists only so the
    # workflow's control can point the probe somewhere that cannot answer "marked as read only" and prove this assertion goes red on every other answer. (GitHub refuses a
    # read-only key's WRITE before it looks the repository up, so aiming the probe at a different GitHub repository does NOT change the answer — the first version of the
    # control did exactly that and the runner showed it proved nothing.)
    probe="$(git -C "$DEST/$repo" push --dry-run "${FETCH_PROBE_URL:-git@github.com:${OWNER}/${repo}.git}" HEAD:refs/heads/zz-platform-ci-write-probe 2>&1)"
    case "$probe" in
      *"marked as read only"*) ;;
      *) oops "$repo: could not confirm the deploy key is read-only (expected GitHub to refuse a write with 'marked as read only'; got: $(printf '%s' "$probe" | tr '\n' ' ' | cut -c1-140))"; rm -rf "${DEST:?}/$repo"; continue;;
    esac
    route="deploy key $var confirmed read-only"
  fi
  # the cited commits must all be present: every branch came with the clone; fetch tags too (a tag can point at history no branch holds)
  git ${GITOPT[@]+"${GITOPT[@]}"} -C "$DEST/$repo" fetch -q --tags origin 2>/dev/null || oops "$repo: cloned, but fetching tags failed"
  if [ "$(git -C "$DEST/$repo" rev-parse --is-shallow-repository)" != "false" ]; then oops "$repo: the clone is shallow — check 4 would miss commits"; continue; fi
  pinned=""
  if [ -n "$LOCK" ]; then
    pin="$(tr -d '\r' < "$LOCK" | awk -v r="$repo" '/^[[:space:]]*#/ || NF==0 {next} $1==r {print $2}')"
    if ! git -C "$DEST/$repo" cat-file -e "$pin^{commit}" 2>/dev/null; then oops "$repo: the pinned commit $pin does not exist in the repository"; continue; fi
    if ! git -C "$DEST/$repo" merge-base --is-ancestor "$pin" origin/HEAD 2>/dev/null; then oops "$repo: the pinned commit $pin is not reachable from the default branch"; continue; fi
    git -C "$DEST/$repo" -c advice.detachedHead=false checkout -q --detach "$pin" 2>/dev/null || { oops "$repo: could not check out the pinned commit $pin"; continue; }
    [ "$(git -C "$DEST/$repo" rev-parse HEAD)" = "$pin" ] || { oops "$repo: after checkout HEAD is not the pinned commit $pin"; continue; }
    pinned="; PINNED at $pin == HEAD"
  fi
  echo "  ok: $repo @ $(git -C "$DEST/$repo" rev-parse --short HEAD) ($(git -C "$DEST/$repo" rev-list --count --all) commits; $route$pinned)"
done
unset GIT_SSH_COMMAND
[ "$fail" -eq 0 ] || { echo "fetch-modules: $fail repositor$([ "$fail" -eq 1 ] && echo y || echo ies) could not be fetched — RED"; exit 1; }
echo "fetch-modules: all three module repositories fetched"
