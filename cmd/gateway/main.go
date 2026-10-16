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
	"github.com/GenJi77JYXC/llm-gateway/internal/api/middleware"
	"github.com/GenJi77JYXC/llm-gateway/internal/breaker"
	"github.com/GenJi77JYXC/llm-gateway/internal/cache"
	"github.com/GenJi77JYXC/llm-gateway/internal/config"
	"github.com/GenJi77JYXC/llm-gateway/internal/embedding"
	"github.com/GenJi77JYXC/llm-gateway/internal/gateway"
	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
	"github.com/GenJi77JYXC/llm-gateway/internal/provider/openai"
	"github.com/GenJi77JYXC/llm-gateway/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
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

	limiter := middleware.NewRateLimiter(cfg.RateLimit)

	// 熔断器（软依赖）：Redis 可用时多实例经 Redis 广播跳闸状态（T4.4），
	// 未配置/连不上/单实例退化为进程内内存熔断。
	// 每个上游实例一个熔断器，Redis 键按上游名区分。
	var sharedBreakers []*breaker.Shared
	defer func() {
		for _, b := range sharedBreakers {
			_ = b.Close()
		}
	}()
	breakerFactory := func(name string) breaker.Interface {
		if cfg.Redis.Addr == "" {
			return breaker.New(breaker.DefaultMaxFailures, breaker.DefaultCooldown)
		}
		sb, err := breaker.NewShared(breaker.SharedOptions{
			Name:        name,
			MaxFailures: breaker.DefaultMaxFailures,
			Cooldown:    breaker.DefaultCooldown,
			Addr:        cfg.Redis.Addr,
			Password:    cfg.Redis.Password,
			DB:          cfg.Redis.DB,
		})
		if err != nil {
			log.Printf("breaker(%s) redis 不可用，降级为内存熔断: %v", name, err)
			return breaker.New(breaker.DefaultMaxFailures, breaker.DefaultCooldown)
		}
		sharedBreakers = append(sharedBreakers, sb)
		return sb
	}
	r := gateway.NewRouterWithFactory(upstreams, breakerFactory)

	// 计量器（软依赖）：配置了 redis 用 Redis 聚合，连不上/未配置退化为内存聚合
	var recorder usage.Recorder = usage.NewMem()
	var exact *cache.Exact
	var semantic *cache.Semantic
	if cfg.Redis.Addr != "" {
		rr, err := usage.NewRedisRecorder(usage.RedisOptions{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		if err != nil {
			log.Printf("redis %s 不可用，token 计量降级为内存聚合: %v", cfg.Redis.Addr, err)
		} else {
			recorder = rr
			// 按日计量指标：scrape 时从 Redis 现查，供成本看板按 key_id 分组
			prometheus.MustRegister(metrics.NewUsageDailyCollector(rr))
			// 优雅停机时排干聚合 channel 再退出（在 server.Shutdown 之后执行）
			defer func() {
				if err := rr.Close(); err != nil {
					log.Printf("usage recorder close: %v", err)
				}
			}()
		}

		// 精确缓存（软依赖）：TTL 缺省 1h，"0" 显式禁用；连不上降级为无缓存直连上游
		exactTTL := time.Hour
		if cfg.Cache.ExactTTL != "" {
			exactTTL, _ = time.ParseDuration(cfg.Cache.ExactTTL) // validate 已保证合法
		}
		if exactTTL > 0 {
			ec, err := cache.NewExact(cache.ExactOptions{
				Addr:     cfg.Redis.Addr,
				Password: cfg.Redis.Password,
				DB:       cfg.Redis.DB,
				TTL:      exactTTL,
			})
			if err != nil {
				log.Printf("精确缓存初始化失败，降级直连上游: %v", err)
			} else {
				exact = ec
				defer func() { _ = ec.Close() }()
			}
		}

		// 语义缓存（软依赖）：需 redis + embedding 双双就位；任一缺失/故障降级为仅精确缓存
		if exact != nil && cfg.Embedding.BaseURL != "" {
			semTTL := exactTTL
			if cfg.Cache.SemanticTTL != "" {
				semTTL, _ = time.ParseDuration(cfg.Cache.SemanticTTL)
			}
			threshold := cfg.Cache.SemanticThreshold
			if threshold == 0 {
				threshold = 0.95
			}
			if semTTL > 0 {
				sem, err := cache.NewSemantic(cache.SemanticOptions{
					Addr:       cfg.Redis.Addr,
					Password:   cfg.Redis.Password,
					DB:         cfg.Redis.DB,
					TTL:        semTTL,
					Threshold:  threshold,
					Dimensions: cfg.Embedding.Dimensions,
					Embedder: embedding.New(embedding.Options{
						BaseURL:    cfg.Embedding.BaseURL,
						APIKey:     cfg.Embedding.APIKey,
						Model:      cfg.Embedding.Model,
						Dimensions: cfg.Embedding.Dimensions,
					}),
				})
				if err != nil {
					log.Printf("语义缓存初始化失败，降级仅精确缓存: %v", err)
				} else {
					semantic = sem
					defer func() { _ = sem.Close() }()
				}
			}
		}
	}

	server := api.NewHTTPServer(cfg.Server.Addr, cfg.Auth.ClientKeys, r, limiter, recorder, exact, semantic)

	// 优雅停机：收到 SIGINT/SIGTERM 后停止接收新连接，等待在途请求完成（最多 15s）
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("llm-gateway listening on %s (providers: %d, models: %d, rate_limit: %v)",
			cfg.Server.Addr, len(cfg.Providers), len(r.Models()), limiter.Enabled())
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
