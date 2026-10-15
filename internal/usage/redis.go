package usage

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GenJi77JYXC/llm-gateway/internal/metrics"
)

// Redis 聚合的默认参数。
const (
	// usageKeyPrefix 计量 hash 键前缀，完整键为 usageKeyPrefix + 日期。
	usageKeyPrefix = "llm:usage:"
	// usageTTL 计量 hash 保留 8 天，按日滚动自动清理。
	usageTTL = 8 * 24 * time.Hour
	// 默认批量与刷盘节奏：满 64 条或每秒刷一次。
	defaultBatchSize     = 64
	defaultFlushInterval = time.Second
	// Redis 挂掉时快速失败，避免后台 goroutine 长期阻塞拖垮排干。
	dialTimeout = 3 * time.Second
	opTimeout   = time.Second
)

// RedisOptions 是 RedisRecorder 的构造参数。
type RedisOptions struct {
	Addr     string
	Password string
	DB       int
	// BatchSize 攒够多少条事件批量写一次，缺省 64
	BatchSize int
	// FlushInterval 最长多久强制刷一次盘，缺省 1s
	FlushInterval time.Duration
}

// RedisRecorder 是异步聚合到 Redis 的计量器。
//
// 数据结构：每天一个 hash，键为 llm:usage:{yyyy-mm-dd}（本地时区），
// field 为 {keyID}|{model}|{kind}（kind: prompt/completion/total），HINCRBY 原子累加，
// hash 整体 TTL 8 天自动清理。
//
// 写入路径：Record 只投 channel（满则丢弃并计数）；
// 后台 goroutine 攒批 pipeline 写入，Redis 故障时整批丢弃并计数，恢复后自动续写。
// 计量是尽力而为组件，任何失败都不阻塞转发主链路。
type RedisRecorder struct {
	client        *redis.Client
	events        chan Event
	closed        chan struct{} // Close 触发，通知后台排干退出
	done          chan struct{} // 后台 goroutine 已退出
	batchSize     int
	flushInterval time.Duration
}

// NewRedisRecorder 创建 Redis 聚合器并启动后台写入协程。
// 连不上 Redis 时返回错误，由调用方决定降级（计量退化为内存聚合）。
func NewRedisRecorder(opts RedisOptions) (*RedisRecorder, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushInterval
	}

	client := redis.NewClient(&redis.Options{
		Addr:        opts.Addr,
		Password:    opts.Password,
		DB:          opts.DB,
		DialTimeout: dialTimeout,
		ReadTimeout: opTimeout,
		WriteTimeout: opTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+opTimeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}

	r := &RedisRecorder{
		client:        client,
		events:        make(chan Event, opts.BatchSize*4),
		closed:        make(chan struct{}),
		done:          make(chan struct{}),
		batchSize:     opts.BatchSize,
		flushInterval: opts.FlushInterval,
	}
	go r.loop()
	return r, nil
}

// Record 投递一条计量事件（非阻塞）；聚合 channel 满时丢弃并计数。
func (r *RedisRecorder) Record(ev Event) {
	select {
	case r.events <- ev:
	default:
		metrics.UsageDroppedTotal.Inc()
	}
}

// Close 停止接收并等待后台把已接收事件排干落盘后关闭连接。
// 调用 Close 后不得再调用 Record。
func (r *RedisRecorder) Close() error {
	close(r.closed)
	<-r.done
	return r.client.Close()
}

// loop 后台聚合循环：攒批 + 定时刷盘，收到关闭信号后排干退出。
func (r *RedisRecorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, r.batchSize)
	for {
		select {
		case ev := <-r.events:
			batch = append(batch, ev)
			if len(batch) >= r.batchSize {
				r.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				r.flush(batch)
				batch = batch[:0]
			}
		case <-r.closed:
			for {
				select {
				case ev := <-r.events:
					batch = append(batch, ev)
				default:
					if len(batch) > 0 {
						r.flush(batch)
					}
					return
				}
			}
		}
	}
}

// flush 把一批事件 pipeline 写入 Redis，失败整批丢弃并计数。
// 使用独立 context：停机排干时也要完成写入，不随请求/停机信号取消。
func (r *RedisRecorder) flush(batch []Event) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout+opTimeout)
	defer cancel()

	key := usageKeyPrefix + time.Now().Format(dayLayout)
	pipe := r.client.Pipeline()
	for _, ev := range batch {
		base := ev.KeyID + "|" + ev.Model
		pipe.HIncrBy(ctx, key, base+"|prompt", int64(ev.PromptTokens))
		pipe.HIncrBy(ctx, key, base+"|completion", int64(ev.CompletionTokens))
		pipe.HIncrBy(ctx, key, base+"|total", int64(ev.TotalTokens))
	}
	pipe.Expire(ctx, key, usageTTL)

	if _, err := pipe.Exec(ctx); err != nil {
		metrics.UsageDroppedTotal.Add(float64(len(batch)))
		log.Printf("[usage] redis 聚合写入失败，丢弃 %d 条事件: %v", len(batch), err)
	}
}
