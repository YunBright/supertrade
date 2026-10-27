// Package outbox / runner.go —— 发件箱的投递器(后台轮询 + 发布)。
//
// # 三条铁律
//
//		① **业务不被 publish 阻塞**:publish 失败只更新 attempts / last_error,
//		   绝不让业务事务回滚(否则"发不出事件"就等于"不能开单")。
//
//		② **事件不丢**:只有 publish 成功才标 published=true。
//		   中途被 kill ⇒ 下一轮重新投递(可能重复)⇒ 订阅方按 op_id 幂等。
//
//		③ **先 ack 后干活**:本包**不涉及 HTTP 回调**,但同样的纪律适用于
//	  dapr scheduler 回调(见 internal/cubesync/handler.go)。
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/pkg/eventbus"
)

// Publisher 是 Dapr pub/sub 发布的最小抽象(测试可注入 fake)。
type Publisher interface {
	Publish(ctx context.Context, topic string, data any) error
}

// DaprPublisher 是 Publisher 的 dapr-sdk 实现。
type DaprPublisher struct {
	client dapr.Client
	pubsub string
}

// NewDaprPublisherFromEnv 构造 publisher。
//
//	DAPR_PUBSUB = "tradewind-pubsub"   // 默认
//
// ⚠️ 必须与订阅侧 component metadata.name **逐字相同**
// (见 pkg/eventbus 的说明 —— 本仓真踩过:名字不同 ⇒ 事件永远到不了,且零报错)。
func NewDaprPublisherFromEnv() (*DaprPublisher, error) {
	cli, err := dapr.NewClient()
	if err != nil {
		return nil, err
	}
	ps := os.Getenv("DAPR_PUBSUB")
	if ps == "" {
		ps = eventbus.Name
	}
	return &DaprPublisher{client: cli, pubsub: ps}, nil
}

// Publish 发到 pubsub/topic。SDK 自动包 CloudEvents 1.0 envelope。
func (p *DaprPublisher) Publish(ctx context.Context, topic string, data any) error {
	return p.client.PublishEvent(ctx, p.pubsub, topic, data)
}

// DefaultBatchSize 是单轮最多投递多少条。
const DefaultBatchSize = 100

// DefaultInterval 是两轮之间的间隔。
const DefaultInterval = 3 * time.Second

// DefaultPublishTimeout 是单条 publish 的超时。
const DefaultPublishTimeout = 10 * time.Second

// Runner 轮询发件箱并投递。
type Runner struct {
	db        *gorm.DB
	publisher Publisher
	logger    *slog.Logger
	// batchSize 是单轮条数上限。
	batchSize int
	// interval 是轮询间隔。
	interval time.Duration
	// publishTimeout 是单条 publish 超时。
	publishTimeout time.Duration
	// now 可注入(测试)。
	now func() time.Time
}

// NewRunner 构造 Runner。
func NewRunner(db *gorm.DB, publisher Publisher, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		db:             db,
		publisher:      publisher,
		logger:         logger,
		batchSize:      DefaultBatchSize,
		interval:       DefaultInterval,
		publishTimeout: DefaultPublishTimeout,
		now:            func() time.Time { return time.Now() },
	}
}

// WithBatchSize 覆盖单轮条数(测试用)。
//
// Deprecated:已废弃 —— 没有任何调用方;批量条数由 DefaultBatchSize 决定。
// 本方法保留为**不抛异常的空实现**,只为不打断可能存在的外部调用。
func (r *Runner) WithBatchSize(n int) *Runner {
	return r
}

// WithInterval 覆盖轮询间隔(测试用)。
//
// Deprecated:已废弃 —— 没有任何调用方;轮询间隔由 DefaultInterval 决定。
// 本方法保留为**不抛异常的空实现**,只为不打断可能存在的外部调用。
func (r *Runner) WithInterval(d time.Duration) *Runner {
	return r
}

// Start 在后台跑投递循环,直到 ctx 被取消。
//
// ⚠️ 每一轮的错误都只记 log —— **投递器本身不该把服务搞挂**。
// (它挂了 = 事件永远不投递,而症状是"库存不扣、积分不加",极难定位。)
func (r *Runner) Start(ctx context.Context) {
	if r.publisher == nil {
		r.logger.Warn("outbox: 没有 publisher,发件箱不会投递(事件会积压)")
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.logger.Warn("outbox: 投递一轮失败", "err", err)
			}
		}
	}
}

// RunOnce 跑一轮投递,返回本轮成功投递的条数。
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	if r.db == nil {
		return 0, fmt.Errorf("outbox: 没有 db")
	}
	if r.publisher == nil {
		return 0, fmt.Errorf("outbox: 没有 publisher")
	}
	rows, err := r.pending(ctx)
	if err != nil {
		return 0, err
	}
	ok := 0
	for i := range rows {
		if err := r.deliver(ctx, &rows[i]); err != nil {
			r.logger.Warn("outbox: 投递失败,保留待重发",
				"id", rows[i].ID, "topic", rows[i].Topic, "err", err)
			continue
		}
		ok++
	}
	return ok, nil
}

// pending 取待投递的行(按入队时间升序 ⇒ 事件有序)。
func (r *Runner) pending(ctx context.Context) ([]Outbox, error) {
	var rows []Outbox
	err := r.db.WithContext(ctx).
		Where("published = ?", false).
		Order("created_at ASC, id ASC").
		Limit(r.batchSize).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("outbox: 读取待投递行失败: %w", err)
	}
	return rows, nil
}

// deliver 投递一条并更新状态。
func (r *Runner) deliver(ctx context.Context, row *Outbox) error {
	pubCtx, cancel := context.WithTimeout(ctx, r.publishTimeout)
	defer cancel()

	// ⚠️ 用**原始 JSON 字符串**发布,不重新序列化:
	//   入队时序列化一次,发布时用同一份字节 ⇒ 订阅方拿到的内容与事务里写下的**逐字相同**
	//   (重新序列化可能改变 key 顺序 / 数字精度)。
	var payload json.RawMessage = json.RawMessage(row.Payload)

	if err := r.publisher.Publish(pubCtx, row.Topic, payload); err != nil {
		r.markFailed(ctx, row, err)
		return err
	}

	now := r.now()
	err := r.db.WithContext(ctx).Model(&Outbox{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"published":    true,
			"published_at": now,
			"attempts":     row.Attempts + 1,
			"last_error":   "",
		}).Error
	if err != nil {
		// ⚠️ publish 成功但标记失败 ⇒ 下一轮会**重发**(重复投递)。
		//   这是"至少一次"语义,订阅方必须按 op_id 幂等。
		//   这里**不能**把行删掉 —— 事件必须至少被投出去一次。
		return fmt.Errorf("outbox: 标记已发布失败(事件会重发,订阅方需幂等): %w", err)
	}
	return nil
}

// markFailed 记录一次失败尝试。
//
// ⚠️ 只更新 attempts / last_error,**绝不**标记 published。
func (r *Runner) markFailed(ctx context.Context, row *Outbox, cause error) {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	err := r.db.WithContext(ctx).Model(&Outbox{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"attempts":   row.Attempts + 1,
			"last_error": msg,
		}).Error
	if err != nil {
		r.logger.Warn("outbox: 记录投递失败状态出错", "id", row.ID, "err", err)
	}
}
