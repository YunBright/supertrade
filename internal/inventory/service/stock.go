// Package service —— inventory 域的业务逻辑(库存快照查询 / 库存流水查询 / sale.paid 扣减)。
//
// # 三条业务规则
//
//  1. **快照是"现在有多少",流水是"为什么"** —— 两者都要,不可互相替代。
//  2. **一次库存变动 = 一行流水 + 快照更新,在同一个事务里** ——
//     否则进程被杀就出现"库存扣了但没流水"(对不上账)或"有流水但没扣"(库存虚高)。
//  3. **同一笔业务单据的同一个商品,只落一行流水** ——
//     靠 stock_moves 的唯一索引 (branch_id, product_id, ref_type, ref_id)
//     + `clause.OnConflict{DoNothing}` 实现,重复投递**不重复扣库存**。
//
// # 金额 / 数量
//
// 一律 decimal(Shopspring),**绝不用 float**。
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/inventory/model"
)

// actor 是系统写入的 created_by / updated_by。
const actor = "system"

// 分页默认值(与 catalog 的商品列表同款)。
const (
	// DefaultPageSize 是列表的默认每页条数。
	DefaultPageSize = 50
	// MaxPageSize 是每页条数上限。
	MaxPageSize = 200
)

// StockService 是库存服务。
type StockService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewStockService 构造 StockService。
func NewStockService(db *gorm.DB) *StockService {
	return &StockService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *StockService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// StockLevelQuery 是库存快照的查询条件。
type StockLevelQuery struct {
	// BranchID 是门店(必填)。跨店查询不走这里。
	BranchID string
	// ProductID 是精确过滤(空 = 不过滤)。
	ProductID string
	// Keyword 是模糊过滤。
	//
	// ⚠️ **只匹配 product_id**:stock_levels 按"只放必要的列"建表,
	//
	//	没有 product_name —— 商品名归 catalog。
	//	⇒ 要按商品名搜,应由调用方拿 catalog 的商品 id 列表再逐个查。
	Keyword string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间。
//
// ⚠️ 这**不是**校验,是兜底:服务层被直接调用时也不该算出负 offset。
func (q StockLevelQuery) normalized() StockLevelQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	return q
}

// ListLevels 分页查库存快照。
//
// 返回 (rows, total):total 是**过滤后的总条数**,不是本页条数。
func (s *StockService) ListLevels(ctx context.Context, q StockLevelQuery) ([]model.StockLevel, int64, error) {
	if strings.TrimSpace(q.BranchID) == "" {
		return nil, 0, model.ErrInvalidInput
	}
	q = q.normalized()

	tx := s.db.WithContext(ctx).Model(&model.StockLevel{}).Where("branch_id = ?", q.BranchID)
	if q.ProductID != "" {
		tx = tx.Where("product_id = ?", q.ProductID)
	}
	if q.Keyword != "" {
		tx = tx.Where("product_id LIKE ?", "%"+q.Keyword+"%")
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.StockLevel
	if err := tx.Order("product_id asc").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// GetLevel 取某个商品在某门店的库存快照;不存在 → model.ErrStockLevelNotFound。
func (s *StockService) GetLevel(ctx context.Context, branchID, productID string) (*model.StockLevel, error) {
	if strings.TrimSpace(branchID) == "" || strings.TrimSpace(productID) == "" {
		return nil, model.ErrInvalidInput
	}
	var row model.StockLevel
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrStockLevelNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// StockMoveQuery 是库存流水的查询条件。
type StockMoveQuery struct {
	// BranchID 是门店(必填)。
	BranchID string
	// ProductID 是精确过滤(空 = 不过滤)。
	ProductID string
	// MoveType 是变动类型过滤(空 = 不过滤)。
	MoveType string
	// OccurredFrom / OccurredTo 是业务发生时刻的区间(含端点);nil = 不限。
	OccurredFrom *time.Time
	OccurredTo   *time.Time
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间。
func (q StockMoveQuery) normalized() StockMoveQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	return q
}

// ListMoves 分页查库存流水。
//
// 返回 (rows, total):total 是**过滤后的总条数**,不是本页条数。
func (s *StockService) ListMoves(ctx context.Context, q StockMoveQuery) ([]model.StockMove, int64, error) {
	if strings.TrimSpace(q.BranchID) == "" {
		return nil, 0, model.ErrInvalidInput
	}
	q = q.normalized()

	tx := s.db.WithContext(ctx).Model(&model.StockMove{}).Where("branch_id = ?", q.BranchID)
	if q.ProductID != "" {
		tx = tx.Where("product_id = ?", q.ProductID)
	}
	if q.MoveType != "" {
		tx = tx.Where("move_type = ?", q.MoveType)
	}
	if q.OccurredFrom != nil {
		tx = tx.Where("occurred_at >= ?", *q.OccurredFrom)
	}
	if q.OccurredTo != nil {
		tx = tx.Where("occurred_at <= ?", *q.OccurredTo)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.StockMove
	if err := tx.Order("occurred_at desc, id desc").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// ---- helpers ----

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
