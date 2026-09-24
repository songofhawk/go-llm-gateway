#!/usr/bin/env bash
# 本机可重复压测：负载端 -> 网关 -> 两个 HTTP Mock LLM，三个角色独立进程。
# 只管理本脚本启动的 PID；退出时停止所有子进程，保留采样和报告。
set -euo pipefail
cd "$(dirname "$0")/.."
GO="${GO:-go}"
OUT="${OUT:-artifacts/stress-$(date +%Y%m%d-%H%M%S)}"
if [ -e "$OUT" ]; then echo "output already exists: $OUT" >&2; exit 1; fi
mkdir -p "$OUT/bin"
OUT="$(cd "$OUT" && pwd)"
export GATEWAY_TOKEN="local-stress-only"
GW_PORT="${GW_PORT:-18080}"
DEBUG_PORT="${DEBUG_PORT:-16060}"
MOCK_A_PORT="${MOCK_A_PORT:-19090}"
MOCK_B_PORT="${MOCK_B_PORT:-19091}"
STRESS_HOST="${STRESS_HOST:-127.0.0.1}"
GW="http://$STRESS_HOST:$GW_PORT"
DEBUG="http://$STRESS_HOST:$DEBUG_PORT"
A="http://$STRESS_HOST:$MOCK_A_PORT"
B="http://$STRESS_HOST:$MOCK_B_PORT"
PIDS=()
stop_children() {
  for pid in "${PIDS[@]-}"; do [ -n "$pid" ] || continue; kill -TERM "$pid" 2>/dev/null || true; done
  for pid in "${PIDS[@]-}"; do [ -n "$pid" ] || continue; wait "$pid" 2>/dev/null || true; done
  PIDS=()
}
trap stop_children EXIT
trap 'stop_children; exit 130' INT TERM
"$GO" build -o "$OUT/bin/gateway" ./cmd/gateway
"$GO" build -o "$OUT/bin/mock-provider" ./cmd/mock-provider
"$GO" build -o "$OUT/bin/loadtest" ./cmd/loadtest
"$GO" version > "$OUT/environment.txt"
uname -a >> "$OUT/environment.txt"
cat > "$OUT/config.json" <<JSON
{"endpoints":[
 {"name":"a","base_url":"$A/v1","capacity":32},
 {"name":"b","base_url":"$B/v1","capacity":32}],
 "routes":{
  "economy":[[{"provider":"a","model":"mock-small"},{"provider":"b","model":"mock-small"}]],
  "balanced":[[{"provider":"a","model":"mock-medium"},{"provider":"b","model":"mock-medium"}]],
  "powerful":[[{"provider":"a","model":"mock-large"},{"provider":"b","model":"mock-large"}]]}}
JSON
ready() {
 local url="$1" pid="$2"
 for _ in $(seq 1 100); do
  if ! kill -0 "$pid" 2>/dev/null; then echo "child failed before readiness: $pid" >&2; return 1; fi
  if curl --noproxy '*' -fsS --max-time 1 -H "Authorization: Bearer $GATEWAY_TOKEN" "$url" >/dev/null 2>&1; then return 0; fi
  sleep .1
 done
 echo "readiness timeout: $url" >&2; return 1
}
fetch() { curl --noproxy '*' -fsS --max-time 30 "$1" -o "$2"; }
snapshot() {
 local dir="$1" stage="$2"
 fetch "$DEBUG/debug/pprof/heap?gc=1" "$dir/$stage-heap.pb.gz"
 fetch "$DEBUG/debug/stats" "$dir/$stage-stats.json"
 fetch "$DEBUG/debug/pprof/goroutine" "$dir/$stage-goroutine.pb.gz"
 fetch "$A/stats" "$dir/$stage-mock-a.json"
 fetch "$B/stats" "$dir/$stage-mock-b.json"
}
# 每个场景重启本脚本自己的进程，CPU/锁/分配画像不会与前一场景混在一起。
scenario() {
 local name="$1" mode="$2" concurrency="$3" delay="$4" chunks="$5" fault="$6" idle="$7"
 local stall="${8:--1}"
 if [[ " ${SCENARIOS:-unary long-stream overload cancel fallback jobs timeout} " != *" $name "* ]]; then return 0; fi
 local dir="$OUT/$name"
 mkdir -p "$dir"
 "$OUT/bin/mock-provider" -addr "$STRESS_HOST:$MOCK_A_PORT" -delay "$delay" -chunks "$chunks" -stall-after "$stall" -fail-every "$fault" >"$dir/mock-a.log" 2>&1 &
 local pa=$!; PIDS+=("$pa")
 "$OUT/bin/mock-provider" -addr "$STRESS_HOST:$MOCK_B_PORT" -delay "$delay" -chunks "$chunks" -stall-after "$stall" >"$dir/mock-b.log" 2>&1 &
 local pb=$!; PIDS+=("$pb")
 ready "$A/healthz" "$pa"; ready "$B/healthz" "$pb"
 "$OUT/bin/gateway" -addr "$STRESS_HOST:$GW_PORT" -pprof "$STRESS_HOST:$DEBUG_PORT" -config "$OUT/config.json" -db "$dir/jobs.db" -workers 16 -queue 256 -streams 64 -unary 64 -idle-timeout 3s -attempt-timeout 30s -total-timeout 60s >"$dir/gateway.log" 2>&1 &
 local pg=$!; PIDS+=("$pg")
 ready "$GW/healthz" "$pg"
 local debug_status
 debug_status=$(curl --noproxy '*' -sS --max-time 2 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $GATEWAY_TOKEN" "$GW/debug/pprof/")
 if [ "$debug_status" != 404 ]; then echo 'pprof unexpectedly exposed on business port' >&2; return 1; fi
 snapshot "$dir" before
 echo "START $name ($mode concurrency=$concurrency)"
 "$OUT/bin/loadtest" -url "$GW" -mode "$mode" -duration 12s -concurrency "$concurrency" -timeout 30s -drain 60s -output "$dir/result.json" >"$dir/load.log" 2>&1 &
 local pl=$!; PIDS+=("$pl")
 fetch "$DEBUG/debug/pprof/profile?seconds=10" "$dir/cpu.pb.gz" &
 local pc=$!; PIDS+=("$pc")
 sleep 3
 snapshot "$dir" during
 wait "$pl"
 wait "$pc"
 PIDS=("$pa" "$pb" "$pg")
 sleep 1
 snapshot "$dir" after
 for kind in allocs block mutex; do fetch "$DEBUG/debug/pprof/$kind" "$dir/$kind.pb.gz"; done
 for kind in cpu block mutex; do "$GO" tool pprof -top -nodecount=15 "$OUT/bin/gateway" "$dir/$kind.pb.gz" >"$dir/$kind-top.txt"; done
 "$GO" tool pprof -top -sample_index=alloc_space -nodecount=15 "$OUT/bin/gateway" "$dir/allocs.pb.gz" >"$dir/allocs-top.txt"
 "$GO" tool pprof -top -sample_index=inuse_space -nodecount=15 "$OUT/bin/gateway" "$dir/after-heap.pb.gz" >"$dir/heap-top.txt"
 if [ "$idle" = yes ]; then
  # Transport 的空闲连接保留 90s。不要把它们的读写 goroutine 当成泄漏。
  echo 'Waiting 95s for idle HTTP connections to expire...'
  sleep 95
  snapshot "$dir" settled
  "$GO" tool pprof -top -nodecount=15 "$OUT/bin/gateway" "$dir/settled-goroutine.pb.gz" >"$dir/settled-goroutine-top.txt"
 fi
 stop_children
 echo "DONE $name: $dir/result.json"
}
scenario unary unary 64 20ms 5 0 no
scenario long-stream stream 64 100ms 100 0 yes
scenario overload stream 192 100ms 100 0 no
scenario cancel cancel 16 20ms 100 0 no
scenario fallback unary "${FALLBACK_CONCURRENCY:-16}" 20ms 5 3 no
scenario jobs jobs 32 40ms 5 0 no
scenario timeout stream 16 20ms 100 0 no 1
printf 'Artifacts: %s\n' "$OUT"
