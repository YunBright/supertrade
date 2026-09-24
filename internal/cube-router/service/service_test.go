package service_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cube-router/model"
	"github.com/YunBright/supertrade/internal/cube-router/service"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
//
// 用 modernc.org/sqlite 走纯 Go 驱动(go-sqlite3 需 CGO,supertrade
// 默认 CGO_ENABLED=0;跟 internal/stocktake/testdb 一致)。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)&_time_format=sqlite"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.BranchCubeSource{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1) // SQLite 单写
	return db
}

func TestService_CreateAndGet(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	row, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "cube-gateway",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)
	assert.Equal(t, "S001", row.BranchID)
	assert.True(t, row.Enabled, "默认 enabled=true")

	got, err := svc.Get(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "cube-gateway", got.CubeSourceName)
}

func TestService_Create_DuplicateFails(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	_, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "cube-gateway",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)

	_, err = svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "other",
		CreatedBy:      "admin",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrAlreadyExists))
}

func TestService_ResolveCubeSource_CachesResult(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	_, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "cube-gateway",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)

	// 第一次 resolve → 查 DB
	name, enabled, err := svc.ResolveCubeSource(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "cube-gateway", name)
	assert.True(t, enabled)

	// 第二次 resolve → 命中缓存,即便直接改 DB 也不应反映(因为缓存 TTL 内)
	require.NoError(t, db.Model(&model.BranchCubeSource{}).
		Where("branch_id = ?", "S001").
		Update("cube_source_name", "different").Error)

	name, _, err = svc.ResolveCubeSource(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "cube-gateway", name, "缓存命中,DB 修改不应立即反映")

	// Invalidate 后再次 resolve → 反映新值
	svc.Invalidate("S001")
	name, _, err = svc.ResolveCubeSource(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "different", name, "Invalidate 后应重新查 DB")
}

func TestService_ResolveCubeSource_NotFound(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	_, _, err := svc.ResolveCubeSource(context.Background(), "missing")
	assert.True(t, errors.Is(err, model.ErrNotFound))
}

func TestService_ResolveCubeSource_Disabled(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	// Create 默认启用,然后 Update 禁用(更贴近真实 admin 工作流)。
	_, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "cube-gateway",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)

	_, err = svc.Update(context.Background(), "S001", "cube-gateway", false, "admin")
	require.NoError(t, err)

	_, _, err = svc.ResolveCubeSource(context.Background(), "S001")
	assert.True(t, errors.Is(err, model.ErrDisabled))
}

func TestService_ResolveCubeSource_TTLExpiry(t *testing.T) {
	db := newTestDB(t)

	// 用可控时钟 + 短 TTL
	var now atomic.Int64
	now.Store(time.Now().UnixNano())

	svc := service.NewService(db,
		service.WithTTL(100*time.Millisecond),
		service.WithClock(func() time.Time {
			return time.Unix(0, now.Load())
		}),
	)

	_, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "v1",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)

	// t=0:resolve 返 v1
	name, _, err := svc.ResolveCubeSource(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "v1", name)

	// 推进时钟 200ms(> TTL),DB 改成 v2
	now.Store(now.Load() + int64(200*time.Millisecond))
	require.NoError(t, db.Model(&model.BranchCubeSource{}).
		Where("branch_id = ?", "S001").
		Update("cube_source_name", "v2").Error)

	// 缓存过期 → 重新查 DB
	name, _, err = svc.ResolveCubeSource(context.Background(), "S001")
	require.NoError(t, err)
	assert.Equal(t, "v2", name, "TTL 过期后应重新读 DB")
}

func TestService_DeleteAndUpdate(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewService(db)

	_, err := svc.Create(context.Background(), model.BranchCubeSource{
		BranchID:       "S001",
		CubeSourceName: "v1",
		CreatedBy:      "admin",
	})
	require.NoError(t, err)

	// Update
	upd, err := svc.Update(context.Background(), "S001", "v2", true, "admin")
	require.NoError(t, err)
	assert.Equal(t, "v2", upd.CubeSourceName)

	// Delete
	require.NoError(t, svc.Delete(context.Background(), "S001"))

	_, err = svc.Get(context.Background(), "S001")
	assert.True(t, errors.Is(err, model.ErrNotFound))
}