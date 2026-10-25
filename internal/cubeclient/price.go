package cubeclient

// internal/cubeclient/price.go —— **批量查当前售价**(零售价 + 促销价)。
//
// # 为什么是"批量"而不是逐个查
//
// 鲜肉域的「预期毛利(预售)结转」要一次性拿到**一批 SKU** 的当前售价
// (一个部位挂多个 SKU、十几个部位 ⇒ 几十个 SKU)。
// 逐个查 = 几十次 cube 往返 ⇒ 单次审计要几十秒。
//
// ⇒ **两条查询**搞定:① product 取零售价;② promotion 取当前生效促销价。
// 促销价必须**单独查**:售价表里没有促销价,而促销是**时效数据**
// (与 internal/pricing/service/batch.go 的口径完全一致)。
//
// # 为什么走 cube 而不是 pricing 服务
//
// ① cube 是本仓读思迅的**唯一**通道,门店维度由 cube-router 按 X-Branch-ID 路由
//    (每个门店一个专属 cube 实例 ⇒ 拿到的就是**本店**的价);
// ② pricing 的 /prices 挂着 `pricing:view` 的**用户权限** ⇒ 门店操作员
//    没有这个 scope 时会 401,而这里要的是**服务级**的档案价,不依赖某个人的权限。
//
// # 促销价取最低那条
//
// 与 [DaprCubeClient.GetActivePromotion] 同口径:同一商品被多张促销单覆盖时,
// 取**促销价最低**的那条(与 pricing 服务的 min_promo_price 语义一致)。

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// OperatorInDateRange 是 cube 的**日期区间**算子。
//
// ⚠️ 对 datetime 列**不能**用 gte / lte:那会编译成字符串比较
// (RTRIM(col) >= ?),对 datetime 恒不匹配 ⇒ **返 0 行且不报错**。
// inDateRange 会 CAST 成 DATE 再比(实测:同一张表同一批数据,gte/lte 返 0 行、
// inDateRange 返 731 行)。
const OperatorInDateRange = "inDateRange"

// 促销时间窗的哨兵边界(够宽,不代表业务边界)。
const (
	promoWindowLower = "1900-01-01"
	promoWindowUpper = "2999-12-31"
)

// BatchCurrentPrices 批量查一批商品的**当前售价**(零售价 + 促销价)。
//
// 返回 map[product_id]ProductPriceQuote:
//   - **没有维护售价**的商品**不在返回里**(调用方据此区分"没维护价"与"0 元");
//   - 有商品但没有生效促销 ⇒ PromoPriceYuan = nil(不是 0)。
//
// onDate 是判定基准日("yyyy-mm-dd");留空 = 今天。
//
// ⚠️ productIDs 会被去重;空集合直接返空 map(**不发查询** —— cube 的 `in`
// 至少要一个值,空集合会 400)。
func (c *DaprCubeClient) BatchCurrentPrices(ctx context.Context, productIDs []string, onDate string) (map[string]ProductPriceQuote, error) {
	out := map[string]ProductPriceQuote{}
	ids := dedupeIDs(productIDs)
	if len(ids) == 0 {
		return out, nil
	}
	day := onDate
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	values := make([]any, 0, len(ids))
	for _, id := range ids {
		values = append(values, id)
	}

	// ---- ① 零售价 ----
	rows, _, err := c.LoadCubeQueryPage(ctx, "product", CubeQuery{
		Measures:   []string{"product.avg_price_yuan"},
		Dimensions: []string{"product.id"},
		Filters: []CubeFilter{
			{Member: "product.id", Operator: OperatorIn, Values: values},
		},
		Order: []CubeOrder{{ID: "product.id", Order: "asc"}},
		Limit: len(ids) + 8,
	})
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return out, nil // 该门店的 cube 没有 product model ⇒ 没有价(不是错误)
		}
		return nil, err
	}
	for _, raw := range rows {
		id := asStr(raw["product.id"])
		if id == "" {
			continue
		}
		price := asDecimalPtr(raw["product.avg_price_yuan"])
		if price == nil {
			continue // 没维护售价 ⇒ 不进结果(调用方要能区分"没有价"与"0 元")
		}
		out[id] = ProductPriceQuote{ProductID: id, PriceYuan: price}
	}

	// ---- ② 当前生效促销价 ----
	promoRows, _, err := c.LoadCubeQueryPage(ctx, "promotion", CubeQuery{
		// min_promo_price 是 measure —— cube 不给 measure 包 RTRIM,
		// 所以它是**真的 number**,不像维度那样以字符串回来。
		Measures: []string{"promotion.count", "promotion.min_promo_price"},
		Dimensions: []string{
			"promotion.item_no", "promotion.plan_no",
			"promotion.begin_day", "promotion.end_day",
		},
		Filters: []CubeFilter{
			{Member: "promotion.item_no", Operator: OperatorIn, Values: values},
			{Member: "promotion.begin_date", Operator: OperatorInDateRange,
				Values: []any{promoWindowLower, day}},
			{Member: "promotion.end_date", Operator: OperatorInDateRange,
				Values: []any{day, promoWindowUpper}},
		},
		Order: []CubeOrder{
			{ID: "promotion.item_no", Order: "asc"},
			{ID: "promotion.plan_no", Order: "asc"},
		},
		Limit: len(ids)*4 + 16,
	})
	if err != nil {
		if errors.Is(err, ErrCubeNotFound) {
			return out, nil // 没有 promotion model ⇒ 没有促销(绝大多数门店的常态)
		}
		return nil, err
	}
	for _, raw := range promoRows {
		id := asStr(raw["promotion.item_no"])
		if id == "" {
			continue
		}
		price := asDecimal(raw["promotion.min_promo_price"])
		// 促销价 <= 0 的单等于"没打折" ⇒ 不进候选
		// (否则一条 0 元促销会盖掉真正有效的促销,让价签印出 ¥0.00)。
		if price.LessThanOrEqual(decimal.Zero) {
			continue
		}
		q, ok := out[id]
		if !ok {
			// 该商品没维护零售价,但有促销价 ⇒ 仍然给(有价就是有价)。
			q = ProductPriceQuote{ProductID: id}
			out[id] = q
		}
		if q.PromoPriceYuan == nil || price.LessThan(*q.PromoPriceYuan) {
			p := price
			q.PromoPriceYuan = &p
			out[id] = q
		}
	}
	return out, nil
}

// dedupeIDs 去重并保持首次出现的顺序(确定性输出,便于断言)。
func dedupeIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
