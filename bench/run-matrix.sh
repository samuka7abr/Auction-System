#!/usr/bin/env bash
# The 36 cells of the matrix, the control, and the aggregation — the loop the
# whole project was built to run.
#
# It adds no mechanism. `bench/run-cell.sh` is the brick and does not change
# behaviour here; what this script puts around it is the recreate of auctiond
# between cells, the proof of which engine is actually up before any load, the
# stop at the first non-green cell, and the resume.
#
# Automation calls this script and never `make matrix`: GNU make collapses any
# recipe failure into its own exit code, 2, and the three codes this loop must
# keep apart — a violated invariant (1), a cell that could not be verified (2)
# and a breached threshold (99) — are exactly the information whoever resumes
# the matrix needs (decisão 93).
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env ]; then
  while IFS= read -r line; do
    case "$line" in '' | \#*) continue ;; esac
    key="${line%%=*}"
    [ -n "${!key:-}" ] || export "${line?}"
  done < .env
fi

# code_of, wait_ready, wait_strategy, AUCTIOND and CLOSERD. The same file the
# chaos loop reads, and for the reason written there (decisão 109).
# shellcheck source=bench/wait.sh
. bench/wait.sh

# Nothing closes mid-cell, the increment is the one every etapa measured, and
# CHAOS is empty in all 36: a cell with a failure injected into it measured
# another system (decisão 92).
ENDS_IN="${ENDS_IN:-30m}"
MIN_INCREMENT="${MIN_INCREMENT:-100}"

# full is the 36 cells and the control. slice is the main graph and nothing
# else: three strategies over three levels of contention, at one scenario and
# one policy, plus the control — ten cells that fit in one working session
# (spec 02). A second script would be a copy of this one whose pre-flight and
# whose rm -rf guards drift apart at the first fix (decisão 109).
PLAN_KIND="${PLAN:-full}"
case "$PLAN_KIND" in
  full | slice) ;;
  *) echo "run-matrix: PLAN é full ou slice, não '$PLAN_KIND'" >&2; exit 2 ;;
esac

# The budget the harness gives itself per cell, passed through to run-cell.sh.
# Empty is every matrix before this one: no timeout anywhere.
CELL_BUDGET="${CELL_BUDGET:-}"
if [ -n "$CELL_BUDGET" ]; then
  case "$CELL_BUDGET" in
    *[!0-9]* | 0) echo "run-matrix: CELL_BUDGET em segundos inteiros maiores que zero, não '$CELL_BUDGET'" >&2; exit 2 ;;
  esac
fi

MATRIX="${MATRIX:-m$(date -u +%Y%m%dT%H%M%S)}"
RESUME="${RESUME:-}"
ROOT="bench/results/$MATRIX"

say() { printf '\n######## %s\n' "$*"; }
# Every abort of this script that is not a cell's own code is a 2: the matrix
# could not be produced, which is the absence of a result and never a result.
die() { echo "run-matrix: $*" >&2; exit 2; }

# ---------------------------------------------------------------- o plano ----
# Contention outermost, strategy innermost (decisão 94): the three engines
# compared at one point of the graph run adjacent in time, inside the same
# window of minutes. Grouping by strategy would save nine minutes and let any
# drift over ninety of them enter the graph wearing a strategy's name.
#
# The slice keeps the numbers the cells have in the full plan — 01, 02, 03, 13,
# 14, 15, 25, 26, 27 and 37 — so a cell measured in the slice is comparable byte
# for byte with the same cell of a future full matrix, and a slice directory can
# be resumed as a full one without renaming anything.
PLAN_ROWS=()
build_plan() {
  local n=0 auctions scenario policy strategy
  for auctions in 1 10 1000; do
    for scenario in ramp last_second_spike; do
      for policy in immediate jitter; do
        for strategy in optimistic pessimistic shard; do
          n=$((n + 1))
          # n is incremented before the skip, and that is the point: the slice
          # selects cells out of the plan of 37, it does not renumber them.
          if [ "$PLAN_KIND" = slice ] &&
            { [ "$scenario" != ramp ] || [ "$policy" != immediate ]; }; then
            continue
          fi
          PLAN_ROWS+=("$(printf '%02d' "$n")-$strategy-a$auctions-$scenario-$policy|$strategy|$auctions|$scenario|$policy")
        done
      done
    done
  done
  # Cell 37 is cell 01 again, last. The aggregator turns the distance between
  # the two into a number with a verdict (decisão 95).
  PLAN_ROWS+=("37-control-optimistic-a1-ramp-immediate|optimistic|1|ramp|immediate")
}

# ramp is 2m of load over a 24s warmup; last_second_spike is 15s over 3s. The
# rest is reset, seed, VACUUM, FLUSHALL, the checker and the recreate.
#
# A cell that does not converge ends at the budget instead of at its scenario,
# so with a budget set the estimate takes whichever is longer: announcing three
# minutes for a cell that will take four is the estimate lying to whoever
# decided, on the strength of it, to wait.
seconds_of() {
  local nominal
  case "$1" in ramp) nominal=190 ;; *) nominal=65 ;; esac
  if [ -n "$CELL_BUDGET" ] && [ "$CELL_BUDGET" -gt "$nominal" ]; then nominal=$CELL_BUDGET; fi
  echo "$nominal"
}

in_plan() {
  local entry
  for entry in "${PLAN_ROWS[@]}"; do
    [ "${entry%%|*}" = "$1" ] && return 0
  done
  return 1
}

# ------------------------------------------------------------- a retomada ----
cell_green() {
  local file="$ROOT/$1/checker.json"
  [ -f "$file" ] || return 1
  [ "$(jq -r 'if .exit == null then "" else .exit end' "$file" 2> /dev/null)" = "0" ]
}

# The only rm -rf in this PR whose path is computed, and it is not written
# without the three guards: a surviving client.json from a k6 that died halfway
# would be verified, next attempt, against a database another cell wrote — a
# false I5 inside the invariant that exists to catch a lost write (decisão 100).
purge_cell() {
  local name=$1 dir
  [ -n "$MATRIX" ] || die "MATRIX vazio: recusando apagar qualquer coisa"
  in_plan "$name" || die "célula fora do plano, recusando apagar: $name"
  dir="$ROOT/$name"
  case "$dir" in
    "bench/results/$MATRIX/"?*) ;;
    *) die "caminho fora de bench/results/$MATRIX, recusando apagar: $dir" ;;
  esac
  rm -rf -- "$dir"
}

# -------------------------------------------------------------- o pré-voo ----
service_up() {
  local id
  id=$(docker compose ps -q "$1" 2> /dev/null | head -1)
  [ -n "$id" ] || return 1
  [ "$(docker inspect -f '{{.State.Running}}' "$id" 2> /dev/null)" = "true" ]
}

preflight() {
  local head commit found=""

  # A matrix measured from a dirty tree is not reproducible: env.sh would write
  # dirty: true into all 37 cells and the aggregator would refuse it two hours
  # later (recusa R4). This is the cheapest refusal in the PR.
  [ -z "$(git status --porcelain 2> /dev/null)" ] ||
    die "árvore suja: uma matriz medida daqui não é reproduzível. Commite ou limpe antes"

  command -v jq > /dev/null || die "jq não encontrado"
  command -v curl > /dev/null || die "curl não encontrado"
  docker compose version > /dev/null 2>&1 || die "docker compose não encontrado"

  service_up postgres || die "postgres não está de pé: rode make up"
  service_up redis || die "redis não está de pé: rode make up"
  # It has taken part in every cell since decisão 80, so it goes up before the
  # first one and is waited for like auctiond.
  docker compose start closerd > /dev/null 2>&1 || die "não consegui subir o closerd"
  wait_ready "$CLOSERD" || die "closerd não respondeu /readyz 200"

  head=$(git rev-parse HEAD)
  if [ -d "$ROOT" ]; then
    [ -n "$RESUME" ] ||
      die "bench/results/$MATRIX já existe. Use RESUME=1 para continuar, ou escolha outro MATRIX"
    # Moved here from the aggregator, where the same refusal costs ninety
    # minutes instead of two seconds: two cells built from different commits
    # are not one matrix (decisão 100).
    for entry in "${PLAN_ROWS[@]}"; do
      commit=$(jq -r 'if .git.commit == null then "" else .git.commit end' \
        "$ROOT/${entry%%|*}/env.json" 2> /dev/null) || continue
      [ -n "$commit" ] || continue
      found=$commit
      [ "$commit" = "$head" ] ||
        die "retomada atravessando commit: $ROOT/${entry%%|*} foi medida em ${commit:0:12} e o HEAD é ${head:0:12}"
    done
    [ -n "$found" ] && say "retomando $MATRIX em ${head:0:12}"
  fi

  # A compile error found after cell 30 would be the most expensive kind.
  go build -o bin/matrix ./cmd/matrix || die "cmd/matrix não compila"
}

# ------------------------------------------------------------- as opções ----
DRY_RUN=""
ONLY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --only)
      [ $# -ge 2 ] || die "--only precisa de uma regex"
      ONLY=$2
      shift
      ;;
    *) die "opção desconhecida: $1 (só existem --dry-run e --only <regex>)" ;;
  esac
  shift
done

case "$MATRIX" in
  chaos-*) die "MATRIX não pode começar com chaos-: esse prefixo é do caos (decisão 92)" ;;
  *[!A-Za-z0-9._-]* | '' | .* ) die "MATRIX inválido: use letras, dígitos, ponto, hífen ou sublinhado" ;;
esac

build_plan

selected=()
total=0
for entry in "${PLAN_ROWS[@]}"; do
  IFS='|' read -r name strategy auctions scenario policy <<< "$entry"
  if [ -n "$ONLY" ] && ! printf '%s' "$name" | grep -Eq "$ONLY"; then continue; fi
  selected+=("$entry")
  total=$((total + $(seconds_of "$scenario")))
done
[ "${#selected[@]}" -gt 0 ] || die "--only '$ONLY' não casou com nenhuma célula"

plan_line() {
  IFS='|' read -r name strategy auctions scenario policy <<< "$1"
  printf '%-40s %-12s %-6s %-18s %-10s %s\n' \
    "$name" "$strategy" "a$auctions" "$scenario" "$policy" "$MATRIX/$name"
}

# --dry-run prints the plan and touches nothing — no preflight, no container, no
# directory. It stays 0 on a dirty tree on purpose: it measures nothing, so
# there is nothing for the tree to make irreproducible.
if [ -n "$DRY_RUN" ]; then
  for entry in "${selected[@]}"; do plan_line "$entry"; done
  exit 0
fi

trap 'echo; echo "run-matrix: interrompida. Retome com: MATRIX=$MATRIX RESUME=1 bench/run-matrix.sh"; exit 130' INT

preflight

say "$MATRIX · plano $PLAN_KIND · ${#selected[@]} célula(s) · estimativa $((total / 60))min"
for entry in "${selected[@]}"; do plan_line "$entry"; done

for entry in "${selected[@]}"; do
  IFS='|' read -r name strategy auctions scenario policy <<< "$entry"

  if [ -n "$RESUME" ] && cell_green "$name"; then
    printf '%-40s pulada\n' "$name"
    continue
  fi
  purge_cell "$name"

  say "$name · $strategy · $auctions leilão(ões) · $scenario · $policy"
  # up -d --build and not start: the strategy is an environment variable of the
  # container, so changing it means recreating. The recreate is also what makes
  # every cell open on a fresh heap, a fresh pool and empty shard inboxes —
  # decisão 13 applied to the process (decisão 94).
  STRATEGY="$strategy" docker compose up -d --build auctiond > /dev/null
  wait_ready "$AUCTIOND"
  wait_ready "$CLOSERD"
  # Not redundant with wait_ready: /readyz can answer 200 an instant before the
  # decorator has bound bid_confirm_duration_seconds{strategy}, and run-cell.sh
  # would then abort on its own pre-flight. Waiting here is the difference
  # between the loop waiting and the loop failing.
  wait_strategy "$strategy"

  code=0
  RUN="$MATRIX/$name" STRATEGY="$strategy" AUCTIONS="$auctions" \
    SCENARIO="$scenario" POLICY="$policy" ENDS_IN="$ENDS_IN" \
    MIN_INCREMENT="$MIN_INCREMENT" CELL_BUDGET="$CELL_BUDGET" \
    CHAOS="" bench/run-cell.sh || code=$?

  # The code leaves this script untranslated. 1 is a result about the engine and
  # deserves to be read before the machine does anything else; 2 is the absence
  # of a result; 99 is the limit of the system showing up, and lowering the
  # threshold to let the matrix finish would be choosing to finish over
  # measuring (decisões 101 and 102).
  if [ "$code" -ne 0 ]; then
    say "PAROU em $name com código $code"
    echo "retome com: MATRIX=$MATRIX RESUME=1 bench/run-matrix.sh" >&2
    exit "$code"
  fi
done

say "agregando $ROOT"
code=0
bin/matrix -dir "$ROOT" -plan "$PLAN_KIND" || code=$?
exit "$code"
