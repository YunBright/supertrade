package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
)

// TestCreate_SaleIDSeedUsesMaxSeqNotRowCount 锁"**每日播种用最大序号,不用行数**"。
//
// # 2026-10-22 生产实测复现的真实缺陷
//
//	原播种是 `countSalesOfDay` = **当天已有多少行** ⇒ 下一个号 = n+1。
//	⇒ 这只在"号段从 1 开始连续"时成立。
//
//	生产实测(2026-10-22 部署 pos 后立刻开单):
//
//		HTTP 500 {"code":"internal_error","message":"pos: 销售单号冲突重试 20 次仍失败"}
//
//	原因:当天已发出的号到 00100,而 count 数出 60 ⇒ 下一个 000061 **已存在**
//	⇒ 重试 62…80 也全存在 ⇒ 20 次用尽 ⇒ 开单 500。
//
//	两个成因叠加:
//	  ① **进程在当天中途重启**(每次部署都重启)⇒ 重新播种;
//	  ② **回放台的行被软删**(`deleted_at` 非空)⇒ count 更偏。
//
// # 这条测试怎么复现
//
//	开 30 张单(00001..00030)⇒ 软删前 20 张 ⇒ 行数只剩 10,
//	但**已发出的最大号是 00030**。
//	⇒ 旧口径:count=10 ⇒ 下一个 00011 ⇒ 已存在 ⇒ 重试 20 次全撞 ⇒ 失败;
//	⇒ 新口径:max=30 ⇒ 下一个 00031 ⇒ 一次成功。
//
//	⚠️ 断言的是"新号 > 任何已用过的号",而不是某个具体数字
//	(具体数字依赖测试的播种时刻)。
func TestCreate_SaleIDSeedUsesMaxSeqNotRowCount(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// 开 30 张单。
	for i := 0; i < 30; i++ {
		svc := service.NewSalesService(db)
		_, _, err := svc.Create(ctx, validInput("op-"+string(rune('a'+i%26))+string(rune('a'+i/26))))
		require.NoError(t, err, "第 %d 张单应开成功", i)
	}
	var maxSeq int64
	require.NoError(t, db.Model(&model.SalesOrder{}).
		Select("COALESCE(MAX(CAST(substr(id, 11, 6) AS INTEGER)), 0)").Scan(&maxSeq).Error)
	require.Equal(t, int64(30), maxSeq, "已发出的最大序号应是 30")

	// 软删前 20 张(模拟回放台 sweep 清理)。
	require.NoError(t, db.Where("id IN (?)",
		db.Model(&model.SalesOrder{}).Select("id").Order("id asc").Limit(20)).
		Delete(&model.SalesOrder{}).Error)
	var left int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Count(&left).Error)
	require.Equal(t, int64(10), left, "软删后只剩 10 行(行数 ≠ 已发出的最大序号)")

	// 模拟**进程重启**(新的 SalesService ⇒ 新的 saleIDGen ⇒ 重新播种)。
	svc := service.NewSalesService(db)
	order, _, err := svc.Create(ctx, validInput("op-after-restart"))
	require.NoError(t, err,
		"重启后开单必须成功 —— 旧口径会撞号并报\"销售单号冲突重试 20 次仍失败\"")

	var seq int64
	require.NoError(t, db.Model(&model.SalesOrder{}).Where("id = ?", order.ID).
		Select("CAST(substr(id, 11, 6) AS INTEGER)").Scan(&seq).Error)
	assert.Greater(t, seq, maxSeq,
		"新单号序号(%d)必须大于**已发出的**最大序号(%d)—— 播种要看最大序号,不是行数", seq, maxSeq)
}
