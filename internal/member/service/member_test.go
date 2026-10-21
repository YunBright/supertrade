package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// TestCalculatePoints_Rounding 是**取整方式的守卫测试**。
//
// ⚠️ 为什么必须锁死:增补 §2.10 的实测样本(思迅 `card_paylist.vip_acc`)是:
//
//	22.20→22  69.60→70  38.50→39  43.00→43  74.00→74  21.70→22  20.50→21  18.00→18
//
//	⇒ **四舍五入 8/8 全中**;数学 ceil 7/8(22.20 会得 23,与思迅对不上)。
//
// ⚠️ 契约里把这条规则叫 "ceiling",但**数学上的 ceil(22.20)=23**,
//
//	与它自己的样本矛盾 ⇒ 本实现按实测样本走(见 service.PointsRounding 的注释)。
//	⇒ 取整方式一旦改成 floor,**每笔都对不上**,而差额看起来"很小",极难察觉。
//
// ⚠️ 反证:本测试同时断言 floor 会给出**不同**的结果 ——
//
//	断言必须能区分不同的取整方式,否则它抓不住缺陷。
func TestCalculatePoints_Rounding(t *testing.T) {
	rate := decimal.NewFromInt(1)
	cases := []struct {
		amount string
		want   string
	}{
		// 契约 §4.7 / 增补 §2.10 点名的那四个样本。
		{"22.20", "22"},
		{"20.50", "21"},
		{"38.50", "39"},
		{"69.60", "70"},
		// 增补 §2.10 实测表的其余四个。
		{"21.70", "22"},
		{"43.00", "43"},
		{"74.00", "74"},
		{"18.00", "18"},
	}
	for _, c := range cases {
		amount, err := decimal.NewFromString(c.amount)
		require.NoError(t, err)
		got := service.CalculatePoints(amount, rate)
		assert.Equal(t, c.want, got.String(), "%s 元应得 %s 积分", c.amount, c.want)
	}
	// ⚠️ 反证 ①:floor 会给出**不同**的值(小数部分 ≥ .5 的四个样本)。
	//
	//	断言必须能区分"四舍五入"与"向下取整",否则它抓不住缺陷。
	for _, c := range []struct{ amount, want string }{
		{"20.50", "21"}, {"38.50", "39"}, {"69.60", "70"}, {"21.70", "22"},
	} {
		amount := decimal.RequireFromString(c.amount)
		assert.NotEqual(t, c.want, amount.Floor().String(),
			"floor(%s)=%s 与期望不同,断言才有区分力", c.amount, amount.Floor())
	}
	// ⚠️ 反证 ②:数学 ceil 会给出**不同**的值(22.20 这一个样本就能区分)。
	//
	//	这条正是"契约写 ceiling 但自己的样本 ceil(22.20)=22"那个矛盾的守卫:
	//	若实现改成数学 ceil,这里立刻变红。
	assert.NotEqual(t, "22", decimal.RequireFromString("22.20").Ceil().String(),
		"数学 ceil(22.20)=23 ≠ 22 —— 实现必须是四舍五入,不是数学 ceil")
	// 金额 <= 0 ⇒ 0 积分(退款单不该产生积分)。
	assert.Equal(t, "0", service.CalculatePoints(decimal.NewFromInt(-10), rate).String())
	assert.Equal(t, "0", service.CalculatePoints(decimal.Zero, rate).String())
	// 积分率 <= 0 ⇒ 回落默认 1.0(配置写坏不该让会员白拿积分)。
	assert.Equal(t, "22", service.CalculatePoints(decimal.RequireFromString("22.20"), decimal.Zero).String())
}

// TestOpenCard_Idempotent 锁"同一 op_id 开卡只开一张"。
func TestOpenCard_Idempotent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	ctx := context.Background()

	row, err := svc.OpenCard(ctx, service.OpenCardInput{
		OpID: "op-open-1", BranchID: "10", ID: "M0001",
		CardTypeID: "T1", Name: "张三", OperatorID: "u-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "M0001", row.ID)
	assert.Equal(t, model.MemberStatusNormal, row.Status)

	// 重放:同一 op_id ⇒ 不开第二张卡,且**不报错**。
	again, err := svc.OpenCard(ctx, service.OpenCardInput{
		OpID: "op-open-1", BranchID: "10", ID: "M0002",
		CardTypeID: "T1", Name: "李四", OperatorID: "u-1",
	})
	require.NoError(t, err, "重放必须返回幂等的成功响应,不能报错")
	assert.Equal(t, "M0001", again.ID, "重放应返回首次开出的那张卡")

	var n int64
	require.NoError(t, db.Model(&model.Member{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "members 只应有 1 行")
}

// TestOpenCard_RejectsTooLongIDs 锁"超长 id / op_id ⇒ 400 而不是 500"。
//
// ⚠️ 2026-10-21 契约测试实测踩到：超长 op_id 写进 varchar(64) 会**数据库报错**
// ⇒ handler 的 mapErr 落到 default 分支 ⇒ 返 **500 internal_error**
// （"系统繁忙，请稍后重试"）—— 与"参数写错了"毫无关系，现场根本想不到是长度。
//
// ⇒ 落库前拦住，返 ErrInvalidInput（⇒ 400 bad_request）。
func TestOpenCard_RejectsTooLongIDs(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	ctx := context.Background()

	long := strings.Repeat("x", model.MaxIDLen+1)

	_, err := svc.OpenCard(ctx, service.OpenCardInput{
		OpID: long, BranchID: "10", ID: "M0001",
		CardTypeID: "T1", Name: "张三", OperatorID: "u-1",
	})
	require.Error(t, err, "超长 op_id 必须被拒")
	assert.ErrorIs(t, err, model.ErrInvalidInput,
		"必须是入参非法(⇒ 400)，不能是内部错误(⇒ 500)")

	_, err = svc.OpenCard(ctx, service.OpenCardInput{
		OpID: "op-open-1", BranchID: "10", ID: long,
		CardTypeID: "T1", Name: "张三", OperatorID: "u-1",
	})
	require.Error(t, err, "超长会员号必须被拒")
	assert.ErrorIs(t, err, model.ErrInvalidInput,
		"必须是入参非法(⇒ 400)，不能是内部错误(⇒ 500)")

	var n int64
	require.NoError(t, db.Model(&model.Member{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "被拒的行不得落库")
}

// TestOpenCard_WritesEvent 锁"开卡事件与会员行同事务落库"。
func TestOpenCard_WritesEvent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	ctx := context.Background()

	_, err := svc.OpenCard(ctx, service.OpenCardInput{
		OpID: "op-open-1", BranchID: "10", ID: "M0001",
		CardTypeID: "T1", Name: "张三", OperatorID: "u-1",
	})
	require.NoError(t, err)

	var row outbox.Outbox
	require.NoError(t, db.Where("topic = ?", service.TopicMemberOpened).First(&row).Error)
	assert.Equal(t, "member:op-open-1", row.ID, "发件箱行 id 带服务命名空间(同库多服务共用一张 outbox 表)")
	assert.Equal(t, "10", row.BranchID)
	assert.Contains(t, row.Payload, `"member_id":"M0001"`)
	assert.Contains(t, row.Payload, `"card_type_id":"T1"`)
}

// TestDeductPoints_Idempotent_ReplayReturnsFirstResult 锁"同一 op_id 只扣一次积分"。
func TestDeductPoints_Idempotent_ReplayReturnsFirstResult(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	// 先赠 100 分。
	_, err := svc.GrantPoints(ctx, service.GrantPointsInput{
		OpID: "op-grant-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(100), OperatorID: "u-1",
	})
	require.NoError(t, err)

	first, err := svc.DeductPoints(ctx, service.DeductPointsInput{
		OpID: "op-deduct-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(30), OperatorID: "u-1",
	})
	require.NoError(t, err)
	require.NotNil(t, first.Ledger)
	assert.Equal(t, "-30", first.Ledger.Points.String(), "扣减的本次变动是负数")
	assert.Equal(t, "100", first.Ledger.PointsAfter.String(), "points_after 是变动后的累计获得值")

	// 重放:同一 op_id ⇒ 只落一条流水,返回**首次结果**,且积分不再变。
	second, err := svc.DeductPoints(ctx, service.DeductPointsInput{
		OpID: "op-deduct-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(30), OperatorID: "u-1",
	})
	require.NoError(t, err, "重放必须返回幂等的成功响应,不能报错")
	assert.Equal(t, first.Ledger.ID, second.Ledger.ID, "重放应返回首次那一行")

	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).
		Where("op_id = ?", "op-deduct-1").Count(&n).Error)
	assert.Equal(t, int64(1), n, "member_point_ledger 只应有 1 行")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "100", member.PointsAccumulated.String())
	assert.Equal(t, "30", member.PointsUsed.String())
	assert.Equal(t, "70", member.RemainingPoints.String(), "剩余积分 = 累计获得 − 累计扣减")
}

// TestDeductPoints_Insufficient 锁"积分不足 ⇒ 409"。
func TestDeductPoints_Insufficient(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	_, err := svc.GrantPoints(ctx, service.GrantPointsInput{
		OpID: "op-grant-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(10), OperatorID: "u-1",
	})
	require.NoError(t, err)

	_, err = svc.DeductPoints(ctx, service.DeductPointsInput{
		OpID: "op-deduct-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(30), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrPointsInsufficient), "应 ErrPointsInsufficient,实际 %v", err)
}

// TestDeductPoints_StatusNotAllowed 锁"冻结的卡不许动积分 ⇒ 409"。
func TestDeductPoints_StatusNotAllowed(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Update("status", model.MemberStatusFrozen).Error)

	_, err := svc.DeductPoints(ctx, service.DeductPointsInput{
		OpID: "op-deduct-1", BranchID: "10", MemberID: "M0001",
		Points: decimal.NewFromInt(1), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberStatusNotAllowed), "应 ErrMemberStatusNotAllowed,实际 %v", err)
}

// TestMemberNotFound 锁"会员不存在 ⇒ 404"。
func TestMemberNotFound(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	ctx := context.Background()

	_, err := svc.Get(ctx, "10", "NOPE")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberNotFound))

	_, err = svc.DeductPoints(ctx, service.DeductPointsInput{
		OpID: "op-deduct-1", BranchID: "10", MemberID: "NOPE",
		Points: decimal.NewFromInt(1), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberNotFound))
}

// TestSoftDelete_MemberNotFound 锁"软删行查不到"。
//
// ⚠️ 这是 `gorm.DeletedAt` 与 `*time.Time` 的分水岭:
//
//	用 `*time.Time` 的话 GORM **不会**自动加 deleted_at IS NULL ⇒ 删了还能查到。
func TestSoftDelete_MemberNotFound(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	require.NoError(t, svc.Delete(ctx, "10", "M0001", "u-1"))

	_, err := svc.Get(ctx, "10", "M0001")
	require.Error(t, err, "软删后详情必须查不到")
	assert.True(t, errors.Is(err, model.ErrMemberNotFound))

	rows, err := svc.ListMembers(ctx, service.ListMembersInput{BranchID: "10"})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "软删后列表里也不该有它")
}

// TestListMembers_ByField 锁"多字段查询(白名单)"。
func TestListMembers_ByField(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Updates(map[string]any{"phone": "13800000001", "remark": "老顾客"}).Error)
	ctx := context.Background()

	rows, err := svc.ListMembers(ctx, service.ListMembersInput{
		BranchID: "10", Field: model.QueryFieldPhone, Q: "138000",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = svc.ListMembers(ctx, service.ListMembersInput{
		BranchID: "10", Field: model.QueryFieldRemark, Q: "老顾客",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = svc.ListMembers(ctx, service.ListMembersInput{
		BranchID: "10", Field: model.QueryFieldCardID, Q: "M0001",
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	// 非法字段 ⇒ 400(白名单挡住 SQL 注入面)。
	_, err = svc.ListMembers(ctx, service.ListMembersInput{
		BranchID: "10", Field: "1=1; DROP TABLE members--", Q: "x",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidQueryField))
}

// TestAccountRecharge_Idempotent 锁"同一 op_id 充值只充一次"。
func TestAccountRecharge_Idempotent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "储值卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	first, err := svc.Recharge(ctx, service.AccountOpInput{
		OpID: "op-recharge-1", BranchID: "10", MemberID: "M0001",
		AmountYuan: decimal.RequireFromString("100.00"), OperatorID: "u-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "100", first.BalanceAfter.String())

	second, err := svc.Recharge(ctx, service.AccountOpInput{
		OpID: "op-recharge-1", BranchID: "10", MemberID: "M0001",
		AmountYuan: decimal.RequireFromString("100.00"), OperatorID: "u-1",
	})
	require.NoError(t, err, "重放必须返回幂等的成功响应")
	assert.Equal(t, first.Ledger.ID, second.Ledger.ID)

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "100", member.BalanceYuan.String(), "余额不能被充两次")

	var n int64
	require.NoError(t, db.Model(&model.MemberAccountLedger{}).
		Where("op_id = ?", "op-recharge-1").Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestAccountRefund_Insufficient 锁"退储超过余额 ⇒ 409"。
func TestAccountRefund_Insufficient(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "储值卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	_, err := svc.Refund(ctx, service.AccountOpInput{
		OpID: "op-refund-1", BranchID: "10", MemberID: "M0001",
		AmountYuan: decimal.NewFromInt(10), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAccountInsufficient))
}

// TestUpdateStatus_Idempotent 锁"同一 op_id 改状态只改一次(且事件只入队一次)"。
func TestUpdateStatus_Idempotent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	_, err := svc.UpdateStatus(ctx, service.UpdateStatusInput{
		OpID: "op-status-1", BranchID: "10", MemberID: "M0001",
		Status: model.MemberStatusFrozen, OperatorID: "u-1",
	})
	require.NoError(t, err)

	_, err = svc.UpdateStatus(ctx, service.UpdateStatusInput{
		OpID: "op-status-1", BranchID: "10", MemberID: "M0001",
		Status: model.MemberStatusFrozen, OperatorID: "u-1",
	})
	require.NoError(t, err, "重放必须返回幂等的成功响应")

	var n int64
	require.NoError(t, db.Model(&outbox.Outbox{}).
		Where("id = ? AND topic = ?", "member:op-status-1", service.TopicMemberUpdated).Count(&n).Error)
	assert.Equal(t, int64(1), n, "member.updated 事件只应入队一次")
}

// TestPasswordHash_Verify 锁"口令哈希可校验,且不存明文"。
func TestPasswordHash_Verify(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	_, err := svc.UpdatePassword(ctx, service.UpdatePasswordInput{
		OpID: "op-pwd-1", BranchID: "10", MemberID: "M0001",
		Password: "123456", OperatorID: "u-1",
	})
	require.NoError(t, err)

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.NotEmpty(t, member.PasswordHash)
	assert.NotContains(t, member.PasswordHash, "123456", "不能存明文")

	ok, err := service.VerifyPassword("123456", member.PasswordHash)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = service.VerifyPassword("654321", member.PasswordHash)
	require.NoError(t, err)
	assert.False(t, ok)

	// 改密时原口令错 ⇒ ErrInvalidOldPassword。
	_, err = svc.UpdatePassword(ctx, service.UpdatePasswordInput{
		OpID: "op-pwd-2", BranchID: "10", MemberID: "M0001",
		Password: "999999", OldPassword: "wrong", OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrInvalidOldPassword))
}
