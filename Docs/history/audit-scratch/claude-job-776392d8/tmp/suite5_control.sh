#!/usr/bin/env bash
# CONTROL for the M2 SUITE 5 attribution question.
# Claim under test: the single FAIL was caused by the worker CONTAINER not running at
# suite-5 time -- not by the Pass 1 rename. Run suite 5's three checks verbatim against
# (a) a dead worker and (b) a live worker, on the SAME tree. If the signature
# PASS/FAIL/PASS appears in (a) and PASS/PASS/PASS in (b), liveness is the cause.
DC="docker compose -p coldmail-send-managed"
suite5() {
  if $DC exec -T worker sh -c 'test -e /secrets/kms/dev-master.key' 2>/dev/null; then
    echo "  FAIL: worker CAN see the kms key"; else echo "  PASS: worker cannot see the kms key"; fi
  if $DC exec -T worker sh -c 'test -r /secrets/mtls/worker.crt' 2>/dev/null; then
    echo "  PASS: worker can read its own mTLS cert"; else echo "  FAIL: worker cannot read its own cert"; fi
  if $DC exec -T control sh -c 'test -r /secrets/kms/dev-master.key' 2>/dev/null; then
    echo "  PASS: Control CAN read the KMS key"; else echo "  FAIL: Control cannot read the KMS key"; fi
}
echo "=== (b) BASELINE: worker LIVE ==="
$DC up -d --no-deps worker >/dev/null 2>&1; sleep 3
docker inspect -f '  container Running={{.State.Running}}' "$($DC ps -q worker)"
suite5
echo
echo "=== (a) CONTROL: worker KILLED (exactly what phase3_send.sh:148 does) ==="
$DC kill worker >/dev/null 2>&1
docker inspect -f '  container Running={{.State.Running}} ExitCode={{.State.ExitCode}}' "$($DC ps -aq worker)"
suite5
echo
echo "=== restore ==="
$DC up -d --no-deps worker >/dev/null 2>&1; sleep 3
docker inspect -f '  container Running={{.State.Running}}' "$($DC ps -q worker)"
