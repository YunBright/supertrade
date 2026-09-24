// Package model 定义 cube-router 服务的 GORM 模型。
//
// 数据归属:cube-router 是 branch_cube_sources 表的唯一拥有方(catalog /
// inventory 不读不写这张表);Admin 通过 /admin/branch-cube-sources CRUD 配置
//"某个门店的 cube 查询应转发到哪个 cube 实例"(六讯思迅 cube 各门店版本不同)。
//
// 设计要点:
//   - branch_id 是 PK(每门店一行;一店一行配置)。uuid.String() 36 字符,放进
//     varchar(64) 仍留余量。
//   - cube_source_name 是 dapr app-id(如 "cube-gateway" / "sixun-hbposv7" 等
//     实际 cube 实例名),dapr service invocation 拼接 URL 时使用。
//   - enabled=false 时该门店 /v1/load 返 503(临时关停)。
//   - created_by 来自 admin 调用方的 JWT sub,做审计。
//
// 索引:
//   - PK(branch_id):主查询"门店 X 用哪个 cube"—— 1 次 lookup。
//   - idx_cube_source(cube_source_name):反向查"哪些门店用 cube Y",
//     后续做限流 / 监控时使用。
package model

import (
	"errors"
	"time"
)

// BranchCubeSource 描述"门店 → cube 实例"的映射。
//
// GORM 标签遵循 stocktake/model/model.go 风格:column 显式、type 显式、
// 索引命名 idx_<table>_<col>。
//
// 注意:CreatedAt / UpdatedAt 用 datetime(不用 timestamptz)以兼容 SQLite 单测;
// 生产 PG 会自动用 timestamp with time zone,GORM 双向兼容。
type BranchCubeSource struct {
	BranchID       string    `gorm:"primaryKey;column:branch_id;type:varchar(64)"`
	CubeSourceName string    `gorm:"column:cube_source_name;type:varchar(64);not null;index:idx_cube_source"`
	Enabled        bool      `gorm:"column:enabled;type:boolean;not null"`
	CreatedBy      string    `gorm:"column:created_by;type:varchar(64);not null"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null"`
}

// TableName 显式指定表名,避免 gorm 默认复数转换。
func (BranchCubeSource) TableName() string { return "branch_cube_sources" }

// 业务错误:sentinel 用于 handler 层 4xx / 5xx 映射。
var (
	// ErrNotFound:cube_source 不存在(branch_id 未配置)。
	ErrNotFound = errors.New("cube_router: branch cube source 未配置")
	// ErrAlreadyExists:PK 冲突。
	ErrAlreadyExists = errors.New("cube_router: branch cube source 已存在")
	// ErrDisabled:该门店的 cube source 被管理员禁用(enabled=false)。
	ErrDisabled = errors.New("cube_router: branch cube source 已禁用")
)