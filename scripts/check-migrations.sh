#!/usr/bin/env bash
# check-migrations.sh [--dir DIR] [--lifecycle-test FILE] — BLOCKING gate on the migration convention (ported from module 2's
# migration_lint.sh, with the one change that matters: THIS gate is red when it finds no migrations, where the original printed "OK").
#
#   - every migration is platform_NNNN_<desc>.up.sql with a matching .down.sql; any other .sql file in the directory is red
#   - versions are contiguous, ascending, and start at 0000 (no gap, no duplicate)
#   - every UP self-records:   INSERT INTO platform.schema_migrations ... 'NNNN'
#   - every DOWN self-removes: DELETE FROM platform.schema_migrations ... 'NNNN'  (or says, in a comment, that it is a forward-only no-op)
#   - no GRANT to PUBLIC; every CREATE TABLE is schema-qualified (a bare name lands in the connection's search_path)
#   - with --lifecycle-test: the newest version is named in that test's `wantChain := []string{...}` literal (the executable expectation
#     of the chain), so adding a migration without extending the test is red
#
# NOT CHECKED (printed on every run): whether the SQL is valid, applies, or grants what it should — only the database tests in
# tests/harness prove that, against a real Postgres, on the runner. This gate reads file names and a few literals.
set -u
DIR="db/migrations"; LIFECYCLE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="${2:?--dir needs a path}"; shift 2;;
    --lifecycle-test) LIFECYCLE="${2:?--lifecycle-test needs a file}"; shift 2;;
    *) echo "check-migrations: unknown argument '$1'" >&2; exit 2;;
  esac
done
fail=0; bad() { echo "  FAIL: $*"; fail=$((fail+1)); }
[ -d "$DIR" ] || { echo "check-migrations: RED — $DIR is not a directory"; exit 1; }

# read a file with CR stripped (a Windows checkout must not change the verdict)
rd() { tr -d '\r' < "$1"; }

ups=$(find "$DIR" -maxdepth 1 -type f -name '*.up.sql' | sort)
nups=0; [ -n "$ups" ] && nups=$(printf '%s\n' "$ups" | wc -l | tr -d ' ')
if [ "$nups" -lt 1 ]; then
  echo "check-migrations: RED — found 0 .up.sql files in $DIR: the gate matched nothing, so it would pass vacuously"
  exit 1
fi

# stray .sql files (anything that is not a platform_NNNN_desc.{up,down}.sql)
for f in $(find "$DIR" -maxdepth 1 -type f -name '*.sql' | sort); do
  b=$(basename "$f")
  case "$b" in
    platform_[0-9][0-9][0-9][0-9]_*.up.sql|platform_[0-9][0-9][0-9][0-9]_*.down.sql) ;;
    *) bad "$b: not named platform_NNNN_<desc>.up.sql or .down.sql";;
  esac
done

prev=""; newest=""
for up in $ups; do
  b=$(basename "$up")
  case "$b" in platform_[0-9][0-9][0-9][0-9]_*.up.sql) ;; *) continue;; esac   # already reported above
  ver=${b#platform_}; ver=${ver%%_*}
  desc=${b#platform_${ver}_}; desc=${desc%.up.sql}
  down="$DIR/platform_${ver}_${desc}.down.sql"
  [ -f "$down" ] || bad "$b has no matching $(basename "$down")"
  if [ -n "$prev" ]; then
    expect=$(printf '%04d' $((10#$prev + 1)))
    [ "$ver" = "$expect" ] || bad "gap or duplicate: after $prev expected $expect, got $ver ($b)"
  else
    [ "$ver" = "0000" ] || bad "the chain must start at 0000, got $ver ($b)"
  fi
  prev="$ver"; newest="$ver"
  c=$(rd "$up" | grep -c -E "INSERT +INTO +platform\.schema_migrations.*'${ver}'" || true)
  [ "$c" -ge 1 ] || bad "$b does not self-record version '$ver' in platform.schema_migrations"
  if [ -f "$down" ]; then
    c=$(rd "$down" | grep -c -E "DELETE +FROM +platform\.schema_migrations.*'${ver}'" || true)
    if [ "$c" -lt 1 ]; then
      c2=$(rd "$down" | grep -c -i -E "forward-only|no-op|cannot safely remove" || true)
      [ "$c2" -ge 1 ] || bad "$(basename "$down") neither DELETEs version '$ver' nor documents a forward-only no-op"
    fi
  fi
  # grants to PUBLIC, and unqualified CREATE TABLE (comment lines stripped first)
  c=$(rd "$up" | sed -E 's/--.*$//' | grep -c -i -E "GRANT[^;]*TO +PUBLIC" || true)
  [ "$c" -eq 0 ] || bad "$b grants to PUBLIC"
  unq=$(rd "$up" | sed -E 's/--.*$//' | grep -i -E "CREATE +TABLE" | grep -v -i -E "CREATE +TABLE( +IF +NOT +EXISTS)? +[a-z_]+\.[a-z_]+" || true)
  [ -z "$unq" ] || bad "$b has a CREATE TABLE that is not schema-qualified: $(printf '%s' "$unq" | head -1 | tr -s ' ')"
done

if [ -n "$LIFECYCLE" ]; then
  if [ ! -f "$LIFECYCLE" ]; then
    bad "--lifecycle-test $LIFECYCLE does not exist: the newest migration's coupling to the lifecycle test cannot be checked"
  else
    lit=$(rd "$LIFECYCLE" | sed -n '/wantChain := \[\]string{/,/}/p')
    if [ -z "$lit" ]; then
      bad "could not find the 'wantChain := []string{...}' literal in $LIFECYCLE (if it moved or was renamed, update this check — do not delete it)"
    else
      c=$(printf '%s\n' "$lit" | grep -c "\"${newest}\"" || true)
      [ "$c" -ge 1 ] || bad "newest migration '$newest' is absent from wantChain in $LIFECYCLE"
    fi
  fi
fi

echo "check-migrations: $nups up files, newest ${newest:-none}, failures: $fail"
echo "NOT CHECKED: that any migration's SQL is valid, applies, or grants what it should (only the harness database tests prove that)"
[ "$fail" -eq 0 ] || { echo "check-migrations: RED"; exit 1; }
echo "check-migrations: OK"
