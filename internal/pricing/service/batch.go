package service

// 批量查价 / 批量查促销(遵循「有源走 cube、无源走本地」)。
//
// # 为什么不再有 /prices/local、/promotions/local
//
// 2026-10-22 决策:端点名字必须与**数据来源**一致。
//
//	旧端点名字里的 "local" 承诺"只读本域 PG",而判据却是"配没配数据源"
//	⇒ 名字与语义两套话:门店 00(配了源)在这两个端点上拿到的仍是
//	  **两天前的快照价**,而 /quotes 拿的是思迅实时价
//	  ⇒ "同一个门店,批量查和单个查的价不一样",现场无从判断哪个对。
//
// ⇒ 现在的口径:**判据只有一个**(internal/cubesource),与查价、导出、导入一致。
//
// # 形状不变
//
// 响应字段与旧的 local 端点**逐字相同**(pos-gateway / POS 不用改解析)。
//
// # ⚠️ 两条路径的**差异**只有一处:会员价
//
// 本域 PG 的 price_lists 有 member_price_yuan,而 cube 的 product model
// **没有会员价维度**(只有 avg/max/min_price_yuan 与 total_cost_yuan)
// ⇒ 有数据源的门店拿不到会员价,那个字段**不给**(而不是给 0 元)。
// 消费方据此区分"没有会员价"与"会员价 0 元"。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// BatchPrice 是批量查价的一行。
type BatchPrice struct {
	// ProductID 是商品号。
	ProductID string
	// PriceYuan 是零售价(字符串)。
	PriceYuan string
	// PromoPriceYuan 是当前生效促销价;nil = 没有生效促销。
	//
	// ⚠️ **零售价与促销价分开给**:成交价 = min(两者),由消费方决定
	// (POS 收银用 min;价签打印用零售价划线 + 促销价)。
	PromoPriceYuan *string
	// MemberPriceYuan 是会员价;nil = 没有会员价(不是"会员价 0 元")。
	MemberPriceYuan *string
	// CostYuan 是成本价;nil = 不知道成本。
	CostYuan *string
}

// BatchPromo 是批量查促销的一行。
type BatchPromo struct {
	ProductID string
	PlanNo    string
	PlanName  string
	// PriceYuan 是促销价(字符串)。
	PriceYuan string
	// OrigPriceYuan 是原价;nil = 没有原价。
	OrigPriceYuan *string
	BeginDay      string
	EndDay        string
}

// BatchPrices 批量查这些商品的**当前生效售价**。
//
// ⚠️ **没维护价的商品不在返回里**(它没有行)——
// 调用方据此区分"没维护售价"与"售价 0 元"。
//
// # ⚠️ 返回顺序 = 入参 ids 的顺序(去重后)
//
// map 迭代顺序是随机的 ⇒ 若直接遍历 map,同一个请求两次调用的行序会不同,
// 客户端拿两次结果做 diff 会看到"假差异"。⇒ 按 ids 顺序输出。
func (s *PriceService) BatchPrices(
	ctx context.Context, branchID string, ids []string, at time.Time,
) ([]BatchPrice, error) {
	ids = normalizeProductIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	_, useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		return s.batchPricesFromCube(ctx, ids, at)
	}
	return s.batchPricesFromLocal(ctx, branchID, ids, at)
}

// BatchPromotions 批量查这些商品**当前生效**的促销。
//
// ⚠️ 同一商品多个生效促销 ⇒ **只返促销价最低的那条**
// (与 cube promotion model 的 min_promo_price 语义一致)。
func (s *PriceService) BatchPromotions(
	ctx context.Context, branchID string, ids []string, at time.Time,
) ([]BatchPromo, error) {
	ids = normalizeProductIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	_, useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		return s.batchPromotionsFromCube(ctx, ids, at)
	}
	return s.batchPromotionsFromLocal(ctx, branchID, ids, at)
}

// batchPricesFromLocal 从本域 PG 取价 + 促销(自营店)。
//
// ⚠️ 促销要**单独查**:售价表里没有促销价,而促销是时效数据。
func (s *PriceService) batchPricesFromLocal(
	ctx context.Context, branchID string, ids []string, at time.Time,
) ([]BatchPrice, error) {
	if s.prices == nil || s.promos == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	prices, err := s.prices.ActivePrices(ctx, branchID, "", ids, at)
	if err != nil {
		return nil, err
	}
	promos, err := s.promos.ActivePromotions(ctx, branchID, ids, at)
	if err != nil {
		return nil, err
	}
	out := make([]BatchPrice, 0, len(prices))
	for _, id := range ids {
		p, ok := prices[id]
		if !ok {
			// 没维护价的商品不在返回里(它没有行)。
			continue
		}
		row := BatchPrice{ProductID: id, PriceYuan: p.PriceYuan.String()}
		if pr, ok := promos[id]; ok && !pr.PriceYuan.IsZero() {
			v := pr.PriceYuan.String()
			row.PromoPriceYuan = &v
		}
		if !p.MemberPriceYuan.IsZero() {
			v := p.MemberPriceYuan.String()
			row.MemberPriceYuan = &v
		}
		if !p.CostYuan.IsZero() {
			v := p.CostYuan.String()
			row.CostYuan = &v
		}
		out = append(out, row)
	}
	return out, nil
}

// batchPromotionsFromLocal 从本域 PG 取当前生效促销。
func (s *PriceService) batchPromotionsFromLocal(
	ctx context.Context, branchID string, ids []string, at time.Time,
) ([]BatchPromo, error) {
	if s.promos == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	promos, err := s.promos.ActivePromotions(ctx, branchID, ids, at)
	if err != nil {
		return nil, err
	}
	out := make([]BatchPromo, 0, len(promos))
	for _, id := range ids {
		p, ok := promos[id]
		if !ok {
			continue
		}
		row := BatchPromo{
			ProductID: id,
			PlanNo:    p.ID,
			PlanName:  p.PlanName,
			PriceYuan: p.PriceYuan.String(),
			BeginDay:  p.BeginAt.Format("2006-01-02"),
			EndDay:    p.EndAt.Format("2006-01-02"),
		}
		if !p.OrigPriceYuan.IsZero() {
			v := p.OrigPriceYuan.String()
			row.OrigPriceYuan = &v
		}
		out = append(out, row)
	}
	return out, nil
}

// batchPricesFromCube 从该门店的 cube 实例取价 + 促销(配了数据源时)。
//
// ⚠️ 用 **in** 过滤一次查完(不是 N 次单查):POS 一次扫一屏商品。
//
// ⚠️ **必须带 measure**:cube 拒收"没有 measure"的查询。
func (s *PriceService) batchPricesFromCube(
	ctx context.Context, ids []string, at time.Time,
) ([]BatchPrice, error) {
	if s.reader == nil {
		return nil, fmt.Errorf("%w: cube 读端未装配", model.ErrCubeSourceUnavailable)
	}
	q := cubeclient.CubeQuery{
		Measures:   []string{"product.avg_price_yuan", "product.total_cost_yuan"},
		Dimensions: []string{"product.id"},
		Filters: []cubeclient.CubeFilter{
			{Member: "product.id", Operator: cubeOperatorIn, Values: anyValues(ids)},
		},
		// ⚠️ order 的 id 必须同时在 SELECT 里。
		Order: []cubeclient.CubeOrder{{ID: "product.id", Order: "asc"}},
		Limit: len(ids) + 8,
		Total: true,
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "product", q)
	if err != nil {
		return nil, err
	}
	// 促销要**单独查**:售价表里没有促销价,而促销是时效数据。
	promos, err := s.batchPromotionsFromCube(ctx, ids, at)
	if err != nil {
		return nil, err
	}
	lowest := make(map[string]BatchPromo, len(promos))
	for _, p := range promos {
		if cur, ok := lowest[p.ProductID]; !ok || promoPriceLess(p.PriceYuan, cur.PriceYuan) {
			lowest[p.ProductID] = p
		}
	}
	out := make([]BatchPrice, 0, len(rows))
	for _, raw := range rows {
		id := exportTextString(raw["product.id"])
		if id == "" {
			continue
		}
		// ⚠️ **没维护价的商品不在返回里**:
		// avg_price_yuan 为 null ⇒ 这个商品没有价(与本地路径"没有行"同义)。
		price, ok := exportAmountValue(raw["product.avg_price_yuan"]).(string)
		if !ok || price == "" {
			continue
		}
		row := BatchPrice{ProductID: id, PriceYuan: price}
		if v, ok := exportAmountValue(raw["product.total_cost_yuan"]).(string); ok && v != "" {
			row.CostYuan = &v
		}
		if p, ok := lowest[id]; ok {
			v := p.PriceYuan
			row.PromoPriceYuan = &v
		}
		out = append(out, row)
	}
	return out, nil
}

// batchPromotionsFromCube 从该门店的 cube 实例取当前生效促销。
//
// ⚠️ 时间窗用 **inDateRange**(不是 gte/lte):后者编译成字符串比较,
// 对 datetime 恒不匹配 ⇒ 返 0 行**且不报错**。
func (s *PriceService) batchPromotionsFromCube(
	ctx context.Context, ids []string, at time.Time,
) ([]BatchPromo, error) {
	if s.reader == nil {
		return nil, fmt.Errorf("%w: cube 读端未装配", model.ErrCubeSourceUnavailable)
	}
	day := at.Format("2006-01-02")
	dims := []string{
		"promotion.item_no", "promotion.plan_no", "promotion.plan_name",
		"promotion.promo_price", "promotion.orig_price",
		"promotion.begin_day", "promotion.end_day",
	}
	order := make([]cubeclient.CubeOrder, 0, len(dims))
	for _, d := range dims {
		order = append(order, cubeclient.CubeOrder{ID: d, Order: "asc"})
	}
	q := cubeclient.CubeQuery{
		Measures:   []string{"promotion.count"},
		Dimensions: dims,
		Filters: []cubeclient.CubeFilter{
			{Member: "promotion.item_no", Operator: cubeOperatorIn, Values: anyValues(ids)},
			{Member: "promotion.begin_date", Operator: cubeOperatorInDateRange,
				Values: []any{exportWindowLower, day}},
			{Member: "promotion.end_date", Operator: cubeOperatorInDateRange,
				Values: []any{day, exportWindowUpper}},
		},
		Order: order,
		Limit: len(ids)*4 + 16,
		Total: true,
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "promotion", q)
	if err != nil {
		return nil, err
	}
	out := make([]BatchPromo, 0, len(rows))
	for _, raw := range rows {
		id := exportTextString(raw["promotion.item_no"])
		if id == "" {
			continue
		}
		row := BatchPromo{
			ProductID: id,
			PlanNo:    exportTextString(raw["promotion.plan_no"]),
			PlanName:  exportTextString(raw["promotion.plan_name"]),
			PriceYuan: cubeclient.ParseDecimal(raw["promotion.promo_price"]).String(),
			BeginDay:  exportTextString(raw["promotion.begin_day"]),
			EndDay:    exportTextString(raw["promotion.end_day"]),
		}
		if v, ok := exportAmountValue(raw["promotion.orig_price"]).(string); ok && v != "" {
			row.OrigPriceYuan = &v
		}
		out = append(out, row)
	}
	return out, nil
}

// cubeOperatorIn 是 cube 的「in」算子(cube 侧 build.go 支持)。
const cubeOperatorIn = "in"

// normalizeProductIDs 去空去重(保持入参顺序)。
func normalizeProductIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// anyValues 把字符串列表转成 cube filter 的值列表。
func anyValues(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// promoPriceLess 比较两个促销价字符串(小者优先)。
//
// ⚠️ 促销价是**十进制金额字符串** ⇒ 必须按数值比,不能按字符串比
// ("9.90" > "10.00" 在字符串比较里成立 ⇒ 会选错促销单)。
func promoPriceLess(a, b string) bool {
	return cubeclient.ParseDecimal(a).LessThan(cubeclient.ParseDecimal(b))
}
