// Package service / export.go —— 价格域的导出(两个出口,数据来源同一条判据)。
//
// # 两个出口
//
//	GET /archive/export   归档导出契约(archivedto.ExportPage)—— 编排方(cubesync)用
//	GET /prices/export    离线价格包(POS 用)—— 形状**不许变**,只换数据来源
//
// # 数据来源:与查价**同一套判据**
//
//	门店配了数据源 ⇒ cube(实时)
//	没配数据源       ⇒ 本域 PG
//
// ⚠️ **配了源但源里没有 ⇒ 导出空数据,不许回落本地**:
// 回落 = 把"源库没有这个商品"说成"本域有旧数据",而真相是"那边没有"。
//
// # cube 侧的取数口径与 cube-router 逐字对应
//
// 同一套 model / 维度 / 度量 / 时间窗(见 cubeExportScopes 的注释)——
// 两边各写一份时,改了一边不会编译报错,症状是"导出了 N 条、导入后某个字段恒为空",
// 而且**没有一行报错**。
//
// # ⚠️ cube 的硬约束(踩过的坑)
//
//	必须带 measure      —— cube 拒收"没有 measure"的查询(QUERY_INVALID)
//	order 的 id 必须在 SELECT 的 dimensions 里 —— 否则 GROUP BY 查询报 not a grouped column
//	offset>0 必须给 order —— 否则 cube 直接 400(分页必须建立在稳定排序上)
//	要总数必须 total:true —— 不给时响应里的 total 恒为 0(那是"没问",不是"总共 0 条")
//	排序列会并列 ⇒ order 末尾追加 id 兜底 —— 没有兜底键就不是全序,翻页会重复/漏行
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// ExportDefaultLimit 是导出端点默认的每页行数。
//
// ⚠️ 比 cube-router 的 5000 小:pricing 走的是 cubeclient 的**同一个** client,
// 而它的单次调用超时是 10s(cube-router 自己的导出用 120s)。
// ⇒ 页太大就可能超时 ⇒ 503。宁可多几次往返,也不要整包失败。
const ExportDefaultLimit = 1000

// ExportMaxLimit 是导出端点的每页行数上限。
const ExportMaxLimit = 5000

// cubeExportPageSize 是"导全量"时单次向 cube 要的行数。
//
// ⚠️ 与 ExportDefaultLimit 同量级(理由同上:cubeclient 单次调用 10s 超时)。
const cubeExportPageSize = 1000

// cubeOperatorInDateRange 是 cube 的日期区间算子。
//
// ⚠️ **必须用 inDateRange,不能用 gte / lte** ——
// 后者编译成 `RTRIM(col) >= ?`(字符串比较),对 datetime 恒不匹配
// ⇒ 返 0 行**且不报错**。同一个坑见 internal/cubeclient 的 GetActivePromotion。
const cubeOperatorInDateRange = "inDateRange"

// cubeOperatorGt 是 cube 的「大于」算子(增量过滤用:只导变过的行)。
//
// ⚠️ **与 inDateRange 不是一回事**:gt 编译成 `col > @p1`,
// 对 live 源的 datetime 列会 CAST 成 DATETIME(保留时分秒)⇒
// "今天 17:20 变过的商品"不会被"今天 12:00 的水位"漏掉。
const cubeOperatorGt = "gt"

// 时间窗的哨兵边界(够宽,不代表业务边界)。
const (
	exportWindowLower = "1900-01-01"
	exportWindowUpper = "2999-12-31"
)

// cubeExportScope 描述 pricing 域的一个可导出资料域(cube 侧取数口径)。
//
// ⚠️ **与 cube-router 的 exportScopes 逐字对应**(同 model / 同维度 / 同度量 /
// 同时间窗)—— 改了一边不会编译报错,症状是静默丢字段。
type cubeExportScope struct {
	name  string
	model string
	// measures 是要 SELECT 的度量(至少一个,cube 要求)。
	measures []string
	// dimensions 是要 SELECT 的维度。
	dimensions []string
	// fields 把 cube 行(形如 "product.id")映射成导出契约的字段名。
	fields map[string]string
	// window 为 true ⇒ 按基准日过滤(**只有促销是时效数据**)。
	window bool
	// changedMember 是该 scope 的"**最后变动时间**"成员(全限定名)。
	//
	// ⚠️ 它与 window 是两回事:
	//   - window         = "在基准日**有效**"(促销的生效期)⇒ inDateRange;
	//   - changedMember = "在基准日**变过**"(商品的改动时间)⇒ gt。
	//
	// ⚠️ **取自 archivedto.ChangedMembers**(不写死字面量):
	// 那是"哪些 scope 能做增量"的唯一真相源(导出端 + 编排端共用)
	// ⇒ 两边各写一份时,改了一边不会编译报错
	// ⇒ 症状是"编排端推进了水位,而导出端没加过滤" ⇒ 漏数据且零报错。
	changedMember string
}

// cubeExportScopes 是 pricing 域可导出的资料域。
//
// ⚠️ 售价与促销**分开两个 scope**(与 cube-router / archivedto 一致):
// 混在一起就得让一个 scope 归两个归属方 ⇒ 两个真相源。
var cubeExportScopes = map[string]cubeExportScope{
	archivedto.ScopePrices: {
		name:       archivedto.ScopePrices,
		model:      "product",
		measures:   []string{"product.avg_price_yuan", "product.total_cost_yuan"},
		dimensions: []string{"product.id"},
		fields: map[string]string{
			"product.id":              "product_id",
			"product.avg_price_yuan":  "price_yuan",
			"product.total_cost_yuan": "cost_yuan",
		},
		// ⚠️ 售价与商品在**源库的同一行**里(思迅 t_bd_item_info.sale_price)
		//   ⇒ 改了价就等于改了那一行 ⇒ 商品的变动时间就是价的变动时间 ✅。
		changedMember: archivedto.ChangedMembers[archivedto.ScopePrices],
	},
	archivedto.ScopePromotions: {
		name:     archivedto.ScopePromotions,
		model:    "promotion",
		measures: []string{"promotion.count"},
		dimensions: []string{
			"promotion.item_no", "promotion.plan_no", "promotion.plan_name",
			"promotion.promo_price", "promotion.orig_price",
			"promotion.begin_day", "promotion.end_day",
		},
		fields: map[string]string{
			"promotion.item_no":     "product_id",
			"promotion.plan_no":     "plan_no",
			"promotion.plan_name":   "plan_name",
			"promotion.promo_price": "price_yuan",
			"promotion.orig_price":  "orig_price_yuan",
			"promotion.begin_day":   "begin_day",
			"promotion.end_day":     "end_day",
		},
		// ⚠️ 促销是**时效数据**:必须按基准日过滤。
		// 不过滤 ⇒ 一次导出把整段历史(源表 31371 行)都带出来,
		// 而查价时按基准日过滤后只剩几百条能用。
		window: true,
	},
}

// ExportScopeNames 是本域支持的导出资料域(给错误文案用)。
func ExportScopeNames() []string {
	out := make([]string, 0, len(cubeExportScopes))
	for name := range cubeExportScopes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ExportArchive 导出该门店某个资料域的一页(archivedto 契约)。
//
// ⚠️ **判据与查价同一套**:配了数据源 ⇒ cube;没配 ⇒ 本域 PG。
//
// ⚠️ 解析失败 / 数据源被关停 ⇒ 上抛(503),**不回落本地**。
//
// # since = 增量水位
//
// 空   ⇒ 全量(默认行为,不变);
// 非空 ⇒ 只导"自该时刻之后变过"的行(cube 查询加 gt 过滤)。
//
// ⚠️ **必须真的落到 cube 查询里**:只在上层记一个 since 却不过滤,
// 等于"编排端推进了水位,导出端仍导全量" ⇒ 增量形同虚设(且零报错)。
func (s *PriceService) ExportArchive(ctx context.Context, branchID, scope string, offset, limit int, asOf, since string) (*archivedto.ExportPage, error) {
	sc, ok := cubeExportScopes[scope]
	if !ok {
		return nil, fmt.Errorf("%w: scope=%q(可用:%s)",
			model.ErrUnsupportedExportScope, scope, strings.Join(ExportScopeNames(), ","))
	}
	branchID = strings.TrimSpace(branchID)
	src, useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}

	page := &archivedto.ExportPage{
		Scope:      sc.name,
		BranchID:   branchID,
		CubeSource: src.CubeSourceName,
		Offset:     offset,
		Limit:      limit,
	}

	if useCube {
		rows, total, err := s.exportFromCube(ctx, sc, asOf, since, offset, limit)
		if err != nil {
			return nil, err
		}
		page.Rows = rows
		page.Count = len(rows)
		page.Total = total
		return page, nil
	}

	all, err := s.exportFromLocal(ctx, sc, branchID, asOf)
	if err != nil {
		return nil, err
	}
	page.Rows = sliceRows(all, offset, limit)
	page.Count = len(page.Rows)
	page.Total = len(all)
	return page, nil
}

// exportFromCube 从该门店的 cube 实例取一页(archivedto 契约)。
func (s *PriceService) exportFromCube(ctx context.Context, sc cubeExportScope, asOf, since string, offset, limit int) ([]map[string]any, int, error) {
	if s.reader == nil {
		return nil, 0, fmt.Errorf("%w: cube 读端未装配", model.ErrCubeSourceUnavailable)
	}
	q := cubeclient.CubeQuery{
		Measures:   sc.measures,
		Dimensions: sc.dimensions,
		Filters:    sc.filters(asOf, since),
		Limit:      limit,
		Offset:     offset,
		// ⚠️ order 必须给:offset>0 时 cube 直接 400;
		// 且 order 的 id 必须**同时在 SELECT 里**(否则 GROUP BY 查询报 not a grouped column)。
		//
		// ⚠️ **按全部维度排序**:分页的稳定性取决于排序键是否**全序**。
		// 维度元组本身就是 GROUP BY 的键 ⇒ 按它排序必是全序。
		Order: sc.order(),
		// ⚠️ total:true 才拿得到"不受 limit/offset 影响的总行数"。
		Total: true,
	}
	raw, total, err := s.reader.LoadCubeQueryPage(ctx, sc.model, q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		out = append(out, sc.buildRow(row))
	}
	return out, total, nil
}

// exportFromLocal 从本域 PG 取该资料域的全部行(再由调用方切页)。
//
// ⚠️ 售价按**基准日**取"生效中"的行(effective_at <= 基准日 <= expired_at),
// 与查价口径一致 —— 否则"查价说有价、导出说没价"。
func (s *PriceService) exportFromLocal(ctx context.Context, sc cubeExportScope, branchID, asOf string) ([]map[string]any, error) {
	if s.prices == nil || s.promos == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	at := dayStart(time.Now(), asOf)
	switch sc.name {
	case archivedto.ScopePrices:
		rows, err := s.prices.ActivePricesAll(ctx, branchID, DefaultPriceListID, at)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"product_id": r.ProductID,
				"price_yuan": r.PriceYuan.String(),
				"cost_yuan":  optionalAmount(r.CostYuan),
			})
		}
		return out, nil
	case archivedto.ScopePromotions:
		rows, err := s.promos.ActivePromotionRows(ctx, branchID, at)
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"product_id":      r.ProductID,
				"plan_no":         r.ID,
				"plan_name":       r.PlanName,
				"price_yuan":      r.PriceYuan.String(),
				"orig_price_yuan": optionalAmount(r.OrigPriceYuan),
				"begin_day":       r.BeginAt.Format("2006-01-02"),
				"end_day":         r.EndAt.Format("2006-01-02"),
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: scope=%q", model.ErrUnsupportedExportScope, sc.name)
	}
}

// filters 造该 scope 的 cube 过滤条件。
//
// ⚠️ **两类过滤不能混**:
//   - **时效过滤**(window)= "在基准日**有效**"(促销的生效期)⇒ inDateRange;
//   - **变动过滤**(changedMember)= "在基准日**变过**"(商品的最后改动时间)⇒ gt。
//
// ⚠️ **只有时效数据(promotions)带时间窗过滤** ——
// 售价不是时效数据,误加时间窗过滤会让它**永远导不出行**(而且不报错)。
//
// ⚠️ **变动过滤只在 since 非空时加**:
// 没给 since(= 全量轮次)就不该加,否则"全量"会变成"只导变过的"。
func (sc cubeExportScope) filters(asOf, since string) []cubeclient.CubeFilter {
	out := []cubeclient.CubeFilter{}
	if sc.window {
		out = append(out,
			cubeclient.CubeFilter{
				Member:   "promotion.begin_date",
				Operator: cubeOperatorInDateRange,
				Values:   []any{exportWindowLower, asOf},
			},
			cubeclient.CubeFilter{
				Member:   "promotion.end_date",
				Operator: cubeOperatorInDateRange,
				Values:   []any{asOf, exportWindowUpper},
			},
		)
	}
	// ⚠️ **增量过滤**:只导"自上次水位之后变过"的行。
	//
	// 少了这一条 ⇒ 每次同步都是全量(27569 行),
	// 而**编排方照样推进水位** ⇒ 下次仍然全量 ⇒ 增量形同虚设。
	if sc.changedMember != "" && since != "" {
		out = append(out, cubeclient.CubeFilter{
			Member:   sc.changedMember,
			Operator: cubeOperatorGt,
			Values:   []any{since},
		})
	}
	return out
}

// order 按**全部维度**升序(cube-router 的 exportOrderFor 同款)。
//
// ⚠️ order 的 id 必须同时在 SELECT 的 dimensions 里 —— 它们都是维度 ⇒ 天然满足。
func (sc cubeExportScope) order() []cubeclient.CubeOrder {
	out := make([]cubeclient.CubeOrder, 0, len(sc.dimensions))
	for _, d := range sc.dimensions {
		out = append(out, cubeclient.CubeOrder{ID: d, Order: "asc"})
	}
	return out
}

// buildRow 把一行 cube 数据整形成导出契约的一行。
//
// ⚠️ 金额类字段导出成**字符串**,null 导出成 null(不是 "0")——
// "没维护价"与"价是 0"必须能区分,否则 POS 会把没维护价的商品显示成 ¥0.00。
func (sc cubeExportScope) buildRow(raw map[string]any) map[string]any {
	row := make(map[string]any, len(sc.fields))
	for cubeKey, outKey := range sc.fields {
		v, ok := raw[cubeKey]
		if !ok {
			continue
		}
		if strings.HasSuffix(outKey, "_yuan") {
			row[outKey] = exportAmountValue(v)
			continue
		}
		row[outKey] = exportTextValue(v)
	}
	return row
}

// exportAmountValue 把 cube 原始值转成导出契约的**金额字符串**。
//
// ⚠️ null ⇒ nil(不是 "0"):"没维护价"与"价是 0"必须能区分。
//
// # ⚠️ 必须能吃 base64(MSSQL live 源)
//
// 思迅直查(product 是 live model)的 decimal 列经 go-mssqldb 以 []byte 返回,
// encoding/json 把 []byte 序列化成 **base64**:
//
//	product.avg_price_yuan = "MTIuMDAwMDAw"  → base64 → "12.000000"
//
// ⇒ 若只按"字面数字"解析,就会把 base64 串**原样带出**
// ⇒ 导入端 parseMoney 失败 ⇒ 那一行被当成"没维护价"跳过
// ⇒ **全量价格一行都没落库,报告只写 skipped 27569,零报错**。
//
// ⚠️ 读不懂的值(脏值)**原样带出** ⇒ 导入端能识别"这个值我读不懂"并报告它
// (而不是静默当 0 —— 那会把脏数据变成"售价 0 元",POS 会照印)。
func exportAmountValue(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if d, err := decimal.NewFromString(s); err == nil {
			return d.String()
		}
		// base64(MSSQL live 源的 decimal):见上面的说明。
		if d, ok := cubeclient.ParseDecimalOK(s); ok {
			return d.String()
		}
		return s
	}
	if d, ok := cubeclient.ParseDecimalOK(v); ok {
		return d.String()
	}
	return nil
}

// exportTextValue 把 cube 原始值转成导出契约的文本值。
func exportTextValue(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return fmt.Sprint(v)
}

// optionalAmount 把"可能没有"的金额转成契约值:零 ⇒ nil(没有),否则字符串。
//
// ⚠️ 本域的 cost / orig_price 用**零值**表示"不知道"(导入端也是这个约定:
// 空串 = 没有会员价 / 不知道成本)。
func optionalAmount(d decimal.Decimal) any {
	if d.IsZero() {
		return nil
	}
	return d.String()
}

// sliceRows 按 offset/limit 切一页(越界返回空切片,不 panic)。
func sliceRows(rows []map[string]any, offset, limit int) []map[string]any {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []map[string]any{}
	}
	end := len(rows)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return rows[offset:end]
}

// ---- 离线价格包(POS)------

// ExportPriceRow 是离线价格包里的一行。
//
// # ⚠️ 零售价与促销价**分开给**
//
//	成交价 = min(price_yuan, promo_price_yuan) —— 由消费方决定
//	(POS 收银用 min;价签打印用零售价划线 + 促销价)。
//
// ⚠️ 只给一个"价"的话,消费方要么按原价收钱(多收),要么自己再查一次 promotions。
//
// ⚠️ **JSON 字段名不许变**:前端 / POS 离线包在用(见 GET /prices/export)。
type ExportPriceRow struct {
	// ProductID 是商品货号(与 catalog.products.id 同一套编码)。
	ProductID string `json:"product_id"`
	// PriceYuan 是零售价(字符串)。
	//
	// ⚠️ 空串 = **没维护零售价**(该商品在价格表里没有行),
	// 与"零售价 0 元"是两件事。
	PriceYuan string `json:"price_yuan"`
	// PromoPriceYuan 是**该基准日生效的促销价**;null = 没有生效促销。
	//
	// ⚠️ **不用 omitempty**:键必须**始终在场**(没促销时值为 null)——
	// 客户端才能区分"没有促销"与"响应里没这个字段"。
	PromoPriceYuan *string `json:"promo_price_yuan"`
	// PromoPlanNo 是促销单号(思迅 plan_no)。
	PromoPlanNo *string `json:"promo_plan_no,omitempty"`
	// PromoPlanName 是促销单名称(印在价签上,用于人工追溯"这个价哪来的")。
	PromoPlanName *string `json:"promo_plan_name,omitempty"`
	// PromoBeginDay / PromoEndDay 是活动起止日(yyyy-mm-dd)。
	PromoBeginDay *string `json:"promo_begin_day,omitempty"`
	PromoEndDay   *string `json:"promo_end_day,omitempty"`
}

// ExportPriceRows 导出门店的离线价格包(POS 用)。
//
// ⚠️ **判据与查价同一套**:配了数据源 ⇒ cube(实时);没配 ⇒ 本域 PG。
//
// ⚠️ 门店 00 的价会因此**从本地快照切成 cube 实时** —— 值可能变
// (快照是 2026-10-20 19:24,实时是当前值)。这是修正,不是回归。
func (s *PriceService) ExportPriceRows(ctx context.Context, branchID string, at time.Time) ([]ExportPriceRow, error) {
	_, useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		return s.exportPriceRowsFromCube(ctx, at)
	}
	return s.exportPriceRowsFromLocal(ctx, branchID, at)
}

// exportPriceRowsFromLocal 从本域 PG 组装离线价格包。
func (s *PriceService) exportPriceRowsFromLocal(ctx context.Context, branchID string, at time.Time) ([]ExportPriceRow, error) {
	if s.prices == nil || s.promos == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	prices, err := s.prices.ActivePricesAll(ctx, branchID, DefaultPriceListID, at)
	if err != nil {
		return nil, err
	}
	promos, err := s.promos.ActivePromotionsAll(ctx, branchID, at)
	if err != nil {
		return nil, err
	}

	out := make([]ExportPriceRow, 0, len(prices)+len(promos))
	seen := make(map[string]bool, len(prices))
	for _, p := range prices {
		row := ExportPriceRow{ProductID: p.ProductID, PriceYuan: p.PriceYuan.String()}
		if promo, ok := promos[p.ProductID]; ok {
			applyLocalPromo(&row, promo)
		}
		seen[p.ProductID] = true
		out = append(out, row)
	}
	// ⚠️ 促销可能落在**没维护零售价**的商品上(源库就是这种脏数据)。
	//   只给"有零售价的行" ⇒ 客户端永远看不到这条促销。
	// ⚠️ map 迭代顺序随机 ⇒ 先把 id 排好序,保证导出结果**可复现**
	//   (客户端拿两次导出做 diff,顺序必须一样)。
	onlyPromo := make([]string, 0, len(promos))
	for pid := range promos {
		if !seen[pid] {
			onlyPromo = append(onlyPromo, pid)
		}
	}
	sort.Strings(onlyPromo)
	for _, pid := range onlyPromo {
		row := ExportPriceRow{ProductID: pid}
		applyLocalPromo(&row, promos[pid])
		out = append(out, row)
	}
	return out, nil
}

// applyLocalPromo 把本地促销挂到一行价格上。
//
// ⚠️ 促销价 **0 元也要给**(不是"没有促销")——
// null 与 "0" 必须能区分,否则客户端会把"打折到 0 元"当成"没促销"。
func applyLocalPromo(row *ExportPriceRow, promo model.Promotion) {
	price := promo.PriceYuan.String()
	row.PromoPriceYuan = &price
	planNo := promo.ID
	row.PromoPlanNo = &planNo
	if promo.PlanName != "" {
		name := promo.PlanName
		row.PromoPlanName = &name
	}
	begin := promo.BeginAt.Format("2006-01-02")
	row.PromoBeginDay = &begin
	end := promo.EndAt.Format("2006-01-02")
	row.PromoEndDay = &end
}

// cubeProductRow 是 cube 的一个商品(只取价格包需要的字段)。
type cubeProductRow struct {
	ID string
	// PriceYuan 是 cube 的 avg_price_yuan(原始值;nil = 没维护价)。
	PriceYuan any
}

// cubePromoRow 是 cube 的一个促销单。
type cubePromoRow struct {
	ItemNo    string
	PlanNo    string
	PlanName  string
	Price     decimal.Decimal
	HasPrice  bool
	OrigPrice string
	BeginDay  string
	EndDay    string
}

// exportPriceRowsFromCube 从 cube 组装离线价格包。
//
// ⚠️ **分批取**:门店 00 实测 27565 个商品,一次拉完会让 sidecar 内存爆,
// 而且 cubeclient 的单次调用超时只有 10s ⇒ 按 cubeExportPageSize 分批。
func (s *PriceService) exportPriceRowsFromCube(ctx context.Context, at time.Time) ([]ExportPriceRow, error) {
	if s.reader == nil {
		return nil, fmt.Errorf("%w: cube 读端未装配", model.ErrCubeSourceUnavailable)
	}
	products, err := s.cubeAllProducts(ctx)
	if err != nil {
		return nil, err
	}
	promos, err := s.cubeActivePromotions(ctx, at)
	if err != nil {
		return nil, err
	}
	// ⚠️ 同一商品多个促销单 ⇒ 取**促销价最低**那条
	// (与本地路径 ActivePromotionsAll 的口径一致)。
	best := map[string]cubePromoRow{}
	for _, p := range promos {
		if !p.HasPrice || p.Price.LessThanOrEqual(decimal.Zero) {
			continue
		}
		cur, ok := best[p.ItemNo]
		if !ok || p.Price.LessThan(cur.Price) {
			best[p.ItemNo] = p
		}
	}

	out := make([]ExportPriceRow, 0, len(products))
	seen := make(map[string]bool, len(products))
	for _, p := range products {
		row := ExportPriceRow{ProductID: p.ID}
		if p.PriceYuan != nil {
			if s, ok := exportAmountValue(p.PriceYuan).(string); ok {
				row.PriceYuan = s
			}
		}
		if promo, ok := best[p.ID]; ok {
			applyCubePromo(&row, promo)
		}
		seen[p.ID] = true
		out = append(out, row)
	}
	// 只有促销、没有商品档案的行(理论上不该有)也要给,否则那条促销会消失。
	onlyPromo := make([]string, 0)
	for itemNo := range best {
		if !seen[itemNo] {
			onlyPromo = append(onlyPromo, itemNo)
		}
	}
	sort.Strings(onlyPromo)
	for _, itemNo := range onlyPromo {
		row := ExportPriceRow{ProductID: itemNo}
		applyCubePromo(&row, best[itemNo])
		out = append(out, row)
	}
	return out, nil
}

// cubeAllProducts 分批取该门店 cube 实例里的全部商品(只取价格包需要的字段)。
func (s *PriceService) cubeAllProducts(ctx context.Context) ([]cubeProductRow, error) {
	q := cubeclient.CubeQuery{
		// ⚠️ **必须带 measure**:cube 拒收"没有 measure"的查询。
		Measures:   []string{"product.avg_price_yuan"},
		Dimensions: []string{"product.id"},
		// ⚠️ order 必须给(offset>0 时 cube 直接 400),且 id 在 SELECT 里。
		Order: []cubeclient.CubeOrder{{ID: "product.id", Order: "asc"}},
		Limit: cubeExportPageSize,
		// ⚠️ total:true 才有"总行数"(不给时恒为 0)。
		Total: true,
	}
	var out []cubeProductRow
	for offset := 0; ; offset += cubeExportPageSize {
		q.Offset = offset
		raw, total, err := s.reader.LoadCubeQueryPage(ctx, "product", q)
		if err != nil {
			return nil, err
		}
		for _, row := range raw {
			out = append(out, cubeProductRow{
				ID:        exportTextString(row["product.id"]),
				PriceYuan: row["product.avg_price_yuan"],
			})
		}
		if len(raw) < cubeExportPageSize || len(out) >= total {
			break
		}
	}
	return out, nil
}

// cubeActivePromotions 分批取基准日**生效**的促销单。
func (s *PriceService) cubeActivePromotions(ctx context.Context, at time.Time) ([]cubePromoRow, error) {
	day := at.Format("2006-01-02")
	q := cubeclient.CubeQuery{
		Measures: []string{"promotion.count"},
		Dimensions: []string{
			"promotion.item_no", "promotion.plan_no", "promotion.plan_name",
			"promotion.promo_price", "promotion.orig_price",
			"promotion.begin_day", "promotion.end_day",
		},
		// ⚠️ 促销是时效数据:必须按基准日过滤(inDateRange,不能用 gte/lte)。
		Filters: []cubeclient.CubeFilter{
			{Member: "promotion.begin_date", Operator: cubeOperatorInDateRange,
				Values: []any{exportWindowLower, day}},
			{Member: "promotion.end_date", Operator: cubeOperatorInDateRange,
				Values: []any{day, exportWindowUpper}},
		},
		// ⚠️ 按**全部维度**排序(与 cube-router 的 exportOrderFor 同款):
		// 维度元组本身就是 GROUP BY 的键 ⇒ 按它排序必是**全序**,
		// 翻页不会重复/漏行(症状是"第 2 页又看到第 1 页的某条")。
		Order: []cubeclient.CubeOrder{
			{ID: "promotion.item_no", Order: "asc"},
			{ID: "promotion.plan_no", Order: "asc"},
			{ID: "promotion.plan_name", Order: "asc"},
			{ID: "promotion.promo_price", Order: "asc"},
			{ID: "promotion.orig_price", Order: "asc"},
			{ID: "promotion.begin_day", Order: "asc"},
			{ID: "promotion.end_day", Order: "asc"},
		},
		Limit: cubeExportPageSize,
		Total: true,
	}
	var out []cubePromoRow
	for offset := 0; ; offset += cubeExportPageSize {
		q.Offset = offset
		raw, total, err := s.reader.LoadCubeQueryPage(ctx, "promotion", q)
		if err != nil {
			return nil, err
		}
		for _, row := range raw {
			p := cubePromoRow{
				ItemNo:    exportTextString(row["promotion.item_no"]),
				PlanNo:    exportTextString(row["promotion.plan_no"]),
				PlanName:  exportTextString(row["promotion.plan_name"]),
				OrigPrice: exportTextString(row["promotion.orig_price"]),
				BeginDay:  exportTextString(row["promotion.begin_day"]),
				EndDay:    exportTextString(row["promotion.end_day"]),
			}
			if v, ok := row["promotion.promo_price"]; ok && v != nil {
				p.Price = cubeclient.ParseDecimal(v)
				p.HasPrice = true
			}
			out = append(out, p)
		}
		if len(raw) < cubeExportPageSize || len(out) >= total {
			break
		}
	}
	return out, nil
}

// applyCubePromo 把 cube 的促销挂到一行价格上。
func applyCubePromo(row *ExportPriceRow, promo cubePromoRow) {
	price := promo.Price.String()
	row.PromoPriceYuan = &price
	planNo := promo.PlanNo
	row.PromoPlanNo = &planNo
	if promo.PlanName != "" {
		name := promo.PlanName
		row.PromoPlanName = &name
	}
	if promo.BeginDay != "" {
		begin := promo.BeginDay
		row.PromoBeginDay = &begin
	}
	if promo.EndDay != "" {
		end := promo.EndDay
		row.PromoEndDay = &end
	}
}

// exportTextString 把 cube 原始值转成**去空格的文本**(空值 → 空串)。
func exportTextString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return fmt.Sprint(v)
}
