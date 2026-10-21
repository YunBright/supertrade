// Package model / product.go —— 商品本地表。
//
// # 本表从"占位"变成"拥有者"(2026-10-20)
//
// 2026-09 重构时这张表只建了结构没实装 CRUD(见下方"为什么本期只建表不实现"的
// 历史说明),`internal/catalog/service/product.go` 里只有一个
// `ErrNotImplemented` 占位。
//
// 现在实装,原因是**自营门店(门店 10)没有 cube 源**:
// `branch_cube_sources` 实测只有 `00` → cube-sixun-ysx-fb 与
// `01` → cube-sixun-hbposv7-jiale,门店 10 的商品档案没有别的地方可取。
//
// # 价格不在本表
//
// ⚠️ **售价/促销归 pricing 服务**(`price_lists` / `promotions`),
// 本表**不放** price 字段。
//
// 理由:价格有生效期与历史版本(小票是对账凭据,改了价不能让历史单对不上),
// 而商品档案是"当前值"。两处都存价 ⇒ 两个真相源。
//
// # 设计要点(对齐 supplier.go 的索引策略)
//
//   - 复合 PK (id, branch_id):同商品名在不同门店可独立维护
//   - idx_product_branch_name(branch_id, name):按商品名查询
//   - idx_product_branch_category(branch_id, category_id):按品类过滤
//   - idx_product_branch_barcode(branch_id, barcode):扫条码精确查
//
// 时间戳字段用 datetime(不用 timestamptz)以兼容 SQLite 单测;
// 生产 PG 会自动用 timestamp with time zone,GORM 双向兼容。
package model

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Product 是 catalog 服务的商品本地表。
//
// 状态(status):
//   - "active"   = 启用
//   - "inactive" = 停用
//
// 软删:用 gorm.DeletedAt(**不是** *time.Time)——
// *time.Time 不会被 GORM 自动过滤,删掉的行照样查得到。
// 同仓的 suppliers 表用的是 *time.Time(历史遗留,列表查询因此不过滤软删),
// 本表一开始就用对的,不再继承那个坑。
type Product struct {
	ID         string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID   string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	Name       string `gorm:"column:name;type:varchar(255);not null;index:idx_product_branch_name" json:"name"`
	CategoryID string `gorm:"column:category_id;type:varchar(64);index:idx_product_branch_category" json:"category_id"`
	SupplierID string `gorm:"column:supplier_id;type:varchar(64)" json:"supplier_id"`
	// SubCode 是**自编码 / 助记码**(思迅 `t_bd_item_info.item_subno`)。
	//
	// ⚠️ **不是唯一的**!实测 ysx 27,565 个商品里:
	//   - 27,547 个自编码对应 1 个货号;
	//   - **9 个自编码对应 2 个货号** —— 全部是「正常品 + 换购品」配对
	//     (例:`6904406383122` → `02000110` 远航九江双蒸酒精品500ml(16.50)
	//      / `02000111` (换购)远航九江双蒸酒精品500ml(5.00))。
	//
	// 源库自己也只给 `item_subno` 建了**非唯一**索引(`i_bd_item_info_1`)
	// ⇒ 1:N 是设计,不是脏数据。
	//
	// ⇒ **扫自编码会命中多个商品** ⇒ 查商品的端点必须返回**候选列表**,
	// 不能假定一个编码对一个商品。
	SubCode string `gorm:"column:sub_code;type:varchar(64);index:idx_product_branch_sub_code" json:"sub_code"`
	// IsExchange 是**换购品**标记(思迅用品名前缀「(换购)」「（换购）」承载)。
	//
	// ⚠️ 思迅**没有**这个字段 —— 它把特殊语义塞在品名里。
	// 实测 19 个品名含"换购"(售价多为 .0000 或象征价 1 元/15 元)⇒ 不单独售卖,
	// 是"搭售/积分换购"的载体。
	// ⇒ 导入时**必须解析成字段**:否则 POS 界面上会出现「(换购)远航九江…」这种脏品名,
	// 而且**没法按"是否换购"过滤**(图1 商品选择器就有「是否积分」这一列)。
	IsExchange bool `gorm:"column:is_exchange;type:boolean;not null;default:false;index:idx_product_branch_exchange" json:"is_exchange"`
	// IsVoid 是**停用品**标记(思迅用品名前缀「(停用)」承载)。
	//
	// 实测 17 个品名以「（停用）」开头;带括号前缀的共 62 个。
	// ⇒ 同样是"解析成字段",而不是靠字符串匹配。
	IsVoid bool `gorm:"column:is_void;type:boolean;not null;default:false;index:idx_product_branch_void" json:"is_void"`
	// Barcode 是**主条码**(冗余自 product_barcodes)。
	//
	// ⚠️ 思迅的**商品条码**在独立表 `t_bd_item_barcode`,与 `item_subno` 是
	// **两回事**(实测两张表零交集)。该表实测每个 item_barcode 只对应 1 个
	// item_no,而**一个 item_no 可有 1~7 个条码** ⇒ 1 商品 : N 条码。
	//
	// ⚠️ 条码**只在本系统维护**(cube 的 product model 不含 barcode)。
	Barcode string `gorm:"column:barcode;type:varchar(64);index:idx_product_branch_barcode" json:"barcode"`
	// Unit 是基本单位(思迅 `unit_no`,实测值直接就是单位名本身如"提")。
	Unit string `gorm:"column:unit;type:varchar(32)" json:"unit"`
	// Spec 是规格(思迅 `item_size`,实测有 "1*10" 这类脏值 —— 原样存,不清洗)。
	Spec      string         `gorm:"column:spec;type:varchar(255)" json:"spec"`
	Status    string         `gorm:"column:status;type:varchar(16);not null;index:idx_product_branch_status" json:"status"`
	CreatedBy string         `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string         `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	CreatedAt time.Time      `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

// TableName 显式指定表名。
func (Product) TableName() string { return "products" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrProductNotFound:按 (id, branch_id) 未命中。
	ErrProductNotFound = errors.New("catalog: product 不存在")
	// ErrProductAlreadyExists:同 (id, branch_id) 已存在。
	ErrProductAlreadyExists = errors.New("catalog: product 已存在")
)
