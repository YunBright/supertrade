// Package outbox —— 发件箱守卫测试。
//
// # 这些测试守的是什么
//
// "事件与业务行在同一个事务里"这条保证,**一旦破掉是零症状的**:
//
//	编译通过、单测全绿、生产跑得好好的,
//	只有"进程恰好在 commit 与 publish 之间被 kill"时才丢事件 ——
//	而那正是"收银机断电/进程被杀"最常发生的时刻。
//
// ⇒ 必须有一条**能真的变红**的守卫测试,而不是靠 code review 盯着。
//
// # 三条不可协商的断言
//
//	① tx 为 nil ⇒ 报错(不许偷偷用别的连接)
//	② 业务事务回滚 ⇒ 事件行**也跟着没了**
//	③ 投递失败 ⇒ 行还在、attempts 递增、published 仍为 false
package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/pkg/outbox"
)

// bizRow 是"业务行"的替身:用来证明它与事件行同生共死。
type bizRow struct {
	ID   string `gorm:"primaryKey;column:id;type:varchar(64)"`
	Note string `gorm:"column:note;type:varchar(64)"`
}

func (bizRow) TableName() string { return "biz_row" }

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// ⚠️ 用**文件型** sqlite 而不是 `:memory:`。
	//
	// `:memory:` 是**按连接**隔离的 ⇒ 必须把连接池压到 1,
	// 于是"事务里再从 db 开一条连接"会**死锁**。
	// 而这恰恰是反证要测的场景(事件写到事务外)——
	// 用内存库时它表现为"测试卡住"而不是"测试变红",没法当守卫。
	//
	// 文件型 sqlite 多个连接看到同一份数据 ⇒ 两种写法都能正常跑完,
	// 断言才有意义。
	dsn := filepath.Join(t.TempDir(), "outbox_test.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&outbox.Outbox{}, &bizRow{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// fakePublisher 是假 publisher:记录投递,并可按需失败。
type fakePublisher struct {
	calls   []publishCall
	fail    bool
	failMsg string
}

type publishCall struct {
	Topic   string
	Payload []byte
}

func (f *fakePublisher) Publish(ctx context.Context, topic string, data any) error {
	if f.fail {
		return errors.New(f.failMsg)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	f.calls = append(f.calls, publishCall{Topic: topic, Payload: raw})
	return nil
}

// TestEnqueueRequiresTx —— tx 为 nil 必须报错。
//
// ⚠️ 这是"不许偷偷用别的连接"的守卫:
// 若实现里改成 `r.db.Create(...)`(Recorder 自己持有 db),
// 事件就会写到**另一个连接**上 ⇒ 与业务行不在同一事务 ⇒ 丢事件。
func TestEnqueueRequiresTx(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}

	err := rec.Enqueue(nil, outbox.Event{
		ID: "op-1", BranchID: "10", Topic: "sale.paid", Payload: map[string]any{"x": 1},
	})
	require.Error(t, err, "tx 为 nil 时必须报错,不能写到别处")
	require.Contains(t, err.Error(), "tx")

	var n int64
	require.NoError(t, db.Model(&outbox.Outbox{}).Count(&n).Error)
	require.Zero(t, n, "报错时不能留下一行事件")
}

// TestEnqueueAndBusinessRowShareOneTransaction —— **核心守卫**。
//
// 业务事务回滚 ⇒ 事件行必须一起消失。
//
// ⇒ 事件与业务**同生共死** ⇒ 进程在 commit 前被杀 ⇒ 什么都没写(一致)
// ⇒ 进程在 commit 后被杀 ⇒ 事件在发件箱里等着(不丢)
func TestEnqueueAndBusinessRowShareOneTransaction(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}

	sentinel := errors.New("force rollback")
	err := db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Create(&bizRow{ID: "B-1", Note: "销售单"}).Error)
		require.NoError(t, rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid",
			Payload: map[string]any{"sale_id": "SO1"},
		}))
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	var bizCount, evCount int64
	require.NoError(t, db.Model(&bizRow{}).Count(&bizCount).Error)
	require.NoError(t, db.Model(&outbox.Outbox{}).Count(&evCount).Error)
	require.Zero(t, bizCount, "业务行应已回滚")
	require.Zero(t, evCount, "事件行必须与业务行同事务回滚(否则事件会指向不存在的业务)")
}

// TestEnqueueInsideCommittedTransactionPersists —— 提交后两行都在。
func TestEnqueueInsideCommittedTransactionPersists(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}

	require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&bizRow{ID: "B-1"}).Error; err != nil {
			return err
		}
		return rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid", Payload: map[string]any{"a": 1},
		})
	}))

	var bizCount, evCount int64
	require.NoError(t, db.Model(&bizRow{}).Count(&bizCount).Error)
	require.NoError(t, db.Model(&outbox.Outbox{}).Count(&evCount).Error)
	require.EqualValues(t, 1, bizCount)
	require.EqualValues(t, 1, evCount)
}

// TestEnqueueRejectsIncompleteEvent —— 缺 ID / topic / branch 都必须报错。
func TestEnqueueRejectsIncompleteEvent(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}

	cases := []struct {
		name string
		ev   outbox.Event
	}{
		{"缺 ID", outbox.Event{BranchID: "10", Topic: "sale.paid", Payload: map[string]any{}}},
		{"缺 topic", outbox.Event{ID: "op-1", BranchID: "10", Payload: map[string]any{}}},
		{"缺 branch", outbox.Event{ID: "op-1", Topic: "sale.paid", Payload: map[string]any{}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
				return rec.Enqueue(tx, c.ev)
			})
			require.Error(t, err)
		})
	}
}

// TestEnqueueDuplicateOpIDKeepsFirst —— 同一个 op_id 重复入队只留一行。
//
// 场景:业务重试导致同一个 op_id 被 Enqueue 两次。
// ⇒ 业务侧本来就有 op_id 幂等,这里再兜一层,免得"重复入队"把
//
//	唯一索引撞成 500(那会让重试永远失败)。
func TestEnqueueDuplicateOpIDKeepsFirst(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}

	require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid", Payload: map[string]any{"n": 1},
		})
	}))

	// 第二次:同一个 op_id,不同 payload ⇒ 期望保留第一条。
	err := db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid", Payload: map[string]any{"n": 2},
		})
	})
	// 要么被唯一索引挡下(报错),要么被 DoNothing 吞掉 —— 两种都可接受,
	// 但**绝不能**留下两行。
	if err != nil {
		t.Logf("第二次入队被唯一索引拒绝(可接受): %v", err)
	}

	var rows []outbox.Outbox
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1, "同一个 op_id 只能有一行事件")
	require.Contains(t, rows[0].Payload, `"n":1`, "应保留第一条")
}

// TestNamespacesDoNotCollide —— 两个服务用**同一个 op_id** 各写一行，不得撞主键。
//
// # 为什么这条要锁死（2026-10-21 生产实测踩到的坑）
//
// pos 与 member **共用同一个 PG 库**，`outbox` 表是**同一张**。
// 若两边的行 id 都直接用业务 op_id：
//
//	pos     写: id = "op-sale-123"  (topic=sale.paid)
//	member 写: id = "op-sale-123"  (topic=member.points.changed)
//	⇒ duplicate key value violates unique constraint "outbox_pkey"
//
// 症状**极其安静**：member 那一整个事务回滚 ⇒ 积分没加、流水没有，
// 而日志里只有一行 SQL 错误（很容易被当成"数据库抖动"忽略掉）。
//
// ⇒ 命名空间是**唯一**的隔离手段 ⇒ 必须有守卫测试。
func TestNamespacesDoNotCollide(t *testing.T) {
	db := newTestDB(t)
	posRec := outbox.NewRecorder("pos")
	memRec := outbox.NewRecorder("member")

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return posRec.Enqueue(tx, outbox.Event{
			ID: "op-sale-1", BranchID: "10", Topic: "sale.paid",
			Payload: map[string]any{"k": "pos"},
		})
	}))

	// ⚠️ 同一个 op_id —— 不加命名空间就会撞主键。
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return memRec.Enqueue(tx, outbox.Event{
			ID: "op-sale-1", BranchID: "10", Topic: "member.points.changed",
			Payload: map[string]any{"k": "member"},
		})
	}))

	var rows []outbox.Outbox
	require.NoError(t, db.Order("id asc").Find(&rows).Error)
	require.Len(t, rows, 2, "两个服务各一行,不能互相挤掉")
	require.Equal(t, "member:op-sale-1", rows[0].ID)
	require.Equal(t, "pos:op-sale-1", rows[1].ID)
}

// TestRowIDIsWhatMustBeQueried —— 查"处理过没有"必须用 RowID。
//
// ⚠️ 用**原始 op_id** 去查 ⇒ 会查到**别的服务**写的行
//
//	（同 op_id、不同 topic）⇒ 误判成"已处理过" ⇒ 业务被静默跳过。
func TestRowIDIsWhatMustBeQueried(t *testing.T) {
	posRec := outbox.NewRecorder("pos")
	memRec := outbox.NewRecorder("member")

	require.Equal(t, "pos:op-1", posRec.RowID("op-1"))
	require.Equal(t, "member:op-1", memRec.RowID("op-1"))
	require.NotEqual(t, posRec.RowID("op-1"), memRec.RowID("op-1"))

	// 空命名空间 = 旧行为（行 id 就是 op_id）。
	require.Equal(t, "op-1", outbox.Recorder{}.RowID("op-1"))
}

// TestRunnerPublishesPendingAndMarksPublished —— 正常投递路径。
func TestRunnerPublishesPendingAndMarksPublished(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}
	require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid",
			Payload: map[string]any{"sale_id": "SO1", "total": "12.50"},
		})
	}))

	pub := &fakePublisher{}
	run := outbox.NewRunner(db, pub, nil)

	n, err := run.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, pub.calls, 1)
	require.Equal(t, "sale.paid", pub.calls[0].Topic)
	// ⚠️ 金额必须是**字符串**且逐字保留:JSON number 到 Dart 侧会变 double,精度当场丢。
	require.Contains(t, string(pub.calls[0].Payload), `"total":"12.50"`)

	var row outbox.Outbox
	require.NoError(t, db.First(&row, "id = ?", "op-1").Error)
	require.True(t, row.Published, "投递成功后应标记 published")
	require.NotNil(t, row.PublishedAt)
	require.Equal(t, 1, row.Attempts)
	require.Empty(t, row.LastError)
}

// TestRunnerFailureKeepsRowForRetry —— 投递失败**不丢事件**。
//
// ⚠️ 这是"publish 失败也不能丢 sale.paid"的守卫:
// 失败时若把行标成 published(或删掉),事件就永远丢了 ⇒
// 库存不扣、积分不加,而且**没有任何日志**说这件事。
func TestRunnerFailureKeepsRowForRetry(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}
	require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return rec.Enqueue(tx, outbox.Event{
			ID: "op-1", BranchID: "10", Topic: "sale.paid", Payload: map[string]any{"a": 1},
		})
	}))

	pub := &fakePublisher{fail: true, failMsg: "sidecar unreachable"}
	run := outbox.NewRunner(db, pub, nil)

	n, err := run.RunOnce(context.Background())
	require.NoError(t, err, "投递失败不应让整轮报错崩掉")
	require.Equal(t, 0, n)

	var row outbox.Outbox
	require.NoError(t, db.First(&row, "id = ?", "op-1").Error)
	require.False(t, row.Published, "投递失败绝不能标记 published")
	require.Equal(t, 1, row.Attempts)
	require.Contains(t, row.LastError, "sidecar unreachable")

	// 第二轮:publisher 恢复 ⇒ 事件必须被投出去。
	pub.fail = false
	n, err = run.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, pub.calls, 1)

	require.NoError(t, db.First(&row, "id = ?", "op-1").Error)
	require.True(t, row.Published)
}

// TestRunnerSkipsAlreadyPublished —— 已发布的行不重复投。
func TestRunnerSkipsAlreadyPublished(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}
	require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
		return rec.Enqueue(tx, outbox.Event{ID: "op-1", BranchID: "10", Topic: "t", Payload: map[string]any{}})
	}))

	pub := &fakePublisher{}
	run := outbox.NewRunner(db, pub, nil)
	_, err := run.RunOnce(context.Background())
	require.NoError(t, err)
	_, err = run.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, pub.calls, 1, "已发布的行不能被再投一次")
}

// TestRunnerDeliversInEnqueueOrder —— 事件按入队顺序投(订阅方依赖顺序)。
func TestRunnerDeliversInEnqueueOrder(t *testing.T) {
	db := newTestDB(t)
	rec := outbox.Recorder{}
	for i := 1; i <= 3; i++ {
		id := "op-" + string(rune('0'+i))
		require.NoError(t, db.WithContext(context.Background()).Transaction(func(tx *gorm.DB) error {
			return rec.Enqueue(tx, outbox.Event{
				ID: id, BranchID: "10", Topic: "t", Payload: map[string]any{"n": i},
			})
		}))
		// 造出可区分的 created_at(SQLite 的时间精度可能只到秒)。
		require.NoError(t, db.Model(&outbox.Outbox{}).Where("id = ?", id).
			Update("created_at", time.Now().Add(time.Duration(i)*time.Second)).Error)
	}

	pub := &fakePublisher{}
	run := outbox.NewRunner(db, pub, nil)
	_, err := run.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, pub.calls, 3)
	require.Contains(t, string(pub.calls[0].Payload), `"n":1`)
	require.Contains(t, string(pub.calls[1].Payload), `"n":2`)
	require.Contains(t, string(pub.calls[2].Payload), `"n":3`)
}

// TestRunnerWithoutPublisherIsNoop —— 没装配 publisher 时不能 panic。
func TestRunnerWithoutPublisherIsNoop(t *testing.T) {
	db := newTestDB(t)
	run := outbox.NewRunner(db, nil, nil)
	_, err := run.RunOnce(context.Background())
	require.Error(t, err, "没 publisher 应明确报错,而不是静默丢事件")
}
