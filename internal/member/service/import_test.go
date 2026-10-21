package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// importCtx 是导入测试的公共上下文。
func importCtx() context.Context { return context.Background() }

// scopeRow 取导入报告里某个 scope 的计数。
func scopeRow(t *testing.T, rep *archivedto.ImportResult, scope string) archivedto.ImportScopeRow {
	t.Helper()
	require.NotNil(t, rep)
	for _, r := range rep.ByScope {
		if r.Scope == scope {
			return r
		}
	}
	t.Fatalf("导入报告里没有 scope=%s 的行: %+v", scope, rep.ByScope)
	return archivedto.ImportScopeRow{}
}

// ---- 导入语义 ----

// TestImportMembers_NewMember_Bootstrap 锁"新会员 ⇒ 用思迅值初始化"。
//
// ⚠️ 这是"bootstrap 基线":积分/储值/消费/状态**从源库取**,
//   因为本域还没有这张卡、没有本域发生过的业务。
func TestImportMembers_NewMember_Bootstrap(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID:                "M0001",
			CardType:          "0", // 会员卡
			Name:              "张三",
			Phone:             "13800000000",
			Gender:            "男",
			Status:            "0", // normal
			FeeAmountYuan:     "10.00",
			LimitedUseCount:   "999998", // 不限次哨兵
			PointsAccumulated: "10",
			PointsUsed:        "2",
			BalanceYuan:       "100.50",
			ConsumedAmountYuan: "88.80",
			ConsumedCount:     "3",
			BranchNo:          "00",
			OperatorID:        "1003",
			OperatedAt:        "2024-05-01 10:00:00",
			ValidFrom:         "2024-05-01",
			ValidUntil:        "2030-05-01",
		}}},
	})
	require.NoError(t, err)

	row := scopeRow(t, rep, archivedto.ScopeMembers)
	assert.Equal(t, 1, row.Received)
	assert.Equal(t, 1, row.Inserted)
	assert.Equal(t, 0, row.Updated)
	assert.Equal(t, 0, row.Skipped)

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "T1", m.CardTypeID, "card_type=0 ⇒ 会员卡")
	assert.Equal(t, model.MemberStatusNormal, m.Status)
	assert.Equal(t, "13800000000", m.Phone)
	assert.Equal(t, 0, m.LimitedUseCount, "999998 ⇒ 0(不限次)")
	assert.Equal(t, "10", m.PointsAccumulated.String())
	assert.Equal(t, "2", m.PointsUsed.String())
	assert.Equal(t, "100.5", m.BalanceYuan.String())
	assert.Equal(t, "88.8", m.ConsumedAmountYuan.String())
	assert.Equal(t, 3, m.ConsumedCount)
	require.NotNil(t, m.ValidFrom)
	assert.Equal(t, 2024, m.ValidFrom.Year())
}

// TestImportMembers_ExistingMember_KeepsFinancialColumns 是本任务**最关键**的一条。
//
// ⚠️ 积分/储值/消费/状态是本域 POS 业务的事实源(sale.paid 订阅会给会员加积分)
//   ⇒ 导入**绝不能**覆盖它们。
//
// ⇒ 已存在的会员 ⇒ 只更新身份字段(姓名/电话/…)。
func TestImportMembers_ExistingMember_KeepsFinancialColumns(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	// 本域已有的业务事实:积分 500、余额 50、状态 frozen。
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Updates(map[string]any{
			"points_accumulated":  decimal.NewFromInt(500),
			"points_used":         decimal.NewFromInt(100),
			"balance_yuan":        decimal.NewFromInt(50),
			"consumed_amount_yuan": decimal.NewFromInt(999),
			"consumed_count":      9,
			"status":              model.MemberStatusFrozen,
		}).Error)

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID:                "M0001",
			CardType:          "0",
			Name:              "李四", // 身份字段:应当被更新
			Phone:             "13900000000",
			Status:            "0",                    // 源库说正常
			PointsAccumulated: "10",                   // 源库的旧值
			PointsUsed:        "0",
			BalanceYuan:       "0",
			ConsumedAmountYuan: "0",
			ConsumedCount:     "0",
		}}},
	})
	require.NoError(t, err)

	row := scopeRow(t, rep, archivedto.ScopeMembers)
	assert.Equal(t, 0, row.Inserted)
	assert.Equal(t, 1, row.Updated, "已存在的会员算 updated")

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	// 身份字段:更新了。
	assert.Equal(t, "李四", m.Name)
	assert.Equal(t, "13900000000", m.Phone)
	// ⚠️ 财务与状态列:**一个都不能变**。
	assert.Equal(t, "500", m.PointsAccumulated.String(), "累计积分不能被导入覆盖")
	assert.Equal(t, "100", m.PointsUsed.String(), "已用积分不能被导入覆盖")
	assert.Equal(t, "50", m.BalanceYuan.String(), "余额不能被导入覆盖")
	assert.Equal(t, "999", m.ConsumedAmountYuan.String(), "累计消费不能被导入覆盖")
	assert.Equal(t, 9, m.ConsumedCount, "消费次数不能被导入覆盖")
	assert.Equal(t, model.MemberStatusFrozen, m.Status, "状态不能被导入覆盖")
}

// TestImportMembers_OverwriteMode_OverwritesFinancial 锁"overwrite ⇒ 连财务列一起覆盖"。
//
// ⚠️ 这是**显式选择**才走的路(incremental 是默认)。
func TestImportMembers_OverwriteMode_OverwritesFinancial(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Updates(map[string]any{
			"points_accumulated": decimal.NewFromInt(500),
			"balance_yuan":       decimal.NewFromInt(50),
			"status":             model.MemberStatusFrozen,
		}).Error)

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "overwrite",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID: "M0001", CardType: "0", Name: "李四", Status: "0",
			PointsAccumulated: "10", PointsUsed: "0", BalanceYuan: "7.77",
			ConsumedAmountYuan: "20", ConsumedCount: "2",
		}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, scopeRow(t, rep, archivedto.ScopeMembers).Updated)

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "10", m.PointsAccumulated.String(), "overwrite 模式才覆盖积分")
	assert.Equal(t, "7.77", m.BalanceYuan.String())
	assert.Equal(t, model.MemberStatusNormal, m.Status)
}

// TestImportMembers_DirtyBalance_SkipsRow 锁"余额解析失败 ⇒ 该行不落库 + warning"。
//
// ⚠️ 思迅的 residual_amt 是密文/脏值("@><:6642675" / "86220.." / ",")。
//   静默当 0 = 把"有余额的会员"变成"余额 0",而余额 0 没人会去查。
func TestImportMembers_DirtyBalance_SkipsRow(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	for _, dirty := range []string{"@><:6642675", "86220..", ","} {
		rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
			TargetBranchID: "10", Mode: "incremental",
			Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
				ID: "M0001", CardType: "0", Name: "张三", Status: "0", BalanceYuan: dirty,
			}}},
		})
		require.NoError(t, err)
		row := scopeRow(t, rep, archivedto.ScopeMembers)
		assert.Equal(t, 1, row.Skipped, "脏值 %q ⇒ 跳过", dirty)
		assert.Equal(t, 0, row.Inserted)
		assert.NotEmpty(t, rep.Warnings, "必须有 warning(不能静默)")
		assert.True(t, strings.Contains(strings.Join(rep.Warnings, "|"), "余额"),
			"warning 必须说清是余额的问题: %v", rep.Warnings)
	}
	var n int64
	require.NoError(t, db.Model(&model.Member{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "脏余额的行不得落库")
}

// TestImportMembers_EmptyBalance_SkipsRow 锁"余额为空 ⇒ 同样跳过 + warning"。
func TestImportMembers_EmptyBalance_SkipsRow(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID: "M0001", CardType: "0", Name: "张三", Status: "0", BalanceYuan: "",
		}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, scopeRow(t, rep, archivedto.ScopeMembers).Skipped)
	assert.NotEmpty(t, rep.Warnings)
	var n int64
	require.NoError(t, db.Model(&model.Member{}).Count(&n).Error)
	assert.Equal(t, int64(0), n)
}

// TestImportMembers_CardTypeWithoutCategory_SkipsRow 锁"类别不存在 ⇒ 跳过 + warning(不自动建类别)"。
//
// ⚠️ 门店10 只有 MT-STD-10(会员卡) / MT-SAV-10(储值卡);
//   源库里 card_type=2(VIP储值卡)的那 1 行**没有对应类别**
//   ⇒ 跳过 + warning,**不自动建类别**(类别是门店自己维护的字典)。
func TestImportMembers_CardTypeWithoutCategory_SkipsRow(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "MT-STD-10", "10", "会员卡")
	seedType(t, db, "MT-SAV-10", "10", "储值卡")

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID: "M0001", CardType: "2", Name: "张三", Status: "0", BalanceYuan: "0",
		}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, scopeRow(t, rep, archivedto.ScopeMembers).Skipped)
	assert.NotEmpty(t, rep.Warnings)

	// ⚠️ 类别**不许**被自动建出来。
	var types int64
	require.NoError(t, db.Model(&model.MemberType{}).Count(&types).Error)
	assert.Equal(t, int64(2), types, "不得自动新建会员类别")
}

// TestImportMembers_UnknownStatus_IsFrozen 锁"card_status=3 ⇒ frozen(保守处理)"。
//
// ⚠️ 思迅 V10 的 3 **没有权威文档** ⇒ 保守按"冻结"处理,
//   绝不写 normal(猜错的代价是一张本该停用的卡在 POS 上能正常消费)。
func TestImportMembers_UnknownStatus_IsFrozen(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID: "M0001", CardType: "0", Name: "张三", Status: "3", BalanceYuan: "0",
		}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, scopeRow(t, rep, archivedto.ScopeMembers).Inserted)

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, model.MemberStatusFrozen, m.Status, "card_status=3 ⇒ 保守按 frozen")
}

// TestImportMembers_StatusMapping 锁"已知的 card_status 映射"。
func TestImportMembers_StatusMapping(t *testing.T) {
	cases := []struct{ src, want string }{
		{"0", model.MemberStatusNormal},
		{"1", model.MemberStatusFrozen},
		{"2", model.MemberStatusLost},
		{"5", model.MemberStatusCancelled},
	}
	for _, c := range cases {
		svc, db := newService(t)
		seedType(t, db, "T1", "10", "会员卡")
		_, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
			TargetBranchID: "10", Mode: "incremental",
			Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
				ID: "M0001", CardType: "0", Name: "张三", Status: c.src, BalanceYuan: "0",
			}}},
		})
		require.NoError(t, err)
		var m model.Member
		require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
		assert.Equal(t, c.want, m.Status, "card_status=%s ⇒ %s", c.src, c.want)
	}
}

// TestImportMembers_SentinelUseNum_LimitsToZero 锁"999998 / 999999 ⇒ 限用次数 0(不限次)"。
func TestImportMembers_SentinelUseNum_LimitsToZero(t *testing.T) {
	for _, sentinel := range []string{"999998", "999999"} {
		svc, db := newService(t)
		seedType(t, db, "T1", "10", "会员卡")
		_, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
			TargetBranchID: "10", Mode: "incremental",
			Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
				ID: "M0001", CardType: "0", Name: "张三", Status: "0", BalanceYuan: "0",
				LimitedUseCount: sentinel,
			}}},
		})
		require.NoError(t, err)
		var m model.Member
		require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
		assert.Equal(t, 0, m.LimitedUseCount, "use_num=%s ⇒ 0(不限次)", sentinel)
	}
}

// TestImportMembers_MemberTypes_NoAutoCreate 锁"会员类别**不自动建**"。
//
// ⚠️ 源库有的类别、本域没有 ⇒ 跳过 + warning(不新建)。
func TestImportMembers_MemberTypes_NoAutoCreate(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "MT-STD-10", "10", "会员卡")

	rep, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{MemberTypes: []archivedto.ImportMemberTypeRow{
			{ID: "0", Name: "会员卡", DiscountRate: "1", AllowStoredValue: "0", AllowPointsPay: "1"},
			{ID: "2", Name: "VIP储值卡", DiscountRate: "0.9", AllowStoredValue: "1", AllowPointsPay: "1"},
		}},
	})
	require.NoError(t, err)
	row := scopeRow(t, rep, archivedto.ScopeMemberTypes)
	assert.Equal(t, 2, row.Received)
	assert.Equal(t, 1, row.Updated, "会员卡 对齐到已有类别")
	assert.Equal(t, 1, row.Skipped, "VIP储值卡 本地没有 ⇒ 跳过")
	assert.NotEmpty(t, rep.Warnings)

	var types int64
	require.NoError(t, db.Model(&model.MemberType{}).Count(&types).Error)
	assert.Equal(t, int64(1), types, "不得自动新建会员类别")
}

// TestImportMembers_Idempotent 锁"重复导入 ⇒ 第二次 inserted=0 / updated=N"。
func TestImportMembers_Idempotent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	row := archivedto.ImportMemberRow{
		ID: "M0001", CardType: "0", Name: "张三", Status: "0", BalanceYuan: "10",
	}
	rep1, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{row}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, scopeRow(t, rep1, archivedto.ScopeMembers).Inserted)

	rep2, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{row}},
	})
	require.NoError(t, err)
	r2 := scopeRow(t, rep2, archivedto.ScopeMembers)
	assert.Equal(t, 0, r2.Inserted, "第二次不能再插一行")
	assert.Equal(t, 1, r2.Updated)

	var n int64
	require.NoError(t, db.Model(&model.Member{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestImportMembers_NormalizesMemberID 锁"会员号去反引号与前导零"。
//
// ⚠️ 口径见 docs/FIELD_SPEC.md(members.id):思迅 card_id 带反引号与前导零,
//   不规整 ⇒ 同一个会员在两端是两个 id ⇒ 查不到 + 重复建卡。
func TestImportMembers_NormalizesMemberID(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	_, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "10", Mode: "incremental",
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{
			ID: "`00123`", CardType: "0", Name: "张三", Status: "0", BalanceYuan: "0",
		}}},
	})
	require.NoError(t, err)
	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "123", "10").First(&m).Error)
	assert.Equal(t, "123", m.ID)
}

// TestImportMembers_TargetBranchRequired 锁"目标门店只从 header 取(必填)"。
func TestImportMembers_TargetBranchRequired(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.ImportMembers(importCtx(), service.MemberImportInput{
		TargetBranchID: "", Mode: "incremental",
	})
	require.Error(t, err)
}
