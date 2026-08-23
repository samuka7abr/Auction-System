#!/usr/bin/env bash
# Writes, to stdout, everything a published number depends on.
#
# Redis is in here from etapa 2 on: the idempotency middleware put it in the hot
# path of the three engines, and a service with no limit on a loaded host is a
# hidden variable inside a published number. The closerd joins them in etapa 4
# for the same reason: it shares the machine with the benchmark in every cell,
# so it goes into env.json or it becomes a hidden variable (decisão 80).
#
# The limits come from `docker inspect` and not from the YAML, for the same
# reason as C1 of spec 01: a limit the Compose silently ignored is not a limit,
# and the env.json of a number that gets published cannot lie about it.
set -euo pipefail
cd "$(dirname "$0")/.."

container() { docker compose ps -q "$1" 2> /dev/null | head -1; }

limits_of() {
  local id
  id=$(container "$1")
  if [ -z "$id" ]; then
    jq -n '{cpus: 0, memoryBytes: 0}'
    return
  fi
  docker inspect -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}' "$id" |
    jq -Rn 'input | split(" ") | {cpus: (.[0] | tonumber / 1e9), memoryBytes: (.[1] | tonumber)}'
}

image_of() {
  local id
  id=$(container "$1")
  [ -n "$id" ] && docker inspect -f '{{.Config.Image}}' "$id" || echo ""
}

commit=$(git rev-parse HEAD 2> /dev/null || echo unknown)
# A number measured from a dirty tree is not reproducible, and saying so is
# cheaper than discovering it later.
if [ -n "$(git status --porcelain 2> /dev/null)" ]; then dirty=true; else dirty=false; fi

# The k6 container is gone by now (--rm), so its limits and image arrive from
# run-cell.sh, which inspected it while it was still alive.
jq -n \
  --arg run "${RUN:-}" \
  --arg startedAt "${STARTED_AT:-}" \
  --arg finishedAt "${FINISHED_AT:-}" \
  --arg commit "$commit" \
  --argjson dirty "$dirty" \
  --arg strategy "${STRATEGY:-optimistic}" \
  --argjson auctions "${AUCTIONS:-1}" \
  --arg policy "${POLICY:-immediate}" \
  --arg scenario "${SCENARIO:-smoke}" \
  --argjson poolSize "${DB_POOL_SIZE:-25}" \
  --arg kernel "$(uname -sr)" \
  --argjson hostCpus "$(nproc)" \
  --argjson hostMemory "$(awk '/MemTotal/ { print $2 * 1024 }' /proc/meminfo)" \
  --arg pgImage "$(image_of postgres)" \
  --arg auctiondImage "$(image_of auctiond)" \
  --arg closerdImage "$(image_of closerd)" \
  --arg k6Image "${K6_IMAGE:-}" \
  --arg pgVersion "$(docker compose exec -T postgres postgres --version 2> /dev/null | tr -d '\r' || echo unknown)" \
  --arg goVersion "$(go version)" \
  --arg k6Version "$(docker compose run --rm --no-deps k6 version 2> /dev/null | tr -d '\r' | head -1 || echo unknown)" \
  --argjson auctiondLimits "$(limits_of auctiond)" \
  --argjson postgresLimits "$(limits_of postgres)" \
  --argjson redisLimits "$(limits_of redis)" \
  --argjson closerdLimits "$(limits_of closerd)" \
  --argjson k6Cpus "${K6_CPUS:-0}" \
  --argjson k6Memory "${K6_MEM_BYTES:-0}" \
  --argjson cpuPctPeak "${GENERATOR_CPU_PCT_PEAK:-0}" \
  '{
    run: $run,
    startedAt: $startedAt,
    finishedAt: $finishedAt,
    git: {commit: $commit, dirty: $dirty},
    cell: {strategy: $strategy, auctions: $auctions, policy: $policy,
           scenario: $scenario, poolSize: $poolSize},
    host: {kernel: $kernel, cpus: $hostCpus, memoryBytes: $hostMemory},
    images: {postgres: $pgImage, auctiond: $auctiondImage, closerd: $closerdImage,
             k6: $k6Image},
    versions: {postgres: $pgVersion, go: $goVersion, k6: $k6Version},
    limits: {auctiond: $auctiondLimits, postgres: $postgresLimits,
             redis: $redisLimits, closerd: $closerdLimits,
             k6: {cpus: $k6Cpus, memoryBytes: $k6Memory}},
    generator: {cpuPctPeak: $cpuPctPeak, saturated: ($cpuPctPeak > 90)}
  }'
