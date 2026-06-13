#!/usr/bin/env bash
# Pandastack cold-start benchmark suite.
# Runs sequential, concurrent, and churn workloads against a live API
# and produces a markdown report with p50/p90/p99 and throughput.
set -euo pipefail

API="${API_URL:-http://localhost:8080}"
TEMPLATE="${TEMPLATE:-ubuntu-24.04}"
SEQ="${SEQ:-20}"
CONC="${CONC:-5}"
CHURN_SECS="${CHURN_SECS:-60}"
OUT="${OUT:-bench/results/full-suite-$(date +%Y%m%d-%H%M%S).md}"

mkdir -p "$(dirname "$OUT")"
ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

create_one() {
  local t0 t1 sb body
  t0=$(ms)
  body=$(curl -fsS -X POST "$API/v1/sandboxes" \
    -H 'content-type: application/json' \
    -d '{"template":"'"$TEMPLATE"'","cpu":1,"memory_mb":512}')
  t1=$(ms)
  sb=$(echo "$body" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
  # wait for running, then read boot_ms
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    row=$(curl -fsS "$API/v1/sandboxes/$sb")
    status=$(echo "$row" | python3 -c 'import json,sys;print(json.load(sys.stdin)["status"])')
    [ "$status" = "running" ] && break
    sleep 0.3
  done
  boot=$(echo "$row" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("boot_ms",0))')
  mode=$(echo "$row" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("boot_mode","?"))')
  printf "%d %d %s %s\n" "$((t1-t0))" "$boot" "$mode" "$sb"
}

percentiles() {
  python3 - <<'PY' "$@"
import sys, statistics
vals = sorted(int(x) for x in sys.argv[1:] if x.isdigit())
if not vals:
    print("n=0")
    sys.exit(0)
def pct(p):
    if not vals: return 0
    k = max(0, min(len(vals)-1, int(round((p/100.0)*(len(vals)-1)))))
    return vals[k]
print(f"n={len(vals)} min={vals[0]} p50={pct(50)} p90={pct(90)} p99={pct(99)} max={vals[-1]} mean={int(statistics.mean(vals))}")
PY
}

{
  echo "# Pandastack benchmark — $(date)"
  echo
  echo "- API: \`$API\`"
  echo "- Template: \`$TEMPLATE\`"
  echo "- Seq=$SEQ, Conc=$CONC, Churn=${CHURN_SECS}s"
  echo
} > "$OUT"

echo ">>> Sequential cold boots ($SEQ runs)" | tee -a "$OUT"
WALL=(); BOOT=()
for i in $(seq 1 "$SEQ"); do
  line=$(create_one)
  w=$(echo "$line" | awk '{print $1}')
  b=$(echo "$line" | awk '{print $2}')
  m=$(echo "$line" | awk '{print $3}')
  sb=$(echo "$line" | awk '{print $4}')
  printf "  [%02d] wall=%5dms boot=%5dms mode=%-6s\n" "$i" "$w" "$b" "$m" | tee -a "$OUT"
  WALL+=("$w"); BOOT+=("$b")
  curl -fsS -X DELETE "$API/v1/sandboxes/$sb" >/dev/null 2>&1 || true
done
{
  echo
  echo "**Sequential wall:** $(percentiles "${WALL[@]}")"
  echo "**Sequential boot_ms:** $(percentiles "${BOOT[@]}")"
  echo
} | tee -a "$OUT"

echo ">>> Concurrent cold boots ($CONC parallel)" | tee -a "$OUT"
TMPD=$(mktemp -d)
T0=$(ms)
for i in $(seq 1 "$CONC"); do
  ( create_one > "$TMPD/$i.out" ) &
done
wait
T1=$(ms)
CW=(); CB=()
for f in "$TMPD"/*.out; do
  line=$(cat "$f")
  CW+=("$(echo "$line" | awk '{print $1}')")
  CB+=("$(echo "$line" | awk '{print $2}')")
  sb=$(echo "$line" | awk '{print $4}')
  curl -fsS -X DELETE "$API/v1/sandboxes/$sb" >/dev/null 2>&1 || true
done
rm -rf "$TMPD"
{
  echo "- Total wall (whole batch): $((T1-T0))ms"
  echo "- Per-request wall: $(percentiles "${CW[@]}")"
  echo "- Per-request boot_ms: $(percentiles "${CB[@]}")"
  echo
} | tee -a "$OUT"

echo ">>> Churn (create+delete for ${CHURN_SECS}s)" | tee -a "$OUT"
COUNT=0; END=$(( $(date +%s) + CHURN_SECS ))
while [ "$(date +%s)" -lt "$END" ]; do
  line=$(create_one) || break
  sb=$(echo "$line" | awk '{print $4}')
  curl -fsS -X DELETE "$API/v1/sandboxes/$sb" >/dev/null 2>&1 || true
  COUNT=$((COUNT+1))
done
{
  echo "- Completed cycles: $COUNT in ${CHURN_SECS}s"
  echo "- Throughput: $(python3 -c "print(round($COUNT/$CHURN_SECS, 2))") sandboxes/sec"
  echo
} | tee -a "$OUT"

echo ">>> Server-side stats from /v1/stats/boot"
curl -fsS "$API/v1/stats/boot" | python3 -m json.tool | tee -a "$OUT"

echo
echo "Wrote report to: $OUT"
