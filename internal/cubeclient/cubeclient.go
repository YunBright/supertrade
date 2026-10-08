// Package cubeclient 提供本系统 → cube sixun 的客户端接口。
//
// 生产实现只有 DaprCubeClient 一个(经 dapr/go-sdk 的 dapr.Client 调用 cube)。
// 没有 mock / 内存实现,也没有运行期开关 ——
// 曾存在的 CUBE_CLIENT_MODE=memory 在 2026-10-08 移除,原因见
// internal/cubeclient/cubeclientfake 包注释(那三个"生产读假数据"的服务)。
//
// 测试请用 internal/cubeclient/cubeclientfake。
//
// DTO 定义见本包 dto.go。
package cubeclient

import "context"

// Client 是 cube sixun 的客户端抽象。
//
// 生产唯一实现是 DaprCubeClient。
type Client interface {
	GetProduct(ctx context.Context, productID string) (*ProductDTO, error)
	GetStock(ctx context.Context, branchID, productID string) (*StockSnapshotDTO, error)

	// SearchProductsByBarcode 按 barcode 查商品(合并该门店 stock),
	// 用于扫商品接口(REQUIREMENTS §2.1.4.1)。
	//
	// barcode 长度策略:
	//   - <5 位:返回空(防全表扫)
	//   - 5~12 位:后缀匹配(逐条 endsWith)
	//   - ≥13 位:精确匹配
	//
	// limit 在 5~12 位时尊重用户;≥13 位自动取 1。
	// 返回的 ProductWithStock.Stock 可能 nil(该门店无 stock 数据)。
	SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) ([]ProductWithStock, error)

	// SearchSuppliers 按 name 模糊查供应商(用于品类 / 供应商下拉)。
	SearchSuppliers(ctx context.Context, query string, limit int) ([]SupplierDTO, error)
}
