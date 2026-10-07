#!/usr/bin/env bash
# 每个实验通过本课测试构造临时 HTTP 服务/SQLite，并检查客户端、取消与资源收尾。
set -euo pipefail
cd "$(dirname "$0")/.."
GO="${GO:-go}"
case "${1:-all}" in
  streaming) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^TestStreamingFlushAndDisconnect$' ;;
  fallback) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^Test(FallbackStatusAndFailureRelease|AttemptTimeoutCanFallback|EmptyStreamCanFallback)$' ;;
  cancel) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^Test(StreamingFlushAndDisconnect|CancellationAndTimeoutRelease|ShutdownCancelsActiveStreams)$' ;;
  capacity) "$GO" test -race -count=1 -timeout 60s -v ./internal/gateway -run '^Test(LoadBalanceCapacityTracksWholeStream|ConcurrentCallsBoundedRaceFree|HTTPAdmissionValidation)$' ;;
  timeout) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^Test(CancellationAndTimeoutRelease|AttemptTimeoutCanFallback|TotalBudgetStopsFallback)$' ;;
  truncation) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^Test(TruncatedStreamNeverFallsBack|DoneDetectorFragmentation|DoneDetectorWholeEventAndLineEndings|CompleteStreamStopsAtDone)$' ;;
  slow) "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^TestSlowDownstreamReleasesResources$' ;;
  recovery) "$GO" test -count=1 -timeout 60s -v ./internal/jobs -run '^TestRecoveryRequeuesRunningAndPromotesSavedResult$' ;;
  jobs) "$GO" test -count=1 -timeout 60s -v ./internal/jobs
        "$GO" test -count=1 -timeout 60s -v ./internal/gateway -run '^TestAsyncHTTPIntegration$' ;;
  all) for lab in streaming fallback cancel capacity timeout truncation slow recovery jobs; do
         printf '\n实验：%s\n' "$lab"
         bash scripts/labs.sh "$lab"
       done ;;
  *) echo '用法：bash scripts/labs.sh [streaming|fallback|cancel|capacity|timeout|truncation|slow|recovery|jobs|all]' >&2; exit 2 ;;
esac
