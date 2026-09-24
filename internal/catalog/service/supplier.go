// Package service / supplier.go —— 供应商业务逻辑。
//
// 业务规则:
//   - 同一 (id, branch_id) 唯一;跨门店可重名/重 ID(各门店独立维护)
//   - 软删:Delete 调用 UPDATE deleted_at = NOW();列表查询自动过滤
//   - 字段校验:id / branch_id / name 必填;type 默认 '0';status 默认 'active'
//
// 错误:
//   - model.ErrSupplierNotFound        → 404
//   - model.ErrSupplierAlreadyExists   → 409
//   - model.ErrInvalidInput            → 400
//   - 其它 DB error                    → 500
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
)

// SupplierService 是 catalog 的供应商服务。
type SupplierService struct {
	db *gorm.DB
	now func() time.Time
}

// NewSupplierService 构造 SupplierService。
func NewSupplierService(db *gorm.DB) *SupplierService {
	return &SupplierService{db: db, now: func() time.Time { return time.Now() }}
}

// CreateInput 是 Create 的入参。
type CreateSupplierInput struct {
	ID        string
	BranchID  string
	Name      string
	Type      string // 可选,默认 "0"
	Contact   string
	Phone     string
	Email     string
	Address   string
	CreatedBy string
}

// Create 新建一个供应商(同 (id, branch_id) 已存在 → 409)。
func (s *SupplierService) Create(ctx context.Context, in CreateSupplierInput) (*model.Supplier, error) {
	if err := validateSupplierInput(in.ID, in.BranchID, in.Name); err != nil {
		return nil, err
	}
	if in.Type == "" {
		in.Type = "0"
	}
	now := s.now()
	row := &model.Supplier{
		ID:        in.ID,
		BranchID:  in.BranchID,
		Name:      in.Name,
		Type:      in.Type,
		Contact:   in.Contact,
		Phone:     in.Phone,
		Email:     in.Email,
		Address:   in.Address,
		Status:    "active",
		CreatedBy: in.CreatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, model.ErrSupplierAlreadyExists
		}
		return nil, err
	}
	return row, nil
}

// Get 按 (id, branch_id) 查一个;不存在 → ErrSupplierNotFound。
func (s *SupplierService) Get(ctx context.Context, branchID, id string) (*model.Supplier, error) {
	var row model.Supplier
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", id, branchID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrSupplierNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// List 查某门店下的供应商列表。
//
// q 为空 → 返回该 branch 全部 active 供应商;
// q 非空 → 按 name 模糊 + id 精确(若以 "SUP-" 开头)。
//
// limit <= 0 → 不限。
func (s *SupplierService) List(ctx context.Context, branchID, q string, limit int) ([]model.Supplier, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	tx := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if q != "" {
		if strings.HasPrefix(q, "SUP-") {
			tx = tx.Where("id = ?", q)
		} else {
			like := "%" + q + "%"
			tx = tx.Where("name LIKE ?", like)
		}
	}
	if limit > 0 {
		tx = tx.Limit(limit)
	}
	var rows []model.Supplier
	if err := tx.Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateInput 是 Update 的入参(只更新非空字段)。
type UpdateSupplierInput struct {
	Name      *string
	Type      *string
	Contact   *string
	Phone     *string
	Email     *string
	Address   *string
	Status    *string
	UpdatedBy string
}

// Update 改一个供应商;按 (id, branch_id) 定位。
func (s *SupplierService) Update(ctx context.Context, branchID, id string, in UpdateSupplierInput) (*model.Supplier, error) {
	updates := map[string]any{"updated_at": s.now()}
	if in.Name != nil { updates["name"] = *in.Name }
	if in.Type != nil { updates["type"] = *in.Type }
	if in.Contact != nil { updates["contact"] = *in.Contact }
	if in.Phone != nil { updates["phone"] = *in.Phone }
	if in.Email != nil { updates["email"] = *in.Email }
	if in.Address != nil { updates["address"] = *in.Address }
	if in.Status != nil { updates["status"] = *in.Status }
	if in.UpdatedBy != "" { updates["updated_by"] = in.UpdatedBy }

	res := s.db.WithContext(ctx).
		Model(&model.Supplier{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, model.ErrSupplierNotFound
	}
	return s.Get(ctx, branchID, id)
}

// Delete 软删一个供应商。
func (s *SupplierService) Delete(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).
		Model(&model.Supplier{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Update("deleted_at", s.now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrSupplierNotFound
	}
	return nil
}

// ---- helpers ----

func validateSupplierInput(id, branchID, name string) error {
	if id == "" || branchID == "" || name == "" {
		return model.ErrInvalidInput
	}
	return nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "duplicate key value violates unique constraint") {
		return true
	}
	if strings.Contains(msg, "UNIQUE constraint failed") {
		return true
	}
	return false
}