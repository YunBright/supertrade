package service

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // pure-Go driver (no CGO)

	"github.com/YunBright/supertrade/internal/masterdata/model"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"}, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.Store{}, &model.Employee{}, &model.Shift{},
	))
	return db
}

// TestStoreUpsert_Idempotent 锁"门店档案重复写不产生新行"。
func TestStoreUpsert_Idempotent(t *testing.T) {
	svc := NewStoreService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Upsert(ctx, StoreInput{BranchID: "10", Name: "云商", PosEnabled: true})
	require.NoError(t, err)
	_, err = svc.Upsert(ctx, StoreInput{BranchID: "10", Name: "云商POS", PosEnabled: true})
	require.NoError(t, err)

	got, err := svc.Get(ctx, "10")
	require.NoError(t, err)
	assert.Equal(t, "云商POS", got.Name, "第二次写应覆盖")
	assert.True(t, got.PosEnabled)
}

// TestStoreUpsert_RequiresName 锁"门店名必填"。
func TestStoreUpsert_RequiresName(t *testing.T) {
	svc := NewStoreService(newTestDB(t))
	_, err := svc.Upsert(context.Background(), StoreInput{BranchID: "10"})
	require.Error(t, err)
}

// TestEmployeeUpsert_Idempotent 锁"收银员档案重复写不产生新行"。
//
// ⚠️ 主键是 (auth_user_id, branch_id) —— 改工号**不会**产生第二行。
func TestEmployeeUpsert_Idempotent(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
	})
	require.NoError(t, err)
	_, err = svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "2002", Name: "张三",
	})
	require.NoError(t, err)

	got, err := svc.Get(ctx, "u-1", "10")
	require.NoError(t, err)
	assert.Equal(t, "2002", got.EmployeeCode, "改工号应覆盖同一行")
}

// TestEmployeeUpsert_RejectsBadRole 锁"角色取值受控"。
//
// ⚠️ 角色决定折扣权限的上限 ⇒ 放任任意字符串等于放任提权。
func TestEmployeeUpsert_RejectsBadRole(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	_, err := svc.Upsert(context.Background(), EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
		Role: "superuser",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role")
}

// TestEmployeeUpsert_RejectsBadStatus 锁"status 取值受控"。
//
// ⚠️ 停用是"改 status",不是删行 ⇒ status 写错一个字符串,
// 收银员就会既不是 active 也不是 inactive(查不出来、也停不掉)。
func TestEmployeeUpsert_RejectsBadStatus(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	_, err := svc.Upsert(context.Background(), EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
		Status: "deleted",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status")
}

// TestEmployeeUpsert_SameBranchSameCode_Conflict 锁"同店同工号不能重复"。
//
// ⚠️ DB 层没有唯一约束(`idx_employee_branch_code` 实际是 employee_code
// **单列非唯一**索引)⇒ 必须在 service 层拦。
// 症状:同店两个收银员同工号 ⇒ POS 授权校验认错人。
func TestEmployeeUpsert_SameBranchSameCode_Conflict(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
	})
	require.NoError(t, err)

	// 另一个 auth user,同一家店,同一个工号 ⇒ 冲突。
	_, err = svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-2", BranchID: "10", EmployeeCode: "1002", Name: "李四",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrEmployeeCodeConflict)
}

// TestEmployeeUpsert_SameCodeDifferentBranch_OK 锁"跨店同工号是允许的"。
//
// ⚠️ `employees` 复合主键含 branch_id ⇒ 同一个 auth user 在多家店轮班时,
// 每家店各有一份档案,工号可以相同。
func TestEmployeeUpsert_SameCodeDifferentBranch_OK(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
	})
	require.NoError(t, err)

	_, err = svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "20", EmployeeCode: "1002", Name: "张三",
	})
	require.NoError(t, err, "跨店同工号必须允许")

	rows, err := svc.List(ctx, "20")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// TestEmployeeUpsert_InactiveAllowed 锁"停用是合法状态"。
func TestEmployeeUpsert_InactiveAllowed(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
		Status: EmployeeStatusInactive,
	})
	require.NoError(t, err)

	got, err := svc.Get(ctx, "u-1", "10")
	require.NoError(t, err)
	assert.Equal(t, EmployeeStatusInactive, got.Status)
}

// TestEmployeeGetByCode 锁"按工号查收银员"。
func TestEmployeeGetByCode(t *testing.T) {
	svc := NewEmployeeService(newTestDB(t))
	ctx := context.Background()
	_, err := svc.Upsert(ctx, EmployeeInput{
		AuthUserID: "u-1", BranchID: "10", EmployeeCode: "1002", Name: "张三",
	})
	require.NoError(t, err)

	got, err := svc.GetByCode(ctx, "10", "1002")
	require.NoError(t, err)
	assert.Equal(t, "u-1", got.ID)

	_, err = svc.GetByCode(ctx, "10", "9999")
	require.Error(t, err, "工号不存在必须报错")
}

// TestShift_OneOpenPerEmployee 锁"一个收银员同时只能有一个未交班的班次"。
//
// ⚠️ 两个未交班的班次 ⇒ "本班销售额"算成两倍 ⇒ 长短款离谱,
//
//	而症状是"下班时对不上账",查不出原因。
func TestShift_OneOpenPerEmployee(t *testing.T) {
	svc := NewShiftService(newTestDB(t))
	ctx := context.Background()

	_, err := svc.Open(ctx, OpenShiftInput{
		BranchID: "10", EmployeeID: "u-1", OpeningAmountYuan: decimal.NewFromInt(100),
	})
	require.NoError(t, err)

	_, err = svc.Open(ctx, OpenShiftInput{
		BranchID: "10", EmployeeID: "u-1", OpeningAmountYuan: decimal.NewFromInt(100),
	})
	require.Error(t, err, "第二个未交班的班次必须被拒")
	assert.Contains(t, err.Error(), "未交班")
}

// TestShift_CloseTwiceIsConflict 锁"交班后不可重复交班"。
//
// ⚠️ 重复交班会把实收金额改掉 ⇒ 等于可以篡改账。
func TestShift_CloseTwiceIsConflict(t *testing.T) {
	svc := NewShiftService(newTestDB(t))
	ctx := context.Background()

	opened, err := svc.Open(ctx, OpenShiftInput{
		BranchID: "10", EmployeeID: "u-1", OpeningAmountYuan: decimal.NewFromInt(100),
	})
	require.NoError(t, err)

	closed, err := svc.Close(ctx, opened.ID, decimal.NewFromInt(500), "", "u-1")
	require.NoError(t, err)
	assert.Equal(t, ShiftStatusClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)
	assert.Equal(t, "500", closed.ClosingAmountYuan.String())

	_, err = svc.Close(ctx, opened.ID, decimal.NewFromInt(999), "", "u-1")
	require.Error(t, err, "已交班的班次不可重复交班")
	assert.Contains(t, err.Error(), "已交班")
}

// TestShift_ListOpen 锁"只列未交班的班次"。
func TestShift_ListOpen(t *testing.T) {
	svc := NewShiftService(newTestDB(t))
	ctx := context.Background()

	opened, err := svc.Open(ctx, OpenShiftInput{BranchID: "10", EmployeeID: "u-1"})
	require.NoError(t, err)
	_, err = svc.Open(ctx, OpenShiftInput{BranchID: "10", EmployeeID: "u-2"})
	require.NoError(t, err)

	rows, err := svc.ListOpen(ctx, "10")
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	_, err = svc.Close(ctx, opened.ID, decimal.Zero, "", "u-1")
	require.NoError(t, err)

	rows, err = svc.ListOpen(ctx, "10")
	require.NoError(t, err)
	assert.Len(t, rows, 1, "交班后不应再出现在未交班列表里")
}

// TestNewShiftID_IsUniquePerSecond 锁"班次 id 逐秒不同"。
//
// ⚠️ id 必须唯一 —— 它是主键。同秒内开两个班次本来就被
//
//	hasOpenShift 拦住了,但 id 仍要保证不同(换收银员时可能同秒)。
func TestNewShiftID_IsUniquePerSecond(t *testing.T) {
	t1 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	t2 := t1.Add(time.Second)
	assert.NotEqual(t, NewShiftID(t1), NewShiftID(t2))
	assert.Contains(t, NewShiftID(t1), "SH")
}
