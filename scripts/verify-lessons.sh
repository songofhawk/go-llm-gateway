#!/usr/bin/env bash
# 每课为独立 Go 模块，root 的 ./... 不会进入它们。
set -euo pipefail
cd "$(dirname "$0")/.."
GO="${GO:-go}"
case "${1:-test}" in test|race|build|vet) mode="${1:-test}" ;; *) echo '用法：bash scripts/verify-lessons.sh [test|race|build|vet]' >&2; exit 2 ;; esac
for lesson in lessons/0[1-7]-*/; do
  printf '\n课程：%s（%s）\n' "$lesson" "$mode"
  (
    cd "$lesson"
    case "$mode" in
      test) "$GO" test -count=1 -timeout 60s ./... ;;
      race) "$GO" test -race -count=1 -timeout 60s ./... ;;
      build) "$GO" build ./... ;;
      vet) "$GO" vet ./... ;;
    esac
  )
done
