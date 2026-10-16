# LLM Gateway

一个用 Go 实现的 LLM 推理网关：统一 OpenAI 兼容协议接入多家大模型，提供流式透传、加权负载均衡、熔断降级、限流、两级缓存与可观测性，支持 K8s 一键部署与 HPA 自动扩缩容。

> 当前进度：**4 周路线图全部完成** ✅（核心转发 → 路由可靠性 → 成本与缓存 → K8s 部署）

## 架构图

```
客户端 (OpenAI 兼容协议, Bearer 鉴权)
   │  POST /v1/chat/completions   GET /v1/models
   ▼
┌─ llm-gateway（K8s Deployment，HPA 2~5 副本） ──────────────┐
│ API 层(gin)：鉴权 → 双维度令牌桶限流 → Prometheus 指标      │
│ 核心层：精确缓存 → 语义缓存(向量) → 按 model 路由           │
│   ├─ 多上游平滑加权轮询(SWRR) + 首字节前失败 fallback       │
│   └─ 三态熔断器：跳闸状态经 Redis 广播，多副本一致(≤500ms)  │
└──────┬──────────────────┬───────────────────┬─────────────┘
       │ 回源              │ 缓存/计量/熔断      │ 指标
       ▼                  ▼                   ▼
  OpenAI 兼容上游      Redis Stack        Prometheus → Grafana
 (DeepSeek/OpenAI…)  (向量/计量/熔断共享)   （成本/命中率看板）
```

## 功能

- `POST /v1/chat/completions`：OpenAI 兼容入口，非流式 + SSE 流式
- 按 `model` 字段路由到不同上游（OpenAI / DeepSeek 等所有 OpenAI 兼容厂商）
- 同一 model 可配多个上游，平滑加权轮询（SWRR）分流
- 客户端 API Key 鉴权（Bearer Token，常量时间比较）
- 三态熔断器（闭合/打开/半开）：连输 3 次熔断 30s，半开单探测试探恢复
- 首字节前失败自动 fallback 到下一候选上游，客户端无感知
- 按 Key + 按 model 双维度令牌桶限流，429 返回 OpenAI 风格错误
- 两级响应缓存（仅非流式）：精确缓存（SHA256 完全相同 prompt）→ 语义缓存（Redis Stack 向量检索，同义改写命中，默认阈值 0.95）；命中不调上游、不计 token
- 缓存两级降级：embedding 故障连续 3 次自动熔断（30s 冷却半开恢复），退化为仅精确缓存；Redis 故障整体直连上游——主链路永不因缓存/计量组件崩溃
- SSE 旁路采集：**TTFT（首 token 延迟）**、token usage（自动注入 `stream_options.include_usage`）
- Prometheus 指标：QPS、延迟分位、TTFT、token 用量、fallback/熔断/限流计数
- 客户端断开立即取消上游请求（`context.Context` 贯穿全链路）
- Docker Compose 一键起：app + Prometheus + Grafana（+ Redis，Week 3 启用）
- K8s 一键部署：`kubectl apply -f deploy/k8s/`（Deployment/Service/ConfigMap/Secret/Redis + 健康探针）
- HPA 按 CPU 自动扩缩容（2~5 副本）；熔断器跳闸状态经 Redis 广播，多副本一致

## 架构分层

```
cmd/gateway          入口装配（config → provider → balancer/breaker → router → middleware → http server）
internal/api         HTTP 适配层（唯一 import gin 的层；SSE flush、鉴权、限流、指标）
  └─ middleware      gin 中间件（鉴权、限流）
internal/gateway     核心层：模型路由、多上游加权轮询、熔断过滤、fallback（零框架依赖）
internal/balancer    平滑加权轮询选择器（泛型，零业务依赖）
internal/breaker     三态熔断器（泛型，零业务依赖）
internal/provider    Provider 抽象（只依赖 context.Context + 领域类型）
  └─ openai          OpenAI 兼容协议实现（DeepSeek 等厂商复用）
internal/model       领域类型（Usage / 错误体 / 模型列表）
internal/config      YAML + 环境变量配置
internal/metrics     Prometheus 指标定义
internal/usage       token 计量（内存聚合 / Redis 异步批量聚合，按 key+model+日期）
internal/cache       两级缓存：Exact（SHA256 精确）+ Semantic（Redis Stack 向量检索）
internal/embedding   OpenAI 兼容 embedding 客户端（语义缓存向量化）
deploy/k8s           K8s manifests（Deployment/Service/ConfigMap/Secret/Redis/HPA）
scripts/mockupstream 本地联调用假上游（支持延迟/错误率故障注入）
scripts/benchmark    压测负载发生器（客户端侧测延迟分位、TTFT、吞吐）
```

分层铁律：`internal/gateway`、`internal/provider`、`internal/balancer`、`internal/breaker` 中 grep 不到任何 HTTP 框架。
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

### 5. Kubernetes（kind 复现，约 10 分钟）

```bash
# 1. 构建镜像（国内构建加 --build-arg GOPROXY=https://goproxy.cn,direct）
docker build -t llm-gateway:local .

# 2. 创建 kind 集群并加载镜像（免节点外网拉取）
kind create cluster
kind load docker-image llm-gateway:local redis/redis-stack:7.4.0-v0

# 3. 填入真实上游凭证（secret.yaml 已被 .gitignore 忽略）
cp deploy/k8s/10-secret.example.yaml deploy/k8s/secret.yaml

# 4. 一键部署（命名空间/配置/凭证/Redis/网关 2 副本）
kubectl apply -f deploy/k8s/

# 5. 验证
kubectl port-forward svc/llm-gateway 8080:80 -n llm-gateway
curl http://127.0.0.1:8080/healthz

# 6. HPA 演示（需 metrics-server；kind 需 --kubelet-insecure-tls 补丁，见 T4.3 commit 说明）
kubectl scale deployment llm-gateway --replicas=3 -n llm-gateway   # 手动扩缩容
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

### 多上游加权 + fallback

```yaml
providers:
  - name: deepseek
    base_url: "https://api.deepseek.com/v1"
    models: ["deepseek-chat"]
    weight: 3          # 3/4 流量走这里
  - name: backup
    base_url: "https://api.siliconflow.cn/v1"
    models: ["deepseek-chat"]
    weight: 1          # 1/4 流量兜底；deepseek 熔断时全部切换过来
```

### 限流

```yaml
rate_limit:
  key_qps: 5         # 每个 client key 每秒 5 次
  key_burst: 10      # 突发容量
  model_qps: 20      # 每个 model 全局每秒 20 次
  model_burst: 40
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
| `llmgateway_requests_total{provider,model,stream,result}` | Counter | 请求计数（result: ok/upstream_error/bad_request/internal_error/cache_hit；缓存命中时 provider=cache） |
| `llmgateway_request_duration_seconds` | Histogram | 端到端耗时 |
| `llmgateway_ttft_seconds{provider,model}` | Histogram | 流式首 token 延迟 |
| `llmgateway_tokens_total{provider,model,kind}` | Counter | prompt/completion/total token |
| `llmgateway_inflight_requests{stream}` | Gauge | 在途请求数 |
| `llmgateway_stream_interrupt_total` | Counter | 上游流异常中断次数 |
| `llmgateway_fallback_total{model,failed_provider}` | Counter | 首字节前失败触发的上游切换次数 |
| `llmgateway_breaker_state{provider}` | Gauge | 熔断器状态（0=closed, 1=open, 2=half_open） |
| `llmgateway_rate_limited_total{dimension}` | Counter | 被限流拒绝的请求数（dimension=key/model） |
| `llmgateway_usage_dropped_total` | Counter | 计量事件丢失数（聚合 channel 满或 Redis 写失败） |
| `llmgateway_usage_daily_tokens{key_id,model,kind,date}` | Gauge | 按日聚合 token 用量（scrape 时从 Redis 现查，供成本看板） |
| `llmgateway_cache_hit_total{type}` | Counter | 缓存命中计数（type=exact/semantic/miss） |

## 设计要点（对照 one-api 的改进）

1. **抽象层不依赖 gin**：Provider 接口只吃 `context.Context` 与自有结构体，可单测、可替换框架。
2. **上游请求绑定 ctx**：客户端断开立即关闭上游连接，不白烧 token（one-api 用 `http.NewRequest` 未绑 ctx）。
3. **流式中断显式报错**：上游中途断流时向客户端补发错误事件 chunk，客户端可区分"完整回答"与"被截断"。
4. **原始 chunk 透传 + 旁路解析**：SSE data 原样转发（零重序列化延迟），只旁路提取 TTFT 和 usage。
5. **平滑加权轮询（SWRR）**：与 Nginx 一致的算法，3:1 权重输出 AABA 序列而非 AAAB，避免瞬时打满单上游。
6. **首字节前 fallback**：5xx/传输错误在拿到响应头前自动切换候选；4xx 认为上游可达不重试；ctx 取消不污染熔断器。
7. **熔断器惰性状态转移**：无后台定时器，在 Allow/State 时检查冷却期，半开只放行一个探测请求。

## 压测数据

本地回环、mock 上游（零处理耗时）条件下实测：

| 指标 | 数值 |
|---|---|
| 非流式转发开销 | p50 ≈1.6ms，p99 ≈3.6ms |
| 流式 TTFT 开销 | p50 ≈0.8ms，p99 ≈2.5ms |
| 单进程 QPS 上限 | ≈9,000 req/s |
| 流式吞吐损耗 | ≈0（经网关 131.3 vs 直连 132.0 QPS） |

K8s（kind 单节点，2 副本 requests cpu=50m）环境实测：

| 指标 | 数值 |
|---|---|
| HPA 扩容 | CPU 冲 502% 时 2→5 副本，决策 ≈64s；负载停止后 5 分钟窗口缩回 |
| 手动扩容 2→3 | 新副本 6.7s 就绪接流量（readiness 探针门控） |
| 多副本熔断一致 | 跳闸经 Redis 广播，感知延迟 ≤500ms |

## Roadmap

- ~~Week 2：负载均衡（加权轮询）、熔断、fallback、限流、压测~~ ✅
- ~~Week 3：token 计量聚合与成本看板、精确缓存、语义缓存（Redis Stack 向量检索）~~ ✅
- ~~Week 4：K8s Deployment + HPA、多实例熔断状态共享~~ ✅
