// Package config 负责加载与校验网关配置。
// 配置来源优先级：配置文件（YAML）中的 ${ENV} 占位符由环境变量展开。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是网关的根配置。
type Config struct {
	Server    ServerConfig     `yaml:"server"`
	Auth      AuthConfig       `yaml:"auth"`
	Providers []ProviderConfig `yaml:"providers"`
}

// ServerConfig 是 HTTP 服务配置。
type ServerConfig struct {
	// Addr 监听地址，默认 :8080
	Addr string `yaml:"addr"`
}

// AuthConfig 是客户端（调用方）鉴权配置。
// Week 1 先用静态 key 列表，后续可替换为 DB 持久化。
type AuthConfig struct {
	// ClientKeys 允许访问网关的 key 列表（Authorization: Bearer <key>）
	ClientKeys []string `yaml:"client_keys"`
}

// ProviderConfig 描述一个模型上游。
// Week 1 所有上游均为 OpenAI 兼容协议（DeepSeek/OpenAI/通义等），
// 仅靠 base_url/api_key/models 区分，type 字段预留给后续非兼容厂商。
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
	if len(c.Providers) == 0 {
		return fmt.Errorf("providers 至少配置一个上游")
	}

	seenNames := make(map[string]struct{}, len(c.Providers))
	seenModels := make(map[string]string)
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
		for _, m := range p.Models {
			if owner, ok := seenModels[m]; ok {
				return fmt.Errorf("model %s 同时配置在 provider %s 和 %s，Week 1 一个模型只能路由到一个上游", m, owner, p.Name)
			}
			seenModels[m] = p.Name
		}
	}
	return nil
}
