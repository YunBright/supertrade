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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// Client 是测试替身,实现 cubeclient.Client。
type Client struct {
	mu         sync.RWMutex
	products   map[string]cubeclient.ProductDTO
	stock      map[string]map[string]cubeclient.StockSnapshotDTO // branch_id → product_id → snapshot
	suppliers  map[string]cubeclient.SupplierDTO
	categories map[string]cubeclient.CategoryDTO
	// saleWays 是「销售方式」字典(思迅 t_bd_base_code,type_no='SW')。
	//
	// 默认给**生产实测的那 6 行**(A 购销 / B 代销 / C 联营 / D 租赁 /
	// E 自产 / F 扣率代销)—— 这不是编的,是两个生产库里的真实字典。
	// 取值来源见 sixun-models/sale_way/schema.yaml。
	saleWays map[string]cubeclient.SaleWayDTO
	promotions []cubeclient.PromotionDTO
	clock      func() time.Time
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
		products:   make(map[string]cubeclient.ProductDTO),
		stock:      make(map[string]map[string]cubeclient.StockSnapshotDTO),
		suppliers:  make(map[string]cubeclient.SupplierDTO),
		categories: make(map[string]cubeclient.CategoryDTO),
		saleWays:   make(map[string]cubeclient.SaleWayDTO),
		promotions: nil,
		clock:      func() time.Time { return time.Now().UTC() },
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
		Contact: "张经理", Phone: "13800000001", SupplyWay: "A",
	}
	c.suppliers["SUP-002"] = cubeclient.SupplierDTO{
		ID: "SUP-002", Name: "农夫山泉代理", Type: "0",
		Contact: "李经理", Phone: "13800000002", SupplyWay: "C",
	}
	c.suppliers["SUP-101"] = cubeclient.SupplierDTO{
		ID: "SUP-101", Name: "鲜肉供应商", Type: "0",
		Contact: "王经理", Phone: "13800000003", SupplyWay: "A",
	}
	// SUP-201 是 P-3001(大白菜)的供应商 —— 必须存在,否则 seed 数据自相矛盾
	//(商品指向一个字典里没有的供应商,join 出来是 null)。
	//
	// ⚠️ 它刻意**没有**销售方式(SUP-201 的 SupplyWay 是空):
	// 实测生产里 sale_way 全都有值,但"某个供应商没有类别"这条降级路径
	// 仍然必须被走到 —— 症状是它落进"未分类"组,而不是被丢掉。
	c.suppliers["SUP-201"] = cubeclient.SupplierDTO{
		ID: "SUP-201", Name: "蔬菜供应商", Type: "0",
		Contact: "赵经理", Phone: "13800000004",
	}

	// 「销售方式」字典:生产实测的那 6 行(思迅 t_bd_base_code,type_no='SW')。
	// 两个生产库内容一致;取值见 sixun-models/sale_way/schema.yaml。
	c.saleWays["A"] = cubeclient.SaleWayDTO{ID: "A", Name: "购销"}
	c.saleWays["B"] = cubeclient.SaleWayDTO{ID: "B", Name: "代销"}
	c.saleWays["C"] = cubeclient.SaleWayDTO{ID: "C", Name: "联营"}
	c.saleWays["D"] = cubeclient.SaleWayDTO{ID: "D", Name: "租赁"}
	c.saleWays["E"] = cubeclient.SaleWayDTO{ID: "E", Name: "自产"}
	c.saleWays["F"] = cubeclient.SaleWayDTO{ID: "F", Name: "扣率代销"}

	// 分类字典。真实实现从 cube 的 category model 取(有真名字);
	// fake 给的是可读名字,这样"join 出来的是不是一个占位名"这类断言才有意义。
	//
	// ⚠️ 这里给的是**树**,不是平铺列表(2026-10-20):
	//
	//	CAT-01 饮料            (顶级)
	//	CAT-02 生鲜            (顶级)
	//	  └ CAT-03 生鲜蔬果     (CAT-02 的子节点)
	//
	// 分类改成树之后,"按 CAT-02 看商品"的语义是「CAT-02 + CAT-03」——
	// fake 必须能表达这个,否则子树展开那条路径在单测里等于没覆盖。
	c.categories["CAT-01"] = cubeclient.CategoryDTO{ID: "CAT-01", Name: "饮料"}
	c.categories["CAT-02"] = cubeclient.CategoryDTO{ID: "CAT-02", Name: "生鲜"}
	c.categories["CAT-03"] = cubeclient.CategoryDTO{
		ID: "CAT-03", Name: "生鲜蔬果", ParentID: "CAT-02",
	}

	// 促销 seed。日期锚在 seed 基准日 2026-09-17 前后,让"生效 / 已过期 /
	// 未开始"三种状态都有样本,而不必依赖运行时的今天。
	//
	// P-1001 有**两条**同期生效的促销(11.00 与 9.90),用来锁"取最低价"这条规则;
	// P-1002 只有一条已过期的(2026-08-01~2026-08-31),用来锁"过期不算生效";
	// 其余商品无促销 —— 这是绝大多数商品的常态。
	p1001Orig := decimal.NewFromFloat(3.5)
	p1002Orig := decimal.NewFromFloat(3.5)
	c.promotions = []cubeclient.PromotionDTO{
		{
			ItemNo: "P-1001", PlanNo: "PS-001", PlanName: "可口可乐促销 A",
			Price: decimal.NewFromFloat(11.00), OrigPrice: &p1001Orig,
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
		{
			ItemNo: "P-1001", PlanNo: "PS-002", PlanName: "可口可乐促销 B",
			Price: decimal.NewFromFloat(9.90), OrigPrice: &p1001Orig,
			BeginDay: "2026-09-01", EndDay: "2026-09-30",
		},
		{
			ItemNo: "P-1002", PlanNo: "PS-003", PlanName: "雪碧过期促销",
			Price: decimal.NewFromFloat(2.50), OrigPrice: &p1002Orig,
			BeginDay: "2026-08-01", EndDay: "2026-08-31",
		},
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

// UpsertProductCategory 改某商品的分类 id(测试用)。
//
// 用来造"商品的分类在字典里查不到"这种场景 —— 生产里 join 不到名字时
// 必须给 null,而不是编一个"分类-<id>"占位名。
func (c *Client) UpsertProductCategory(productID, categoryID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.products[productID]
	if !ok {
		return
	}
	p.CategoryID = categoryID
	c.products[productID] = p
}

// UpsertPromotions 覆盖某商品的促销集合(测试用)。
//
// 传空切片表示"该商品没有任何促销"。
func (c *Client) UpsertPromotions(itemNo string, promos ...cubeclient.PromotionDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := make([]cubeclient.PromotionDTO, 0, len(c.promotions))
	for _, p := range c.promotions {
		if p.ItemNo != itemNo {
			kept = append(kept, p)
		}
	}
	for i := range promos {
		p := promos[i]
		p.ItemNo = itemNo
		kept = append(kept, p)
	}
	c.promotions = kept
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

// GetActivePromotion 与真实实现同语义:活动期**包含** onDate 且促销价 > 0,
// 多单覆盖取**最低价**那一条;没有则返 (nil, nil) 而不是 error。
func (c *Client) GetActivePromotion(_ context.Context, itemNo, onDate string) (*cubeclient.PromotionDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	day := onDate
	if day == "" {
		day = c.clock().Format("2006-01-02")
	}
	var best *cubeclient.PromotionDTO
	for _, p := range c.promotions {
		if p.ItemNo != itemNo {
			continue
		}
		if p.BeginDay > day || p.EndDay < day {
			continue
		}
		if p.Price.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if best == nil || p.Price.LessThan(best.Price) {
			cp := p
			best = &cp
		}
	}
	return best, nil
}

// ---- 商品选择控件(ProductPicker)用的只读查询 ----
//
// 语义与真实实现(cubeclient/catalog.go)对齐:
//
//   - 排序按 id 升序(product.id 是主键 ⇒ 全序,分页不会重复/漏行)
//   - 商品**编码**与**名称**的关键词匹配都是**大小写无关**
//     (cube 的 containsCI:列与模式都转小写再比)
//   - total 是"过滤后的总条数",不是本页条数

// ListProducts 分页查商品。
func (c *Client) ListProducts(_ context.Context, q cubeclient.ProductQuery) (*cubeclient.ProductPageDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	matched := c.matchProducts(q)
	return &cubeclient.ProductPageDTO{
		Rows:  sliceProducts(matched, q.Offset, q.Limit),
		Total: len(matched),
	}, nil
}

// SearchProducts 按关键词查商品(编码命中 + 名称命中,按 id 去重合并)。
func (c *Client) SearchProducts(_ context.Context, q cubeclient.ProductQuery) (*cubeclient.ProductPageDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	kw := strings.TrimSpace(q.Keyword)
	if kw == "" {
		matched := c.matchProducts(q)
		return &cubeclient.ProductPageDTO{
			Rows:  sliceProducts(matched, q.Offset, q.Limit),
			Total: len(matched),
		}, nil
	}

	lower := strings.ToLower(kw)
	seen := map[string]bool{}
	var merged []cubeclient.ProductDTO
	for _, p := range c.matchProducts(q) {
		// 编码与名称都是**大小写无关**匹配(对齐 cube 的 containsCI)。
		hit := strings.Contains(strings.ToLower(p.ID), lower) ||
			strings.Contains(strings.ToLower(p.Name), lower)
		if !hit || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		merged = append(merged, p)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return &cubeclient.ProductPageDTO{
		Rows:  sliceProducts(merged, q.Offset, q.Limit),
		Total: len(merged),
	}, nil
}

// ListCategories 取分类字典。
func (c *Client) ListCategories(_ context.Context) (*cubeclient.CategoryListDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]cubeclient.CategoryDTO, 0, len(c.categories))
	for _, cat := range c.categories {
		out = append(out, cat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &cubeclient.CategoryListDTO{Rows: out, Total: len(out)}, nil
}

// ListSuppliers 取供应商字典。
func (c *Client) ListSuppliers(_ context.Context) (*cubeclient.SupplierListDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]cubeclient.SupplierDTO, 0, len(c.suppliers))
	for _, s := range c.suppliers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &cubeclient.SupplierListDTO{Rows: out, Total: len(out)}, nil
}

// ListSaleWays 取「销售方式」字典。
//
// fake 默认给**生产实测的那 6 行**(A 购销 / B 代销 / C 联营 / D 租赁 /
// E 自产 / F 扣率代销)—— 不是编的,是两个生产库 t_bd_base_code 里
// type_no='SW' 的真实字典。
func (c *Client) ListSaleWays(_ context.Context) (*cubeclient.SaleWayListDTO, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]cubeclient.SaleWayDTO, 0, len(c.saleWays))
	for _, t := range c.saleWays {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &cubeclient.SaleWayListDTO{Rows: out, Total: len(out)}, nil
}

// SetSupplier 覆盖/新增一个供应商(测试用来构造特定场景)。
func (c *Client) SetSupplier(s cubeclient.SupplierDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suppliers[s.ID] = s
}

// SetSaleWays 覆盖整个「销售方式」字典(测试用来构造"字典为空"或
// "字典里没有某个代码"这两种降级场景)。
func (c *Client) SetSaleWays(rows []cubeclient.SaleWayDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saleWays = make(map[string]cubeclient.SaleWayDTO, len(rows))
	for _, r := range rows {
		c.saleWays[r.ID] = r
	}
}

// CountProductsByCategory 返回「每个分类下的商品数」。
func (c *Client) CountProductsByCategory(_ context.Context) (map[string]int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := map[string]int{}
	for _, p := range c.products {
		if p.CategoryID == "" {
			continue
		}
		out[p.CategoryID]++
	}
	return out, nil
}

// CountProductsBySupplier 返回「每个供应商下的商品数」。
func (c *Client) CountProductsBySupplier(_ context.Context) (map[string]int, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := map[string]int{}
	for _, p := range c.products {
		if p.SupplierID == "" {
			continue
		}
		out[p.SupplierID]++
	}
	return out, nil
}

// matchProducts 返回过滤后的商品(按 id 升序)。
//
// # ⚠️ 这里必须与真实实现(cubeclient/catalog.go 的 productPageQuery)**逐条对齐**
//
// fake 最大的风险不是"数据是假的",而是**语义分叉**:真实实现改了而 fake 没跟着改,
// 于是单测全绿、生产是错的。2026-10-20 就踩过一次:
// 真实实现把"空集合"从"不过滤"改成"命中 0 行"(供应商类别展开成空集),
// fake 还在按"不过滤"处理 —— 症状是服务层单测说 total=5、
// 生产上那个端点返回的是全店 27565 个商品。
//
// 三条要对齐的规则:
//
//  1. **分类**:CategoryIDs 优先(集合成员判断),否则 CategoryID 精确等于;
//  2. **供应商**:SupplierIDs(集合成员)**与** SupplierID(精确等于)是 **AND**,
//     两者都给就取交集(真实实现是两个独立的 if,不是 switch);
//  3. **nil 与空切片语义不同**:nil = 不过滤;空切片 = 按空集合过滤 ⇒ 命中 0 行。
//
// 调用方必须已持有读锁。
func (c *Client) matchProducts(q cubeclient.ProductQuery) []cubeclient.ProductDTO {
	catSet := make(map[string]bool, len(q.CategoryIDs))
	for _, id := range q.CategoryIDs {
		if id != "" {
			catSet[id] = true
		}
	}
	supSet := make(map[string]bool, len(q.SupplierIDs))
	for _, id := range q.SupplierIDs {
		if id != "" {
			supSet[id] = true
		}
	}
	out := make([]cubeclient.ProductDTO, 0, len(c.products))
	for _, p := range c.products {
		// 分类:switch(CategoryIDs 优先)。
		switch {
		case q.CategoryIDs != nil:
			if !catSet[p.CategoryID] {
				continue
			}
		case q.CategoryID != "":
			if p.CategoryID != q.CategoryID {
				continue
			}
		}
		// 供应商:两个过滤**都生效**(交集),不是二选一。
		if q.SupplierIDs != nil && !supSet[p.SupplierID] {
			continue
		}
		if q.SupplierID != "" && p.SupplierID != q.SupplierID {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sliceProducts 切出 [offset, offset+limit) 这一段(越界安全)。
func sliceProducts(rows []cubeclient.ProductDTO, offset, limit int) []cubeclient.ProductDTO {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []cubeclient.ProductDTO{}
	}
	end := len(rows)
	if limit > 0 && offset+limit < len(rows) {
		end = offset + limit
	}
	return rows[offset:end]
}
