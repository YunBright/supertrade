// Package cubeclient 提供本系统 → cube sixun 的客户端接口 + 共享 DTO。
//
// 本期实现:
//   - cubeclientfake.Client(测试替身,仅 _test 引用;不在生产路径)
//   - DaprCubeClient(经 dapr/go-sdk 调 cube /v1/load)
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

// ErrCubeNotFound 表示**目标 cube 明确返回"没有这条数据"**(HTTP 404)。
// 各 GetX 方法负责包成具体错误(ErrProductNotFound / ErrStockNotFound 等)。
//
// 引入原因:DaprCubeClient.LoadCubeQuery 的 404 不能直接定为"商品"或"库存"——
// cube 本身只说"找不到",具体语义由调用方决定。
//
// ⚠️ 只允许用于"数据确实不存在"。调用链故障(方法名写错、source 未注册、
// 连接被拒、超时、5xx)必须走 ErrCubeUnavailable。把故障当成 not-found,
// 会让上层把基础设施问题翻译成"本门店没有这个商品"——
// 历史上就是这么把一次 404 故障显示成了业务结论。
var ErrCubeNotFound = errors.New("cubeclient: cube 404 not found")

// ErrCubeUnavailable 表示**调用链路故障**,与数据是否存在无关。
//
// 涵盖:方法未实现(Unimplemented)、连接失败(Unavailable)、超时(DeadlineExceeded)、
// sidecar / cube 进程 5xx。任何情况下都不得翻译成"商品不存在"。
var ErrCubeUnavailable = errors.New("cubeclient: cube 调用链路不可用")

// ErrCubeBadRequest 表示查询本身不合法(schema 字段不存在、filters 写错等),
// 属于调用方 bug,应暴露为 400 而不是"查不到"。
var ErrCubeBadRequest = errors.New("cubeclient: cube 拒绝该查询")

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

// StockSnapshotDTO 是某门店 cube 实例里某商品的库存快照。
//
// 本系统调用 cube 时,book_qty 必须用该 DTO 的 Quantity,快照时间用 UpdatedAt。
//
// 门店作用域说明:
//   - BranchID 是**本系统的 branch_id**(X-Branch-ID 的值),是路由键,
//     不是从 cube 响应里取的。门店隔离由 X-Branch-ID → cube-router →
//     branch_cube_sources → 该门店专属 cube 实例完成。
//   - cube 的 stock.branch_id 是思迅 t_im_branch_stock.branch_no(实测 "0001"),
//     与本系统 branch_id("00")是无关的两套编码,不可混用、不可用作过滤条件。
//
// 跨店阻断:stocktake 新增明细时保证"该门店的 cube 里存在该商品的库存记录",
// 不存在则 ErrStockNotFound。
type StockSnapshotDTO struct {
	ProductID   string          `json:"product_id"`    // cube stock.product_id
	BranchID    string          `json:"branch_id"`     // 本系统 branch_id(X-Branch-ID),非思迅 branch_no
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
