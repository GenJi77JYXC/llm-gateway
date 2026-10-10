# ---- 构建阶段 ----
FROM golang:1.26-alpine AS builder

WORKDIR /build

# 先拷依赖文件利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /build/gateway ./cmd/gateway

# ---- 运行阶段 ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /build/gateway /app/gateway

# 非 root 用户由 distroless nonroot 提供
USER nonroot:nonroot

EXPOSE 8080
# 配置文件通过 compose 挂载到 /app/configs/config.yaml，密钥经环境变量在运行时展开
ENTRYPOINT ["/app/gateway", "-config", "/app/configs/config.yaml"]
