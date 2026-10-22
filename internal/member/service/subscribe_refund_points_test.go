package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// seedMemberWithPoints 造一个带累计积分的会员。
func seedMemberWithPoints(t *testing.T, db *gorm.DB, id, branchID string, points decimal.Decimal) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", Name: "张三",
		Status:            model.MemberStatusNormal,
		PointsAccumulated: points, PointsUsed: decimal.Zero,
		BalanceYuan: decimal.Zero, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// TestOnSalePaid_ReturnOrder_DeductsPoints 锁"**退货扣回积分**"。
//
// # 为什么(2026-10-22 源库实测)
//
//	思迅 `t_rm_vip_info_sign` 全库 18,407 行里只有 **8 行** `vip_acc_num < 0`,
//	而这 8 行**全部**是退货台单据(flow_no 前 2 位 = "07"):
//
//		07202610120002 −3 / 07202610120001 −31 / 07202610030001 −76 / …
//
//	⇒ 思迅**按退掉金额等额扣回积分**(积分率 = 1 分/元)。
//
// ⚠️ 旧实现对 order_status="R" 直接 Skip ⇒ 顾客退了货积分还留在账上
//   ⇒ 积分虚高 ⇒ 抵扣时能抵不该抵的钱。
func TestOnSalePaid_ReturnOrder_DeductsPoints(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMemberWithPoints(t, db, "M0001", "10", decimal.NewFromInt(100))
	ctx := context.Background()

	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-refund-1",
		SaleID:          "SO20261021000002",
		BranchID:        "10",
		MemberID:        "M0001",
		OperatorID:      "u-1",
		TotalAmountYuan: decimal.RequireFromString("-31.00"),
		OrderStatus:     "R",
	})
	require.NoError(t, err)
	assert.False(t, res.Skipped, "有积分可扣 ⇒ 不能跳过")
	assert.Equal(t, "31", res.Points.String(), "退 31 元 ⇒ 扣回 31 分")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "69", member.PointsAccumulated.String(), "累计积分 100 − 31 = 69")

	// 流水:direction=deduct、points 为负。
	var rows []model.MemberPointLedger
	require.NoError(t, db.Where("member_id = ?", "M0001").Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, model.PointDirectionDeduct, rows[0].Direction)
	assert.Equal(t, "-31", rows[0].Points.String(), "deduct 的 points 是负数")
	assert.Equal(t, "69", rows[0].PointsAfter.String())
}

// TestOnSalePaid_ReturnOrder_NotNegative 锁"扣回**不能把累计扣成负数**"。
//
// ⚠️ 顾客可能已经把积分用掉了 ⇒ 只能扣到 0。
func TestOnSalePaid_ReturnOrder_NotNegative(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMemberWithPoints(t, db, "M0001", "10", decimal.NewFromInt(5))
	ctx := context.Background()

	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-refund-1",
		SaleID:          "SO20261021000002",
		BranchID:        "10",
		MemberID:        "M0001",
		TotalAmountYuan: decimal.RequireFromString("-31.00"),
		OrderStatus:     "R",
	})
	require.NoError(t, err)
	assert.Equal(t, "5", res.Points.String(), "只有 5 分可扣 ⇒ 扣 5,不扣成负数")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "0", member.PointsAccumulated.String(), "累计积分不能为负")
}

// TestOnSalePaid_ReturnOrder_DuplicateDelivery_DeductsOnce 是**幂等守卫**。
func TestOnSalePaid_ReturnOrder_DuplicateDelivery_DeductsOnce(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMemberWithPoints(t, db, "M0001", "10", decimal.NewFromInt(100))
	ctx := context.Background()

	payload := service.SalePaidPayload{
		OpID:            "op-refund-1",
		SaleID:          "SO20261021000002",
		BranchID:        "10",
		MemberID:        "M0001",
		TotalAmountYuan: decimal.RequireFromString("-31.00"),
		OrderStatus:     "R",
	}
	_, err := svc.OnSalePaid(ctx, payload)
	require.NoError(t, err)
	res, err := svc.OnSalePaid(ctx, payload)
	require.NoError(t, err)
	assert.True(t, res.Replayed, "同 op_id 重复投递 ⇒ 幂等命中")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "69", member.PointsAccumulated.String(), "重复投递不得二次扣积分")

	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "积分流水只有 1 行")
}
