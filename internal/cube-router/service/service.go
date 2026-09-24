// Package service 封装 cube-router 业务逻辑:
//
//   - branch_cube_sources 表的 CRUD(供 admin 端点)
//   - 按 branch_id 解析 cube_source_name 的 router 缓存(供 /v1/load 转发)
//
// router 缓存策略:
//   - 内存 map[branch_id]entry,entry 含 cube_source_name + enabled + 拉取时间
//   - TTL 60s(可在 NewService 时改),过期重新查 DB
//   - admin CRUD 调用 Invalidate(branchID) 主动失效(让配置变更立即生效)
//
// 错误:
//   - ErrNotFound  → handler 返 404 cube_source_not_configured
//   - ErrDisabled  → handler 返 503 cube_source_disabled
//   - DB 错误       → handler 返 500 internal_error
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cube-router/model"
)

// DefaultTTL 是 router 缓存默认有效期。
const DefaultTTL = 60 * time.Second

// Service 是 cube-router 的业务服务。
type Service struct {
	db   *gorm.DB
	ttl  time.Duration
	now  func() time.Time // 可注入(测试用)

	mu       sync.RWMutex
	cache    map[string]cacheEntry // branch_id → entry
}

// cacheEntry 是 router 缓存的单个 entry。
type cacheEntry struct {
	cubeSourceName string
	enabled        bool
	loadedAt       time.Time
}

// Option 调整 Service 行为。
type Option func(*Service)

// WithTTL 覆盖默认缓存有效期。
func WithTTL(d time.Duration) Option { return func(s *Service) { s.ttl = d } }

// WithClock 覆盖默认时钟(测试用)。
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService 构造 Service。
func NewService(db *gorm.DB, opts ...Option) *Service {
	s := &Service{
		db:    db,
		ttl:   DefaultTTL,
		now:   time.Now,
		cache: make(map[string]cacheEntry),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ResolveCubeSource 按 branch_id 解析 cube_source_name,返回名称 + enabled。
//
// 行为:
//   - 缓存命中且未过期 → 直接返
//   - 缓存 miss 或过期   → 查 DB 并回写缓存
//   - DB 返 ErrRecordNotFound → 返 model.ErrNotFound(handler 映 404)
//   - DB 其他 error → 原样返回(handler 映 500)
func (s *Service) ResolveCubeSource(ctx context.Context, branchID string) (cubeSourceName string, enabled bool, err error) {
	if branchID == "" {
		return "", false, errors.New("cube_router: branchID 不能为空")
	}
	s.mu.RLock()
	entry, ok := s.cache[branchID]
	s.mu.RUnlock()
	if ok && s.now().Sub(entry.loadedAt) < s.ttl {
		if !entry.enabled {
			return entry.cubeSourceName, false, model.ErrDisabled
		}
		return entry.cubeSourceName, true, nil
	}

	// 缓存 miss / 过期 → 查 DB。
	var row model.BranchCubeSource
	qerr := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		First(&row).Error
	if errors.Is(qerr, gorm.ErrRecordNotFound) {
		// 缓存 miss 写入"不存在"的负面缓存,避免反复打 DB。
		s.mu.Lock()
		s.cache[branchID] = cacheEntry{loadedAt: s.now()}
		s.mu.Unlock()
		return "", false, model.ErrNotFound
	}
	if qerr != nil {
		return "", false, qerr
	}

	// 写缓存。
	s.mu.Lock()
	s.cache[branchID] = cacheEntry{
		cubeSourceName: row.CubeSourceName,
		enabled:        row.Enabled,
		loadedAt:       s.now(),
	}
	s.mu.Unlock()

	if !row.Enabled {
		return row.CubeSourceName, false, model.ErrDisabled
	}
	return row.CubeSourceName, true, nil
}

// Invalidate 主动失效某 branch_id 的缓存(用于 admin CRUD 后立即生效)。
//
// 传空字符串 → 清空全部缓存。
func (s *Service) Invalidate(branchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if branchID == "" {
		s.cache = make(map[string]cacheEntry)
		return
	}
	delete(s.cache, branchID)
}

// ---- admin CRUD ----

// Create 新建一个 branch_cube_source。
//
// branch_id 已存在 → 返 model.ErrAlreadyExists。
//
// Enabled 默认 true(caller 不显式置 true 时 service 兜底,避免 GORM 把零值
// 误读成"未设"而插入 false)。
func (s *Service) Create(ctx context.Context, in model.BranchCubeSource) (*model.BranchCubeSource, error) {
	if in.BranchID == "" || in.CubeSourceName == "" || in.CreatedBy == "" {
		return nil, errors.New("cube_router: BranchID / CubeSourceName / CreatedBy 必填")
	}
	in.Enabled = true // 默认启用;要禁用请在 Create 后调 Update
	in.CreatedAt = s.now()
	in.UpdatedAt = s.now()
	if err := s.db.WithContext(ctx).Create(&in).Error; err != nil {
		// gorm 不会直接返 ErrRecordNotFound;PK 冲突为 unique violation,
		// 这里用字符串匹配兜底(避免引 pg 驱动包)。
		if isUniqueViolation(err) {
			return nil, model.ErrAlreadyExists
		}
		return nil, err
	}
	s.Invalidate(in.BranchID)
	return &in, nil
}

// Get 查一个 branch_cube_source,不存在 → model.ErrNotFound。
func (s *Service) Get(ctx context.Context, branchID string) (*model.BranchCubeSource, error) {
	var row model.BranchCubeSource
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrNotFound
		}
		return nil, err
	}
	return &row, nil
}

// List 列出全部(供 admin 管理 UI)。
func (s *Service) List(ctx context.Context) ([]model.BranchCubeSource, error) {
	var rows []model.BranchCubeSource
	if err := s.db.WithContext(ctx).Order("branch_id").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// Update 修改 CubeSourceName / Enabled。branch_id 不可改。
func (s *Service) Update(ctx context.Context, branchID string, cubeSourceName string, enabled bool, updatedBy string) (*model.BranchCubeSource, error) {
	if cubeSourceName == "" {
		return nil, errors.New("cube_router: CubeSourceName 必填")
	}
	res := s.db.WithContext(ctx).
		Model(&model.BranchCubeSource{}).
		Where("branch_id = ?", branchID).
		Updates(map[string]any{
			"cube_source_name": cubeSourceName,
			"enabled":          enabled,
			"updated_at":       s.now(),
			// updated_by 暂不存(避免无对应字段),后续如需审计再加列。
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, model.ErrNotFound
	}
	s.Invalidate(branchID)
	return s.Get(ctx, branchID)
}

// Delete 删一个 branch_cube_source。
func (s *Service) Delete(ctx context.Context, branchID string) error {
	res := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Delete(&model.BranchCubeSource{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrNotFound
	}
	s.Invalidate(branchID)
	return nil
}

// isUniqueViolation 判断 error 是否为 unique constraint 冲突(兼容 sqlite / postgres)。
//
// gorm v1.25 没有公开 ErrDuplicatedKey,只能用错误信息字符串匹配。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// postgres
	if contains(msg, "duplicate key value violates unique constraint") {
		return true
	}
	// sqlite
	if contains(msg, "UNIQUE constraint failed") {
		return true
	}
	return false
}

// contains 是 strings.Contains 的本地别名(避免引入 strings 包到 hot path)。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}