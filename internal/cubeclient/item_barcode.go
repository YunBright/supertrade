package cubeclient

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ItemBarcodeDTO 是一行商品条码(思迅 `t_bd_item_barcode`)。
//
// # 它是什么
//
//	思迅的**商品条码不在商品主表上**:
//	  - `t_bd_item_info` 没有 barcode 列;
//	  - 条码在独立表 `t_bd_item_barcode` ⇒ **一品多码**。
//
// 实测(ysx 生产库,2026-10-22):**155 行 / 111 个商品**
// (每商品 1~7 条;孤儿条码 0 条)。
// ⇒ 绝大多数商品**没有独立条码**,它们的条码就是 `item_no` 本身。
//
// # 基数
//
//	条码 → 商品 = **1:1**(一个条码只对应一个货号)
//	商品 → 条码 = **1:N**
type ItemBarcodeDTO struct {
	// Barcode 是条码本身(cube `item_barcode.barcode`)。
	Barcode string
	// ProductID 是货号(cube `item_barcode.product_id`)= `product.id`。
	ProductID string
}

// itemBarcodeLimit 是"取商品条码全表"的上限。
//
// ⚠️ 155 行,给 500 就是**两个数量级**的余量 ——
//   上限给小了会**静默截断**(条码表只增不减,而超限的行再也查不到),
//   症状是"某个条码偶尔搜不到"且零报错。
const itemBarcodeLimit = 500

// itemBarcodeTTL 是商品条码快照的存活时间。
//
// ⚠️ 60 秒是刻意选的:
//   条码**几乎不变**(155 行,一天也涨不了几个),但它是"最需要当下"的那类数据
//   (新印的条码要立刻能扫)。
//   ⇒ 60 秒的窗口对"新条码要立刻生效"是够的,而缓存命中率接近 100%。
//
// ⚠️ 对比:cube 仓把 `item_barcode` 做成 **live** 模型的理由之一就是
//   "快照过期 = 条码是上次刷新时的条码"。
//   ⇒ 这里用**短 TTL** 换掉"每次查询都打源库",而不是把它变成一个长期快照。
const itemBarcodeTTL = 60 * time.Second

// itemBarcodeCache 是"商品条码全表"的短 TTL 快照。
//
// # 为什么需要它
//
//	cube **没有 OR 算子** ⇒ "货号/名称/助记码/条形码四种命中"必须拆开查再合并。
//	而条形码那张表**只有 155 行** ⇒ 与其每次查 cube,不如**缓存整表**,
//	在 Go 侧做子串匹配 ⇒ **绝大多数搜索 0 次额外 cube 查询**。
//
// # stale-on-error
//
//	拉取失败时**继续用旧快照**(不清空)——
//	与 cubesource.CachedResolver 的 stale-on-error 同款:
//	"拿不到新数据"不等于"没有数据"。
//
//	⚠️ 但**没有旧快照**时(首次就失败)⇒ 只能降级成"没有条形码命中",
//	这时**必须记日志** —— 静默降级会让"扫条码搜不到"变成一个没人知道的原因。
type itemBarcodeCache struct {
	mu        sync.RWMutex
	rows      []ItemBarcodeDTO
	fetchedAt time.Time
	ttl       time.Duration
	now       func() time.Time
}

// newItemBarcodeCache 构造缓存(now 可注入,便于测试)。
func newItemBarcodeCache(ttl time.Duration, now func() time.Time) *itemBarcodeCache {
	if ttl <= 0 {
		ttl = itemBarcodeTTL
	}
	if now == nil {
		now = time.Now
	}
	return &itemBarcodeCache{ttl: ttl, now: now}
}

// get 返回当前快照(可能为空)。
//
// 第二个返回值 = 快照是不是"新鲜的"(false = 过期了,调用方应当重新拉)。
func (c *itemBarcodeCache) get() ([]ItemBarcodeDTO, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.rows == nil {
		return nil, false
	}
	return c.rows, c.now().Sub(c.fetchedAt) < c.ttl
}

// put 写入一份新快照。
func (c *itemBarcodeCache) put(rows []ItemBarcodeDTO) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = rows
	c.fetchedAt = c.now()
}

// ListItemBarcodes 取商品条码全表(直查 cube 的 `item_barcode` 模型)。
//
// # 为什么是"全表"而不是"带 filter 查"
//
//	这张表**只有 155 行** ⇒ 全表拉一次的成本 ≈ 带 filter 查一次
//	(实测公网端到端 96ms vs 109ms,差异主要是往返不是查询)。
//	⇒ 全表可以**缓存**并复用(带 filter 的结果没法按关键词缓存),
//	而缓存命中时是 **0 次 cube 查询**。
//
// # cube 查询
//
//	{
//	  "measures":   ["item_barcode.count"],
//	  "dimensions": ["item_barcode.barcode","item_barcode.product_id"],
//	  "order":      [{"id":"item_barcode.barcode","order":"asc"}],
//	  "limit":      500,
//	  "total":      true
//	}
//
// ⚠️ `item_barcode` 是 **live** 模型 ⇒ SQL 直通源库(不走 DuckDB 快照)。
func (c *DaprCubeClient) ListItemBarcodes(ctx context.Context) ([]ItemBarcodeDTO, error) {
	rows, _, err := c.LoadCubeQueryPage(ctx, "item_barcode", CubeQuery{
		Measures:   []string{"item_barcode.count"},
		Dimensions: []string{"item_barcode.barcode", "item_barcode.product_id"},
		Order:      []CubeOrder{{ID: "item_barcode.barcode", Order: "asc"}},
		Limit:      itemBarcodeLimit,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ItemBarcodeDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, ItemBarcodeDTO{
			Barcode:   asStr(row["item_barcode.barcode"]),
			ProductID: asStr(row["item_barcode.product_id"]),
		})
	}
	return out, nil
}

// itemBarcodeSnapshot 返回商品条码快照(缓存优先;过期或缺失时拉一次)。
//
// ⚠️ 拉取失败**不返回错误**:条形码是**附加**命中方式,
//   它挂了不该让整个商品搜索失败 ⇒ 用旧快照继续(有旧快照时),
//   实在没有就降级成"没有条形码命中"并**记日志**。
func (c *DaprCubeClient) itemBarcodeSnapshot(ctx context.Context) []ItemBarcodeDTO {
	if rows, fresh := c.barcodes.get(); fresh {
		return rows
	}
	rows, err := c.ListItemBarcodes(ctx)
	if err != nil {
		// ⚠️ 拿不到**不等于**没有 ⇒ 先试旧快照(stale-on-error)。
		if stale, ok := c.barcodes.get(); ok {
			c.logger().Warn("catalog: 取商品条码快照失败,沿用旧快照",
				"err", err, "rows", len(stale))
			return stale
		}
		// 连旧快照都没有 ⇒ 只能降级。**必须记日志**:静默降级会让
		// "扫条码搜不到"变成一个没人知道的原因。
		c.logger().Warn("catalog: 取商品条码快照失败,本轮不按条形码命中",
			"err", err)
		return nil
	}
	c.barcodes.put(rows)
	return rows
}

// barcodeProductIDs 在条码快照里找出**包含关键词**的条码对应的货号集合。
//
// # 匹配口径:与 cube 的 `containsCI` 一致
//
//	cube 的 containsCI 编译成 `LOWER(RTRIM(col)) LIKE '%lower(v)%'`
//	⇒ 列侧 RTRIM、模式侧 lower ⇒ 这里做同样的事:
//	  `strings.Contains(lower(trim(barcode)), lower(trim(kw)))`。
//
// ⚠️ 口径必须**与 cube 一致**,否则会出现"同一关键词在本地路径命中、
//   在 cube 路径不命中"⇒ 两条路径的答案不一样(契约就废了)。
func barcodeProductIDs(rows []ItemBarcodeDTO, keyword string) []string {
	kw := strings.ToLower(strings.TrimSpace(keyword))
	if kw == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if r.ProductID == "" || seen[r.ProductID] {
			continue
		}
		b := strings.ToLower(strings.TrimSpace(r.Barcode))
		if b == "" || !strings.Contains(b, kw) {
			continue
		}
		seen[r.ProductID] = true
		out = append(out, r.ProductID)
	}
	return out
}

// toAnySlice 把 []string 转成 []any(cube 的 filter 值是 []any)。
func toAnySlice(v []string) []any {
	out := make([]any, 0, len(v))
	for _, s := range v {
		out = append(out, s)
	}
	return out
}

// logger 返回该 client 用的 logger(没设就用默认的)。
func (c *DaprCubeClient) logger() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}
