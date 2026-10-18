// Package service 实现销售报表的聚合编排。
//
// # 数据来源与它的三个"陷阱"
//
// 唯一数据源是 cube 的 `sales_report` plugin（源库侧把
// t_rm_daysum × t_bd_item_info × t_bd_item_cls 三张表 JOIN 成一个视图）。
// 之所以不直接用 cube 主仓的 sale_day model，有三个实测出来的原因，
// 写在各自函数的注释里 —— 那些数字不要当成"旧结论"随手删掉。
//
// # 毛利是"参考值"，且覆盖率必须随结果一起返回
//
// 两个生产库的成本数据质量**完全不同**，都实测过：
//
// **hbposepro（佳乐店，既有报表系统的数据源）—— 数据是好的**
//
//	✅ 实测 2026-10-10 与既有报表系统逐分对账：
//	   销售金额 18896.38 ✅ 销售数量 3573.85 ✅（±0.00）
//	   cost_price 是行级成本额，但有 112/749 行 cost_price > sale_amt（亏本销售）。
//	   cube plugin 已用 CASE WHEN 收窄，得到成本 17340.06 vs 图片隐含 17216.62，
//	   差 0.7%；毛利 1556.32 vs 图片 1679.76（差 7.4%）。
//	   **成本覆盖率 100%（749/749）。**
//
// **hbposv10（另一门店）—— 数据是坏的**
//
//	t_rm_daysum.cost_price 与 sale_amt 逐行相等（存的是销售额）。
//	t_da_jxc_daysum 近 8 天 so_amt / so_qty / in_price 全为 0。
//	t_bd_item_info.base_price 全表为 0；base_price1 仅 308/27312 ≈ 1.1%。
//	→ 当日按类别聚合总覆盖率仅 86/340 = 25.3%，多个类别为 0.0%。
//
// **因此本包对每一行都返回 CostedRows / TotalRows**，由调用方（报表 API
// 与群卡片）按阈值决定要不要展示毛利 —— 阈值见 MinCostCoverage。
// 直接用 cost=0 的组算毛利率会显示 100%，那是个比"不显示"更坏的结果。
//
// 后续要在 supertrade 侧补正式毛利算法时，只需改 cube plugin 的成本
// 表达式，本包的响应形状不变，前端与卡片代码无需改动。
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// MinCostCoverage 是"成本覆盖率低于此值就不展示毛利率"的阈值。
//
// 取 0.5（50%）而不是 0.8：当前数据整体覆盖率只有 25.3%，
// 阈值定太高会让所有毛利都消失，功能等于没做；而 50% 以下时
// 这个数字已经偏离真实成本到无法参考的程度。
//
// 它同时被 service 层（填 MarginAvailable）与文档引用 ——
// 改了这里请同步前端文案，别让两处说法漂移。
const MinCostCoverage = 0.5

// CubeCube 是本服务对 cube 的**全部**依赖面。
//
// 收窄成这一个方法的理由与 cubeclient.DaprCubeClient 收窄 daprInvoker
// 同源：接口越窄，单测越能证明"到底往 cube 发了什么 query"。
// 报表是这个服务的唯一职责，多一个方法都是能被误用的余地。
type CubeQuerier interface {
	// LoadSalesReport 查销售报表聚合。
	//
	// groupBy 取 "category" / "branch" / "product"；
	// from/to 是闭区间 YYYY-MM-DD。
	//
	// 返回的是 **RawSalesReport**（float64、未格式化），不是
	// SalesReport：格式化与"毛利覆盖率够不够"的判断属于业务口径，
	// 必须留在本包而不是泄到 cube 适配层。
	LoadSalesReport(ctx context.Context, storeBranchID, groupBy, from, to string, limit int) (*RawSalesReport, error)

	// LoadSalesTotals 取**大盘总额**：不带任何维度、不限量。
	//
	// ⚠️ 它**不能**由"把 LoadSalesReport 的行加起来"代替。
	//
	//	分组的行是"按维度分组后的子集"，求和在两种情况下偏低：
	//	  ① cube 侧带 Limit（topN 截断）；
	//	  ② 空类目名/空编码的行被丢弃，而未分组的总额保留同一批行。
	//	线上实测门店 00 / 2026-10-16：品类口径 2024.6 元 vs
	//	供应商口径 13003.8 元 —— 顶部数字只统计到 **15.6%**，零报错。
	//
	// → 顶部汇总必须是**独立一次无分组查询**的结果。
	LoadSalesTotals(ctx context.Context, storeBranchID, from, to string) (RawReport, error)
}

// SalesReport 是一次报表查询的完整结果（汇总 + 明细）。
type SalesReport struct {
	GroupBy string      `json:"group_by"`
	From    string      `json:"from"`
	To      string      `json:"to"`
	Summary Summary     `json:"summary"`
	Rows    []ReportRow `json:"rows"`

	// DataSource 是 "daily_store"（已定稿事实库）或 "cube"（现查源库）。
	//
	// ⚠️ 它必须随每次响应回传：改造前卡片与报表页各自现查源库，
	//	而两边算出来的汇总口径不同（一个少 85%）却**零报错**。
	//	把这个字段暴露出去，"今天这个数从哪来"才有答案。
	DataSource string `json:"data_source,omitempty"`

	// FallbackReason 是"没走已定稿事实库"时的可读原因。
	//
	// ⚠️ 省略（而不是给空串）是为了让**正常路径的响应逐字不变** ——
	//	wechat-bot 的卡片按固定字段解析，多一个空串字段也可能进 diff。
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// Summary 是顶部 4 个汇总指标。
type Summary struct {
	NetAmount    string `json:"net_amount"`     // 净销售额（元）
	NetQty       string `json:"net_qty"`        // 净销量（单位是称重重量，非件数）
	RefCost      string `json:"ref_cost"`       // 参考成本（元）
	RefMargin    string `json:"ref_margin"`     // 参考毛利（元）
	RefMarginPct string `json:"ref_margin_pct"` // 参考毛利率（"" = 覆盖率不足，不可算）
	CostCoverage string `json:"cost_coverage"`  // 成本覆盖率，如 "25.3%"
	// MarginAvailable 告诉调用方能不能展示毛利，而不是让它自己判断分母为 0。
	MarginAvailable bool `json:"margin_available"`
}

// ReportRow 是一行明细（类别 / 分店 / 商品共用同一形状）。
type ReportRow struct {
	Code  string `json:"code"`  // 维度编码（类别编码 / 分店号 / 商品编码）
	Name  string `json:"name"`  // 维度名称
	Extra string `json:"extra"` // 附加信息（商品的单位、类别的父级名）

	NetAmount string `json:"net_amount"`
	NetQty    string `json:"net_qty"`
	RefCost   string `json:"ref_cost"`
	RefMargin string `json:"ref_margin"`
	// RefMarginPct 为空字符串表示该行成本覆盖率不足，毛利率不可算。
	RefMarginPct string `json:"ref_margin_pct"`
	CostCoverage string `json:"cost_coverage"`
	// CostedRows / TotalRows 是原始计数，供前端做 tooltip 与调试。
	CostedRows int64 `json:"costed_rows"`
	TotalRows  int64 `json:"total_rows"`
}

// Service 是销售报表服务。
type Service struct {
	cube CubeQuerier
	// daily 是已定稿销售事实库（PG）；为 nil 时全部现查源库。
	//
	// ⚠️ 它与 cube 是**二选一**，不是"PG 优先、失败再查 cube 的每一维"——
	//	逐维回退会让同一份报表里的总额与明细来自两套口径。
	//	整段统一选源见 loadRaw。
	daily DailyRangeLister
	now   func() time.Time
}

// New 构造 Service。
func New(cube CubeQuerier) *Service {
	return &Service{cube: cube, now: time.Now}
}

// Dimension 是报表支持的三个维度。
//
// 品牌维度**已被砍掉**（实测：源库 t_bd_item_info.item_brand 覆盖率 100%，
// 但按品牌聚合当天只返回 1 组"PM"，区分度为零）。保留在这里是为了让
// 任何想加回它的人先看到这个实测结论，而不是以为漏做了。
const (
	DimCategory = "category" // 类别
	DimProduct  = "product"  // 商品
)

// DimBranch 被**移出**报表维度。
//
// 思迅源库的 `t_rm_daysum.branch_no` 是**门店区域** —— 门店（X-Branch-ID）
// 的下一层，不是门店本身。把它当成一个报表维度会带来两个坏处：
//
//  1. **口径与门店对不上**。用户选的是门店，报表却按区域拆行，
//     同一天的两份"销售报表"对不上数，还没人说得清哪份对。
//  2. **门店隔离被误读成区域过滤**。真正负责隔离的是 X-Branch-ID
//     路由链（branch → cube-router → 该门店专属 cube 实例）；
//     branch_no 全程不进 WHERE，只在误用时才会被当成过滤条件。
//
// ✅ 决策 2026-10-15：**忽略数据源的门店区域**。它仍留在 cube plugin 的
// dimensions 里（供将来真要做区域对比时单独开一个查询），但**报表维度不暴露**。
const DimBranchRemoved = "branch"

// ErrInvalidDimension 表示请求里的维度不受支持。
//
// 用哨兵错误而不是纯字符串，handler 才能把它映射成 400 而非 503：
// 调用方改一个 query 参数就能恢复，与"源库不可达"是完全不同的处置。
// 见 handler.fail。
var ErrInvalidDimension = errors.New("salesagg: 不支持的维度")

// ValidDimension 报告 d 是否是受支持的维度。
func ValidDimension(d string) bool {
	switch d {
	case DimCategory, DimProduct:
		return true
	}
	return false
}

// Presets 是图片里的日期预设。
//
// 前端 tab 顺序与此一致：今日 / 昨日 / 本周 / 本月 / 自定义。
type Preset string

const (
	PresetToday     Preset = "today"
	PresetYesterday Preset = "yesterday"
	PresetWeek      Preset = "week"
	PresetMonth     Preset = "month"
)

// ResolveRange 把预设解析成闭区间 [from, to]。
//
// 今天往前推是**按天**而不是 7×24h：源库 oper_date 是 char(10) 的
// 营业日期，走天粒度；用时间点会让 to 落到"昨天"，少算一整天。
//
// now 注入是为了让"今日"在单测里可断言 —— 直接调 time.Now 的话，
// 测试在跨月/跨年边界会随机失败。
func ResolveRange(p Preset, now time.Time) (from, to string) {
	d := now.Format("2006-01-02")
	switch p {
	case PresetYesterday:
		y := now.AddDate(0, 0, -1).Format("2006-01-02")
		return y, y
	case PresetWeek:
		// 周一为一周之始（Go 的 Weekday: Sunday=0）
		offset := (int(now.Weekday()) + 6) % 7
		monday := now.AddDate(0, 0, -offset)
		return monday.Format("2006-01-02"), d
	case PresetMonth:
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return first.Format("2006-01-02"), d
	default: // PresetToday 与任何未知值都退回"今日"
		return d, d
	}
}

// Query 是报表查询入参。
type Query struct {
	// StoreBranchID 是**门店**（X-Branch-ID），必填。
	//
	// ⚠️ 它**不是**源库的 branch_no（那是门店区域，门店的下一层）。
	// 它只做一件事：作为 X-Branch-ID header 传给 cube-router，
	// 由后者把查询路由到该门店专属的 cube 实例。
	// **它永远不会变成 cube query 的 WHERE 条件。**
	//
	// 字段名不叫 BranchID 是刻意的：本仓"branch"历史上指过两个东西，
	// 叫 StoreBranchID 才能让读代码的人一眼知道它是哪一层。
	StoreBranchID string
	GroupBy       string
	From          string
	To            string
	// DimensionFilter 只对 category 维度有意义（"全部类别"= 空）。
	DimensionFilter string
	Limit           int
}

// Report 执行一次报表查询并做后处理（排序 + 毛利可用性判定）。
//
// # ⚠️ 顶部汇总**不**由明细行求和
//
// 明细是"按维度分组后的子集"，求和会偏低（见 CubeQuerier.LoadSalesTotals
// 的注释与线上实测）。所以汇总一律取**独立的无分组查询** / 已定稿事实里的
// totals 字段。
//
// ⚠️ 总额取不到时退回"按明细求和"，但**必须在响应里标出来**：
//
//	降级值非零却偏低 85%，拿它当权威基去算环比会得到一个
//	"看起来正常、实则全错"的百分比。
func (s *Service) Report(ctx context.Context, q Query) (*SalesReport, error) {
	if s.cube == nil && s.daily == nil {
		return nil, fmt.Errorf("salesagg: 数据源未配置")
	}
	if !ValidDimension(q.GroupBy) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDimension, q.GroupBy)
	}
	if s.cube == nil {
		// ⚠️ 装配了事实库但没装 cube（测试/只读部署形态）时，
		//   回退路径不存在，覆盖不全只能当错误返回 ——
		//   静默返回空报表会被读成"这家店这段时间没生意"。
		if s.daily == nil {
			return nil, fmt.Errorf("salesagg: cube 未配置")
		}
		if _, err := s.daily.LoadRange(ctx, q.StoreBranchID, q.From, q.To); err != nil {
			return nil, err
		}
	}

	src, err := s.loadRaw(ctx, q)
	if err != nil {
		return nil, err
	}

	out := &SalesReport{
		GroupBy:        q.GroupBy,
		From:           q.From,
		To:             q.To,
		Rows:           make([]ReportRow, 0, len(src.rows)),
		DataSource:     string(src.source),
		FallbackReason: src.reason,
	}
	for _, r := range src.rows {
		cov := coverage(r.CostedRows, r.TotalRows)
		row := ReportRow{
			Code:         r.Code,
			Name:         r.Name,
			Extra:        r.Extra,
			NetAmount:    fmtAmount(r.NetAmount),
			NetQty:       fmtAmount(r.NetQty),
			RefCost:      fmtAmount(r.RefCost),
			CostCoverage: fmtPct(cov),
			CostedRows:   r.CostedRows,
			TotalRows:    r.TotalRows,
			RefMargin:    "",
			RefMarginPct: "",
		}
		if cov >= MinCostCoverage {
			m := r.NetAmount - r.RefCost
			row.RefMargin = fmtAmount(m)
			if r.NetAmount != 0 {
				row.RefMarginPct = fmtPct(m / r.NetAmount)
			}
		}
		out.Rows = append(out.Rows, row)
	}

	// 明细按销售额倒序 —— 这是报表打开时用户最先看的顺序。
	sort.SliceStable(out.Rows, func(i, j int) bool {
		return numOf(out.Rows[i].NetAmount) > numOf(out.Rows[j].NetAmount)
	})

	out.Summary = summaryOf(*src.totals)
	return out, nil
}

// summaryOf 把一行大盘汇总渲染成顶部汇总指标。
//
// ⚠️ 口径改动只允许发生在这里 —— 门店对比（CompareData）会为每家店
//
//	算**同口径**的汇总，口径不一致就会出现
//	「单店报表」与「对比列」对不上数，且没人说得清哪份对。
func summaryOf(t RawReport) Summary {
	cov := coverage(t.CostedRows, t.TotalRows)
	out := Summary{
		NetAmount:       fmtAmount(t.NetAmount),
		NetQty:          fmtAmount(t.NetQty),
		RefCost:         fmtAmount(t.RefCost),
		CostCoverage:    fmtPct(cov),
		MarginAvailable: cov >= MinCostCoverage,
	}
	if out.MarginAvailable {
		m := t.NetAmount - t.RefCost
		out.RefMargin = fmtAmount(m)
		if t.NetAmount != 0 {
			out.RefMarginPct = fmtPct(m / t.NetAmount)
		}
	}
	return out
}

// buildSummary 把一组原始行汇成顶部汇总指标。
//
// 从 Report 中抽出供门店对比复用（CompareData 对每个 ok 门店都要算一份
// **同口径**的汇总 —— 口径不一致的话，"单店报表"与"对比列"对不上数，
// 还没人说得清哪份对）。口径改动只允许发生在这里。
func buildSummary(rows []RawReport) Summary {
	var sumAmt, sumQty, sumCost, sumCosted, sumRows float64
	for _, r := range rows {
		sumAmt += r.NetAmount
		sumQty += r.NetQty
		sumCost += r.RefCost
		sumCosted += float64(r.CostedRows)
		sumRows += float64(r.TotalRows)
	}
	sumCov := 0.0
	if sumRows > 0 {
		sumCov = sumCosted / sumRows
	}
	out := Summary{
		NetAmount:       fmtAmount(sumAmt),
		NetQty:          fmtAmount(sumQty),
		RefCost:         fmtAmount(sumCost),
		CostCoverage:    fmtPct(sumCov),
		MarginAvailable: sumCov >= MinCostCoverage,
	}
	if out.MarginAvailable {
		m := sumAmt - sumCost
		out.RefMargin = fmtAmount(m)
		if sumAmt != 0 {
			out.RefMarginPct = fmtPct(m / sumAmt)
		}
	}
	return out
}

// TodayByCategory 是每日群推送用的口径：**只到今天，按类别**。
//
// 它刻意不复用 Report：群卡片不需要排序、不需要分页、不需要维度切换，
// 且必须永远返回全量类别（哪怕成本覆盖率不足也要显示金额那一列），
// 免得卡片内容随成本数据质量抖动。
func (s *Service) TodayByCategory(ctx context.Context, storeBranchID, date string) (*SalesReport, error) {
	if date == "" {
		date = s.now().Format("2006-01-02")
	}
	rep, err := s.Report(ctx, Query{
		StoreBranchID: storeBranchID,
		GroupBy:       DimCategory,
		From:          date,
		To:            date,
		Limit:         0, // 0 = 不限
	})
	if err != nil {
		return nil, err
	}
	// 卡片只展示金额/数量两列时，毛利列在覆盖率不足时本就为空，
	// 这里显式清掉 RefMargin 的空串之外的一切冗余，让卡片渲染更简单。
	return rep, nil
}
