// Package cubeclientfake 提供 cubeclient.Client 的测试替身。
//
// ⚠️ 本包**只允许被 _test.go 引用**。
//
// 它原先以 cubeclient.InMemoryClient 的身份住在生产包 internal/cubeclient 里,
// 并可通过 CUBE_CLIENT_MODE=memory 在运行时被选中 —— 这造成两个真实问题:
//
//  1. 生产事故:catalog / inventory / fresh-meat 三个服务的 systemd unit 没有设
//     CUBE_CLIENT_MODE,而 NewClientFromEnv 的默认值是 "memory",
//     于是它们在生产**读的是下面的假数据**,不是真实 cube,且没有任何报错。
//  2. mock 与真实实现同包,任何 `NewInMemoryClient()` 调用都能通过编译进入生产二进制,
//     review 时难以区分"这是 mock"还是"这是实现"。
//
// 现在生产路径只剩 DaprCubeClient 一个实现,fake 只活在测试里。
// 换掉运行期开关 = 不可能再把假数据接进线上。
//
// 语义对齐:branchID 表示"哪一家门店的数据集",对应生产里 cube-router 按
// branch_cube_sources 选出的那一个专属 cube 实例 —— 不是思迅 branch_no。
package cubeclientfake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// Client 是测试替身,实现 cubeclient.Client。
type Client struct {
	mu        sync.RWMutex
	products  map[string]cubeclient.ProductDTO
	stock     map[string]map[string]cubeclient.StockSnapshotDTO // branch_id → product_id → snapshot
	suppliers map[string]cubeclient.SupplierDTO
	clock     func() time.Time
}

// 编译期确认实现完整接口。
var _ cubeclient.Client = (*Client)(nil)

// New 构造带默认 seed 数据的 fake(5 个商品 + 2 个门店库存 + 3 个供应商)。
//
// 默认商品:
//
//	P-1001 可口可乐 330ml   (S001=100 / S002=80)
//	P-1002 雪碧 330ml        (S001=50  / S002=40)
//	P-1003 农夫山泉 550ml    (S001=200 / S002 无 → 跨店阻断用例)
//	P-2001 五花肉(生鲜)      (S001=15.5)
//	P-3001 大白菜(生鲜蔬果)  (S001=50)
//
// 默认供应商:
//
//	SUP-001 可口可乐华南 / SUP-002 农夫山泉代理 / SUP-101 鲜肉供应商
func New() *Client {
	c := &Client{
		products:  make(map[string]cubeclient.ProductDTO),
		stock:     make(map[string]map[string]cubeclient.StockSnapshotDTO),
		suppliers: make(map[string]cubeclient.SupplierDTO),
		clock:     func() time.Time { return time.Now().UTC() },
	}
	c.seed()
	return c
}

// SetClock 注入时间;传 nil 恢复 time.Now().UTC()。
func (c *Client) SetClock(fn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		fn = func() time.Time { return time.Now().UTC() }
	}
	c.clock = fn
}

func (c *Client) seed() {
	now := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	// 2026-10-08:seed 补 PriceYuan。以前 fake 不带售价,而 SearchProductRow.Price
	// 直接取 r.Product.PriceYuan —— 不 seed 就让所有"售价"断言永远拿不到非 nil,
	// 等于这条路径没有任何测试覆盖。
	p1001 := decimal.NewFromFloat(3.5)
	p1002 := decimal.NewFromFloat(3.5)
	p1003 := decimal.NewFromFloat(2.0)
	p2001 := decimal.NewFromFloat(42.0)
	p3001 := decimal.NewFromFloat(4.5)
	c.products["P-1001"] = cubeclient.ProductDTO{
		ID: "P-1001", Name: "可口可乐 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567890", PriceYuan: &p1001,
	}
	c.products["P-1002"] = cubeclient.ProductDTO{
		ID: "P-1002", Name: "雪碧 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567891", PriceYuan: &p1002,
	}
	c.products["P-1003"] = cubeclient.ProductDTO{
		ID: "P-1003", Name: "农夫山泉 550ml", CategoryID: "CAT-01",
		SupplierID: "SUP-002", Unit: "瓶", Spec: "550ml",
		Status: "active", Barcode: "6901234567892", PriceYuan: &p1003,
	}
	c.products["P-2001"] = cubeclient.ProductDTO{
		ID: "P-2001", Name: "五花肉", CategoryID: "CAT-02",
		SupplierID: "SUP-101", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "2001", PriceYuan: &p2001,
	}
	c.products["P-3001"] = cubeclient.ProductDTO{
		ID: "P-3001", Name: "大白菜", CategoryID: "CAT-03",
		SupplierID: "SUP-201", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "3001", PriceYuan: &p3001,
	}

	c.stock["S001"] = map[string]cubeclient.StockSnapshotDTO{
		"P-1001": {ProductID: "P-1001", BranchID: "S001", Quantity: decimal.NewFromInt(100), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1002": {ProductID: "P-1002", BranchID: "S001", Quantity: decimal.NewFromInt(50), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1003": {ProductID: "P-1003", BranchID: "S001", Quantity: decimal.NewFromInt(200), AvgCostYuan: decimal.NewFromFloat(1.8), UpdatedAt: now},
		"P-2001": {ProductID: "P-2001", BranchID: "S001", Quantity: decimal.NewFromFloat(15.5), AvgCostYuan: decimal.NewFromFloat(38.0), UpdatedAt: now},
		"P-3001": {ProductID: "P-3001", BranchID: "S001", Quantity: decimal.NewFromFloat(50.0), AvgCostYuan: decimal.NewFromFloat(3.5), UpdatedAt: now},
	}
	// S002 不含 P-1003 —— GetStock(S002, P-1003) → ErrStockNotFound(跨店阻断用例)
	c.stock["S002"] = map[string]cubeclient.StockSnapshotDTO{
		"P-1001": {ProductID: "P-1001", BranchID: "S002", Quantity: decimal.NewFromInt(80), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
		"P-1002": {ProductID: "P-1002", BranchID: "S002", Quantity: decimal.NewFromInt(40), AvgCostYuan: decimal.NewFromFloat(2.5), UpdatedAt: now},
	}

	c.suppliers["SUP-001"] = cubeclient.SupplierDTO{
		ID: "SUP-001", Name: "可口可乐华南", Type: "0",
		Contact: "张经理", Phone: "13800000001",
	}
	c.suppliers["SUP-002"] = cubeclient.SupplierDTO{
		ID: "SUP-002", Name: "农夫山泉代理", Type: "0",
		Contact: "李经理", Phone: "13800000002",
	}
	c.suppliers["SUP-101"] = cubeclient.SupplierDTO{
		ID: "SUP-101", Name: "鲜肉供应商", Type: "0",
		Contact: "王经理", Phone: "13800000003",
	}
}

// UpsertStock 模拟库存变化(测试用)。
func (c *Client) UpsertStock(branchID, productID string, qty, avgCost decimal.Decimal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stock[branchID] == nil {
		c.stock[branchID] = map[string]cubeclient.StockSnapshotDTO{}
	}
	c.stock[branchID][productID] = cubeclient.StockSnapshotDTO{
		ProductID: productID, BranchID: branchID,
		Quantity: qty, AvgCostYuan: avgCost, UpdatedAt: c.clock(),
	}
}

// UpsertProductPrice 设置/清除某商品的售价(测试用)。
//
// 传 nil 模拟"该商品没维护售价" —— 这是与"售价为 0 元"必须区分的状态:
// 生产里 DaprCubeClient 用 asDecimalPtr,缺列时给 nil;前端据此显示
// 「未维护」而不是「¥ 0.00」。
func (c *Client) UpsertProductPrice(productID string, price *decimal.Decimal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.products[productID]
	if !ok {
		return
	}
	p.PriceYuan = price
	c.products[productID] = p
}

func (c *Client) GetProduct(_ context.Context, productID string) (*cubeclient.ProductDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.products[productID]
	if !ok {
		return nil, fmt.Errorf("%w: product_id=%s", cubeclient.ErrProductNotFound, productID)
	}
	cp := p // 拷贝,避免调用方改动内部状态
	return &cp, nil
}

func (c *Client) GetStock(_ context.Context, branchID, productID string) (*cubeclient.StockSnapshotDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	br, ok := c.stock[branchID]
	if !ok {
		return nil, fmt.Errorf("%w: branch_id=%s", cubeclient.ErrStockNotFound, branchID)
	}
	s, ok := br[productID]
	if !ok {
		return nil, fmt.Errorf("%w: branch_id=%s product_id=%s", cubeclient.ErrStockNotFound, branchID, productID)
	}
	cp := s
	cp.UpdatedAt = c.clock()
	return &cp, nil
}

// SearchProductsByBarcode 与真实实现同策略:
// <5 位返空;5~12 位后缀匹配并尊重 limit;≥13 位精确匹配且只取 1 条。
func (c *Client) SearchProductsByBarcode(_ context.Context, barcode, branchID string, limit int) ([]cubeclient.ProductWithStock, error) {
	if len(barcode) < 5 {
		return []cubeclient.ProductWithStock{}, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	var ids []string
	if len(barcode) >= 13 {
		for id, p := range c.products {
			if p.Barcode == barcode {
				ids = append(ids, id)
				break
			}
		}
		limit = 1
	} else {
		for id := range c.products {
			if strings.HasSuffix(id, barcode) || strings.HasSuffix(c.products[id].Barcode, barcode) {
				ids = append(ids, id)
			}
		}
		if limit > 0 && len(ids) > limit {
			ids = ids[:limit]
		}
	}

	out := make([]cubeclient.ProductWithStock, 0, len(ids))
	for _, id := range ids {
		p := c.products[id]
		ps := cubeclient.ProductWithStock{Product: &p}
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

func (c *Client) SearchSuppliers(_ context.Context, query string, limit int) ([]cubeclient.SupplierDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]cubeclient.SupplierDTO, 0, len(c.suppliers))
	for _, s := range c.suppliers {
		if query != "" && !strings.Contains(s.Name, query) && !strings.Contains(s.ID, query) {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}
