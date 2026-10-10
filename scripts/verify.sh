#!/usr/bin/env bash
# Week 1 验收脚本：验证健康检查、鉴权、模型列表、非流式/流式转发、指标。
# 用法：
#   GATEWAY_URL=http://127.0.0.1:8080 \
#   CLIENT_KEY=sk-gateway-dev-123456 \
#   MODEL=deepseek-chat \
#   bash scripts/verify.sh
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8080}"
CLIENT_KEY="${CLIENT_KEY:-sk-gateway-dev-123456}"
MODEL="${MODEL:-deepseek-chat}"

echo "== 1. /healthz =="
curl -fsS "$GATEWAY_URL/healthz"; echo

echo "== 2. 无 key 访问应 401 =="
curl -s -o /dev/null -w "status=%{http_code} (expect 401)\n" "$GATEWAY_URL/v1/models"

echo "== 3. /v1/models =="
curl -fsS "$GATEWAY_URL/v1/models" -H "Authorization: Bearer $CLIENT_KEY"; echo

echo "== 4. 非流式 chat/completions =="
curl -fsS "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"用一句话介绍你自己\"}]}"; echo

echo "== 5. 流式 chat/completions（观察逐 chunk 输出与 [DONE] 收尾）=="
curl -N -fsS "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$MODEL\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"数 1 到 3\"}]}"; echo

echo "== 6. 未知模型应 400 =="
curl -s -o /dev/null -w "status=%{http_code} (expect 400)\n" \
  "$GATEWAY_URL/v1/chat/completions" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"not-exist","messages":[{"role":"user","content":"hi"}]}'

echo "== 7. /metrics 中的网关指标 =="
curl -fsS "$GATEWAY_URL/metrics" | grep -E 'llmgateway_(requests_total|ttft_seconds|tokens_total)' | head -20

echo "all checks done."
