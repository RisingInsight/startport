#!/usr/bin/env bash
# coverage_gate.sh 覆盖率门禁：总语句覆盖率不得低于基线（防回退）。
#
# 用法：
#   bash scripts/coverage_gate.sh          # 跑全量测试并比对基线（CI 与本地同口径）
#
# 基线更新（新增代码导致覆盖率合理回落时）：
#   1. 本地跑本脚本，读出 total coverage 数值
#   2. 向下取整到 0.1 后再减 0.4 的机器容差，写入 scripts/coverage-baseline.txt 首行
#   3. 随 PR 提交，并在 PR 描述里说明回落原因
#
# 为什么是「总语句覆盖率」而不是逐包：逐包门禁对 main/装配类包零覆盖太脆，
# 总量口径对「删测试/测试失效」敏感（正是要防的回退形态），对正常重构不敏感。
set -euo pipefail
cd "$(dirname "$0")/.."

BASELINE_FILE=scripts/coverage-baseline.txt
if [[ ! -f "$BASELINE_FILE" ]]; then
  echo "::error::missing $BASELINE_FILE" >&2
  exit 1
fi
BASELINE=$(head -1 "$BASELINE_FILE" | grep -oE '^[0-9]+\.?[0-9]*' || true)
if [[ -z "$BASELINE" ]]; then
  echo "::error::$BASELINE_FILE 首行应是数值（当前基线）" >&2
  exit 1
fi

go test ./... -coverprofile=coverage.gate.out -covermode=atomic -count=1 > /dev/null
TOTAL=$(go tool cover -func=coverage.gate.out | awk '/^total:/ {sub(/%/, "", $NF); print $NF}')
rm -f coverage.gate.out

echo "total coverage: ${TOTAL}% (baseline ${BASELINE}%)"
if awk -v t="$TOTAL" -v b="$BASELINE" 'BEGIN { exit (t+0 >= b+0) ? 0 : 1 }'; then
  echo "coverage gate PASS"
else
  echo "::error::coverage regressed: ${TOTAL}% < baseline ${BASELINE}%" >&2
  echo "如属新增代码导致的合理回落：按 scripts/coverage_gate.sh 头部流程更新基线并随 PR 提交。" >&2
  exit 1
fi
