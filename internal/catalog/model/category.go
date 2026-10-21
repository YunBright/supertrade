// Package model / category.go —— 分类本地表。
//
// # 为什么需要这张表
//
// 2026-09 重构后,catalog 的分类是**纯 cube 转发**(`GET /categories?with_counts=`
// 走 cube-router → 思迅 `t_bd_item_cls`)。自营门店(门店 10)没有 cube 源
// (`branch_cube_sources` 里只有 00 / 01),所以门店 10 的分类必须由本系统自己持有。
//
// # 为什么分类必须带 parent_id
//
// 思迅的分类是**多级自关联**(`t_bd_item_cls.cls_parent`),实测门店 00 有
// 3 个顶级 / 186 个节点 / 最深 3 层(生鲜 → 水果 → 进口水果)。
//
// ⚠️ **导入时必须带父类别**:cube 的 `AGENTS.md` 记过一个坑 ——
// 思迅的类别报表口径是按**父类**(生鲜联营 / 百货联营)算的,不是按末级类别。
// 丢了 parent_id,门店 10 的"按类别"报表口径就会变,而且**不报任何错**。
//
// # 索引设计要点
//
//   - 复合 PK (id, branch_id):同名分类在不同门店可独立维护
//   - idx_category_branch_parent(branch_id, parent_id):按父节点取子节点(建树用)
//   - idx_category_branch_name(branch_id, name):按名称查
//
// 时间戳字段用 datetime(不用 timestamptz)以兼容 SQLite 单测;
// 生产 PG 会自动用 timestamp with time zone,GORM 双向兼容。
package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Category 是 catalog 服务的分类本地表。
//
// 状态(status):
//   - "active"   = 启用
//   - "inactive" = 停用
//
// 软删:用 gorm.DeletedAt(**不是** *time.Time)——
// *time.Time 不会被 GORM 自动过滤,删掉的行照样查得到。
// 同仓的 suppliers 表用的是 *time.Time(历史遗留,列表查询因此不过滤软删),
// 本表与 products 表一开始就用对的,不再继承那个坑。
type Category struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	Name     string `gorm:"column:name;type:varchar(255);not null;index:idx_category_branch_name" json:"name"`
	ParentID string `gorm:"column:parent_id;type:varchar(64);index:idx_category_branch_parent" json:"parent_id"`
	// SortOrder 是同级展示顺序。升序排,相同则按 id。
	SortOrder int            `gorm:"column:sort_order;type:integer;not null;default:0" json:"sort_order"`
	Status    string         `gorm:"column:status;type:varchar(16);not null;index:idx_category_branch_status" json:"status"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Category) TableName() string { return "categories" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrCategoryNotFound:按 (id, branch_id) 未命中。
	ErrCategoryNotFound = errors.New("catalog: category 不存在")
	// ErrCategoryAlreadyExists:同 (id, branch_id) 已存在。
	ErrCategoryAlreadyExists = errors.New("catalog: category 已存在")
	// ErrCategoryCycle:把分类挂到自己的子孙下面会成环。
	//
	// 分类是树,成环 ⇒ 建树时无限递归(且**不报错**,直接把服务打挂)。
	// 与其等到建树崩,不如在写入时就拒掉。
	ErrCategoryCycle = errors.New("catalog: category 父子关系成环")
)
