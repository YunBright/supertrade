package service_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

func boolPtr(b bool) *bool                      { return &b }
func intPtr(i int) *int                         { return &i }
func strPtr(s string) *string                   { return &s }
func decPtr(d decimal.Decimal) *decimal.Decimal { return &d }

// TestUpsertSettings_Create_ThenReadBack 锁"写完能读回来"。
func TestUpsertSettings_Create_ThenReadBack(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	view, err := svc.UpsertSettings(ctx, service.SettingsInput{
		BranchID:          "10",
		MembershipEnabled: boolPtr(true),
		PointsRate:        decPtr(decimal.NewFromFloat(1.5)),
		PointsDeductRate:  decPtr(decimal.NewFromInt(1)),
		PasswordRequired:  boolPtr(true),
		FaceRequired:      boolPtr(false),
		MaxFacePerMember:  intPtr(3),
		UpdatedBy:         "admin-1",
	})
	require.NoError(t, err)
	assert.True(t, view.MembershipEnabled)
	assert.Equal(t, "1.5", view.PointsRate.String())
	assert.Equal(t, "1", view.PointsDeductRate.String())
	assert.True(t, view.PasswordRequired)
	assert.Equal(t, 3, view.MaxFacePerMember)

	// 再读一次(走 GET /config 的读侧)⇒ 值一致。
	got, err := svc.GetSettings(ctx, "10")
	require.NoError(t, err)
	assert.True(t, got.MembershipEnabled)
	assert.Equal(t, "1.5", got.PointsRate.String())
}

// TestUpsertSettings_PartialUpdate_KeepsOtherFields 锁"局部更新不冲掉别的字段"。
//
// ⚠️ 管理端配置页可能只改一个开关 ⇒ 全量替换会把没改的字段清掉。
func TestUpsertSettings_PartialUpdate_KeepsOtherFields(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertSettings(ctx, service.SettingsInput{
		BranchID:          "10",
		MembershipEnabled: boolPtr(true),
		PointsRate:        decPtr(decimal.NewFromFloat(1.5)),
		PasswordRequired:  boolPtr(true),
	})
	require.NoError(t, err)

	// 只改会员开关。
	_, err = svc.UpsertSettings(ctx, service.SettingsInput{
		BranchID:          "10",
		MembershipEnabled: boolPtr(false),
	})
	require.NoError(t, err)

	got, err := svc.GetSettings(ctx, "10")
	require.NoError(t, err)
	assert.False(t, got.MembershipEnabled, "会员开关应被改掉")
	assert.Equal(t, "1.5", got.PointsRate.String(), "积分率不能被冲掉")
	assert.True(t, got.PasswordRequired, "密码必填不能被冲掉")
}

// TestUpsertSettings_PointsRateZero_Rejected 锁"points_rate 必须 > 0"。
//
// ⚠️ 读侧 `GetSettings` 的口径是"PointsRate > 0 才生效,否则回落默认 1"
// ⇒ 写 0 进去会被读成 1("我明明设了不积分")⇒ 必须在写侧拒掉。
func TestUpsertSettings_PointsRateZero_Rejected(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.UpsertSettings(context.Background(), service.SettingsInput{
		BranchID:   "10",
		PointsRate: decPtr(decimal.Zero),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestUpsertSettings_NegativeDeductRate_Rejected 锁"抵扣率不能为负"。
func TestUpsertSettings_NegativeDeductRate_Rejected(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.UpsertSettings(context.Background(), service.SettingsInput{
		BranchID:         "10",
		PointsDeductRate: decPtr(decimal.NewFromInt(-1)),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestUpsertSettings_BranchRequired 锁"门店必填"(门店只来自 X-Branch-ID)。
func TestUpsertSettings_BranchRequired(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.UpsertSettings(context.Background(), service.SettingsInput{
		BranchID:          "",
		MembershipEnabled: boolPtr(true),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestUpsertType_Create_ThenReadBack 锁"建类别能读回来"。
func TestUpsertType_Create_ThenReadBack(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	row, err := svc.UpsertType(ctx, service.TypeInput{
		ID:               "T-STORED",
		BranchID:         "10",
		Name:             "储值卡",
		DiscountRate:     decPtr(decimal.NewFromFloat(0.95)),
		FeeAmountYuan:    decPtr(decimal.NewFromInt(10)),
		AllowStoredValue: boolPtr(true),
		AllowPointsPay:   boolPtr(true),
		Priority:         intPtr(1),
		CreatedBy:        "admin-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "T-STORED", row.ID)
	assert.Equal(t, "10", row.BranchID)
	assert.Equal(t, "储值卡", row.Name)
	assert.Equal(t, "0.95", row.DiscountRate.String())
	assert.Equal(t, "10", row.FeeAmountYuan.String())
	assert.True(t, row.AllowStoredValue)
	assert.Equal(t, 1, row.Priority)
}

// TestUpsertType_SecondCall_UpdatesNotDuplicates 锁"重复写是更新不是新增"。
func TestUpsertType_SecondCall_UpdatesNotDuplicates(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
		DiscountRate: decPtr(decimal.NewFromFloat(1)),
	})
	require.NoError(t, err)
	_, err = svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10", Name: "普通会员卡",
	})
	require.NoError(t, err)

	rows, err := svc.ListTypes(ctx, "10")
	require.NoError(t, err)
	require.Len(t, rows, 1, "同一个 (id, branch_id) 不能出现两行")
	assert.Equal(t, "普通会员卡", rows[0].Name)
}

// TestUpsertType_PartialUpdate_KeepsStatus 锁"局部更新**不复活**已停用的类别"。
//
// ⚠️ 漏合并 `cur.Status` 的症状(2026-10-21 真 bug):
//
//	把类别停用(status=inactive)后,只改一下折扣率(不传 status)
//	⇒ status 被**静默重置成 active** ⇒ 用户明明停用了的类别又出现了。
func TestUpsertType_PartialUpdate_KeepsStatus(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
		Status: strPtr("inactive"),
	})
	require.NoError(t, err)

	// 只改折扣率,**不传 status**。
	_, err = svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
		DiscountRate: decPtr(decimal.NewFromFloat(0.9)),
	})
	require.NoError(t, err)

	got, err := svc.GetType(ctx, "10", "T1")
	require.NoError(t, err)
	assert.Equal(t, "inactive", got.Status, "局部更新不能把停用状态复活成 active")
	assert.Equal(t, "0.9", got.DiscountRate.String())
}

// TestUpsertType_PartialUpdate_KeepsName 锁"只改一个字段时 name 可以不传"。
func TestUpsertType_PartialUpdate_KeepsName(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
	})
	require.NoError(t, err)

	// **不传 name** ⇒ 保持原名(而不是 400)。
	_, err = svc.UpsertType(ctx, service.TypeInput{
		ID: "T1", BranchID: "10",
		Priority: intPtr(5),
	})
	require.NoError(t, err)

	got, err := svc.GetType(ctx, "10", "T1")
	require.NoError(t, err)
	assert.Equal(t, "会员卡", got.Name)
	assert.Equal(t, 5, got.Priority)
}

// TestUpsertSettings_MaxFacePerMember_ZeroIsKept 锁"0 = 不限制"要原样读回。
//
// ⚠️ 之前读侧是"只有 > 0 才生效" ⇒ 写 0 会被读成默认 1
// ⇒ 用户设了"不限制人脸"却看到"只能录 1 个"。
func TestUpsertSettings_MaxFacePerMember_ZeroIsKept(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertSettings(ctx, service.SettingsInput{
		BranchID:         "10",
		MaxFacePerMember: intPtr(0),
	})
	require.NoError(t, err)

	got, err := svc.GetSettings(ctx, "10")
	require.NoError(t, err)
	assert.Equal(t, 0, got.MaxFacePerMember, "0 是合法值(不限制),不能被读成默认 1")
}

// TestUpsertType_DiscountRateOutOfRange_Rejected 锁"折扣率必须在 [0,1]"。
func TestUpsertType_DiscountRateOutOfRange_Rejected(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.UpsertType(context.Background(), service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
		DiscountRate: decPtr(decimal.NewFromFloat(1.2)),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestUpsertType_InvalidStatus_Rejected 锁"status 白名单"。
func TestUpsertType_InvalidStatus_Rejected(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.UpsertType(context.Background(), service.TypeInput{
		ID: "T1", BranchID: "10", Name: "会员卡",
		Status: strPtr("deleted"),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestUpsertType_BranchIsolation 锁"类别按门店维护,不跨店合并"。
func TestUpsertType_BranchIsolation(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{ID: "T1", BranchID: "10", Name: "会员卡"})
	require.NoError(t, err)
	_, err = svc.UpsertType(ctx, service.TypeInput{ID: "T1", BranchID: "20", Name: "另一家店的卡"})
	require.NoError(t, err)

	rows10, err := svc.ListTypes(ctx, "10")
	require.NoError(t, err)
	require.Len(t, rows10, 1)
	assert.Equal(t, "会员卡", rows10[0].Name)

	rows20, err := svc.ListTypes(ctx, "20")
	require.NoError(t, err)
	require.Len(t, rows20, 1)
	assert.Equal(t, "另一家店的卡", rows20[0].Name)
}

// TestDeleteType_NoMember_SoftDeleted 锁"没人用的类别能删(软删)"。
func TestDeleteType_NoMember_SoftDeleted(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{ID: "T1", BranchID: "10", Name: "会员卡"})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteType(ctx, "10", "T1"))

	rows, err := svc.ListTypes(ctx, "10")
	require.NoError(t, err)
	assert.Empty(t, rows, "删完列表里不该再有")

	// ⚠️ 软删 ⇒ 行还在表里(历史单据仍能对上类别)。
	var n int64
	require.NoError(t, db.Model(&model.MemberType{}).
		Where("id = ? AND branch_id = ?", "T1", "10").
		Unscoped().Count(&n).Error)
	assert.Equal(t, int64(1), n, "软删不该物理删行")
}

// TestDeleteType_MemberReferences_Conflict 锁"还有卡在用的类别不能删"。
//
// ⚠️ 删了 ⇒ POS 的"会员类别"下拉里会突然少一项,已开的卡就选不回来了。
func TestDeleteType_MemberReferences_Conflict(t *testing.T) {
	svc, db := newService(t)
	ctx := context.Background()

	_, err := svc.UpsertType(ctx, service.TypeInput{ID: "T1", BranchID: "10", Name: "会员卡"})
	require.NoError(t, err)
	seedMember(t, db, "M0001", "10")

	err = svc.DeleteType(ctx, "10", "T1")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrMemberTypeInUse)

	// 类别还在。
	rows, err := svc.ListTypes(ctx, "10")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// TestDeleteType_NotFound 锁"删不存在的类别报错"。
func TestDeleteType_NotFound(t *testing.T) {
	svc, _ := newService(t)
	err := svc.DeleteType(context.Background(), "10", "NOPE")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrMemberTypeNotFound)
}
