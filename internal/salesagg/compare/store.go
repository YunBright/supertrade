package compare

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/YunBright/core/testrun"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	// ErrNotFound 表示模板不存在（handler 映射 404 compare_template_not_found）。
	ErrNotFound = errors.New("compare: 模板不存在")

	// ErrConflict 表示乐观锁版本不匹配 —— 别人先改了，需要刷新后重试
	//（handler 映射 409 compare_template_conflict）。
	ErrConflict = errors.New("compare: 模板已被他人修改，请刷新后重试")
)

// OpenDB 打开 PostgreSQL（sales-agg 门店对比模板专用）。
//
// dsn 必须非空，缺失直接报错（启动失败），不提供任何 fallback ——
// 与 internal/stocktake/db.go 同一口径：静默降级只会把"没配 DSN"变成
// "上线后第一次保存模板时 500"。
//
// 连接池默认值：MaxOpenConns=10, MaxIdleConns=2, ConnMaxLifetime=1h。
// 由 caller 负责调 AutoMigrate(...)。
func OpenDB(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("salesagg/compare: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("salesagg/compare: open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)
	// 溯源记账到旁表 testrun_rows（core/testrun），见 internal/stocktake/db.go 说明。
	// 漏注册的表现是静默的：契约测试造的模板照常落库但没记账，收尾 sweep 找不到。
	if err := testrun.EnsureTable(db); err != nil {
		return nil, err
	}
	if err := testrun.RegisterGorm(db); err != nil {
		return nil, err
	}
	return db, nil
}

// Store 是对比模板的持久化仓库。
type Store struct {
	db *gorm.DB
}

// NewStore 构造 Store。
func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// List 返回全部模板，按 updated_at 倒序（最近被改的排最前）。
//
// 不分页：模板是全员共享的 wiki 式资源，数量级是个位数到几十，
// 分页只会给前端增加"翻页找不到刚建的模板"一类麻烦。
func (s *Store) List(ctx context.Context) ([]Template, error) {
	var out []Template
	err := s.db.WithContext(ctx).
		Order("updated_at DESC").
		Find(&out).Error
	return out, err
}

// Get 按 id 取单个模板；不存在返回 ErrNotFound。
func (s *Store) Get(ctx context.Context, id string) (*Template, error) {
	var t Template
	err := s.db.WithContext(ctx).
		Where("id = ?", id).
		First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Create 创建模板：分配 uuid 主键、revision 从 1 起。
func (s *Store) Create(ctx context.Context, t *Template) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.Revision == 0 {
		t.Revision = 1
	}
	return s.db.WithContext(ctx).Create(t).Error
}

// Update 整体替换业务列，带乐观锁：仅当库里 revision 仍是 expected
// 时才更新（新 revision = expected + 1）。
//
// created_by / created_at / created_by_name **不更新**（创建即定格）。
// updated_at 交给 gorm 自动维护（Model 感知 UpdatedAt 字段）。
//
// RowsAffected == 0 有两种含义，必须区分清楚：
//   - 行还在但 revision 对不上 → ErrConflict（409，用户该刷新）；
//   - 行已不存在 → ErrNotFound（404）。
//
// 区分的办法是回查一次 —— 不区分的后果是"两个人同时编辑"与
// "模板刚被删"得到同一种提示，用户会去刷新一张已不存在的模板。
func (s *Store) Update(ctx context.Context, t *Template, expectedRevision int64) error {
	res := s.db.WithContext(ctx).Model(&Template{}).
		Where("id = ? AND revision = ?", t.ID, expectedRevision).
		Updates(map[string]any{
			"name":            t.Name,
			"dimension":       t.Dimension,
			"stores":          t.StoresJSON,
			"rows":            t.RowsJSON,
			"revision":        expectedRevision + 1,
			"updated_by":      t.UpdatedBy,
			"updated_by_name": t.UpdatedByName,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		if _, err := s.Get(ctx, t.ID); err != nil {
			return err
		}
		return ErrConflict
	}
	t.Revision = expectedRevision + 1
	return nil
}

// Delete 物理删除；删了 0 行 = 模板不存在。
func (s *Store) Delete(ctx context.Context, id string) error {
	res := s.db.WithContext(ctx).
		Where("id = ?", id).
		Delete(&Template{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
