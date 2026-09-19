//go:build ignore
// +build ignore

// MOVED -> ../cubeclient/ (same basename). 本文件已废弃,忽略 build。

// Package cubeclient 提供本系统 → cube sixun 的客户端接口。
//
// 本期实现:InMemoryClient(用真实六讯思迅 schema 的 mock 数据,服务单测/台本地开发用)
// 后续 Phase:HTTPCubeClient(经 dapr service invocation 调 cube-gateway /v1/load)
//
// 关键设计(REQUIREMENTS §7.5):
//   - cube product.id 即思迅 item_no(string),与本系统 StocktakeLine.ProductID 同类型
//   - cube stock 维度 (product_id, branch_id) 必须存在,否则视为跨店阻断
//   - book_qty 必须是录入该行时刻的 cube 库存快照,带快照时间 book_qty_at
package cubeclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/YunBright/supertrade/internal/stocktake/model"
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

// Client 是 cube sixun 的客户端抽象。
//
// 实现 InMemoryClient / HTTPCubeClient(后续)。
type Client interface {
	GetProduct(ctx context.Context, productID string) (*model.ProductDTO, error)
	GetStock(ctx context.Context, branchID, productID string) (*model.StockSnapshotDTO, error)

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
	SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) ([]model.ProductWithStock, error)

	// SearchSuppliers 按 name 模糊查供应商(用于品类 / 供应商下拉)。
	SearchSuppliers(ctx context.Context, query string, limit int) ([]model.SupplierDTO, error)
}

// InMemoryClient 是用真实六讯思迅 schema 的 mock 数据实现的 Client。
//
// mock 数据来源:F:\go\src\github.com\YunBright\cube\sixun-models
//   - product schema: id / name / category_id / supplier_id / status / created_at
//   - stock schema:   product_id / branch_id / quantity / avg_cost / updated_at
//   - supplier schema: id / name / type / contact / phone / email / address / status
//
// 数据条目是常量,key = item_no / (branch, item_no) / supplier_id,用 RWMutex 保护,
// 满足单元测试 / 本地开发的可重复性。
type InMemoryClient struct {
	mu        sync.RWMutex
	products  map[string]model.ProductDTO                  // item_no → product
	stock     map[string]map[string]model.StockSnapshotDTO // branch_id → item_no → snapshot
	suppliers map[string]model.SupplierDTO                 // supplier_id → supplier
	clock     func() time.Time                             // 注入时间,默认 time.Now().UTC()
}

// key 组合库存 key。
func stockKey(branchID, productID string) string {
	return branchID + "|" + productID
}

// NewInMemoryClient 构造默认 mock 客户端(自带 5 个商品 + 2 个分店的库存 + 3 个供应商)。
//
// 默认商品:
//   - P-1001 可口可乐 330ml(S001=100/S002=80)
//   - P-1002 雪碧 330ml  (S001=50/S002=40)
//   - P-1003 农夫山泉 550ml(S001=200/S002=0 - S002 不存在,用于跨店阻断测试)
//   - P-2001 五花肉(生鲜,S001=15kg)
//   - P-3001 大白菜(生鲜蔬果,S001=50kg)
//
// 默认供应商:
//   - SUP-001 可口可乐华南(type=0)
//   - SUP-002 农夫山泉代理(type=0)
//   - SUP-101 鲜肉供应商(type=0)
func NewInMemoryClient() *InMemoryClient {
	c := &InMemoryClient{
		products:  make(map[string]model.ProductDTO),
		stock:     make(map[string]map[string]model.StockSnapshotDTO),
		suppliers: make(map[string]model.SupplierDTO),
		clock:     func() time.Time { return time.Now().UTC() },
	}
	c.seed()
	return c
}

// SetClock 注入时间(测试用)。注入 nil 恢复 time.Now().UTC()。
func (c *InMemoryClient) SetClock(fn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		fn = func() time.Time { return time.Now().UTC() }
	}
	c.clock = fn
}

func (c *InMemoryClient) seed() {
	now := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	c.products["P-1001"] = model.ProductDTO{
		ID: "P-1001", Name: "可口可乐 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567890",
	}
	c.products["P-1002"] = model.ProductDTO{
		ID: "P-1002", Name: "雪碧 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567891",
	}
	c.products["P-1003"] = model.ProductDTO{
		ID: "P-1003", Name: "农夫山泉 550ml", CategoryID: "CAT-01",
		SupplierID: "SUP-002", Unit: "瓶", Spec: "550ml",
		Status: "active", Barcode: "6901234567892",
	}
	c.products["P-2001"] = model.ProductDTO{
		ID: "P-2001", Name: "五花肉", CategoryID: "CAT-02",
		SupplierID: "SUP-101", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "2001",
	}
	c.products["P-3001"] = model.ProductDTO{
		ID: "P-3001", Name: "大白菜", CategoryID: "CAT-03",
		SupplierID: "SUP-201", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "3001",
	}

	// S001 分店
	c.stock["S001"] = map[string]model.StockSnapshotDTO{
		"P-1001": {ProductID: "P-1001", BranchID: "S001", Quantity: decimal.NewFromInt(100), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1002": {ProductID: "P-1002", BranchID: "S001", Quantity: decimal.NewFromInt(50), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1003": {ProductID: "P-1003", BranchID: "S001", Quantity: decimal.NewFromInt(200), AvgCostYuan: decimal.NewFromFloat(1.8), UpdatedAt: now},
		"P-2001": {ProductID: "P-2001", BranchID: "S001", Quantity: decimal.NewFromFloat(15.5), AvgCostYuan: decimal.NewFromFloat(38.0), UpdatedAt: now},
		"P-3001": {ProductID: "P-3001", BranchID: "S001", Quantity: decimal.NewFromFloat(50.0), AvgCostYuan: decimal.NewFromFloat(3.5), UpdatedAt: now},
	}
	// S002 分店(P-1003 不存在,用于跨店阻断测试)
	c.stock["S002"] = map[string]model.StockSnapshotDTO{
		"P-1001": {ProductID: "P-1001", BranchID: "S002", Quantity: decimal.NewFromInt(80), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1002": {ProductID: "P-1002", BranchID: "S002", Quantity: decimal.NewFromInt(40), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		// "P-1003" 不在 S002,GetStock(S002, P-1003) → ErrStockNotFound
	}

	// 供应商 mock(对齐 cube supplier schema)
	c.suppliers["SUP-001"] = model.SupplierDTO{
		ID: "SUP-001", Name: "可口可乐华南", Type: "0",
		Contact: "张经理", Phone: "13800000001",
	}
	c.suppliers["SUP-002"] = model.SupplierDTO{
		ID: "SUP-002", Name: "农夫山泉代理", Type: "0",
		Contact: "李经理", Phone: "13800000002",
	}
	c.suppliers["SUP-101"] = model.SupplierDTO{
		ID: "SUP-101", Name: "鲜肉供应商", Type: "0",
		Contact: "王经理", Phone: "13800000003",
	}
}

// UpsertStock 测试 / 本地开发用,模拟 cube 库存变化。
//
// 在 service 单元测试中可调用以模拟"录入实盘后库存已被销售扣减"。
func (c *InMemoryClient) UpsertStock(branchID, productID string, qty decimal.Decimal, avgCost decimal.Decimal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stock[branchID] == nil {
		c.stock[branchID] = map[string]model.StockSnapshotDTO{}
	}
	c.stock[branchID][productID] = model.StockSnapshotDTO{
		ProductID: productID, BranchID: branchID,
		Quantity: qty, AvgCostYuan: avgCost, UpdatedAt: c.clock(),
	}
}

// GetProduct 按 item_no 查商品。
func (c *InMemoryClient) GetProduct(_ context.Context, productID string) (*model.ProductDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.products[productID]
	if !ok {
		return nil, fmt.Errorf("%w: product_id=%s", ErrProductNotFound, productID)
	}
	cp := p // 拷贝,避免外部修改
	return &cp, nil
}

// GetStock 按 (branch_id, product_id) 查库存快照。
//
// 跨店阻断:若 stock[branch_id][product_id] 不存在,返回 ErrStockNotFound。
func (c *InMemoryClient) GetStock(_ context.Context, branchID, productID string) (*model.StockSnapshotDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	br, ok := c.stock[branchID]
	if !ok {
		return nil, fmt.Errorf("%w: branch_id=%s", ErrStockNotFound, branchID)
	}
	s, ok := br[productID]
	if !ok {
		return nil, fmt.Errorf("%w: branch_id=%s product_id=%s", ErrStockNotFound, branchID, productID)
	}
	// UpdatedAt 用当前 clock(模拟"录入此刻从 cube 拉的快照")
	cp := s
	cp.UpdatedAt = c.clock()
	return &cp, nil
}

// SearchProductsByBarcode 按 barcode 查商品,合并该门店 stock 快照。
//
// 长度策略:
//   - <5 位:返回空(防全表扫)
//   - 5~12 位:后缀匹配,limit 由用户控制
//   - ≥13 位:精确匹配,limit 自动取 1
//
// 返回的 ProductWithStock.Stock 可能为 nil(该门店无 stock 数据,
// 但商品存在——例如新品刚上架 stock 维度还没建)。
func (c *InMemoryClient) SearchProductsByBarcode(_ context.Context, barcode, branchID string, limit int) ([]model.ProductWithStock, error) {
	if len(barcode) < 5 {
		return []model.ProductWithStock{}, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// 收集命中商品
	var ids []string
	if len(barcode) >= 13 {
		// 精确:遍历 products,Barcode 字段等于 barcode
		for id, p := range c.products {
			if p.Barcode == barcode {
				ids = append(ids, id)
				break // 精确匹配只取第一条
			}
		}
		limit = 1
	} else {
		// 后缀匹配:遍历全部 products,barcode (或 product.id) 后缀等于 barcode
		for id := range c.products {
			if endsWith(id, barcode) || endsWith(c.products[id].Barcode, barcode) {
				ids = append(ids, id)
			}
		}
		if limit > 0 && len(ids) > limit {
			ids = ids[:limit]
		}
	}

	out := make([]model.ProductWithStock, 0, len(ids))
	for _, id := range ids {
		p := c.products[id]
		ps := model.ProductWithStock{Product: &p}
		// 合并该门店 stock
		if branchID != "" {
			if br, ok := c.stock[branchID]; ok {
				if s, ok := br[id]; ok {
					cp := s
					cp.UpdatedAt = c.clock()
					ps.Stock = &cp
				}
			}
		}
		out = append(out, ps)
	}
	return out, nil
}

// SearchSuppliers 按 name 模糊查供应商(query 空 → 返回全部)。
//
// limit ≤ 0 表示不限。
func (c *InMemoryClient) SearchSuppliers(_ context.Context, query string, limit int) ([]model.SupplierDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]model.SupplierDTO, 0, len(c.suppliers))
	for _, s := range c.suppliers {
		if query != "" && !contains(s.Name, query) && !contains(s.ID, query) {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// endsWith s 是否以 suffix 结尾。
func endsWith(s, suffix string) bool {
	if len(suffix) > len(s) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

// contains s 是否包含 substr。
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}