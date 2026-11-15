#!/usr/bin/env bash
# classify.sh — turns the stderr of `psql -X -b -v ON_ERROR_STOP=0 -v VERBOSITY=verbose -f FILE` into one RECORD per failed statement, classifies each record, and
# compares the BLOCKER records with the committed list tests/supabase/known-blockers.txt. Text in, text out: it never touches a database, so
# tests/supabase/classify-selftest.sh proves it offline, and tests/supabase/run.sh uses exactly the same code on the runner.
#
#   classify.sh [--chain NAME] [--stats FILE] [INPUT|-]     PARSE    psql stderr (a file, or stdin) -> TSV records on stdout (one line per failed statement)
#   classify.sh --compare KNOWN [--chains a,b] RECORDS      COMPARE  the BLOCKER records against KNOWN; exit 1 on any NEW, STALE, malformed or duplicate entry
#   classify.sh --table   [--chains a,b] RECORDS            a table of every failure: chain, file:line, SQLSTATE, class, first 160 chars of the statement, the message
#   classify.sh --summary --chains a,b   RECORDS            failures / blockers / cascades per chain (a chain with none prints 0)
#   classify.sh --suggest [--chains a,b] RECORDS            the distinct blockers as known-blockers.txt lines (FILE|SQLSTATE|MESSAGE), to copy from a first run
#   classify.sh --decode                                    stdin -> stdout: undo the TSV escaping of one column (the statement of a record is exact, newlines included)
#
# RECORD = 8 tab-separated columns:  chain  file  line  sqlstate  class  message  statement  detail
#   file       the BASENAME of the path psql printed after `psql:` (a Windows `D:/x/y.sql` and a relative `a/b.sql` both reduce to the file name)
#   line       the number psql printed after the file name (the line on which psql finished reading the statement)
#   sqlstate   the five characters of the verbose form `ERROR:  42501: message`; `-----` when the line carries none (VERBOSITY was not honoured)
#   class      BLOCKER | CASCADE (below)
#   message / statement / detail   escaped so that a record is ALWAYS one line:  % -> %25   TAB -> %09   CR -> %0D   newline -> %0A   (`--decode` undoes it)
#              statement = the text psql echoed after `STATEMENT:` for -b, EXACT, multi-line statements included; empty if psql echoed none
#              detail    = the lines between the error line and the STATEMENT echo (DETAIL, HINT, CONTEXT, LINE n / caret, LOCATION)
#
# CLASSES. BLOCKER: SQLSTATE 42501 (insufficient_privilege), OR the message (or its DETAIL/HINT/CONTEXT lines, case-insensitively) matches
#   must be superuser | permission denied to | must have (the )?(CREATEROLE|ADMIN|SET)<space> | must be (owner|able to SET ROLE) | only roles with |
#   is a reserved role | permission denied for (database|schema|function|language|tablespace)
# CASCADE: everything else (42P01 relation does not exist, 3F000 schema does not exist, 42704, 42710 duplicate ...). A cascade is LISTED and never JUDGED: it usually
# follows an earlier blocker. That is a judgement of this script, not a proof: a non-privilege failure that is a real incompatibility (a missing extension, say) also lands
# here, so a human reads the table.
#
# WHAT IT ASSUMES ABOUT psql (no psql exists where this was written; the first runner run must confirm each one, and run.sh's control statements fail loudly if they are wrong):
#   1. an error is printed on STDERR as   psql:FILE:LINE: ERROR:  SQLSTATE: message   (two spaces after the colon; the SQLSTATE appears because VERBOSITY=verbose),
#      followed by unprefixed continuation lines (DETAIL:, HINT:, CONTEXT:, LINE n: ..., a caret line, LOCATION:  func, file:line);
#   2. -b / --echo-errors then prints   psql:FILE:LINE: STATEMENT:  <the statement text>   on stderr, the text continuing over further UNPREFIXED lines when it spans several;
#      the line carries the same FILE:LINE as the error (if it does not, the echo is counted as an ORPHAN and the run goes red); a `STATEMENT:` line WITHOUT the prefix, directly
#      after the error's own lines, is accepted as well (the one place this script tolerates a second shape, because the echo's prefix is the assumption least sure);
#   3. every other message (NOTICE, WARNING, ...) carries the same `psql:FILE:LINE: ` prefix, so it ends the previous record;
#   4. psql's own failures (cannot connect, no such file) are lines starting `psql: ` — counted, never parsed as a failed statement;
#   5. results and command tags go to STDOUT; run.sh gives this script stderr only, so a result row can never be mistaken for the tail of a statement.
# The FILE in the prefix must contain no whitespace (run.sh runs psql from the file's directory with the bare file name, so it never does).
#
# Every input line has its `\r` stripped. Needs bash, awk (POSIX: it runs under gawk and mawk), tr. Exit: 0 ok, 1 compare red, 2 usage error.
set -u -o pipefail
export LC_ALL=C

usage() { sed -n '2,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2; }
die2() { echo "classify: $*" >&2; exit 2; }

MODE=parse; CHAIN="-"; STATS=""; KNOWN=""; CHAINS=""; INPUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --chain)   [ $# -ge 2 ] || die2 "--chain needs a value"; CHAIN="$2"; shift 2;;
    --stats)   [ $# -ge 2 ] || die2 "--stats needs a file"; STATS="$2"; shift 2;;
    --chains)  [ $# -ge 2 ] || die2 "--chains needs a comma-separated list"; CHAINS="$2"; shift 2;;
    --compare) [ $# -ge 2 ] || die2 "--compare needs the known-blockers file"; MODE=compare; KNOWN="$2"; shift 2;;
    --table)   MODE=table; shift;;
    --summary) MODE=summary; shift;;
    --suggest) MODE=suggest; shift;;
    --decode)  MODE=decode; shift;;
    -h|--help) usage;;
    -)         INPUT="-"; shift;;
    -*)        die2 "unknown argument: $1";;
    *)         [ -z "$INPUT" ] || die2 "more than one input given ($INPUT and $1)"; INPUT="$1"; shift;;
  esac
done

# shared awk helpers. NO single quote may appear anywhere in an awk program below (they live in single-quoted shell strings).
AWK_COMMON='
function dec(s) { gsub(/%0A/, "\n", s); gsub(/%09/, "\t", s); gsub(/%0D/, "\r", s); gsub(/%25/, "%", s); return s }
function trim(s) { sub(/^[ \t\r\n]+/, "", s); sub(/[ \t\r\n]+$/, "", s); return s }
function collapse(s) { gsub(/[ \t\r\n]+/, " ", s); return trim(s) }
function clip(s, n) { s = collapse(s); if (length(s) > n) s = substr(s, 1, n) "..."; return s }
function mkwant(list,    n, a, i) { nwant = 0; if (list == "") return; n = split(list, a, ","); for (i = 1; i <= n; i++) want[a[i]] = 1; nwant = n }
function wanted(c) { return (nwant == 0) || (c in want) }
'

# ---- PARSE ------------------------------------------------------------------------------------------------------------------------------------------------------
AWK_PARSE='
function enc(s) { gsub(/%/, "%25", s); gsub(/\t/, "%09", s); gsub(/\r/, "%0D", s); return s }
function base(p,    n, a) { gsub(/\\/, "/", p); n = split(p, a, "/"); return a[n] }
function classify_rec(st, m, d,    t) {
  if (st == "42501") return "BLOCKER"
  t = tolower(m "\n" d)
  if (t ~ BLOCKER_RE) return "BLOCKER"
  return "CASCADE"
}
function flush(    i, nn, st, dt, dr, cls) {
  if (!have) { state = 0; return }
  nn = nstm; while (nn > 0 && stm[nn] == "") nn--
  st = ""; for (i = 1; i <= nn; i++) st = st (i > 1 ? "%0A" : "") enc(stm[i])
  dt = ""; dr = ""
  for (i = 1; i <= ndet; i++) { dt = dt (i > 1 ? "%0A" : "") enc(det[i]); dr = dr det[i] "\n" }
  cls = classify_rec(sqlstate, msg, dr)
  printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", chain, fname, fline, sqlstate, cls, enc(msg), st, dt
  nrec++; if (cls == "BLOCKER") nblk++; else ncas++
  have = 0; nstm = 0; ndet = 0; state = 0
}
# "psql:FILE:LINE: REST" -> P_FILE, P_LINE, P_REST. FILE has no space, so the first space ends the locus; the LAST colon of the locus separates FILE from LINE.
function prefix(L,    sp, tok, k, c) {
  if (substr(L, 1, 5) != "psql:") return 0
  sp = index(L, " "); if (sp == 0) return 0
  tok = substr(L, 6, sp - 6)
  if (length(tok) < 4 || substr(tok, length(tok), 1) != ":") return 0
  tok = substr(tok, 1, length(tok) - 1)
  c = 0; for (k = length(tok); k >= 1; k--) if (substr(tok, k, 1) == ":") { c = k; break }
  if (c < 2) return 0
  P_LINE = substr(tok, c + 1); if (P_LINE !~ /^[0-9][0-9]*$/) return 0
  P_FILE = substr(tok, 1, c - 1); P_REST = substr(L, sp + 1)
  return 1
}
BEGIN {
  SS = "[0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ]"; RE_STATE = "^" SS SS SS SS SS ": "
  BLOCKER_RE = "must be superuser|permission denied to|must have (the )?(createrole|admin|set) |must be (owner|able to set role)|only roles with|is a reserved role|permission denied for (database|schema|function|language|tablespace)"
  sf = ENVIRON["CLS_STATS_FILE"]; chain = ENVIRON["CLS_CHAIN"]
  nlines = 0; nprefixed = 0; nrec = 0; nblk = 0; ncas = 0; norph = 0; npe = 0; nua = 0; have = 0; state = 0; nstm = 0; ndet = 0
}
{
  L = $0; sub(/\r$/, "", L); nlines++
  if (prefix(L)) {
    nprefixed++
    if (match(P_REST, /^(ERROR|FATAL|PANIC):[ ]*/)) {                 # a failed statement (or a lost connection)
      flush()
      body = substr(P_REST, RLENGTH + 1)
      if (body ~ RE_STATE) { sqlstate = substr(body, 1, 5); msg = substr(body, 8) } else { sqlstate = "-----"; msg = body }
      have = 1; state = 1; nstm = 0; ndet = 0; hfile = P_FILE; hline = P_LINE; fname = base(P_FILE); fline = P_LINE
    } else if (P_REST ~ /^STATEMENT:/) {                               # the -b echo of the statement that just failed
      if (have && state == 1 && P_FILE == hfile && P_LINE == hline) {
        s = substr(P_REST, 11); if (substr(s, 1, 2) == "  ") s = substr(s, 3); else sub(/^[ ]+/, "", s)
        nstm = 1; stm[1] = s; state = 2
      } else { flush(); norph++; if (norph <= 10) orr[norph] = L; state = 3 }
    } else { flush() }                                                 # NOTICE / WARNING / INFO ... ends the previous record
    next
  }
  if (substr(L, 1, 6) == "psql: ") { flush(); npe++; if (npe <= 10) pe[npe] = L; next }
  if (state == 1 && L ~ /^STATEMENT:/) {                               # tolerated: a psql that does not give the -b echo the psql:FILE:LINE: prefix
    s = substr(L, 11); if (substr(s, 1, 2) == "  ") s = substr(s, 3); else sub(/^[ ]+/, "", s)
    nstm = 1; stm[1] = s; state = 2; next
  }
  if (state == 1) { det[++ndet] = L; next }
  if (state == 2) { stm[++nstm] = L; next }
  if (state == 3) next
  if (L ~ /(ERROR|FATAL|PANIC):  /) { nua++; if (nua <= 10) ua[nua] = L }
}
END {
  flush()
  if (sf != "") {
    print "lines=" nlines > sf
    print "prefixed_lines=" nprefixed > sf
    print "records=" nrec > sf
    print "blockers=" nblk > sf
    print "cascades=" ncas > sf
    print "orphan_statements=" norph > sf
    print "psql_errors=" npe > sf
    print "unattributed_error_lines=" nua > sf
    for (i = 1; i <= norph && i <= 10; i++) print "orphan_statement: " orr[i] > sf
    for (i = 1; i <= npe && i <= 10; i++) print "psql_error_line: " pe[i] > sf
    for (i = 1; i <= nua && i <= 10; i++) print "unattributed_line: " ua[i] > sf
    close(sf)
  }
}
'

# ---- COMPARE ----------------------------------------------------------------------------------------------------------------------------------------------------
# An observed blocker is COVERED by a listed line when file == FILE, SQLSTATE == SQLSTATE and its message CONTAINS the substring. Every observed blocker must be covered
# (else NEW BLOCKER) and every listed line must cover at least one observed blocker (else STALE ENTRY): the two sets are equal, or the run is red.
AWK_COMPARE='
function malformed(n, why) { printf "MALFORMED KNOWN-BLOCKER LINE %d (%s): %s\n", n, kname, why; nmal++ }
BEGIN {
  SS = "[0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ]"; kname = ENVIRON["CLS_KNOWN_NAME"]; mkwant(chains)
  nk = 0; kl = 0; nobs = 0; ncasc = 0; nnew = 0; nstale = 0; nmal = 0; ndup = 0; nbad = 0
}
FILENAME == ARGV[1] {
  kl++
  line = $0; sub(/\r$/, "", line); t = trim(line)
  if (t == "" || substr(t, 1, 1) == "#") next
  p1 = index(t, "|")
  if (p1 == 0) { malformed(kl, "expected FILE|SQLSTATE|MESSAGE-SUBSTRING, found no bar"); next }
  r = substr(t, p1 + 1); p2 = index(r, "|")
  if (p2 == 0) { malformed(kl, "expected FILE|SQLSTATE|MESSAGE-SUBSTRING, found only two fields"); next }
  f = trim(substr(t, 1, p1 - 1)); s = trim(substr(r, 1, p2 - 1)); m = trim(substr(r, p2 + 1))
  if (f == "") { malformed(kl, "empty FILE"); next }
  if (f ~ /[\/\\]/) { malformed(kl, "FILE must be a basename, got " f); next }
  if (s !~ ("^" SS SS SS SS SS "$")) { malformed(kl, "SQLSTATE must be five characters from 0-9 A-Z, got " s); next }
  if (m == "") { malformed(kl, "empty MESSAGE-SUBSTRING: an entry must say what fails"); next }
  key = f "|" s "|" m
  if (key in seenkey) { printf "DUPLICATE ENTRY line %d of %s repeats line %d: %s\n", kl, kname, seenkey[key], key; ndup++; next }
  seenkey[key] = kl; nk++; kf[nk] = f; ks[nk] = s; km[nk] = m; kline[nk] = kl; kmatch[nk] = 0
  next
}
FILENAME == ARGV[2] {
  line = $0; sub(/\r$/, "", line)
  if (line == "" || substr(line, 1, 1) == "#") next
  n = split(line, c, "\t")
  if (n < 7) { printf "MALFORMED RECORD ROW %d of the records file: %d columns, want 8\n", FNR, n; nbad++; next }
  if (!wanted(c[1])) next
  if (c[5] != "BLOCKER") { ncasc++; next }
  nobs++; och[nobs] = c[1]; ofile[nobs] = c[2]; oline[nobs] = c[3]; ostate[nobs] = c[4]; omsg[nobs] = dec(c[6]); ostmt[nobs] = dec(c[7])
}
END {
  for (o = 1; o <= nobs; o++) {
    hit = 0
    for (i = 1; i <= nk; i++) if (kf[i] == ofile[o] && ks[i] == ostate[o] && index(omsg[o], km[i]) > 0) { kmatch[i]++; hit = 1 }
    if (!hit) {
      printf "NEW BLOCKER file=%s line=%s sqlstate=%s chain=%s message=\"%s\" statement: %s\n", ofile[o], oline[o], ostate[o], och[o], clip(omsg[o], 160), clip(ostmt[o], 160)
      nnew++
    }
    key = ofile[o] "|" ostate[o] "|" omsg[o]; if (!(key in dk)) { dk[key] = 1; ndistinct++ }
  }
  for (i = 1; i <= nk; i++) if (kmatch[i] == 0) {
    printf "STALE ENTRY %s line %d: %s|%s|%s - no observed blocker has this file, SQLSTATE and message substring (it no longer fails, or its message changed): fix or remove it\n", kname, kline[i], kf[i], ks[i], km[i]
    nstale++
  }
  if (nnew + nstale + nmal + ndup + nbad > 0) {
    printf "compare: RED - NEW=%d STALE=%d MALFORMED=%d DUPLICATE=%d BADROWS=%d (observed blockers: %d in %d distinct (file, SQLSTATE, message); listed entries: %d; cascades not judged: %d)\n", nnew, nstale, nmal, ndup, nbad, nobs, ndistinct, nk, ncasc
    exit 1
  }
  printf "compare: OK - the %d observed blocker(s) (%d distinct (file, SQLSTATE, message)) are exactly the %d listed entr%s; %d cascade(s) listed above are not judged\n", nobs, ndistinct, nk, (nk == 1 ? "y" : "ies"), ncasc
  exit 0
}
'

# ---- TABLE / SUMMARY / SUGGEST / DECODE -------------------------------------------------------------------------------------------------------------------------
AWK_TABLE='
BEGIN { FS = "\t"; mkwant(chains); n = 0 }
/^#/ || /^$/ { next }
{
  if (!wanted($1)) next
  n++
  printf "  %-17s %-58s %-5s %-8s %s\n", "[" $1 "]", $2 ":" $3, $4, $5, clip(dec($7), 160)
  printf "  %-17s message: %s\n", "", clip(dec($6), 300)
}
END { if (n == 0) print "  (no failures)" }
'
AWK_SUMMARY='
BEGIN { FS = "\t"; nc = split(chains, order, ",") }
/^#/ || /^$/ { next }
{ tot[$1]++; if ($5 == "BLOCKER") blk[$1]++; else cas[$1]++ }
END { for (i = 1; i <= nc; i++) { c = order[i]; printf "  %-17s failures=%d blockers=%d cascades=%d\n", c, tot[c] + 0, blk[c] + 0, cas[c] + 0 } }
'
AWK_SUGGEST='
BEGIN { FS = "\t"; mkwant(chains) }
/^#/ || /^$/ { next }
{
  if (!wanted($1) || $5 != "BLOCKER") next
  m = dec($6); sub(/\n.*/, "", m); m = trim(m)
  key = $2 "|" $4 "|" m
  if (!(key in seen)) { seen[key] = 1; print key }
}
'
AWK_DECODE='{ print dec($0) }'

need_records() {
  [ -n "$INPUT" ] && [ "$INPUT" != "-" ] || die2 "this mode needs a records file argument"
  [ -f "$INPUT" ] || die2 "records file missing: $INPUT"
}

case "$MODE" in
  parse)
    [ -n "$INPUT" ] || INPUT="-"
    if [ "$INPUT" != "-" ] && [ ! -f "$INPUT" ]; then die2 "input file missing: $INPUT"; fi
    [ -z "$STATS" ] || : > "$STATS" || die2 "cannot write the stats file $STATS"
    if [ "$INPUT" = "-" ]; then
      CLS_STATS_FILE="$STATS" CLS_CHAIN="$CHAIN" awk "$AWK_PARSE"
    else
      CLS_STATS_FILE="$STATS" CLS_CHAIN="$CHAIN" awk "$AWK_PARSE" "$INPUT"
    fi;;
  compare)
    if [ ! -f "$KNOWN" ]; then echo "compare: RED - the known-blockers file is missing: $KNOWN (a missing list is never read as an empty one)"; exit 1; fi
    need_records
    CLS_KNOWN_NAME="$(basename "$KNOWN")" awk -v chains="$CHAINS" "$AWK_COMMON $AWK_COMPARE" "$KNOWN" "$INPUT";;
  table)   need_records; awk -v chains="$CHAINS" "$AWK_COMMON $AWK_TABLE" "$INPUT";;
  summary) need_records; [ -n "$CHAINS" ] || die2 "--summary needs --chains"; awk -v chains="$CHAINS" "$AWK_COMMON $AWK_SUMMARY" "$INPUT";;
  suggest) need_records; awk -v chains="$CHAINS" "$AWK_COMMON $AWK_SUGGEST" "$INPUT";;
  decode)  awk "$AWK_COMMON $AWK_DECODE";;
esac
