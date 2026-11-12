#!/usr/bin/env bash
# End-to-end test on real network devices. Boots two NanoNOS switches and an FRRouting router in
# Linux network namespaces, starts LabQ on them, and drives a day in the life of a lab through
# LabQ's API:
#
#   1. Three jobs contend for nanonos-1: an interop run, a fabric run with a link failover, and a
#      run triggered by a signed GitHub push webhook (in CI it sets a real commit status).
#   2. nanonos-2 crashes. LabQ retries the job, reports a lab error instead of a test failure,
#      and quarantines the switch after three strikes, without blaming nanonos-1.
#   3. A job queues for the quarantined switch. LabQ is killed with SIGKILL and restarted; its
#      write-ahead log brings back the queue and the quarantine.
#   4. An operator restarts nanonos-2 and returns it to service. The queued job runs and passes.
#
# Run as root. NANONOS is a NanoNOS build directory (nanonosd and nosctl); LABQ is the labq
# binary (default: labq at the repository root). Needs iproute2, ethtool, FRRouting, jq, curl and
# openssl.
#
#   NANONOS=~/nanonos/build sudo -E scripts/lab.sh
#
# LAB_KEEP=1 leaves the lab and its dashboard (http://127.0.0.1:8080) running until Ctrl-C.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
NANONOS=${NANONOS:?set NANONOS to a NanoNOS build directory}
LABQ=${LABQ:-$ROOT/labq}
RUN=/tmp/labq-lab # configs/lab.json and configs/frr-1.conf refer to this directory
API=http://127.0.0.1:8080
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
REPO=${GITHUB_REPOSITORY:-local/labq}
SHA=${HEAD_SHA:-${GITHUB_SHA:-1111111111111111111111111111111111111111}}
export LABQ_WEBHOOK_SECRET=${LABQ_WEBHOOK_SECRET:-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')}

[ "$(id -u)" = 0 ] || { echo "run as root: NANONOS=... sudo -E $0"; exit 1; }
for tool in ip ethtool jq curl openssl vtysh /usr/lib/frr/zebra /usr/lib/frr/bgpd "$NANONOS/nanonosd" "$NANONOS/nosctl" "$LABQ"; do
  command -v "$tool" > /dev/null || { echo "missing: $tool"; exit 1; }
done

NAMESPACES=()
declare -A PID
LABQ_PID=

note() { printf '\n=== %s\n' "$*"; }
pass() {
  echo "PASS: $*"
  echo "- PASS: $*" >> "$SUMMARY"
}
fail() {
  echo "FAIL: $*"
  echo "- FAIL: $*" >> "$SUMMARY"
  for f in "$RUN"/*.log "$RUN"/frr/*.log; do
    if [ -e "$f" ]; then
      echo "===== $f (last 40 lines)"
      tail -n 40 "$f"
    fi
  done
  exit 1
}

cleanup() {
  set +e
  [ -n "$LABQ_PID" ] && kill "$LABQ_PID" 2> /dev/null
  for pid in "${PID[@]}"; do kill "$pid" 2> /dev/null; done
  for p in "$RUN"/frr/*.pid; do [ -e "$p" ] && kill "$(cat "$p")" 2> /dev/null; done
  sleep 0.5
  for ns in "${NAMESPACES[@]}"; do ip netns del "$ns" 2> /dev/null; done
}
trap cleanup EXIT

wait_for() { # wait_for DESCRIPTION TIMEOUT_S command...
  local what=$1 timeout=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" > /dev/null 2>&1; do
    ((SECONDS < deadline)) || fail "timed out after ${timeout} s waiting for $what"
    sleep 0.25
  done
}

# --- The lab -------------------------------------------------------------------------------

make_ns() {
  ip netns del "$1" 2> /dev/null || true
  ip netns add "$1"
  NAMESPACES+=("$1")
  ip -n "$1" link set lo up
  ip netns exec "$1" sysctl -qw net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1
}

# cable NS1 IF1 NS2 IF2: a veth pair. Offloads are off so that frames crossing a userspace switch
# carry complete checksums.
cable() {
  ip link add "$2" netns "$1" type veth peer name "$4" netns "$3"
  local end ns dev
  for end in "$1:$2" "$3:$4"; do
    ns=${end%%:*} dev=${end#*:}
    ip netns exec "$ns" ethtool -K "$dev" rx off tx off tso off gso off gro off > /dev/null 2>&1 || true
    ip -n "$ns" link set "$dev" up
  done
}

start_switch() { # start_switch NAME NAMESPACE
  ip netns exec "$2" "$NANONOS/nanonosd" -c "$ROOT/configs/$1.conf" -s "$RUN/$1.sock" -t 2 >> "$RUN/$1.log" 2>&1 &
  PID[$1]=$!
  wait_for "$1's CLI" 15 "$NANONOS/nosctl" -s "$RUN/$1.sock" show version
}

start_frr() {
  local dir=$RUN/frr
  mkdir -p "$dir"
  cp "$ROOT/configs/frr-1.conf" "$dir/bgpd.conf"
  chown -R frr:frr "$dir"
  local common=(-z "$dir/zserv.api" --vty_socket "$dir" -u frr -g frr)
  ip netns exec lq-frr /usr/lib/frr/zebra -d -i "$dir/zebra.pid" "${common[@]}"
  ip netns exec lq-frr /usr/lib/frr/bgpd -d -f "$dir/bgpd.conf" -i "$dir/bgpd.pid" "${common[@]}"
  wait_for "FRRouting's vty" 15 ip netns exec lq-frr vtysh --vty_socket "$dir" -c 'show bgp summary'
}

start_labq() {
  "$LABQ" serve -config "$ROOT/configs/lab.json" -addr 127.0.0.1:8080 -wal "$RUN/labq.wal" \
    -webhook-preset interop -webhook-team ci -test-wait 60s >> "$RUN/labq.log" 2>&1 &
  LABQ_PID=$!
  wait_for "LabQ to serve" 15 curl -fs "$API/healthz"
}

# --- LabQ's API --------------------------------------------------------------------------------

api() { # api METHOD PATH [JSON]
  local args=(-sS --fail-with-body -X "$1" -H 'Content-Type: application/json')
  if [ $# -ge 3 ]; then args+=(--data-binary "$3"); fi
  curl "${args[@]}" "$API$2"
}

submit() { # submit NAME PRESET TEAM PRIORITY: queues a job and prints its ID
  api POST /api/v1/jobs "$(jq -nc --arg n "$1" --arg p "$2" --arg t "$3" --argjson pr "$4" \
    '{name: $n, preset: $p, team: $t, priority: $pr}')" | jq -r .id
}

job() { api GET "/api/v1/jobs/$1"; }
job_state() { job "$1" | jq -r .state; }
finished() {
  local s
  s=$(job_state "$1") || return 1
  [ "$s" != queued ] && [ "$s" != running ]
}
device_status() { api GET /api/v1/devices | jq -r --arg d "$1" '.devices[] | select(.id == $d) | .status'; }

expect_job() { # expect_job ID STATE TIMEOUT_S
  wait_for "job $1 to finish" "$3" finished "$1"
  local state
  state=$(job_state "$1")
  if [ "$state" != "$2" ]; then
    job "$1" | jq .
    fail "job $1 ended '$state', expected '$2'"
  fi
}

failover() { # prints the drop-detection and recovery seconds of a job's last link-failover test
  job "$1" | jq -r '[.results[] | select(.test == "link-failover")] | last | .measurements |
    "\((.detect_s * 10 | round) / 10) \((.recover_s * 10 | round) / 10)"'
}

# github_push sends a push delivery for $SHA, signed like GitHub signs it, and prints the job ID.
github_push() {
  local body sig
  body=$(jq -nc --arg repo "$REPO" --arg sha "$SHA" --arg ref "${GITHUB_REF:-refs/heads/main}" \
    '{ref: $ref, after: $sha, deleted: false, repository: {full_name: $repo},
      head_commit: {url: "https://github.com/\($repo)/commit/\($sha)"}}')
  sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$LABQ_WEBHOOK_SECRET" -r | cut -d' ' -f1)
  curl -sS --fail-with-body -X POST -H 'Content-Type: application/json' -H 'X-GitHub-Event: push' \
    -H "X-Hub-Signature-256: sha256=$sig" --data-binary "$body" "$API/api/v1/github/webhook" | jq -r .job
}

commit_status_success() {
  local state
  state=$(curl -fsS -H "Authorization: Bearer $GITHUB_TOKEN" -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/$REPO/commits/$SHA/statuses" |
    jq -r '[.[] | select(.context == "labq/qualification")] | first | .state // "none"')
  [ "$state" = success ]
}

# --- The scenario ------------------------------------------------------------------------------

rm -rf "$RUN"
mkdir -p "$RUN"
echo "### LabQ on real devices: 2 NanoNOS switches and an FRRouting router" >> "$SUMMARY"

note "Booting the lab: nanonos-1 is cabled to frr-1 and to nanonos-2"
for ns in lq-n1 lq-n2 lq-frr; do make_ns "$ns"; done
cable lq-n1 n1-frr lq-frr eth0
cable lq-n1 n1-n2 lq-n2 n2-n1
ip -n lq-frr addr add 10.0.0.1/31 dev eth0
start_frr
start_switch nanonos-1 lq-n1
start_switch nanonos-2 lq-n2
start_labq
frr_version=$(ip netns exec lq-frr vtysh --vty_socket "$RUN/frr" -c 'show version' | grep -o 'FRRouting [^ ]*' | head -1)
pass "lab up: nanonos-1 and nanonos-2 (NanoNOS) and frr-1 ($frr_version) in network namespaces, managed by LabQ"

note "1. Three jobs contend for nanonos-1"
j1=$(submit interop-nightly interop wan 1)
j2=$(submit fabric-nightly fabric datacenter 1)
j3=$(github_push)
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-GitHub-Event: push' \
  -H 'X-Hub-Signature-256: sha256=00' --data-binary '{}' "$API/api/v1/github/webhook")
[ "$code" = 401 ] || fail "a webhook with a bad signature got HTTP $code, expected 401"
pass "a webhook with a bad signature was rejected (HTTP 401)"
for j in "$j1" "$j2" "$j3"; do expect_job "$j" passed 180; done
read -r detect recover < <(failover "$j2")
pass "interop ($j1), fabric ($j2) and GitHub push ($j3) jobs took turns on nanonos-1 and passed; in the failover, the peer saw the session drop in ${detect} s and it was Established again ${recover} s after the port came back"
if [ -n "${GITHUB_TOKEN:-}" ] && [ "$REPO" != local/labq ]; then
  wait_for "a success status on $SHA" 60 commit_status_success
  pass "GitHub shows the labq/qualification status as success on commit ${SHA:0:7}"
fi

note "2. nanonos-2 crashes"
kill -9 "${PID[nanonos-2]}"
wait "${PID[nanonos-2]}" 2> /dev/null || true
j4=$(submit fabric-during-outage fabric datacenter 1)
expect_job "$j4" error 60
attempts=$(job "$j4" | jq .attempts)
[ "$attempts" = 3 ] || fail "job $j4 ran $attempts times, expected 3"
[ "$(device_status nanonos-2)" = quarantined ] || fail "nanonos-2 is $(device_status nanonos-2), expected quarantined"
[ "$(device_status nanonos-1)" = available ] || fail "nanonos-1 was blamed for nanonos-2's crash"
pass "job $j4 ran 3 times and ended as a lab error, not a test failure; nanonos-2 was quarantined and nanonos-1 was not blamed"

note "3. A job waits for the quarantined switch; LabQ is killed with SIGKILL and restarted"
j5=$(submit fabric-after-repair fabric datacenter 2)
sleep 2
[ "$(job_state "$j5")" = queued ] || fail "job $j5 is $(job_state "$j5"), expected queued"
kill -9 "$LABQ_PID"
wait "$LABQ_PID" 2> /dev/null || true
start_labq
[ "$(job_state "$j5")" = queued ] || fail "job $j5 did not survive the restart"
[ "$(device_status nanonos-2)" = quarantined ] || fail "the quarantine did not survive the restart"
events=$(grep 'replayed write-ahead log' "$RUN/labq.log" | tail -1 | grep -o 'events=[0-9]*' | cut -d= -f2)
pass "after SIGKILL, LabQ restarted and replayed $events events from its write-ahead log: job $j5 is still queued and nanonos-2 is still quarantined"

note "4. An operator restarts nanonos-2 and returns it to service"
start_switch nanonos-2 lq-n2
api POST /api/v1/devices/nanonos-2/status '{"status": "available", "note": "nanonosd restarted after a crash"}' > /dev/null
expect_job "$j5" passed 180
read -r detect2 recover2 < <(failover "$j5")
pass "job $j5 ran once nanonos-2 was back and passed (failover: drop seen in ${detect2} s, Established again after ${recover2} s)"

api GET /api/v1/export/results.csv > "$RUN/results.csv"
api GET /api/v1/export/jobs.ndjson > "$RUN/jobs.ndjson"
{
  echo
  echo "| Job | Name | Team | Result | Attempts | Devices | Summary |"
  echo "|---|---|---|---|---:|---|---|"
  api GET /api/v1/jobs | jq -r '.jobs | sort_by(.id | ltrimstr("j-") | tonumber) | .[] |
    "| \(.id) | \(.name) | \(.team) | \(.state) | \(.attempts) | \((.assignment // {}) | to_entries | map("\(.key): \(.value)") | join(", ")) | \((.message // "") | gsub("\\|"; "/")) |"'
  echo
  echo "| Device | Status | Jobs run | Failures in a row |"
  echo "|---|---|---:|---:|"
  api GET /api/v1/devices | jq -r '.devices[] | "| \(.id) | \(.status) | \(.jobsRun) | \(.failures) |"'
} >> "$SUMMARY"

"$NANONOS/nosctl" -s "$RUN/nanonos-1.sock" show bgp summary
"$NANONOS/nosctl" -s "$RUN/nanonos-1.sock" show lldp neighbors
ip netns exec lq-frr vtysh --vty_socket "$RUN/frr" -c 'show bgp ipv4 unicast summary'
echo "All lab scenarios passed."

if [ "${LAB_KEEP:-0}" = 1 ]; then
  echo "The lab is still up. Dashboard: $API (Ctrl-C to stop)"
  wait "$LABQ_PID"
fi
