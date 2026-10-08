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
	c.products["P-1001"] = cubeclient.ProductDTO{
		ID: "P-1001", Name: "可口可乐 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567890",
	}
	c.products["P-1002"] = cubeclient.ProductDTO{
		ID: "P-1002", Name: "雪碧 330ml", CategoryID: "CAT-01",
		SupplierID: "SUP-001", Unit: "瓶", Spec: "330ml",
		Status: "active", Barcode: "6901234567891",
	}
	c.products["P-1003"] = cubeclient.ProductDTO{
		ID: "P-1003", Name: "农夫山泉 550ml", CategoryID: "CAT-01",
		SupplierID: "SUP-002", Unit: "瓶", Spec: "550ml",
		Status: "active", Barcode: "6901234567892",
	}
	c.products["P-2001"] = cubeclient.ProductDTO{
		ID: "P-2001", Name: "五花肉", CategoryID: "CAT-02",
		SupplierID: "SUP-101", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "2001",
	}
	c.products["P-3001"] = cubeclient.ProductDTO{
		ID: "P-3001", Name: "大白菜", CategoryID: "CAT-03",
		SupplierID: "SUP-201", Unit: "kg", Spec: "新鲜",
		Status: "active", Barcode: "3001",
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