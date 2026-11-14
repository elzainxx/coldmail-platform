#!/usr/bin/env bash
# fetch-modules.sh <dest-directory> — clone the three PRIVATE module repositories into <dest> over SSH, each with its own READ-ONLY deploy key.
#
# Why deploy keys: a workflow's default token cannot read a sibling repository, and a personal access token would carry the owner's whole account. A deploy key belongs to
# exactly ONE repository and is read-only unless it was created with --allow-write, so the blast radius of a leaked key is one repository, read-only. There are three,
# one per module repository, created with `gh repo deploy-key add` (no --allow-write); each private half is an Actions secret on THIS repository:
#
#   coldmail-verify        <- DEPLOY_KEY_COLDMAIL_VERIFY
#   coldmail-send-managed  <- DEPLOY_KEY_COLDMAIL_SEND_MANAGED
#   coldmail-send-byo      <- DEPLOY_KEY_COLDMAIL_SEND_BYO
#
# The key text arrives in the environment under those names. THERE IS NO SKIP PATH: a key that is missing, empty, not a private key, unknown to GitHub, revoked, or
# registered on a different repository makes this script exit non-zero and say which repository and why. A check that silently did not run is a false green, so the
# caller (platform-check.yml) has no conditional around this step either. Key material is written only to a mode-600 file in a private temp directory that is removed on
# every exit, and is never echoed.
#
# Host authenticity: GitHub's published ed25519 host key is pinned below and StrictHostKeyChecking is on, so nothing is trusted on first use. If GitHub ever rotates its
# host key this script fails closed ("Host key verification failed"); the new value is at https://api.github.com/meta (.ssh_keys) — compare its fingerprint with
# https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints before updating the line.
set -u
DEST="${1:?usage: fetch-modules.sh <dest-directory>}"
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

for pair in coldmail-verify:DEPLOY_KEY_COLDMAIL_VERIFY coldmail-send-managed:DEPLOY_KEY_COLDMAIL_SEND_MANAGED coldmail-send-byo:DEPLOY_KEY_COLDMAIL_SEND_BYO; do
  repo="${pair%%:*}"; var="${pair##*:}"; key="${!var:-}"
  if [ -z "$key" ]; then
    oops "$repo: the secret $var is missing or empty — a skipped checkout is a false green, so this is red"
    continue
  fi
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
  # Anything else — success (a writable key!), "Repository not found", a timeout — is red, because the property is then unproven. FETCH_PROBE_REPO exists only so the
  # workflow's control can aim the probe at a repository the key cannot see and prove this assertion goes red on any answer other than the read-only refusal.
  probe="$(git -C "$DEST/$repo" push --dry-run "git@github.com:${OWNER}/${FETCH_PROBE_REPO:-$repo}.git" HEAD:refs/heads/zz-platform-ci-write-probe 2>&1)"
  case "$probe" in
    *"marked as read only"*) ;;
    *) oops "$repo: could not confirm the deploy key is read-only (expected GitHub to refuse a write with 'marked as read only'; got: $(printf '%s' "$probe" | tr '\n' ' ' | cut -c1-140))"; rm -rf "${DEST:?}/$repo"; continue;;
  esac
  # the cited commits must all be present: every branch came with the clone; fetch tags too (a tag can point at history no branch holds)
  git -C "$DEST/$repo" fetch -q --tags origin 2>/dev/null || oops "$repo: cloned, but fetching tags failed"
  if [ "$(git -C "$DEST/$repo" rev-parse --is-shallow-repository)" != "false" ]; then oops "$repo: the clone is shallow — check 4 would miss commits"; continue; fi
  echo "  ok: $repo @ $(git -C "$DEST/$repo" rev-parse --short HEAD) ($(git -C "$DEST/$repo" rev-list --count --all) commits; deploy key $var confirmed read-only)"
done
unset GIT_SSH_COMMAND
[ "$fail" -eq 0 ] || { echo "fetch-modules: $fail repositor$([ "$fail" -eq 1 ] && echo y || echo ies) could not be fetched — RED"; exit 1; }
echo "fetch-modules: all three module repositories fetched"
