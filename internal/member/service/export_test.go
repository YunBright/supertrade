package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// seedExportMember 直接插一行会员(绕开开卡的类别校验)。
//
// ⚠️ op_id 上有**唯一索引** ⇒ 每行必须给不同的 op_id。
func seedExportMember(t *testing.T, db *gorm.DB, id, branchID string, accumulated, used decimal.Decimal) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, OpID: "op-" + id, CardTypeID: "T1", Name: "会员" + id,
		Status:            model.MemberStatusNormal,
		PointsAccumulated: accumulated, PointsUsed: used,
		BalanceYuan: decimal.Zero, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// TestService_ExportMembers_FillsRemainingPoints 锁"**每行都过 fillDerived**"。
//
// ⚠️ remaining_points 是**派生**字段(gorm:"-" 不落库),
// 忘了填 ⇒ 客户端拿到的剩余积分全是 0 ⇒ 算积分、抵扣积分全错,而且零报错。
func TestService_ExportMembers_FillsRemainingPoints(t *testing.T) {
	svc, db := newService(t)
	seedExportMember(t, db, "M0001", "10", decimal.NewFromInt(100), decimal.NewFromInt(30))

	rows, err := svc.ExportMembers(context.Background(), "10")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "70", rows[0].RemainingPoints.String(),
		"剩余积分必须被填上(累计获得 − 累计扣减)")
}

// TestService_ExportMembers_NotLimited 锁"**不受 MaxListLimit 限制**"。
//
// ⚠️ 门店 10 有 4128 个会员,listMembers 的 limit 上限是 200
// ⇒ 导出必须一次给全。
func TestService_ExportMembers_NotLimited(t *testing.T) {
	svc, db := newService(t)
	const n = service.MaxListLimit + 10
	for i := 0; i < n; i++ {
		seedExportMember(t, db, fmt.Sprintf("M%04d", i), "10", decimal.Zero, decimal.Zero)
	}

	rows, err := svc.ExportMembers(context.Background(), "10")
	require.NoError(t, err)
	assert.Len(t, rows, n, "导出不受 MaxListLimit 限制")
}

// TestService_ExportMembers_BranchScoped 锁"只导本门店的会员"。
func TestService_ExportMembers_BranchScoped(t *testing.T) {
	svc, db := newService(t)
	seedExportMember(t, db, "M0001", "10", decimal.Zero, decimal.Zero)
	seedExportMember(t, db, "M0002", "01", decimal.Zero, decimal.Zero)

	rows, err := svc.ExportMembers(context.Background(), "10")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "M0001", rows[0].ID)
}

// TestService_ExportMembers_RequiresBranch 锁"空门店 ⇒ ErrInvalidInput"。
func TestService_ExportMembers_RequiresBranch(t *testing.T) {
	svc, _ := newService(t)

	_, err := svc.ExportMembers(context.Background(), "  ")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidInput), "应返 ErrInvalidInput,实际 %v", err)
}
