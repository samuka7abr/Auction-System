#!/usr/bin/env bash
# The five chaos cells, in sequence, stopping at the first non-zero code.
#
# Automation calls this script and never `make chaos`: GNU make collapses any
# recipe failure into its own exit code, 2, and the checker distinguishes a
# violated invariant (1) from a cell that could not be verified (2) on purpose
# since etapa 1 — which is exactly the difference a loop needs to decide between
# stopping and re-running the cell (decisão 93).
#
# The RUN of every cell is prefixed with `chaos-`, and that prefix is the whole
# mechanism keeping these numbers out of the matrix: etapa 5 writes cell names,
# etapa 6 reads the matrix, and neither ever reads this prefix (decisão 92).
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env ]; then
  while IFS= read -r line; do
    case "$line" in '' | \#*) continue ;; esac
    key="${line%%=*}"
    [ -n "${!key:-}" ] || export "${line?}"
  done < .env
fi

# code_of, wait_ready, wait_strategy, and AUCTIOND/CLOSERD with them. The two
# waits used to live here; the matrix of etapa 5 needs the same two, with the
# same semantics, and a second copy of `wait_strategy` is a second copy of the
# fact that the strategy label on /metrics is the proof of which engine is up
# (decisão 109). check_cure stayed: it is the chaos loop's, and the matrix has
# nothing to cure.
# shellcheck source=bench/wait.sh
. bench/wait.sh

# scenario | strategy | ends-in | auctions, and the RUN is chaos-<name>.
# closerd-kill runs twice: the second time against the engine that decides the
# closing in memory, which had never met a real ends_at under load (decisão 87).
CELLS=(
  "closerd-kill|closerd-kill|optimistic|45s|10"
  "closerd-kill-shard|closerd-kill|shard|45s|10"
  "auctiond-kill|auctiond-kill|shard|30m|10"
  "redis-pause|redis-pause|optimistic|30m|10"
  "pool-saturation|pool-saturation|pessimistic|30m|1"
)

say() { printf '\n######## %s\n' "$*"; }

# Between scenarios, and never as an afterthought: a Redis left paused or a
# transaction left holding a row would poison every cell that comes after, and
# the poisoned cell would still look like a result.
check_cure() {
  local paused locks failed=0
  [ "$(code_of "$AUCTIOND/readyz")" = "200" ] || { echo "run-all: auctiond não voltou" >&2; failed=1; }
  [ "$(code_of "$CLOSERD/readyz")" = "200" ] || { echo "run-all: closerd não voltou" >&2; failed=1; }
  paused=$(docker inspect -f '{{.State.Paused}}' "$(docker compose ps -q redis | head -1)" 2> /dev/null || echo unknown)
  [ "$paused" = "false" ] || { echo "run-all: redis ficou pausado ($paused)" >&2; failed=1; }
  locks=$(docker compose exec -T postgres psql -U "${POSTGRES_USER:-auction}" -d "${POSTGRES_DB:-auction}" -qtA \
    -c "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'chaos-inject'" 2> /dev/null | tr -d '[:space:]' || echo 0)
  [ "${locks:-0}" = "0" ] || { echo "run-all: sobrou transação do injetor ($locks)" >&2; failed=1; }
  return "$failed"
}

for cell in "${CELLS[@]}"; do
  IFS='|' read -r name scenario strategy ends_in auctions <<< "$cell"
  run="chaos-$name"

  say "$run · $strategy · ENDS_IN=$ends_in · $auctions leilão(ões)"
  STRATEGY="$strategy" docker compose up -d --build auctiond > /dev/null
  docker compose start closerd > /dev/null
  wait_ready "$AUCTIOND"
  wait_ready "$CLOSERD"
  # /readyz answering 200 is not the same statement as "this process is running
  # the engine the cell asked for": the recreate can be ready a moment before the
  # decorator has bound bid_confirm_duration_seconds{strategy}, and run-cell.sh
  # then aborts on its own pre-flight. Asked here, in the same terms, so the loop
  # waits instead of failing.
  wait_strategy "$strategy"

  CHAOS="$scenario" RUN="$run" STRATEGY="$strategy" ENDS_IN="$ends_in" \
    AUCTIONS="$auctions" SCENARIO=ramp POLICY=immediate bench/run-cell.sh

  say "conferindo a cura antes do cenário seguinte"
  check_cure
done

say "os cinco cenários passaram"
