// Package service / category.go —— 分类业务逻辑(本地表 CRUD)。
//
// # 分类是树,写入时就要挡住成环
//
// 分类通过 parent_id 自关联(思迅 `t_bd_item_cls.cls_parent`,实测最深 3 层)。
// 如果 A 的父是 B、B 的父是 A,建树时就是无限递归 —— 而且**不报错**,
// 直接把请求打挂(栈溢出)。
//
// ⇒ Update / Create 时检查"把 parent 挂到自己的子孙下"并拒掉
// (model.ErrCategoryCycle → 400)。
//
// # 父节点不存在怎么办
//
// 数据不一致时(父 id 在库里查不到),该节点当**顶级**处理,不能让它凭空消失。
// 这与 cube 侧的 buildCategoryTree 是同一套口径(见 service/catalog.go)。
//
// 错误:
//
//   - model.ErrCategoryNotFound      → 404
//   - model.ErrCategoryAlreadyExists → 409
//   - model.ErrCategoryCycle         → 400
//   - model.ErrInvalidInput          → 400
//   - 其它 DB error                  → 500
package service

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
)

// CategoryService 是 catalog 的分类服务。
type CategoryService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewCategoryService 构造 CategoryService。
func NewCategoryService(db *gorm.DB) *CategoryService {
	return &CategoryService{db: db, now: func() time.Time { return time.Now() }}
}

// CreateCategoryInput 是 Create 的入参。
type CreateCategoryInput struct {
	ID       string
	BranchID string
	Name     string
	ParentID string
	// SortOrder 可选,默认 0。
	SortOrder int
	// Status 可选,默认 "active"。
	Status    string
	CreatedBy string
}

// Create 新建一个分类(同 (id, branch_id) 已存在 → 409)。
func (s *CategoryService) Create(ctx context.Context, in CreateCategoryInput) (*model.Category, error) {
	if err := validateCategoryInput(in.ID, in.BranchID, in.Name); err != nil {
		return nil, err
	}
	if in.Status == "" {
		in.Status = "active"
	}
	if in.ParentID == in.ID {
		// 自己当自己的父 ⇒ 立刻成环。
		return nil, model.ErrCategoryCycle
	}
	now := s.now()
	row := &model.Category{
		ID:        in.ID,
		BranchID:  in.BranchID,
		Name:      in.Name,
		ParentID:  in.ParentID,
		SortOrder: in.SortOrder,
		Status:    in.Status,
		CreatedBy: in.CreatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, model.ErrCategoryAlreadyExists
		}
		return nil, err
	}
	return row, nil
}

// Get 按 (id, branch_id) 查一个;不存在 → ErrCategoryNotFound。
func (s *CategoryService) Get(ctx context.Context, branchID, id string) (*model.Category, error) {
	var row model.Category
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", id, branchID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, model.ErrCategoryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// List 查某门店下的全部分类(平铺)。
//
// 排序:先按 parent_id,再按 sort_order,最后按 id —— 保证同一父下的兄弟
// 顺序稳定,且**顶级分类聚在一起**(parent_id 为空的排在前面)。
func (s *CategoryService) List(ctx context.Context, branchID string) ([]model.Category, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	var rows []model.Category
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("parent_id, sort_order, id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateCategoryInput 是 Update 的入参(只更新非空字段)。
type UpdateCategoryInput struct {
	Name      *string
	ParentID  *string
	SortOrder *int
	Status    *string
	UpdatedBy string
}

// Update 改一个分类;按 (id, branch_id) 定位。
func (s *CategoryService) Update(ctx context.Context, branchID, id string, in UpdateCategoryInput) (*model.Category, error) {
	if in.ParentID != nil {
		if err := s.checkNoCycle(ctx, branchID, id, *in.ParentID); err != nil {
			return nil, err
		}
	}
	updates := map[string]any{"updated_at": s.now()}
	if in.Name != nil {
		updates["name"] = *in.Name
	}
	if in.ParentID != nil {
		updates["parent_id"] = *in.ParentID
	}
	if in.SortOrder != nil {
		updates["sort_order"] = *in.SortOrder
	}
	if in.Status != nil {
		updates["status"] = *in.Status
	}
	if in.UpdatedBy != "" {
		updates["updated_by"] = in.UpdatedBy
	}

	res := s.db.WithContext(ctx).
		Model(&model.Category{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, model.ErrCategoryNotFound
	}
	return s.Get(ctx, branchID, id)
}

// Delete 软删一个分类。
//
// ⚠️ **不级联删子分类**:子分类的 parent_id 会指向一个已删节点。
// 建树时"父节点不存在 ⇒ 当顶级处理"这条规则会兜住它(不会丢数据,
// 只是那棵子树被提到顶级)。要真删,调用方自己先删子节点。
func (s *CategoryService) Delete(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).
		Model(&model.Category{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Update("deleted_at", s.now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrCategoryNotFound
	}
	return nil
}

// SubtreeIDs 返回某分类**含自身**的全部子孙 id。
//
// 用途:按分类过滤商品时,语义是「该分类 + 它的所有子孙」(分类是树)。
// 与 cube 侧 categorySubtree 同一口径 ⇒ 门店 10 的筛选结果与门店 00 一致。
//
// ⚠️ 成环保护:visited 集合一旦命中就停,不死循环。
func (s *CategoryService) SubtreeIDs(ctx context.Context, branchID, rootID string) ([]string, error) {
	rows, err := s.List(ctx, branchID)
	if err != nil {
		return nil, err
	}
	children := map[string][]string{}
	for _, r := range rows {
		if r.ParentID != "" {
			children[r.ParentID] = append(children[r.ParentID], r.ID)
		}
	}
	out := []string{}
	visited := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		out = append(out, id)
		for _, c := range children[id] {
			walk(c)
		}
	}
	walk(rootID)
	return out, nil
}

// checkNoCycle 检查"把 id 的父改成 parentID"会不会成环。
//
// 做法:从 parentID 出发往上走父链,若中途撞到 id ⇒ 成环。
//
// ⚠️ 往上也可能自己撞环(数据已不一致),用 visited 集合切断。
func (s *CategoryService) checkNoCycle(ctx context.Context, branchID, id, parentID string) error {
	if parentID == "" {
		return nil
	}
	if parentID == id {
		return model.ErrCategoryCycle
	}
	rows, err := s.List(ctx, branchID)
	if err != nil {
		return err
	}
	parentOf := make(map[string]string, len(rows))
	for _, r := range rows {
		parentOf[r.ID] = r.ParentID
	}
	visited := map[string]bool{}
	for cur := parentID; cur != ""; {
		if visited[cur] {
			// 数据本身已有环 —— 不由这次修改造成,不拦。
			break
		}
		visited[cur] = true
		if cur == id {
			return model.ErrCategoryCycle
		}
		cur = parentOf[cur]
	}
	return nil
}

// ---- helpers ----

// validateCategoryInput 校验必填字段。
func validateCategoryInput(id, branchID, name string) error {
	if strings.TrimSpace(id) == "" ||
		strings.TrimSpace(branchID) == "" ||
		strings.TrimSpace(name) == "" {
		return model.ErrInvalidInput
	}
	return nil
}
