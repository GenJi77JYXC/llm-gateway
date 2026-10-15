package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
)

// 语义缓存的 Redis 对象命名。
const (
	semanticIndex     = "llm:idx:semantic" // RediSearch 索引名
	semanticKeyPrefix = "llm:sem:"         // 缓存条目 hash 前缀（索引作用域）
)

// Embedder 抽象 embedding 调用（*embedding.Client 隐式满足），便于测试注入假实现。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SemanticOptions 是 Semantic 的构造参数。
type SemanticOptions struct {
	Addr     string
	Password string
	DB       int
	// TTL 缓存条目有效期（与精确缓存一致，默认 1h）
	TTL time.Duration
	// Threshold 余弦相似度命中阈值（roadmap 默认 0.95）
	Threshold float64
	// Dimensions 向量维度，建索引时必须与 embedding 模型实际输出一致
	Dimensions int
	// Embedder embedding 客户端
	Embedder Embedder
}

// Semantic 是语义缓存：prompt 向量化后经 RediSearch KNN 检索近似条目。
//
// 数据模型：每条缓存为 hash（llm:sem:{sha256(model+text)}），
// 字段 model(TAG 索引) / vector(VECTOR 索引) / prompt / response；
// 索引 FLAT 暴力精确扫描（缓存量级小，优于 HNSW 近似），
// hash 过期后 RediSearch 自动清除索引条目，与精确缓存同生共死。
type Semantic struct {
	client    *redis.Client
	embedder  Embedder
	ttl       time.Duration
	threshold float64
}

// NewSemantic 创建语义缓存：Ping 探测 + 建立向量索引（已存在则复用）。
// Redis 无 search 模块或不可达时返回错误，由调用方降级（仅精确缓存）。
func NewSemantic(opts SemanticOptions) (*Semantic, error) {
	if opts.TTL <= 0 {
		return nil, fmt.Errorf("semantic cache TTL 必须为正数")
	}
	if opts.Threshold <= 0 || opts.Threshold > 1 {
		return nil, fmt.Errorf("semantic threshold 须在 (0,1] 区间")
	}
	if opts.Dimensions <= 0 {
		return nil, fmt.Errorf("semantic dimensions 须为正整数")
	}
	if opts.Embedder == nil {
		return nil, fmt.Errorf("semantic embedder 不能为空")
	}

	client := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  dialTimeout,
		ReadTimeout:  opTimeout,
		WriteTimeout: opTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+opTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}

	// 建索引：model TAG 过滤 + vector FLAT 余弦距离；已存在（重启场景）忽略错误复用
	err := client.Do(ctx, "FT.CREATE", semanticIndex,
		"ON", "HASH",
		"PREFIX", 1, semanticKeyPrefix,
		"SCHEMA",
		"model", "TAG",
		"vector", "VECTOR", "FLAT", 6,
		"TYPE", "FLOAT32",
		"DIM", opts.Dimensions,
		"DISTANCE_METRIC", "COSINE",
	).Err()
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "already exists"):
			// 复用已有索引，但需验证维度与当前 embedding 模型一致：
			// 换模型（如 1024→1536 维）后旧索引不可用，DROP 重建自愈
			// （缓存条目 TTL 短，重建索引代价小；旧维度条目随 TTL 自然过期）
			if perr := probeVectorDim(ctx, client, opts.Dimensions); perr != nil {
				_ = client.Do(ctx, "FT.DROPINDEX", semanticIndex).Err()
				if cerr := createVectorIndex(ctx, client, opts.Dimensions); cerr != nil {
					_ = client.Close()
					return nil, fmt.Errorf("索引维度漂移后重建失败: %w", cerr)
				}
			}
		default:
			_ = client.Close()
			return nil, fmt.Errorf("创建向量索引失败（Redis 需 Stack 版本含 search 模块）: %w", err)
		}
	}

	return &Semantic{
		client:    client,
		embedder:  opts.Embedder,
		ttl:       opts.TTL,
		threshold: opts.Threshold,
	}, nil
}

// createVectorIndex 执行 FT.CREATE 建立语义向量索引。
func createVectorIndex(ctx context.Context, client *redis.Client, dims int) error {
	return client.Do(ctx, "FT.CREATE", semanticIndex,
		"ON", "HASH",
		"PREFIX", 1, semanticKeyPrefix,
		"SCHEMA",
		"model", "TAG",
		"vector", "VECTOR", "FLAT", 6,
		"TYPE", "FLOAT32",
		"DIM", dims,
		"DISTANCE_METRIC", "COSINE",
	).Err()
}

// probeVectorDim 用零向量 KNN 探测现有索引维度是否与 dims 匹配（不匹配时 RediSearch 报 blob size 错误）。
func probeVectorDim(ctx context.Context, client *redis.Client, dims int) error {
	zero := make([]byte, 4*dims)
	_, err := client.FTSearchWithArgs(ctx, semanticIndex, "*=>[KNN 1 @vector $BLOB AS dist]",
		&redis.FTSearchOptions{
			Params:         map[string]interface{}{"BLOB": zero},
			DialectVersion: 2,
		}).Result()
	return err
}

// Lookup 查询语义缓存：text 向量化后 KNN 检索最近邻，相似度 ≥ threshold 判定命中。
//
// 返回 (命中响应, query 向量, error)：
//   - 命中：(response, vec, nil)
//   - 未命中：(nil, vec, nil)——vec 非 nil 供 Store 复用，避免回源后二次 embed
//   - 故障：(nil, nil, err)——调用方按 miss 降级继续走真实上游
func (s *Semantic) Lookup(ctx context.Context, model, text string) ([]byte, []float32, error) {
	vec, err := s.embedder.Embed(ctx, text)
	if err != nil {
		return nil, nil, fmt.Errorf("embedding 调用失败: %w", err)
	}

	// TAG 过滤锁定同 chat model + KNN 1 最近邻；dist 为余弦距离 = 1 - 相似度
	query := fmt.Sprintf("(@model:{%s})=>[KNN 1 @vector $BLOB AS dist]", escapeTag(model))
	res, err := s.client.FTSearchWithArgs(ctx, semanticIndex, query, &redis.FTSearchOptions{
		Params:         map[string]interface{}{"BLOB": vecToBytes(vec)},
		Return:         []redis.FTSearchReturn{{FieldName: "response"}, {FieldName: "dist"}},
		SortBy:         []redis.FTSearchSortBy{{FieldName: "dist", Asc: true}},
		DialectVersion: 2,
	}).Result()
	if err != nil {
		return nil, vec, fmt.Errorf("向量检索失败: %w", err)
	}

	if res.Total == 0 || len(res.Docs) == 0 {
		return nil, vec, nil
	}
	doc := res.Docs[0]
	dist, err := strconv.ParseFloat(doc.Fields["dist"], 64)
	if err != nil {
		return nil, vec, fmt.Errorf("解析向量距离失败: %w", err)
	}
	if 1-dist < s.threshold {
		return nil, vec, nil // 最近邻也不够像，miss
	}
	return []byte(doc.Fields["response"]), vec, nil
}

// Store 写入语义缓存（幂等：同 model+text 覆盖旧条目并重置 TTL）。
// vec 通常为 Lookup 返回的 query 向量；传 nil 表示调用方无向量（跳过写入由调用方判定）。
func (s *Semantic) Store(ctx context.Context, model, text string, vec []float32, response []byte) error {
	key := semanticKey(model, text)

	pipe := s.client.Pipeline()
	pipe.HSet(ctx, key,
		"model", model,
		"prompt", text,
		"response", response,
		"vector", vecToBytes(vec),
	)
	pipe.Expire(ctx, key, s.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// semanticKey 计算语义缓存条目 key：llm:sem:{sha256(model+text)}。
// 同 model+text 幂等覆盖，保证同一 prompt 只有一份缓存条目。
func semanticKey(model, text string) string {
	sum := sha256.Sum256([]byte(model + "\n" + text))
	return semanticKeyPrefix + hex.EncodeToString(sum[:])
}

// Close 关闭底层连接。
func (s *Semantic) Close() error {
	return s.client.Close()
}

// MessagesText 从请求体提取 messages 拼接为 embedding 输入文本。
//
// 拼接全部 messages 的 "role: content"（每行一条）：多轮对话上下文不同则
// 向量不同，宁可降低命中率也不让"省略上下文的相似问"错误命中旧答案。
// content 为 JSON string 时去引号，多模态数组用原始 JSON（仍能形成稳定文本）。
// 解析失败或无 messages 返回 ""，调用方按不可缓存处理。
func MessagesText(raw []byte) string {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range body.Messages {
		content := string(m.Content)
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			content = s
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteByte('\n')
	}
	return b.String()
}

// vecToBytes 序列化 float32 切片为小端字节序（RediSearch VECTOR 字段要求）。
func vecToBytes(vec []float32) []byte {
	b := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

// escapeTag 转义 RediSearch TAG 查询中的特殊字符（model 名含连字符等）。
func escapeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
	return b.String()
}
