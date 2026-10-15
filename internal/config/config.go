// Package config 负责加载与校验网关配置。
// 配置来源优先级：配置文件（YAML）中的 ${ENV} 占位符由环境变量展开。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是网关的根配置。
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Auth      AuthConfig       `yaml:"auth"`
	RateLimit RateLimitConfig  `yaml:"rate_limit"`
	Redis     RedisConfig      `yaml:"redis"`
	Cache     CacheConfig      `yaml:"cache"`
	Providers []ProviderConfig `yaml:"providers"`
}

// ServerConfig 是 HTTP 服务配置。
type ServerConfig struct {
	// Addr 监听地址，默认 :8080
	Addr string `yaml:"addr"`
}

// ClientKey 是一个允许访问网关的客户端凭证。
type ClientKey struct {
	// Name 客户端身份标识，token 计量按它归属（看板按 name 分组），如 dev、ci-bot
	Name string `yaml:"name"`
	// Key 调用方请求中 Authorization: Bearer <key> 的 <key>
	Key string `yaml:"key"`
}

// AuthConfig 是客户端（调用方）鉴权配置。
// Week 1 先用静态 key 列表，后续可替换为 DB 持久化。
type AuthConfig struct {
	// ClientKeys 允许访问网关的凭证列表
	ClientKeys []ClientKey `yaml:"client_keys"`
}

// RateLimitConfig 是双维度令牌桶限流配置（可选）。
// 任一维度 QPS <= 0 表示该维度不限流；缺省整段省略即完全不限流。
type RateLimitConfig struct {
	// KeyQPS 每个 client key 的请求速率（次/秒）
	KeyQPS float64 `yaml:"key_qps"`
	// KeyBurst key 维度突发容量，缺省按 max(1, KeyQPS) 处理
	KeyBurst int `yaml:"key_burst"`
	// ModelQPS 每个 model 的全局请求速率（所有 key 共享）
	ModelQPS float64 `yaml:"model_qps"`
	// ModelBurst model 维度突发容量，缺省按 max(1, ModelQPS) 处理
	ModelBurst int `yaml:"model_burst"`
}

// RedisConfig 是 Redis 连接配置（可选）。
// Week 3 起用于计量聚合与缓存；缺省整段省略 = 不使用 Redis（计量退化为内存聚合）。
type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

// CacheConfig 是响应缓存配置（可选，依赖 redis 段，redis 未配置时缓存整体不生效）。
type CacheConfig struct {
	// ExactTTL 精确缓存有效期（如 "1h"、"30m"）；缺省 1h；显式 "0" = 禁用精确缓存
	ExactTTL string `yaml:"exact_ttl"`
}

// ProviderConfig 描述一个模型上游。
// Week 1 所有上游均为 OpenAI 兼容协议（DeepSeek/OpenAI/通义等），
// 仅靠 base_url/api_key/models 区分，type 字段预留给后续非兼容厂商。
// Week 2 起同一个 model 可配置在多个上游中，按 weight 加权轮询分发。
type ProviderConfig struct {
	// Name 上游实例名，用于日志与指标 label，如 deepseek、openai
	Name string `yaml:"name"`
	// Type 协议类型，当前仅支持 openai（默认值）
	Type string `yaml:"type"`
	// BaseURL 上游 OpenAI 兼容端点，如 https://api.deepseek.com/v1
	BaseURL string `yaml:"base_url"`
	// APIKey 上游密钥，建议用 ${DEEPSEEK_API_KEY} 形式从环境变量注入
	APIKey string `yaml:"api_key"`
	// Models 该上游支持的模型名（客户端请求中的 model 字段）
	Models []string `yaml:"models"`
	// Weight 负载均衡权重（同一 model 的多个上游之间生效），<=0 时按 1 处理
	Weight int `yaml:"weight"`
}

// Load 从 path 读取 YAML 配置，展开其中的 ${ENV} 或 $ENV 环境变量后解析。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if len(c.Auth.ClientKeys) == 0 {
		return fmt.Errorf("auth.client_keys 至少配置一个")
	}
	seenKeys := make(map[string]struct{}, len(c.Auth.ClientKeys))
	for i := range c.Auth.ClientKeys {
		ck := &c.Auth.ClientKeys[i]
		if ck.Name == "" {
			return fmt.Errorf("auth.client_keys[%d].name 不能为空（token 计量按 name 归属）", i)
		}
		if ck.Key == "" {
			return fmt.Errorf("auth.client_keys[%d].key 不能为空", i)
		}
		if _, ok := seenKeys[ck.Key]; ok {
			return fmt.Errorf("auth.client_keys[%d].key 重复", i)
		}
		seenKeys[ck.Key] = struct{}{}
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("providers 至少配置一个上游")
	}

	// 缓存 TTL 合法性提前校验，避免运行期才发现配置写错
	if c.Cache.ExactTTL != "" {
		if _, err := time.ParseDuration(c.Cache.ExactTTL); err != nil {
			return fmt.Errorf("cache.exact_ttl 无法解析: %w", err)
		}
	}

	// 限流 burst 缺省补齐为对应 QPS（至少 1），保证限流器有可用突发容量
	if c.RateLimit.KeyQPS > 0 && c.RateLimit.KeyBurst <= 0 {
		c.RateLimit.KeyBurst = max(1, int(c.RateLimit.KeyQPS))
	}
	if c.RateLimit.ModelQPS > 0 && c.RateLimit.ModelBurst <= 0 {
		c.RateLimit.ModelBurst = max(1, int(c.RateLimit.ModelQPS))
	}

	seenNames := make(map[string]struct{}, len(c.Providers))
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name 不能为空", i)
		}
		if _, ok := seenNames[p.Name]; ok {
			return fmt.Errorf("provider name 重复: %s", p.Name)
		}
		seenNames[p.Name] = struct{}{}

		if p.Type == "" {
			p.Type = "openai"
		}
		if p.Type != "openai" {
			return fmt.Errorf("provider %s 暂不支持的 type: %s（Week 1 仅支持 openai）", p.Name, p.Type)
		}
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s 的 base_url 不能为空", p.Name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.APIKey == "" {
			return fmt.Errorf("provider %s 的 api_key 不能为空（请检查对应环境变量是否设置）", p.Name)
		}
		if len(p.Models) == 0 {
			return fmt.Errorf("provider %s 至少配置一个 model", p.Name)
		}
		if p.Weight <= 0 {
			p.Weight = 1
		}
		// 注意：Week 2 起允许同一 model 出现在多个上游（按 weight 负载均衡）
	}
	return nil
}
