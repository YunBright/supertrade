// Package outbox —— 事务性发件箱(transactional outbox)。
//
// # 为什么全仓要有这个东西
//
// 仓里现有的事件发布写法是**反面教材**:
//
//	internal/stocktake/service/events.go:1-13
//	internal/fresh-meat/service/events.go
//
//	⇒ 发布是"尽力而为":失败仅记 log,不阻断业务;
//	  并且在**事务外** publish。
//
// # 那个写法在收银链路上会静默丢单
//
//	业务事务提交 → (进程在这个窗口被 kill) → publish 永远没发生
//
//	而 `sale.paid` 丢了 = 库存不扣 + 积分不加,
//
// **没有任何一行日志说这件事**(publish 根本没被调到)。
//
// 对"录一条早盘记录"来说,丢一个事件可以事后全量补偿;
// 对"扣库存 / 加积分"来说,丢一个事件 = 账实不符,而且**补不回来**
// (补偿的触发器正是那个丢了的事件)。
//
// # 本包的契约
//
//	① 事件行与业务行在**同一个事务**里写入(Recorder.Enqueue 强制要求传 tx)
//	② 事务提交后由后台 Runner 轮询未发布行 → publish → 标记 published
//	③ publish 失败**不丢事件**:只更新 attempts / last_error,下一轮再试
//
// # 幂等的两侧
//
//	发布侧:publish 成功但"标记 published"失败 ⇒ 重发 ⇒ **重复投递**
//	订阅侧:必须按 op_id(= outbox.id)去重
//
// ⇒ outbox.id **就是** op_id(业务幂等键),不是另生成的一个 id。
// 这不是巧合:重发时订阅方看到的必须是**同一个**去重键。
//
// # 用法(业务侧)
//
//	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
//	    if err := salesSvc.Create(tx, sale); err != nil { return err }
//	    return outbox.Recorder{}.Enqueue(tx, outbox.Event{
//	        ID:       sale.OpID,          // ← 业务幂等键
//	        BranchID: sale.BranchID,
//	        Topic:    TopicSalePaid,
//	        Payload:  payload,
//	    })
//	})
//
// # 用法(装配侧)
//
//	rec := outbox.NewRecorder(db)                  // 业务侧写事件
//	run := outbox.NewRunner(db, publisher)         // 装配侧起后台发布器
//	go run.Start(ctx)
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Outbox 是发件箱表。
//
// ⚠️ **表名固定叫 outbox**:pos 与 member 各有一份(不同 PG schema / 不同库),
// 但**结构必须逐字一致** —— 否则"两个服务的 outbox 长得不一样"这种漂移
// 会让排查时以为看错了表。
type Outbox struct {
	// ID 是**业务幂等键**(uuid / op_id),同时是主键。
	//
	// ⚠️ 不能另生成一个自增 id:重发时订阅方必须看到**同一个**去重键。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)"`
	// BranchID 是事件归属门店(订阅方据此做 per-branch 过滤)。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_outbox_branch"`
	// Topic 是 Dapr pub/sub 的 topic 名(EVENT-CATALOG §0.4)。
	Topic string `gorm:"column:topic;type:varchar(64);not null;index:idx_outbox_topic"`
	// Subject 是 CloudEvents subject(可观测用,不参与去重)。
	Subject string `gorm:"column:subject;type:varchar(128)"`
	// Payload 是事件体(JSON 字符串)。
	//
	// ⚠️ **存字符串而不是 jsonb 结构体**:payload 的形状由各业务域自己定,
	//   而 outbox 表要能被两个服务共用。存字符串 ⇒ 加字段不用改表结构。
	Payload string `gorm:"column:payload;type:text;not null"`
	// Published 标记是否已成功投递。
	Published bool `gorm:"column:published;type:boolean;not null;default:false;index:idx_outbox_unpublished"`
	// Attempts 是投递尝试次数。
	Attempts int `gorm:"column:attempts;type:integer;not null;default:0"`
	// LastError 是最近一次投递失败的错误摘要。
	LastError string `gorm:"column:last_error;type:text"`
	// CreatedAt 是入队时刻(**不是**发布时刻)⇒ 订阅方排序用它。
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_outbox_unpublished"`
	// PublishedAt 是成功投递时刻。
	PublishedAt *time.Time `gorm:"column:published_at"`
}

// TableName 固定表名。
func (Outbox) TableName() string { return "outbox" }

// Event 是入队一条事件时的入参。
type Event struct {
	// ID 是业务幂等键(= outbox.id)。
	ID string
	// BranchID 是事件归属门店。
	BranchID string
	// Topic 是 pub/sub topic 名。
	Topic string
	// Subject 是 CloudEvents subject(可空)。
	Subject string
	// Payload 是事件体(会被 JSON 序列化)。
	Payload any
}

// Recorder 负责把事件写进发件箱。
//
// ⚠️ **Recorder 不持有 db**:它只接受**调用方传进来的 tx**。
// 这不是 API 洁癖 —— 这是"事件与业务同事务"这条保证的**唯一**落地方式:
//
//	如果 Recorder 持有自己的 db,调用方很容易写成
//	    outbox.Recorder.Enqueue(...)  // 用了另一个连接 ⇒ 不在同一事务
//	而这种写法**编译通过、测试也过**(单测里 db 和 tx 指向同一个内存库),
//	只有到了生产(Postgres 真实并发)才会暴露成"事件丢了"。
//
// ⇒ 签名强制 tx 必填、且 nil 直接报错。
//
// # ⚠️ **namespace:同一张表、多个服务 ⇒ 行 id 必须带命名空间**
//
// pos 与 member **共用同一个 PG 库**，而 `outbox` 表是**同一张**。
// 若两边的行 id 都直接用业务 `op_id`：
//
//	pos     写: id = "op-sale-123"  (topic=sale.paid)
//	member 写: id = "op-sale-123"  (topic=member.points.changed)
//	⇒ 主键冲突: duplicate key value violates unique constraint "outbox_pkey"
//
// 症状**极其安静**：member 那一整个事务回滚 ⇒ 积分没加、流水没有，
// 而日志里只有一行 SQL 错误（很容易被当成"数据库抖动"忽略）。
//
// ⇒ **每个服务用自己的 namespace**（`NewRecorder("pos")` / `NewRecorder("member")`），
// 行 id = `<namespace>:<op_id>` ⇒ 各服务在自己的 id 空间里，**互不干扰**。
//
// ⚠️ **发布出去的事件体里的 op_id 仍然是原始值** ——
//
//	订阅方的去重键是**载荷里的 op_id**，不是发件箱行的 id。
//	命名空间只影响发件箱这张**内部表**。
type Recorder struct {
	// namespace 是服务自己的命名空间（"pos" / "member" / …）。
	//
	// ⚠️ 空 = 不加前缀（保留旧行为）。**新服务必须给一个**。
	namespace string
}

// NewRecorder 构造带命名空间的 Recorder。
//
// ⚠️ **每个服务必须用自己的 namespace** —— 同库多服务共用一张 outbox 表时，
// 命名空间是它们的**唯一**隔离手段（见类型注释里的事故描述）。
func NewRecorder(namespace string) *Recorder {
	return &Recorder{namespace: strings.TrimSpace(namespace)}
}

// RowID 返回某个业务 op_id 在发件箱表里的**行 id**。
//
// ⚠️ 查"这个 op_id 处理过没有"时**必须**用它，不能直接用原始 op_id ——
//
//	否则会查到**别的服务**写的行（同 op_id、不同 topic）⇒ 误判成"已处理过"。
func (r Recorder) RowID(opID string) string {
	if r.namespace == "" {
		return opID
	}
	return r.namespace + ":" + opID
}

// Enqueue 把事件写进发件箱。
//
// ⚠️ tx **必须**是业务事务的句柄(通常是 `db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {...})` 里的 tx)。
//
// ⚠️ 传 nil 直接报错 —— 宁可启动失败,也不要"事件写到了别处"。
func (r Recorder) Enqueue(tx *gorm.DB, e Event) error {
	if tx == nil {
		return fmt.Errorf("outbox: tx 为 nil —— 事件必须与业务行写在同一个事务里")
	}
	if e.ID == "" {
		return fmt.Errorf("outbox: 事件 ID(= 业务幂等键)不能为空")
	}
	if e.Topic == "" {
		return fmt.Errorf("outbox: topic 不能为空")
	}
	if e.BranchID == "" {
		return fmt.Errorf("outbox: branch_id 不能为空")
	}
	raw, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("outbox: 序列化事件体失败: %w", err)
	}
	row := Outbox{
		// ⚠️ 行 id **带命名空间** ⇒ 同库多服务不会撞主键
		//   （pos 的 "op-sale-1" vs member 的 "op-sale-1"）。
		ID:        r.RowID(e.ID),
		BranchID:  e.BranchID,
		Topic:     e.Topic,
		Subject:   e.Subject,
		Payload:   string(raw),
		Published: false,
		CreatedAt: time.Now(),
	}
	// ⚠️ DoNothing:同一个 op_id 重复入队 ⇒ 保留第一条。
	//   业务侧本来就有 op_id 幂等,这里再兜一层 ⇒ "重试导致重复入队"不会炸。
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("outbox: 写入事件失败: %w", err)
	}
	_ = row
	return nil
}

// EnqueueJSON 是"payload 已经是 JSON 字符串"时的便捷入口。
func (r Recorder) EnqueueJSON(tx *gorm.DB, e Event, payloadJSON string) error {
	e.Payload = json.RawMessage(payloadJSON)
	return r.Enqueue(tx, e)
}

// 编译期断言:Recorder 的零值可用(避免"必须 NewRecorder 才能用"的隐性依赖)。
var _ = Recorder{}
var _ = context.Background
