// Package cubeclient 提供本系统 → cube sixun 的客户端接口 + 共享 DTO。
//
// 本期实现:
//   - InMemoryClient(用真实六讯思迅 schema 的 mock 数据,服务单测/台本地开发用)
//   - HTTPCubeClient(经 Dapr service invocation 调 cube-gateway /v1/load)
//
// cube 响应 DTO(本包内定义):
//   - ProductDTO / StockSnapshotDTO / SupplierDTO 与 cube 各 model schema 一一对应
//   - 本系统不创建对应的 PG 表(REQUIREMENTS §7.5),只通过 Go struct 做类型契约
//
// 关键设计(REQUIREMENTS §7.5):
//   - cube product.id 即思迅 item_no(string),与本系统 StocktakeLine.ProductID 同类型
//   - cube stock 维度 (product_id, branch_id) 必须存在,否则视为跨店阻断
//   - book_qty 必须是录入该行时刻的 cube 库存快照,带快照时间 book_qty_at
package cubeclient

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ErrProductNotFound 业务侧:商品在 cube 中不存在。
var ErrProductNotFound = errors.New("cubeclient: 商品在 cube 中不存在")

// ErrStockNotFound 业务侧:某 (branch_id, product_id) 在 cube stock 中不存在
//
// 触发场景:跨店盘点(product_id 不在该 branch_id 下存在 cube stock)。
var ErrStockNotFound = errors.New("cubeclient: 该分店下的商品 stock 不存在(疑似跨店盘点)")

// ErrCubeNotFound 通用 cube 404(cube 找不到指定 entity);
// 各 GetX 方法负责包成具体错误(ErrProductNotFound / ErrStockNotFound 等)。
//
// 引入原因:HTTPCubeClient.LoadCubeQuery 的 404 不能直接定为"商品"或"库存"——
// cube 本身只说"找不到",具体语义由调用方决定。
var ErrCubeNotFound = errors.New("cubeclient: cube 404 not found")

// ---- cube 响应 DTO(非 GORM,只作类型契约 / 响应类型) ----

// ProductDTO 是 cube `product` model 的响应子集,作为本系统的响应类型契约。
//
// 字段含义与 cube `F:\go\src\github.com\YunBright\cube\sixun-models\product\schema.yaml` 一致;
// 本系统不创建对应的 PG 表(REQUIREMENTS §7.5),但 Go struct 字段名 / 类型 / JSON tag 全部对齐。
type ProductDTO struct {
	ID         string `json:"id"`               // cube product.id = 思迅 item_no
	Name       string `json:"name"`             // cube product.name
	CategoryID string `json:"category_id"`      // cube product.category_id
	SupplierID string `json:"supplier_id"`      // cube product.supplier_id
	Unit       string `json:"unit,omitempty"`   // 商品基本单位(扩展字段)
	Spec       string `json:"spec,omitempty"`   // 规格(扩展字段)
	Status     string `json:"status"`           // cube product.status
	Barcode    string `json:"barcode,omitempty"` // 主条码(扩展,给扫码用)
}

// StockSnapshotDTO 是 cube `stock` model 在某 (branch_id, product_id) 下的快照。
//
// 本系统调用 cube 时,book_qty 必须用该 DTO 的 Quantity,快照时间用 UpdatedAt。
// 跨店阻断:本系统的 stocktake_lines 必须保证 cube 上 (branch_id, product_id) 存在。
type StockSnapshotDTO struct {
	ProductID   string          `json:"product_id"`    // cube stock.product_id
	BranchID    string          `json:"branch_id"`     // cube stock.branch_id
	Quantity    decimal.Decimal `json:"quantity"`      // cube stock.total_quantity
	AvgCostYuan decimal.Decimal `json:"avg_cost_yuan"` // cube stock.avg_cost
	UpdatedAt   time.Time       `json:"updated_at"`    // cube stock.updated_at(快照时间)
}

// SupplierDTO 是 cube `supplier` model 的响应子集。
//
// 字段含义与 cube `F:\go\src\github.com\YunBright\cube\sixun-models\supplier\schema.yaml` 对齐;
// 本系统不创建 PG `supplier` 表(REQUIREMENTS §7.5),只作为响应类型契约。
type SupplierDTO struct {
	ID      string `json:"id"`                // cube supplier.id
	Name    string `json:"name"`              // cube supplier.name
	Type    string `json:"type"`              // "0"=供应商 / "1"=客户
	Contact string `json:"contact,omitempty"` // 联系人
	Phone   string `json:"phone,omitempty"`   // 电话
	Email   string `json:"email,omitempty"`   // 邮箱
	Address string `json:"address,omitempty"` // 地址
	Status  string `json:"status,omitempty"`  // cube supplier.status
}

// ProductWithStock 是 product + 该门店 stock 的复合(用于一次性聚合接口)。
//
// 用于 `/api/v1/products/search`(REQUIREMENTS §2.1.4.1)。
type ProductWithStock struct {
	Product *ProductDTO       `json:"product"`
	Stock   *StockSnapshotDTO `json:"stock,omitempty"` // 可能 nil(该门店无 stock 数据)
}
