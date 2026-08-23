#!/usr/bin/env bash
# Injects one failure from outside the processes under test, collects the
# evidence that it landed while the window is still open, and cures the system
# whatever happens.
#
# Nothing in this repository knows this script exists (decisão 81): no binary
# reads a chaos variable, no handler carries an injection branch, and the
# binaries that run a chaos cell are byte for byte the ones that run the matrix.
# Everything here is done through docker, psql, redis and /metrics.
#
# The evidence has to be collected from outside and *inside* the window because
# it dies with it: a container that already restarted does not report that it
# restarted, and a Redis that was already unpaused does not remember standing
# still (decisão 84).
#
# It never writes to the database. The only SQL is SELECT, and the only
# SELECT ... FOR UPDATE lives in a transaction that ends in ROLLBACK — a row
# inserted from here would be a row the client never counted, and I5 would
# report the instrument as a violation (decisão 90).
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env ]; then
  while IFS= read -r line; do
    case "$line" in '' | \#*) continue ;; esac
    key="${line%%=*}"
    [ -n "${!key:-}" ] || export "${line?}"
  done < .env
fi

SCENARIO_NAME="${1:-}"
RESULTS_DIR="${2:-}"

STRATEGY="${STRATEGY:-optimistic}"
PGUSER_="${POSTGRES_USER:-auction}"
PGDB_="${POSTGRES_DB:-auction}"
AUCTIOND="${AUCTIOND_URL:-http://localhost:${HTTP_PORT:-8080}}"
CLOSERD="${CLOSERD_URL:-http://localhost:${CLOSERD_PORT:-8081}}"

# The row lock of scenario 4, in seconds, and the cap on the convergence wait of
# scenario 1 (decisão 88).
LOCK_HOLD=8
CONVERGE_DEADLINE=120

START_EPOCH=$(date +%s)
TARGET=""
LANDED=false
STEPS='[]'
EVIDENCE='{}'
BACKLOG_PEAK=0
PENDING_PEAK=0
NAP_PID=""
LOCK_PID=""
READYZ_PID=""
READYZ_FILE=""

usage() {
  cat >&2 <<'EOF'
usage: chaos/inject.sh <scenario> <results-dir>

  closerd-kill        SIGKILL on the closerd twice, with auctions expiring
  closerd-kill-shard  the same injection, run against the shard engine
  auctiond-kill       SIGKILL on the auctiond at 500 VUs
  redis-pause         docker pause on Redis for five seconds
  pool-saturation     a row lock held from outside for eight seconds
EOF
  exit 2
}

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
log() { printf '%s  chaos[%s] %s\n' "$(now)" "${SCENARIO_NAME:-?}" "$*"; }

# One line per step, timestamped, on stdout: the output of this script is the
# timeline of the scenario, and it is what goes in the PR (RF01).
step() {
  STEPS=$(jq --arg at "$(now)" --arg a "$1" --arg t "$2" \
    '. += [{at: $at, action: $a, target: $t}]' <<< "$STEPS")
  log "$1 $2"
}

ev_num() { EVIDENCE=$(jq --arg k "$1" --argjson v "${2:-0}" '.[$k] = $v' <<< "$EVIDENCE"); }
ev_str() { EVIDENCE=$(jq --arg k "$1" --arg v "${2:-}" '.[$k] = $v' <<< "$EVIDENCE"); }
ev_raw() { EVIDENCE=$(jq --arg k "$1" --argjson v "$2" '.[$k] = $v' <<< "$EVIDENCE"); }

cid() { docker compose ps -q "$1" 2> /dev/null | head -1; }
started_at() { docker inspect -f '{{.State.StartedAt}}' "$(cid "$1")" 2> /dev/null || echo ""; }

# Bash only runs a trap between commands, so a plain `sleep 40` would swallow the
# SIGTERM run-cell.sh sends and delay the cure by the whole remaining step.
nap() {
  sleep "$1" &
  NAP_PID=$!
  wait "$NAP_PID" 2> /dev/null || :
  NAP_PID=""
}

# at holds until <seconds> after this script started. Every scenario is written
# on that clock, and the clock starts with the measured load.
at() {
  local remain=$((START_EPOCH + $1 - $(date +%s)))
  if [ "$remain" -gt 0 ]; then nap "$remain"; fi
}

# metric <base-url> <series> [label=value ...]
#
# Matched by name plus every label fragment given, in any order and never as one
# full string: the exposition sorts labels alphabetically, so a whole-line match
# answers nothing the day a series gains a label — and answering nothing here
# reads downstream as a zero, which is the one failure mode this whole spec is
# about (decisão 59).
metric() {
  local url=$1 name=$2
  shift 2
  curl -s --max-time 3 "$url/metrics" 2> /dev/null |
    awk -v name="$name" -v labels="$*" '
      $1 == name || index($1, name "{") == 1 {
        ok = 1
        n = split(labels, want, " ")
        for (i = 1; i <= n; i++) if (index($1, want[i]) == 0) ok = 0
        if (ok) v = $2
      }
      END { print v }' || true
}

http_code() {
  local code
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$1" 2> /dev/null) || code=000
  echo "${code:-000}"
}

# Code and body in one round trip. With Redis frozen every /readyz costs the
# client's read timeout, and asking twice stretched a five second pause into
# sixteen — the injector has to fit inside the window it opened, not widen it.
# One request, in the background, leaving body, code and elapsed seconds on
# three lines of <out>.
probe_async() {
  local url=$1 timeout=$2 out=$3 start
  start=$(date +%s)
  (
    resp=$(curl -s -m "$timeout" -w '\n%{http_code}' "$url" 2> /dev/null) || resp=$'\n000'
    code=$(printf '%s' "$resp" | tail -n1)
    body=$(printf '%s' "$resp" | sed '$d' | tr -d '\n')
    printf '%s\n%s\n%s\n' "$body" "${code:-000}" "$(($(date +%s) - start))" > "$out"
  ) &
  READYZ_PID=$!
}

wait_probe() {
  local deadline=$(($(date +%s) + $2))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    [ -s "$1" ] && return 0
    nap 1
  done
  return 0
}

pg() {
  docker compose exec -T postgres psql -U "$PGUSER_" -d "$PGDB_" -v ON_ERROR_STOP=1 -qtA \
    -c "$1" 2> /dev/null | tr -d '[:space:]' || true
}

# Echoes the seconds it took, or -1 if it never answered.
wait_ready() {
  local start
  start=$(date +%s)
  while [ $(($(date +%s) - start)) -lt "$2" ]; do
    if [ "$(http_code "$1/readyz")" = "200" ]; then echo $(($(date +%s) - start)); return 0; fi
    nap 1
  done
  echo -1
}

bigger() { awk -v a="$1" -v b="$2" 'BEGIN { print (b + 0 > a + 0) ? b : a }'; }

# The two queue gauges are read from the producer, which stays alive while the
# consumer is dead — that is the whole reason they live there (decisão 73).
sample_queue() {
  local b p
  b=$(metric "$AUCTIOND" stream_backlog_entries)
  p=$(metric "$AUCTIOND" stream_pending_entries)
  if [ -n "$b" ]; then BACKLOG_PEAK=$(bigger "$BACKLOG_PEAK" "$b"); fi
  if [ -n "$p" ]; then PENDING_PEAK=$(bigger "$PENDING_PEAK" "$p"); fi
}

observe_until() {
  while [ $(($(date +%s) - START_EPOCH)) -lt "$1" ]; do
    sample_queue
    nap 1
  done
}

write_report() {
  [ -n "$RESULTS_DIR" ] || return 0
  mkdir -p "$RESULTS_DIR"
  jq -n --arg s "$SCENARIO_NAME" --arg t "$TARGET" --arg st "$STRATEGY" \
    --argjson landed "$LANDED" --argjson steps "$STEPS" --argjson ev "$EVIDENCE" \
    '{scenario: $s, target: $t, strategy: $st, landed: $landed, steps: $steps, evidence: $ev}' \
    > "$RESULTS_DIR/chaos.json"
}

# Unconditional, idempotent, and never dependent on which step was running: an
# execution that leaves Redis paused poisons every cell that comes after it.
# chaos.json is written from here too, so that a scenario that aborted halfway
# still says on disk that it aborted (RF01).
cure() {
  local rc=$?
  trap - EXIT
  set +e
  [ -n "$NAP_PID" ] && kill "$NAP_PID" 2> /dev/null
  local redis_id
  redis_id=$(cid redis)
  [ -n "$redis_id" ] && docker unpause "$redis_id" > /dev/null 2>&1
  # start and never `up -d`: up recreates the container and can reseed it with a
  # STRATEGY other than the one this cell verified before the load (RF01).
  docker compose start auctiond closerd > /dev/null 2>&1
  if [ -n "$LOCK_PID" ]; then
    kill "$LOCK_PID" 2> /dev/null
    wait "$LOCK_PID" 2> /dev/null
  fi
  [ -n "$READYZ_PID" ] && kill "$READYZ_PID" 2> /dev/null
  [ -n "$READYZ_FILE" ] && rm -f "$READYZ_FILE"
  write_report
  log "cura concluída, código $rc, landed=$LANDED"
  exit "$rc"
}

# ---------------------------------------------------------------------------

# The closerd dies ten seconds before the first ends_at and comes back twice.
# The second kill is what gives XAUTOCLAIM something to reclaim: three seconds
# of life is a high chance of a message in hand and no XACK behind it.
scenario_closerd_kill() {
  TARGET=closerd
  local before1 after1 before2 after2

  before1=$(started_at closerd)
  at 35
  step kill closerd
  docker kill -s KILL "$(cid closerd)" > /dev/null

  # The auctions expire with nobody consuming: the sweep republishes, the
  # backlog grows, nothing closes — and every late bid still gets a 410, because
  # what refuses it is ends_at against the database clock and not the column.
  observe_until 75
  step start closerd
  docker compose start closerd > /dev/null
  after1=$(started_at closerd)

  kill_on_pending 3
  before2=$(started_at closerd)
  step kill closerd
  docker kill -s KILL "$(cid closerd)" > /dev/null
  nap 2
  step start closerd
  docker compose start closerd > /dev/null
  wait_ready "$CLOSERD" 60 > /dev/null
  after2=$(started_at closerd)

  if [ -n "$after1" ] && [ "$before1" != "$after1" ] && [ "$before2" != "$after2" ]; then
    LANDED=true
  fi
  ev_str startedAtBefore "$before1"
  ev_str startedAtAfter "$after1"
  ev_str startedAtBeforeSecondKill "$before2"
  ev_str startedAtAfterSecondKill "$after2"

  wait_for_load
  converge
  collect_worker_counters Final
  collect_db_evidence
}

# Convergence is a property of the time between two instants, and it is measured
# after the load — while k6 is still ramping the sweep is still publishing
# (decisão 88). K6_NAME arrives from run-cell.sh; without it there is no load to
# wait for and the measurement starts at once.
wait_for_load() {
  local name="${K6_NAME:-}" deadline=$(($(date +%s) + 300))
  [ -n "$name" ] || return 0
  log "esperando a carga terminar"
  while [ "$(date +%s)" -lt "$deadline" ]; do
    docker ps --format '{{.Names}}' 2> /dev/null | grep -qx "$name" || return 0
    nap 2
  done
}

converge() {
  local start b p
  start=$(date +%s)
  log "medindo a convergência: pending e backlog a zero"
  while [ $(($(date +%s) - start)) -lt "$CONVERGE_DEADLINE" ]; do
    b=$(metric "$AUCTIOND" stream_backlog_entries)
    p=$(metric "$AUCTIOND" stream_pending_entries)
    if [ "${b:-x}" = "0" ] && [ "${p:-x}" = "0" ]; then
      local took=$(($(date +%s) - start))
      ev_raw converged true
      ev_num convergedAfterSeconds "$took"
      log "convergiu em ${took}s"
      return 0
    fi
    nap 2
  done
  ev_raw converged false
  ev_num convergedAfterSeconds -1
  log "NÃO convergiu em ${CONVERGE_DEADLINE}s"
}

# Kills the worker the instant the producer reports an entry delivered and not
# yet acknowledged: a message in the consumer's hand with no XACK behind it is
# the only state XAUTOCLAIM has anything to recover from, and it is what the
# second kill of this scenario exists to produce.
#
# The poll has no sleep in it because the window is milliseconds: ten entries,
# one in flight at a time (decisão 77), each an UPDATE. The producer is asked
# and not the consumer, for the same reason the gauges live there — the process
# being killed cannot be the one reporting on it (decisão 73).
#
# When the deadline closes with no catch, the counters of this process are read
# before the kill instead. A reading taken after it is a reading of its
# successor, which answers zero and makes the zero look like evidence.
kill_on_pending() {
  local deadline=$(($(date +%s) + $1)) p
  while [ "$(date +%s)" -lt "$deadline" ]; do
    p=$(metric "$AUCTIOND" stream_pending_entries)
    if [ -n "$p" ] && [ "$p" != "0" ]; then
      ev_raw killedWithMessageInHand true
      log "pending=$p: matando o closerd com a mensagem na mão"
      return 0
    fi
  done
  ev_raw killedWithMessageInHand false
  collect_worker_counters BeforeSecondKill
}

# The worker's own counters, taken from whichever process is alive right now.
# They are recorded per reading and never merged: a counter that reset is the
# statement, and adding two of them up would erase it.
collect_worker_counters() {
  local applied already gone early
  ev_num "claimed$1" "$(metric "$CLOSERD" stream_claimed_total)"
  applied=$(metric "$CLOSERD" auction_closings_total 'result="applied"')
  already=$(metric "$CLOSERD" auction_closings_total 'result="already_closed"')
  gone=$(metric "$CLOSERD" auction_closings_total 'result="gone"')
  early=$(metric "$CLOSERD" auction_closings_total 'result="early"')
  ev_raw "closings$1" "$(jq -n --argjson a "${applied:-0}" --argjson c "${already:-0}" \
    --argjson g "${gone:-0}" --argjson e "${early:-0}" \
    '{applied: $a, already_closed: $c, gone: $g, early: $e}')"
}

# The database at the end, which is the only source that does not restart. The
# last two are the columns I4 exists for, cobradas here with auctions actually
# expiring under load for the first time in the project (decisão 87).
collect_db_evidence() {
  ev_num backlogPeak "${BACKLOG_PEAK:-0}"
  ev_num pendingPeak "${PENDING_PEAK:-0}"
  ev_num closedRows "$(pg "SELECT count(*) FROM auctions WHERE status = 'closed'")"
  ev_num lateBids "$(pg "SELECT count(*) FROM bids b JOIN auctions a ON a.id = b.auction_id WHERE b.created_at > a.ends_at")"
  ev_num closedBeforeEndsAt "$(pg "SELECT count(*) FROM auctions WHERE closed_at < ends_at")"
}

# The only engine with state that dies with the process. The counter is read on
# both sides of the kill because a counter only walks backwards in a new
# process — a live one cannot (decisão 84).
scenario_auctiond_kill() {
  TARGET=auctiond
  local before after cbefore cafter ready
  at 50
  cbefore=$(metric "$AUCTIOND" bid_confirm_duration_seconds_count "strategy=\"$STRATEGY\"")
  before=$(started_at auctiond)

  step kill auctiond
  docker kill -s KILL "$(cid auctiond)" > /dev/null
  at 53
  step start auctiond
  docker compose start auctiond > /dev/null
  ready=$(wait_ready "$AUCTIOND" 60)
  after=$(started_at auctiond)
  at 55
  cafter=$(metric "$AUCTIOND" bid_confirm_duration_seconds_count "strategy=\"$STRATEGY\"")

  # landed is about the injection, not about its consequence: the process died
  # and came back, proved from outside. The counter corroborates it, and it is
  # written down whole so the reader can check the claim against the number.
  if [ -n "$after" ] && [ "$before" != "$after" ] &&
    [ "$(awk -v a="${cbefore:-0}" -v b="${cafter:-0}" 'BEGIN { print (b <= a) ? 1 : 0 }')" = "1" ]; then
    LANDED=true
  fi
  ev_str startedAtBefore "$before"
  ev_str startedAtAfter "$after"
  ev_num confirmCountBefore "${cbefore:-0}"
  ev_num confirmCountAfter "${cafter:-0}"
  ev_raw counterRegressed "$(awk -v a="${cbefore:-0}" -v b="${cafter:-0}" 'BEGIN { print (b < a) ? "true" : "false" }')"
  ev_num readyAfterSeconds "${ready:--1}"
}

# Three promises written in three different etapas, charged at once: idempotency
# fails closed (decisão 36), the process stays alive with /readyz red and
# /healthz green, and the two queue gauges go SILENT rather than to zero,
# because the XINFO GROUPS of the scrape fails (decisões 59 and 73).
scenario_redis_pause() {
  TARGET=redis
  local abefore aafter paused gauges ready
  at 50
  abefore=$(metric "$AUCTIOND" bid_outcomes_total 'outcome="accepted"')

  step pause redis
  docker pause "$(cid redis)" > /dev/null
  local paused_at
  paused_at=$(date +%s)
  nap 1
  paused=$(docker inspect -f '{{.State.Paused}}' "$(cid redis)" 2> /dev/null || echo false)
  ev_raw paused "${paused:-false}"
  ev_num healthzDuringPause "$(http_code "$AUCTIOND/healthz")"
  # Asked in the background, and the pause is not held for it. The 503 that
  # names the redis check costs the client's ping retries to arrive, and under
  # five hundred VUs it also waits on a connection pool every one of them is
  # blocked on — waiting for it inline turned a five second pause into sixteen,
  # which is the injector widening the window it exists to observe.
  #
  # The request is issued inside the window, so a 503 can only have been caused
  # by the frozen Redis. readyzAnsweredAfterSeconds says whether the answer beat
  # the unpause, and nothing here asserts that it did.
  local readyz_out
  readyz_out=$(mktemp)
  READYZ_FILE="$readyz_out"
  probe_async "$AUCTIOND/readyz" 30 "$readyz_out"
  # Not zero: absent. A panel showing an empty queue during a Redis blackout
  # would be lying in exactly the incident it exists to cover.
  gauges=$(curl -s --max-time 5 "$AUCTIOND/metrics" 2> /dev/null |
    grep -cE '^(stream_backlog_entries|stream_pending_entries) ' || true)
  ev_num queueGaugeLinesDuringPause "${gauges:-0}"
  if [ "${paused:-false}" = "true" ]; then LANDED=true; fi

  at 55
  # Held past the five seconds of RF05 when the probe has not answered yet:
  # under load the readiness ping first waits for a pool connection that every
  # blocked request is holding, so the 503 that names the check costs several
  # seconds to arrive — and unpausing before it lands replaces the evidence with
  # a 200 answered by a Redis that had already come back. pauseSeconds records
  # the window that really happened, and nothing here rounds it down to five.
  wait_probe "$readyz_out" 10
  step unpause redis
  docker unpause "$(cid redis)" > /dev/null
  ev_num pauseSeconds "$(($(date +%s) - paused_at))"

  wait "$READYZ_PID" 2> /dev/null || :
  READYZ_PID=""
  ev_num readyzDuringPause "$(sed -n 2p "$readyz_out")"
  ev_str readyzBodyDuringPause "$(sed -n 1p "$readyz_out")"
  ev_num readyzAnsweredAfterSeconds "$(sed -n 3p "$readyz_out")"
  rm -f "$readyz_out"
  READYZ_FILE=""
  ready=$(wait_ready "$AUCTIOND" 60)
  at 56
  aafter=$(metric "$AUCTIOND" bid_outcomes_total 'outcome="accepted"')
  ev_num acceptedBefore "${abefore:-0}"
  ev_num acceptedAfter "${aafter:-0}"
  # Written down as a number and never asserted to be zero: a request already
  # inside the handler when Redis froze completes normally, and that is right.
  ev_num acceptedDuringPause "$(awk -v a="${abefore:-0}" -v b="${aafter:-0}" 'BEGIN { print b - a }')"
  ev_num readyAfterSeconds "${ready:--1}"
  ev_num queueGaugeLinesAfterUnpause "$(curl -s --max-time 5 "$AUCTIOND/metrics" 2> /dev/null |
    grep -cE '^(stream_backlog_entries|stream_pending_entries) ' || true)"
}

# A row lock held from outside, never pg_sleep on N connections: what has to fill
# is the pool of the auctiond, and the honest way to fill it is to make the work
# it already does take longer (decisão 90). The transaction ends in ROLLBACK,
# always, and it is the only SELECT ... FOR UPDATE in this file.
scenario_pool_saturation() {
  TARGET=postgres
  local aid ebefore eduring eafter abefore aduring locks
  aid=$(jq -r '.[0].id' bench/auctions.json 2> /dev/null || echo "")
  if [ -z "$aid" ] || [ "$aid" = "null" ]; then
    log "bench/auctions.json não tem leilão: abortando"
    ev_str abortedBecause "bench/auctions.json sem leilão"
    return 1
  fi

  at 50
  ebefore=$(metric "$AUCTIOND" db_pool_empty_acquire_total)
  abefore=$(metric "$AUCTIOND" db_pool_conns 'state="acquired"')

  step lock postgres
  docker compose exec -T -e PGAPPNAME=chaos-inject postgres \
    psql -U "$PGUSER_" -d "$PGDB_" -v ON_ERROR_STOP=1 -qtA \
    -c "BEGIN; SELECT id FROM auctions WHERE id = '$aid' FOR UPDATE; SELECT pg_sleep($LOCK_HOLD); ROLLBACK;" \
    > /dev/null 2>&1 &
  LOCK_PID=$!

  nap 3
  # Scoped to this transaction by application_name: the pessimistic engine takes
  # the same lock mode on the same table, and a count that could not tell them
  # apart would prove nothing.
  locks=$(pg "SELECT count(*) FROM pg_locks l
                JOIN pg_stat_activity a ON a.pid = l.pid
                JOIN pg_class c ON c.oid = l.relation
               WHERE a.application_name = 'chaos-inject'
                 AND c.relname = 'auctions' AND l.granted")
  ev_num rowLocksHeld "${locks:-0}"
  ev_num waitingLocks "$(pg "SELECT count(*) FROM pg_locks WHERE NOT granted" || echo 0)"
  ev_num healthzDuringLock "$(http_code "$AUCTIOND/healthz")"
  # Asked in the background and read after the lock is gone. Decisão 90 expected
  # a possible 503 here, because the Postgres probe takes a connection from the
  # same pool; what a synchronous probe records instead is its own timeout,
  # which says more about the injector than about the design. The code and the
  # seconds it took are written down and not judged.
  local readyz_out
  readyz_out=$(mktemp)
  READYZ_FILE="$readyz_out"
  probe_async "$AUCTIOND/readyz" 30 "$readyz_out"
  eduring=$(metric "$AUCTIOND" db_pool_empty_acquire_total)
  aduring=$(metric "$AUCTIOND" db_pool_conns 'state="acquired"')
  if [ "${locks:-0}" -gt 0 ]; then LANDED=true; fi

  at 58
  wait "$LOCK_PID" 2> /dev/null || :
  LOCK_PID=""
  step rollback postgres

  at 60
  wait "$READYZ_PID" 2> /dev/null || :
  READYZ_PID=""
  ev_num readyzDuringLock "$(sed -n 2p "$readyz_out")"
  ev_str readyzBodyDuringLock "$(sed -n 1p "$readyz_out")"
  ev_num readyzAnsweredAfterSeconds "$(sed -n 3p "$readyz_out")"
  rm -f "$readyz_out"
  READYZ_FILE=""
  eafter=$(metric "$AUCTIOND" db_pool_empty_acquire_total)
  ev_num emptyAcquireBefore "${ebefore:-0}"
  ev_num emptyAcquireDuring "${eduring:-0}"
  ev_num emptyAcquireAfter "${eafter:-0}"
  ev_num emptyAcquireDelta "$(awk -v a="${ebefore:-0}" -v b="${eafter:-0}" 'BEGIN { print b - a }')"
  ev_num acquiredBefore "${abefore:-0}"
  ev_num acquiredDuring "${aduring:-0}"
  ev_num acquiredAfter "$(metric "$AUCTIOND" db_pool_conns 'state="acquired"' || echo 0)"
  ev_num poolMax "$(metric "$AUCTIOND" db_pool_conns 'state="max"' || echo 0)"
}

# ---------------------------------------------------------------------------

[ -n "$SCENARIO_NAME" ] && [ -n "$RESULTS_DIR" ] || usage
case "$SCENARIO_NAME" in
  closerd-kill | closerd-kill-shard | auctiond-kill | redis-pause | pool-saturation) ;;
  *)
    echo "chaos/inject.sh: cenário desconhecido: $SCENARIO_NAME" >&2
    usage
    ;;
esac

trap cure EXIT
trap 'log "sinal recebido"; exit 143' TERM INT

log "início · estratégia=$STRATEGY · resultados=$RESULTS_DIR"
case "$SCENARIO_NAME" in
  closerd-kill | closerd-kill-shard) scenario_closerd_kill ;;
  auctiond-kill) scenario_auctiond_kill ;;
  redis-pause) scenario_redis_pause ;;
  pool-saturation) scenario_pool_saturation ;;
esac
