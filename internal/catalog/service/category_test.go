package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

func TestCategoryService_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	row, err := svc.Create(context.Background(), service.CreateCategoryInput{
		ID: "01", BranchID: "10", Name: "生鲜", ParentID: "", SortOrder: 10,
		CreatedBy: "admin",
	})
	require.NoError(t, err)
	assert.Equal(t, "active", row.Status, "默认 status=active")

	got, err := svc.Get(context.Background(), "10", "01")
	require.NoError(t, err)
	assert.Equal(t, "生鲜", got.Name)
}

func TestCategoryService_Create_DuplicateFails(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	_, err := svc.Create(context.Background(), service.CreateCategoryInput{
		ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), service.CreateCategoryInput{
		ID: "01", BranchID: "10", Name: "另一个生鲜", CreatedBy: "admin",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrCategoryAlreadyExists))
}

func TestCategoryService_Create_SelfParentIsCycle(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	_, err := svc.Create(context.Background(), service.CreateCategoryInput{
		ID: "01", BranchID: "10", Name: "生鲜", ParentID: "01", CreatedBy: "admin",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrCategoryCycle), "自己当自己的父必须当场拒掉")
}

func TestCategoryService_Update_RejectsCycle(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	// 生鲜 → 水果 → 进口水果
	for _, in := range []service.CreateCategoryInput{
		{ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin"},
		{ID: "0101", BranchID: "10", Name: "水果", ParentID: "01", CreatedBy: "admin"},
		{ID: "010101", BranchID: "10", Name: "进口水果", ParentID: "0101", CreatedBy: "admin"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	// 把「生鲜」的父挂到它的孙子「进口水果」下 ⇒ 成环。
	bad := "010101"
	_, err := svc.Update(context.Background(), "10", "01", service.UpdateCategoryInput{ParentID: &bad})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrCategoryCycle), "把父挂到孙子下必须被拒")

	// 原来的父子关系没被动过。
	got, err := svc.Get(context.Background(), "10", "01")
	require.NoError(t, err)
	assert.Equal(t, "", got.ParentID, "被拒的修改不能落库")
}

func TestCategoryService_Update_AllowsMovingToUnrelatedParent(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	for _, in := range []service.CreateCategoryInput{
		{ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin"},
		{ID: "02", BranchID: "10", Name: "百货", CreatedBy: "admin"},
		{ID: "0201", BranchID: "10", Name: "纸品", ParentID: "02", CreatedBy: "admin"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	// 把「纸品」从「百货」挪到「生鲜」下 —— 合法(不是自己的子孙)。
	newParent := "01"
	got, err := svc.Update(context.Background(), "10", "0201", service.UpdateCategoryInput{ParentID: &newParent})
	require.NoError(t, err)
	assert.Equal(t, "01", got.ParentID)
}

func TestCategoryService_SubtreeIDs(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	for _, in := range []service.CreateCategoryInput{
		{ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin"},
		{ID: "0101", BranchID: "10", Name: "水果", ParentID: "01", CreatedBy: "admin"},
		{ID: "010101", BranchID: "10", Name: "进口水果", ParentID: "0101", CreatedBy: "admin"},
		{ID: "02", BranchID: "10", Name: "百货", CreatedBy: "admin"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	ids, err := svc.SubtreeIDs(context.Background(), "10", "01")
	require.NoError(t, err)
	assert.Len(t, ids, 3, "子树 = 自己 + 两个子孙")
	assert.Contains(t, ids, "01")
	assert.Contains(t, ids, "0101")
	assert.Contains(t, ids, "010101")
	assert.NotContains(t, ids, "02", "别人的子树不能混进来")
}

func TestCategoryService_Delete_SoftDeletes(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	_, err := svc.Create(context.Background(), service.CreateCategoryInput{
		ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin",
	})
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "10", "01"))

	_, err = svc.Get(context.Background(), "10", "01")
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrCategoryNotFound))

	rows, err := svc.List(context.Background(), "10")
	require.NoError(t, err)
	assert.Empty(t, rows, "软删的行不能出现在列表里")
}

func TestCategoryService_List_IsolatesBranches(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewCategoryService(db)

	for _, in := range []service.CreateCategoryInput{
		{ID: "01", BranchID: "10", Name: "生鲜", CreatedBy: "admin"},
		{ID: "01", BranchID: "11", Name: "别家店的生鲜", CreatedBy: "admin"},
	} {
		_, err := svc.Create(context.Background(), in)
		require.NoError(t, err)
	}

	rows, err := svc.List(context.Background(), "10")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "生鲜", rows[0].Name)
}
