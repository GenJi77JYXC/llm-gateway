# LLM Gateway

一个用 Go 实现的 LLM 推理网关：统一 OpenAI 兼容协议接入多家大模型，提供流式透传、模型路由、可观测性。

> 当前进度：**Week 1（核心转发链路）**，后续规划：负载均衡/熔断降级 → 计量计费/语义缓存 → K8s 部署。

## 功能（Week 1）

- `POST /v1/chat/completions`：OpenAI 兼容入口，非流式 + SSE 流式
- 按 `model` 字段路由到不同上游（OpenAI / DeepSeek 等所有 OpenAI 兼容厂商）
- 客户端 API Key 鉴权（Bearer Token，常量时间比较）
- SSE 旁路采集：**TTFT（首 token 延迟）**、token usage（自动注入 `stream_options.include_usage`）
- Prometheus 指标：QPS、端到端耗时、TTFT、token 用量、在途请求、流中断次数
- 客户端断开立即取消上游请求（`context.Context` 贯穿全链路）
- Docker Compose 一键起：app + Prometheus + Grafana（+ Redis，Week 3 启用）

## 架构分层

```
cmd/gateway          入口装配（config → provider → router → http server）
internal/api         HTTP 适配层（唯一 import gin 的层；SSE flush、鉴权、指标）
  └─ middleware      gin 中间件
internal/gateway     核心层：模型路由、请求归一化（零框架依赖）
internal/provider    Provider 抽象（只依赖 context.Context + 领域类型）
  └─ openai          OpenAI 兼容协议实现（DeepSeek 等厂商复用）
internal/model       领域类型（Usage / 错误体 / 模型列表）
internal/config      YAML + 环境变量配置
internal/metrics     Prometheus 指标定义
```

分层铁律：`internal/gateway`、`internal/provider`、`internal/model` 中 grep 不到任何 HTTP 框架。
新增上游厂商只需实现 `provider.Provider` 接口。

## 快速开始

### 1. 准备配置

```bash
cp configs/config.example.yaml configs/config.yaml   # 按需删减/修改上游
cp .env.example .env                                 # 填入上游 API Key
```

至少需要一家可用的 OpenAI 兼容上游（DeepSeek 充值少量额度即可联调）。

### 2. 本地运行

```bash
make run
# 或
go run ./cmd/gateway -config configs/config.yaml
```

### 3. 验证

```bash
make verify
# 等价于：GATEWAY_URL=... CLIENT_KEY=... MODEL=deepseek-chat bash scripts/verify.sh
```

### 4. Docker Compose 全栈

```bash
make docker-up
# app       http://127.0.0.1:8080
# grafana   http://127.0.0.1:3000  (admin/admin)
# prometheus http://127.0.0.1:9090
```

## 使用示例

```bash
# 非流式
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gateway-dev-123456" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"你好"}]}'

# 流式（-N 关闭缓冲，观察逐 token 输出）
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gateway-dev-123456" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"数 1 到 3"}]}'
```

## API

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 健康检查 |
| GET | `/metrics` | 无 | Prometheus 指标 |
| GET | `/v1/models` | Bearer | 已配置的模型列表 |
| POST | `/v1/chat/completions` | Bearer | OpenAI 兼容对话补全（流式/非流式） |

## 关键指标

| 指标 | 类型 | 说明 |
|---|---|---|
| `llmgateway_requests_total{provider,model,stream,result}` | Counter | 请求计数（result: ok/interrupted/upstream_error/internal_error） |
| `llmgateway_request_duration_seconds` | Histogram | 端到端耗时 |
| `llmgateway_ttft_seconds{provider,model}` | Histogram | 流式首 token 延迟 |
| `llmgateway_tokens_total{provider,model,kind}` | Counter | prompt/completion/total token |
| `llmgateway_inflight_requests{stream}` | Gauge | 在途请求数 |
| `llmgateway_stream_interrupt_total` | Counter | 上游流异常中断次数 |

## 设计要点（对照 one-api 的改进）

1. **抽象层不依赖 gin**：Provider 接口只吃 `context.Context` 与自有结构体，可单测、可替换框架。
2. **上游请求绑定 ctx**：客户端断开立即关闭上游连接，不白烧 token（one-api 用 `http.NewRequest` 未绑 ctx）。
3. **流式中断显式报错**：上游中途断流时向客户端补发错误事件 chunk，客户端可区分"完整回答"与"被截断"。
4. **原始 chunk 透传 + 旁路解析**：SSE data 原样转发（零重序列化延迟），只旁路提取 TTFT 和 usage。

## Roadmap

- Week 2：负载均衡（加权轮询/最低延迟）、熔断、fallback、限流、压测
- Week 3：token 计量聚合与成本看板、精确缓存、语义缓存（Redis Stack 向量检索）
- Week 4：K8s Deployment + HPA、多实例熔断状态共享
