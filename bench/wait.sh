# shellcheck shell=bash
# The two waits both compose loops need, and the reason they live in one file.
#
# `wait_strategy` is not a formatting convenience: it encodes a fact — that
# bid_confirm_duration_seconds_count{strategy="..."} is the PROOF of which
# engine the process is running, and not what the compose file asked for. Two
# copies of that fact mean one of them is left behind the day the series is
# renamed, and the one left behind hangs for 90s and takes a two-hour matrix
# down with it (decisão 109).
#
# Sourced, never executed: no shebang, no `set -euo pipefail` of its own, and
# the caller's errexit is what decides whether a failed wait aborts. `check_cure`
# deliberately did NOT move here — it belongs to the chaos loop, and the matrix
# has nothing to cure.
#
# Both loads .env before reading this file, so the ports are already in the
# environment by the time these two defaults are evaluated.
AUCTIOND="${AUCTIOND_URL:-http://localhost:${HTTP_PORT:-8080}}"
# Read by the callers, not here: the matrix waits on it every cell and the chaos
# loop asks it for the cure.
# shellcheck disable=SC2034
CLOSERD="${CLOSERD_URL:-http://localhost:${CLOSERD_PORT:-8081}}"

# Who is complaining: `run-all` under the chaos loop — the same word the message
# carried before it moved — and `run-matrix` under the matrix.
WAIT_WHO="$(basename "$0" .sh)"

code_of() { curl -s -o /dev/null -m 5 -w '%{http_code}' "$1" 2> /dev/null || echo 000; }

wait_ready() {
  local url=$1 deadline=$(($(date +%s) + 90))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ "$(code_of "$url/readyz")" = "200" ]; then return 0; fi
    sleep 2
  done
  echo "$WAIT_WHO: $url/readyz never answered 200" >&2
  return 1
}

wait_strategy() {
  local deadline=$(($(date +%s) + 90))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if curl -sf -m 5 "$AUCTIOND/metrics" 2> /dev/null |
      grep -q "bid_confirm_duration_seconds_count{strategy=\"$1\"}"; then
      return 0
    fi
    sleep 2
  done
  echo "$WAIT_WHO: auctiond never published strategy=$1" >&2
  return 1
}
