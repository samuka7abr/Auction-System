#!/usr/bin/env bash
# One benchmark cell, end to end: reset, seed, warmup, reset, load, verify.
#
# This is the brick etapa 5 repeats 36 times. It stops at the first error
# because a cell that half-ran is a row of results nobody can explain, and
# because a green matrix built on one silent failure is worse than no matrix.
set -euo pipefail
cd "$(dirname "$0")/.."

# .env carries the compose credentials. Only variables the caller did not set
# are taken from it, so `make bench STRATEGY=x` is never silently overridden by
# the STRATEGY line in .env.
if [ -f .env ]; then
  while IFS= read -r line; do
    case "$line" in '' | \#*) continue ;; esac
    key="${line%%=*}"
    [ -n "${!key:-}" ] || export "${line?}"
  done < .env
fi

RUN="${RUN:-$(date -u +%Y%m%dT%H%M%S)}"
AUCTIONS="${AUCTIONS:-1}"
POLICY="${POLICY:-immediate}"
SCENARIO="${SCENARIO:-smoke}"
STRATEGY="${STRATEGY:-optimistic}"
MIN_INCREMENT="${MIN_INCREMENT:-100}"
# Loose enough that nothing closes mid-cell: an auction dying under load mixes
# contention with the closing edge in one number, and the edge deserves a cell
# of its own (etapa 5). I6 warns if any bid still meets a closed auction.
#
# BENCH_ENDS_IN is the name the Makefile and the chaos checkpoints use for the
# same knob, and it is accepted here so that a caller that skips `make bench`
# does not silently get the 30m default — a chaos cell that never reaches an
# ends_at proves nothing, and would say so nowhere.
ENDS_IN="${ENDS_IN:-${BENCH_ENDS_IN:-30m}}"

# The failure injected from outside, empty in every cell of the matrix. Setting
# it changes three things and nothing else: the injector runs alongside the
# measured load, k6 exit code 99 stops being fatal, and the injector is waited
# for afterwards (RF08).
CHAOS="${CHAOS:-}"

POSTGRES_USER="${POSTGRES_USER:-auction}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-auction}"
POSTGRES_DB="${POSTGRES_DB:-auction}"
DB_URL="${DATABASE_URL:-postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable}"
AUCTIOND_URL="${AUCTIOND_URL:-http://localhost:${HTTP_PORT:-8080}}"

RESULTS="bench/results/$RUN"
MANIFEST="bench/auctions.json"
K6_NAME="bid-storm-k6-$RUN"
STATS="$(mktemp -d)"

pg() { docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -qtA "$@"; }
say() { printf '\n== %s\n' "$*"; }

# A cell run against the wrong engine produces a plausible, false row and leaves
# no trace, which makes it the most expensive mistake possible in a matrix of 36.
# /metrics is asked rather than the compose file because the decorator binds
# bid_confirm_duration_seconds{strategy} at boot: this is what the process IS
# running, not what somebody meant to configure.
say "checking auctiond at $AUCTIOND_URL, strategy=$STRATEGY"
if [ "$(curl -s -o /dev/null -w '%{http_code}' "$AUCTIOND_URL/readyz")" != "200" ]; then
  echo "run-cell: auctiond is not ready at $AUCTIOND_URL/readyz" >&2
  exit 1
fi
if ! curl -sf "$AUCTIOND_URL/metrics" | grep -q "bid_confirm_duration_seconds_count{strategy=\"$STRATEGY\"}"; then
  echo "run-cell: auctiond is not running BID_STRATEGY=$STRATEGY" >&2
  exit 1
fi

go build -o bin/seed ./cmd/seed
go build -o bin/checker ./cmd/checker
mkdir -p "$RESULTS"

# TRUNCATE + seed + VACUUM ANALYZE. The seed binary owns the truncate so that
# the wipe and the refill cannot drift apart, and every cell of the matrix opens
# on an empty table with fresh statistics (decisão 13).
reset() {
  DATABASE_URL="$DB_URL" ./bin/seed -truncate \
    -auctions="$AUCTIONS" -ends-in="$ENDS_IN" -min-increment="$MIN_INCREMENT" -out="$MANIFEST"
  pg -c 'VACUUM ANALYZE;' > /dev/null
  # The state a cell opens on now includes Redis: an idempotency entry that
  # survived from the warmup would answer the measured cell with a stored 201
  # that wrote no row. The nonce inside bid-storm.js covers whoever runs k6 by
  # hand; this covers the matrix.
  docker compose exec -T redis redis-cli FLUSHALL > /dev/null
}

k6_run() {
  docker compose run --rm --no-deps --name "$K6_NAME" --user "$(id -u):$(id -g)" k6 \
    run /bench/bid-storm.js \
    -e RUN="$RUN" -e SCENARIO="$SCENARIO" -e RETRY_POLICY="$POLICY" \
    -e BID_STRATEGY="$STRATEGY" -e RESULTS_DIR=/bench/results "$@"
}

# A limit only moves the problem if nobody looks at it. When the generator
# saturates, the queue forms in the client while the client's clock is already
# running, and the effect arrives looking like server latency.
watch_generator() {
  # Its own subshell, and errexit off inside it: the first samples land before
  # `docker compose run` has created the container, and a failing `docker stats`
  # would otherwise take the watcher down before the load even starts.
  set +e
  local peak=0 now limits
  while :; do
    if [ ! -s "$STATS/limits" ]; then
      # Captured rather than redirected: a failing `docker inspect -f` still
      # prints a bare newline, and a one-byte file passes `-s` — which would
      # freeze the limits at "unknown" for the rest of the cell.
      limits=$(docker inspect -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.Config.Image}}' \
        "$K6_NAME" 2> /dev/null)
      case "$limits" in [0-9]*) echo "$limits" > "$STATS/limits" ;; esac
    fi
    now=$(docker stats --no-stream --format '{{.CPUPerc}}' "$K6_NAME" 2> /dev/null | tr -d '%' | head -1)
    # A container that just started reports "--" for a cycle or two.
    if [ -n "${now:-}" ] && [ "$now" != "--" ]; then
      peak=$(awk -v a="$peak" -v b="$now" 'BEGIN { print (b > a) ? b : a }')
      echo "$peak" > "$STATS/peak"
    fi
    sleep 1
  done
}

stop_watching() {
  [ -n "${WATCHER:-}" ] || return 0
  kill "$WATCHER" 2> /dev/null || :
  wait "$WATCHER" 2> /dev/null || :
  WATCHER=""
}

# SIGTERM and not SIGKILL, and this is the whole reason the cure lives in a trap
# inside the injector: the cell aborting is exactly when a Redis left paused or a
# row left locked would poison every cell that comes after.
# shellcheck disable=SC2329  # reached from the EXIT trap, like stop_watching
stop_injector() {
  [ -n "${INJECTOR:-}" ] || return 0
  kill -TERM "$INJECTOR" 2> /dev/null || :
  wait "$INJECTOR" 2> /dev/null || :
  INJECTOR=""
}

# The injector measures the convergence of the closing after the load, so it is
# waited for and never killed on the happy path (decisão 88).
wait_injector() {
  [ -n "${INJECTOR:-}" ] || return 0
  local pid="$INJECTOR"
  INJECTOR=""
  if ! wait "$pid"; then
    echo "run-cell: the injector exited non-zero" >&2
    return 1
  fi
}

# Armed only now, and this is not cosmetic: a trap set before these functions
# exist answers "command not found" when the pre-flight fails, and the cell
# exits 127 instead of the 1 or 2 the loop of etapa 5 reads (decisão 93).
trap 'stop_watching; stop_injector; rm -rf "$STATS"' EXIT

say "reset, seed $AUCTIONS auction(s), vacuum"
reset

# The warmup bids, and bids become rows. Without a second reset the measured
# cell would open on a dirty table with moved statistics — decisão 13 broken
# inside the cell instead of between cells. What the warmup leaves behind is
# exactly what survives a TRUNCATE, and the whole reason it exists: open pool
# connections, per-connection plans, a warm page cache and a settled Go heap.
say "warmup (discarded)"
k6_run -e WARMUP=1 --quiet

say "reset again: the warmup wrote rows, and the measured cell cannot inherit them"
reset

started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
watch_generator &
WATCHER=$!
# Alongside the measured load and never before the warmup, which has to be a
# warmup and not an injection (decisão 82).
if [ -n "$CHAOS" ]; then
  say "chaos: injecting $CHAOS from outside the processes"
  STRATEGY="$STRATEGY" K6_NAME="$K6_NAME" chaos/inject.sh "$CHAOS" "$RESULTS" &
  INJECTOR=$!
fi
say "load: scenario=$SCENARIO auctions=$AUCTIONS policy=$POLICY"
set +e
k6_run
k6_code=$?
set -e
# 99 is a breached threshold, and http_req_failed is exactly the threshold a live
# injection breaks — the first confirmation that the failure landed, coming from
# the client and not from the injector. It is accepted only under chaos, and
# every other code keeps aborting either way: a generator that crashed is not an
# injection (decisão 83).
if [ "$k6_code" -ne 0 ]; then
  if [ -n "$CHAOS" ] && [ "$k6_code" -eq 99 ]; then
    say "k6 exited 99: the threshold broke under $CHAOS, which is the evidence"
  else
    echo "run-cell: k6 exited $k6_code" >&2
    exit "$k6_code"
  fi
fi
stop_watching
wait_injector
finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)

peak_raw=$(cat "$STATS/peak" 2> /dev/null || echo 0)
read -r nanocpus memory image < "$STATS/limits" 2> /dev/null || { nanocpus=0; memory=0; image=""; }
k6_cpus=$(awk -v n="${nanocpus:-0}" 'BEGIN { print n / 1e9 }')
# docker stats reports 100% per core, so a 2.0-CPU container peaks at 200%. What
# matters is the fraction of the generator's own limit.
peak_pct=$(awk -v p="${peak_raw:-0}" -v c="$k6_cpus" 'BEGIN { printf "%.1f", (c > 0) ? p / c : 0 }')

say "recording the environment"
RUN="$RUN" STRATEGY="$STRATEGY" AUCTIONS="$AUCTIONS" POLICY="$POLICY" SCENARIO="$SCENARIO" \
  STARTED_AT="$started" FINISHED_AT="$finished" \
  K6_CPUS="$k6_cpus" K6_MEM_BYTES="${memory:-0}" K6_IMAGE="${image:-}" \
  GENERATOR_CPU_PCT_PEAK="$peak_pct" \
  bench/env.sh > "$RESULTS/env.json"

say "checking the invariants"
set +e
DATABASE_URL="$DB_URL" ./bin/checker -run="$RUN" -json | tee "$RESULTS/checker.txt"
code=${PIPESTATUS[0]}
set -e
exit "$code"
