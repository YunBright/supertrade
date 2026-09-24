package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.Supplier{}, &model.Product{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

func TestSupplierService_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	row, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "可口可乐华南",
		Type: "0", Contact: "张经理", Phone: "13800000001",
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.Equal(t, "active", row.Status, "默认 status=active")
	assert.Equal(t, "SUP-001", row.ID)

	got, err := svc.Get(context.Background(), "S001", "SUP-001")
	require.NoError(t, err)
	assert.Equal(t, "可口可乐华南", got.Name)
}

func TestSupplierService_Create_DuplicateFails(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "A",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "B",
		CreatedBy: "admin",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrSupplierAlreadyExists))
}

func TestSupplierService_Create_SameID_DifferentBranch_OK(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "门店1供应商",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S002", Name: "门店2供应商",
		CreatedBy: "admin",
	})
	require.NoError(t, err, "不同 branch 应允许同 ID")
}

func TestSupplierService_Create_RequiredFields(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	cases := []service.CreateSupplierInput{
		{ID: "", BranchID: "S001", Name: "x", CreatedBy: "admin"},
		{ID: "SUP-001", BranchID: "", Name: "x", CreatedBy: "admin"},
		{ID: "SUP-001", BranchID: "S001", Name: "", CreatedBy: "admin"},
	}
	for i, in := range cases {
		_, err := svc.Create(context.Background(), in)
		assert.True(t, errors.Is(err, model.ErrInvalidInput), "case %d 应 ErrInvalidInput", i)
	}
}

func TestSupplierService_Get_BranchIsolation(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "S001 only",
		CreatedBy: "admin",
	})
	require.NoError(t, err)

	// S002 查 SUP-001 应不存在(跨店隔离)
	_, err = svc.Get(context.Background(), "S002", "SUP-001")
	assert.True(t, errors.Is(err, model.ErrSupplierNotFound))
}

func TestSupplierService_List_FilterByBranch(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, _ = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "A", CreatedBy: "admin",
	})
	_, _ = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-002", BranchID: "S001", Name: "B", CreatedBy: "admin",
	})
	_, _ = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S002", Name: "C", CreatedBy: "admin",
	})

	rows, err := svc.List(context.Background(), "S001", "", 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2, "S001 应只有 2 条")

	rows, err = svc.List(context.Background(), "S002", "", 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "S002 应只有 1 条")
}

func TestSupplierService_List_FilterByQuery(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, _ = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "可口可乐华南", CreatedBy: "admin",
	})
	_, _ = svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-002", BranchID: "S001", Name: "农夫山泉代理", CreatedBy: "admin",
	})

	// 按 name 模糊
	rows, err := svc.List(context.Background(), "S001", "可乐", 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "可口可乐华南", rows[0].Name)

	// 按 ID 精确(SUP- 前缀)
	rows, err = svc.List(context.Background(), "S001", "SUP-002", 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "SUP-002", rows[0].ID)
}

func TestSupplierService_Update_PartialFields(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "旧名",
		Phone: "13800000001", CreatedBy: "admin",
	})
	require.NoError(t, err)

	newName := "新名"
	newStatus := "inactive"
	upd, err := svc.Update(context.Background(), "S001", "SUP-001", service.UpdateSupplierInput{
		Name:      &newName,
		Status:    &newStatus,
		UpdatedBy: "admin2",
	})
	require.NoError(t, err)
	assert.Equal(t, "新名", upd.Name)
	assert.Equal(t, "inactive", upd.Status)
	assert.Equal(t, "13800000001", upd.Phone, "未改的字段应保留")
}

func TestSupplierService_Delete_SoftDelete(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewSupplierService(db)

	_, err := svc.Create(context.Background(), service.CreateSupplierInput{
		ID: "SUP-001", BranchID: "S001", Name: "x", CreatedBy: "admin",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "S001", "SUP-001"))

	// Get 仍能找到(DeletedAt 非零)
	got, err := svc.Get(context.Background(), "S001", "SUP-001")
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt, "软删应设置 DeletedAt")

	// List 不过滤 DeletedAt(本期列表含全部 active + inactive;后续可加 status 过滤)
	// 这里只验证软删生效,不再做列表断言。
}