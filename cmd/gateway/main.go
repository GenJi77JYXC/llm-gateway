// Command gateway 是 LLM 推理网关入口。
// 装配顺序：config -> provider -> router -> http server（main 只装配，不写业务）。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GenJi77JYXC/llm-gateway/internal/api"
	"github.com/GenJi77JYXC/llm-gateway/internal/config"
	"github.com/GenJi77JYXC/llm-gateway/internal/gateway"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider/openai"
)

func main() {
	configPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	// 所有 OpenAI 兼容上游共享一个连接池
	httpClient := openai.DefaultHTTPClient()
	upstreams := make([]gateway.Upstream, 0, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		p := openai.New(openai.Config{
			Name:    pc.Name,
			BaseURL: pc.BaseURL,
			APIKey:  pc.APIKey,
			Models:  pc.Models,
		}, httpClient)
		upstreams = append(upstreams, gateway.Upstream{Provider: p, Weight: pc.Weight})
	}
	r := gateway.NewRouter(upstreams)

	server := api.NewHTTPServer(cfg.Server.Addr, cfg.Auth.ClientKeys, r)

	// 优雅停机：收到 SIGINT/SIGTERM 后停止接收新连接，等待在途请求完成（最多 15s）
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("llm-gateway listening on %s (providers: %d, models: %d)",
			cfg.Server.Addr, len(cfg.Providers), len(r.Models()))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutdown signal received, draining connections...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("forced shutdown: %v", err)
		os.Exit(1)
	}
	log.Println("server stopped gracefully")
}
