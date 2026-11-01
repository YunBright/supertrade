// Package service / stocking.go —— 备货系数（2026-10-31 用户口径）。
//
// # 用户口径
//
//	「需要维护一个备货系数表（倍数和周期），在审批流程中将所涉及 SKU 的备货系数
//	 随库存数、周期销售数一起给 LLM。……没有该系数时触发该计算逻辑。」
//
// # 两个数怎么用
//
//	周期销售数 = 日平均销量 × 备货周期
//	日平均销量 = 今天往前 **采样天数** 的销量 ÷ **有货天数**
//	采样天数   = max(今天 − 该 SKU 最后一次收货日, 备货周期) × 2
//	有货天数   = 采样窗口内该 SKU **真正有库存**的天数（见 inStockDays）
//	合理备货量 = 周期销售数 × 备货倍数
//
// ⚠️ 分母是**有货天数**，不是采样天数（2026-11-01 用户口径）。
//
//	断货期（卖完了等补货）的 0 销量不是"没需求"：水果木瓜-00418 十天卖光
//	10.36、之后 21 天缺货，按采样天数(54)算日均只有 0.19 ⇒ 周期销售数 1.342
//	⇒ 11.25 的进货被判 6.4 倍过量;按有货天数(6 天)算日均 1.73 ⇒ 周期销售数
//	12.1 ⇒ 合理备货量 15.9,同样这批货只有 0.71 倍。差 9 倍,全是缺货天数造的。
//
// ⚠️ 采样天数里的「收货日」用**实际收货日**（采购单备注里写的那个，
// 见 receipt_date.go），不是制单日 —— 延后录入的单这是唯一正确的口径。
//
// # 系数怎么算（本文件 computeStockingCoefficient）
//
// 备货周期看**进货频率**：同一 SKU 的相邻两次进货间隔取中位数（少于一单进货
// 用默认 7 天，夹在 [3,60] 天）。进货越频繁，周期越短。
//
// 备货倍数看**需求波动**：日销量的变异系数 CV（标准差/均值）越大，越需要缓冲，
// 倍数越高；周期本身越长，到货延迟的影响越大，再加一点补偿。结果夹在 [1,3]。
//
// 这套口径刻意**简单可解释**（管理后台要能让人看懂"为什么是这个数"），
// 也不需要训练/依赖外部模型。
package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

const (
	// stockingLookbackDays 是备货系数的历史回看天数。
	stockingLookbackDays = 180
	// stockingMinPeriodDays / stockingMaxPeriodDays 是备货周期的夹取范围。
	stockingMinPeriodDays     = 3
	stockingMaxPeriodDays     = 60
	stockingDefaultPeriodDays = 7
	// stockingMinMultiplier / stockingMaxMultiplier 是备货倍数的夹取范围。
	stockingMinMultiplier = 1.0
	stockingMaxMultiplier = 3.0
	// stockingSampleDaysFloor 是采样天数下限（数据太薄的 SKU 也至少有 30 天可比）。
	stockingSampleDaysFloor = 30
	// stockingSampleDaysCap 是采样天数上限（防止早期脏日期把窗口拉到几年）。
	stockingSampleDaysCap = 365
	// stockingMaxSampleGroups 是采样窗口分组的查询次数上限（每次一组 cube 查询）。
	stockingMaxSampleGroups = 8
)

// SigStockingExcess 是「按备货系数看进货明显过量」的提示信号（warn，不阻断）。
const SigStockingExcess = "stocking_excess"

// stockingQuerier 是备货系数算法额外需要的 cube 能力（可选接口）。
//
// ⚠️ 用**类型断言**而不是加进 CubeQuerier：既有单测的 fake 实现了
// CubeQuerier 但不实现这两个方法，直接把方法加进接口会让所有 fake 编译失败。
// 生产注入的 DaprCubeQuerier 两个方法都有。
type stockingQuerier interface {
	ListDailySales(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]DailySaleRow, error)
	ListProductPurchaseDates(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]ProductPurchaseDateRow, error)
}

// StockingFact 是给 LLM 看的单个 SKU 备货事实。
//
// ⚠️ 只放**业务事实**，不放原始每日曲线（几十个 SKU × 上百天会把 prompt 撑爆）。
type StockingFact struct {
	ProductID   string
	ProductName string
	Multiplier  decimal.Decimal // 备货倍数
	PeriodDays  int             // 备货周期
	SampleDays  int             // 采样天数（取销量的窗口长度）
	// InStockDays 是采样窗口内该 SKU **真正有库存**的天数，也是日平均销量的分母
	//（≠ SampleDays：断货期的 0 销量不算"没需求"，见 inStockDays）。
	InStockDays    int
	AvgDailySales  decimal.Decimal // 日平均销量 = 窗口销量 / 有货天数
	PeriodSalesQty decimal.Decimal // 周期销售数 = 日平均 × 周期
	StockQty       decimal.Decimal // 当前库存
	// ReceivedQty 是本次**净进货量**(PI 实收 − RO 退货)。
	//
	// ⚠️ 10 天合并付款时它是多日累加值,不能直接比「合理备货量」——
	// 比较用 CompareQty()(多日取日均),见 purchase_basis.go。
	ReceivedQty decimal.Decimal
	// ReturnedQty 是本次结算内的退货量(正数,0 = 无退货)。
	ReturnedQty decimal.Decimal
	// PurchaseDays 是有效收货日跨度(含首尾);1 = 单日或非有效多日。
	PurchaseDays int
	// DailyAvgQty 是日均采购量 = ReceivedQty / PurchaseDays(单日为 0,不用它)。
	DailyAvgQty decimal.Decimal
	// FirstDate / LastDate 是多日收货的首尾收货日(单日为空,回显用)。
	FirstDate string
	LastDate  string
	// ReasonableQty 是合理备货量 = 周期销售数 × 倍数。
	ReasonableQty decimal.Decimal
	// CoverageDays 是 (库存 + 本次进货) / 日平均销量 的可售天数（无销量时为 0）。
	CoverageDays decimal.Decimal
	// Estimated 表示系数是**本次现算**的（库里原本没有）。
	Estimated bool
}

// CompareQty 返回判"进货是否过量"用的量。
//
// 多日合并付款 ⇒ 日均采购量;单日 ⇒ 净进货量本身(见 purchase_basis.go)。
func (f StockingFact) CompareQty() decimal.Decimal {
	if f.PurchaseDays > 1 {
		return f.DailyAvgQty
	}
	return f.ReceivedQty
}

// ---- 算法 ----

// firstReceiptDate 返回进货日期里**最早**的一天（不晚于今天）；取不到返回空。
func firstReceiptDate(dates []string, today time.Time) string {
	var first time.Time
	for _, d := range dates {
		t, ok := parseDateOnly(d)
		if !ok || t.After(today) {
			continue
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	if first.IsZero() {
		return ""
	}
	return first.Format("2006-01-02")
}

// lastSaleDate 返回窗口内**最后一次有销量**的日子（售罄日的估计）；取不到返回空。
func lastSaleDate(daily []DailySaleRow, from, to time.Time) string {
	last := ""
	for _, r := range daily {
		if !r.Quantity.GreaterThan(decimal.Zero) {
			continue
		}
		t, ok := parseCubeTime(r.BusinessDate)
		if !ok || t.Before(from) || !t.Before(to) {
			continue
		}
		if d := normalizeCubeDate(r.BusinessDate); d > last {
			last = d
		}
	}
	return last
}

// inStockDays 估算该 SKU 在采样窗口内**真正有货**的天数（日平均销量的分母）。
//
// # 为什么不能直接用采样天数
//
// 断货（卖完了等下一次进货）期间的 0 销量不是"没有需求"。水果木瓜-00418：
// 10-05 进货 18.75 → 10-10 前卖光 → 之后 21 天缺货。按 54 天采样算日均 0.19，
// 按有货的 6 天算日均 1.73 —— 同一条数据差 9 倍，直接决定"进货是否过量"。
//
// # 口径
//
//	起点 = 该 SKU 本单采购单里的**最早实际收货日**（早于窗口 ⇒ 用窗口起点）；
//	终点 = 当前**还有库存** ⇒ 今天（还在卖）；
//	       已无库存（≈售罄）⇒ 窗口内**最后一次有销量的日子**；
//	结果夹在 [1, sampleDays]。
//
// stockQty/stockKnown 取不到库存时按"还在卖"处理（end=今天）：分母更大 ⇒
// 日均更小 ⇒ 判过量更严。宁可偏严，也不要因为取不到库存就把过量放过去。
func inStockDays(windowFrom time.Time, sampleDays int, stockQty decimal.Decimal, stockKnown bool,
	firstReceipt, lastSale string, today time.Time) int {
	if sampleDays <= 0 {
		return 1
	}
	start := windowFrom
	if t, ok := parseDateOnly(firstReceipt); ok && !t.Before(windowFrom) && !t.After(today) {
		start = t
	}
	end := today
	if stockKnown && !stockQty.GreaterThan(decimal.Zero) {
		if t, ok := parseDateOnly(lastSale); ok && !t.Before(start) && !t.After(today) {
			end = t
		}
	}
	n := int(end.Sub(start).Hours()/24) + 1
	if n < 1 {
		n = 1
	}
	if n > sampleDays {
		n = sampleDays
	}
	return n
}

// purchaseIntervals 返回商品历史进货的间隔天数中位数与进货次数。
//
// 入参是采购单制单日期（YYYY-MM-DD，可重复）。先按天去重再算相邻间隔：
// 同一天录两张单不代表进了两次货（录单员补录很常见）。
func purchaseIntervals(dates []string) (medianDays float64, intervals int) {
	parsed := make([]time.Time, 0, len(dates))
	for _, s := range dates {
		if t, ok := parseCubeTime(s); ok {
			parsed = append(parsed, t)
		}
	}
	if len(parsed) < 2 {
		return 0, 0
	}
	sort.Slice(parsed, func(i, j int) bool { return parsed[i].Before(parsed[j]) })
	uniq := make([]time.Time, 0, len(parsed))
	for _, t := range parsed {
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		if len(uniq) == 0 || !uniq[len(uniq)-1].Equal(d) {
			uniq = append(uniq, d)
		}
	}
	if len(uniq) < 2 {
		return 0, 0
	}
	gaps := make([]float64, 0, len(uniq)-1)
	for i := 1; i < len(uniq); i++ {
		g := uniq[i].Sub(uniq[i-1]).Hours() / 24
		if g <= 0 {
			continue
		}
		gaps = append(gaps, g)
	}
	if len(gaps) == 0 {
		return 0, 0
	}
	sort.Float64s(gaps)
	mid := len(gaps) / 2
	if len(gaps)%2 == 1 {
		return gaps[mid], len(gaps)
	}
	return (gaps[mid-1] + gaps[mid]) / 2, len(gaps)
}

// dailySalesStats 统计 [from, to) 的销量合计、按采样天数的日均、以及
// **有销售的那些天**的变异系数。
//
// ⚠️ 变异系数只在"有销售的天"上算：把没卖的日子当 0 会让"偶尔卖一件"的商品
// CV 爆表，倍数被推到上限，反而给滞销品备更多货。
func dailySalesStats(rows []DailySaleRow, from, to time.Time, sampleDays int) (avg, cv, total float64) {
	observed := make([]float64, 0, len(rows))
	for _, r := range rows {
		t, ok := parseCubeTime(r.BusinessDate)
		if !ok {
			continue
		}
		if t.Before(from) || !t.Before(to) {
			continue
		}
		q := r.Quantity
		if q.IsZero() {
			continue
		}
		f, _ := q.Float64()
		total += f
		observed = append(observed, f)
	}
	if sampleDays > 0 {
		avg = total / float64(sampleDays)
	}
	if len(observed) < 2 {
		return avg, 0, total
	}
	mean := total / float64(len(observed))
	var sumSq float64
	for _, v := range observed {
		d := v - mean
		sumSq += d * d
	}
	std := math.Sqrt(sumSq / float64(len(observed)))
	if mean > 0 {
		cv = std / mean
	}
	return avg, cv, total
}

// computeStockingCoefficient 按历史进货频率 + 日销量波动算一个 SKU 的备货系数。
//
// 结果同时落统计快照（采样天数/日均/周期销售数），方便管理后台解释"这个数怎么来的"。
//
// ⚠️ 日均销量/周期销售数的分母同样是**有货天数**（见 inStockDays，2026-11-01 用户口径），
//
//	与审批用的口径保持一致，否则后台看到的值和审批判过的值对不上。
func computeStockingCoefficient(branchID, productID, name string, daily []DailySaleRow, purchaseDates []string,
	stockQty decimal.Decimal, stockKnown bool, today time.Time, now time.Time) model.StockingCoefficient {
	intervalMed, intervals := purchaseIntervals(purchaseDates)
	period := stockingDefaultPeriodDays
	if intervals > 0 {
		period = int(math.Round(intervalMed))
	}
	period = clampInt(period, stockingMinPeriodDays, stockingMaxPeriodDays)

	sampleDays := period * 2
	if sampleDays < stockingSampleDaysFloor {
		sampleDays = stockingSampleDaysFloor
	}
	if sampleDays > stockingLookbackDays {
		sampleDays = stockingLookbackDays
	}
	from := today.AddDate(0, 0, -sampleDays+1)
	to := today.AddDate(0, 0, 1)
	// 有货天数：起点=最早进货日，终点=还有库存⇒今天 / 已售罄⇒最后一次有销量的日子。
	firstReceipt := firstReceiptDate(purchaseDates, today)
	lastSale := lastSaleDate(daily, from, to)
	inStock := inStockDays(from, sampleDays, stockQty, stockKnown, firstReceipt, lastSale, today)
	avg, cv, _ := dailySalesStats(daily, from, to, inStock)

	// 倍数 = 1 + 0.5×CV + 周期补偿（周期越长，到货延迟影响越大）。
	mult := 1.0 + 0.5*cv
	switch {
	case period >= 30:
		mult += 0.3
	case period >= 14:
		mult += 0.2
	}
	mult = clampFloat(mult, stockingMinMultiplier, stockingMaxMultiplier)

	row := model.StockingCoefficient{
		BranchID:       branchID,
		ProductID:      productID,
		ProductName:    name,
		Multiplier:     roundDecimal(mult, 2),
		PeriodDays:     period,
		SampleDays:     sampleDays,
		AvgDailySales:  roundDecimal(avg, 3),
		PeriodSalesQty: roundDecimal(avg*float64(period), 3),
		ReceiptCount:   intervals + 1,
		IntervalDays:   int(math.Round(intervalMed)),
		DailySalesCV:   roundDecimal(cv, 3),
		Source:         model.StockingSourceAuto,
		ComputedAt:     now,
	}
	if intervals == 0 {
		row.ReceiptCount = len(purchaseDates)
		row.IntervalDays = 0
	}
	return row
}

// ---- 读/写库 ----

// stockingCoefficientsOf 读本门店指定商品的备货系数（查不到的商品不出现在 map 里）。
func (s *Service) stockingCoefficientsOf(ctx context.Context, branchID string, products []string) (map[string]model.StockingCoefficient, error) {
	out := make(map[string]model.StockingCoefficient, len(products))
	if len(products) == 0 {
		return out, nil
	}
	var rows []model.StockingCoefficient
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id IN ?", branchID, products).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load stocking coefficients: %w", err)
	}
	for _, r := range rows {
		out[strings.TrimSpace(r.ProductID)] = r
	}
	return out, nil
}

// ensureStockingCoefficients 为**缺失**的 SKU 现算并落库（用户口径：没有系数时触发计算）。
//
// 返回新算出来的系数（已落库）。cube 不可用 / 查询失败 ⇒ 返回空 + 告警：
// 备货系数是**辅助研判**，不能因为它拿不到就把整单审核搞挂。
func (s *Service) ensureStockingCoefficients(ctx context.Context, branchID string, products []string, names map[string]string, today time.Time) map[string]model.StockingCoefficient {
	out := make(map[string]model.StockingCoefficient, len(products))
	if len(products) == 0 || s.cube == nil {
		return out
	}
	sq, ok := s.cube.(stockingQuerier)
	if !ok {
		return out
	}
	lookbackFrom := today.AddDate(0, 0, -stockingLookbackDays+1).Format("2006-01-02")
	to := today.AddDate(0, 0, 1).Format("2006-01-02")
	daily, err := sq.ListDailySales(ctx, branchID, products, lookbackFrom, to)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取逐日销量失败,备货系数跳过本次计算",
			"branch_id", branchID, "products", len(products), "err", err)
		return out
	}
	byProduct := make(map[string][]DailySaleRow, len(products))
	for _, r := range daily {
		byProduct[r.ProductID] = append(byProduct[r.ProductID], r)
	}
	purchases, err := sq.ListProductPurchaseDates(ctx, branchID, products, lookbackFrom, to)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取进货日期失败,备货系数按默认周期计算",
			"branch_id", branchID, "products", len(products), "err", err)
	}
	purchaseDates := make(map[string][]string, len(products))
	for _, r := range purchases {
		purchaseDates[r.ProductID] = append(purchaseDates[r.ProductID], r.SheetDate)
	}
	now := s.now()
	// 库存快照：有货天数的终点（无库存 ⇒ 停在最后一次有销量的日子）。
	stockQty := make(map[string]decimal.Decimal, len(products))
	stockKnown := make(map[string]bool, len(products))
	if rows, err := s.cube.GetStocks(ctx, branchID, products); err != nil {
		s.logger.Warn("settlement-auditor: 取库存失败,备货系数按\"还在卖\"算有货天数",
			"branch_id", branchID, "products", len(products), "err", err)
	} else {
		for _, r := range rows {
			stockQty[r.ProductID] = r.Quantity
			stockKnown[r.ProductID] = true
		}
	}
	for _, pid := range products {
		row := computeStockingCoefficient(branchID, pid, names[pid], byProduct[pid], purchaseDates[pid],
			stockQty[pid], stockKnown[pid], today, now)
		if err := s.upsertStockingCoefficient(ctx, &row); err != nil {
			s.logger.Warn("settlement-auditor: 落库备货系数失败", "product_id", pid, "err", err)
			continue
		}
		out[pid] = row
	}
	return out
}

// upsertStockingCoefficient 按 (branch_id, product_id) 写入或更新。
//
// ⚠️ 已存在 **manual** 行时**不覆盖**：人改过的参数是业务判断
// （见 model.StockingSourceManual 的注释）。
func (s *Service) upsertStockingCoefficient(ctx context.Context, row *model.StockingCoefficient) error {
	var existing model.StockingCoefficient
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", row.BranchID, row.ProductID).
		First(&existing).Error
	switch {
	case err == nil:
		if existing.Source == model.StockingSourceManual {
			*row = existing
			return nil
		}
		row.ID = existing.ID
		row.CreatedAt = existing.CreatedAt
		return s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{
			"product_name":     row.ProductName,
			"multiplier":       row.Multiplier,
			"period_days":      row.PeriodDays,
			"sample_days":      row.SampleDays,
			"avg_daily_sales":  row.AvgDailySales,
			"period_sales_qty": row.PeriodSalesQty,
			"receipt_count":    row.ReceiptCount,
			"interval_days":    row.IntervalDays,
			"daily_sales_cv":   row.DailySalesCV,
			"source":           model.StockingSourceAuto,
			"computed_at":      row.ComputedAt,
		}).Error
	case err == gorm.ErrRecordNotFound:
		row.ID = newUUID()
		return s.db.WithContext(ctx).Create(row).Error
	default:
		return err
	}
}

// ---- 审批流水线：备货事实 ----

// stockingFactsForAudit 为本次审批涉及的 SKU 组装备货事实（缺系数时现算）。
//
// 全部失败路径都 **只告警不阻断**（与毛利成本口径同一取舍）：
// 备货系数是 LLM 的参考事实，不是审批结论的必要条件。
func (s *Service) stockingFactsForAudit(
	ctx context.Context,
	branchID string,
	products []string,
	purchase map[string]purchaseBasis,
	sheetDates map[string]string,
	productSheets map[string][]string,
	names map[string]string,
	today time.Time,
) []StockingFact {
	if len(products) == 0 || s.cube == nil {
		return nil
	}
	sq, ok := s.cube.(stockingQuerier)
	if !ok {
		return nil
	}
	coefs, err := s.stockingCoefficientsOf(ctx, branchID, products)
	if err != nil {
		s.logger.Warn("settlement-auditor: 读备货系数失败,跳过备货评估", "err", err)
		return nil
	}
	missing := make([]string, 0, len(products))
	for _, pid := range products {
		if _, ok := coefs[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	estimated := s.ensureStockingCoefficients(ctx, branchID, missing, names, today)
	for pid, c := range estimated {
		coefs[pid] = c
	}

	stocks := make(map[string]decimal.Decimal, len(products))
	if rows, err := s.cube.GetStocks(ctx, branchID, products); err != nil {
		s.logger.Warn("settlement-auditor: 取库存失败,备货事实里库存记 0", "err", err)
	} else {
		for _, st := range rows {
			stocks[strings.TrimSpace(st.ProductID)] = st.Quantity
		}
	}

	// 每个商品的采样天数（用户口径：max(今天-实际收货日, 周期)×2）。
	type skuPlan struct {
		pid          string
		sampleDays   int
		period       int
		mult         decimal.Decimal
		firstReceipt string // 窗口内最早的实际收货日(有货天数起点;空=取不到)
	}
	plans := make([]skuPlan, 0, len(products))
	for _, pid := range products {
		c, ok := coefs[pid]
		if !ok {
			continue
		}
		period := c.PeriodDays
		if period <= 0 {
			period = stockingDefaultPeriodDays
		}
		// 该商品在本单涉及的采购单里**最晚**的实际收货日（取不到 ⇒ 今天）。
		var receipt time.Time
		// firstReceipt 是**最早**的实际收货日 —— 「有货天数」的起点（见 inStockDays）。
		firstReceipt := ""
		for _, sid := range productSheets[pid] {
			d := strings.TrimSpace(sheetDates[sid])
			if d == "" {
				continue
			}
			if firstReceipt == "" || d < firstReceipt {
				firstReceipt = d
			}
			if t, ok := parseCubeTime(d); ok && !t.After(today) && t.After(receipt) {
				receipt = t
			}
		}
		if receipt.IsZero() {
			receipt = today
		}
		daysSince := int(today.Sub(receipt).Hours() / 24)
		if daysSince < 0 {
			daysSince = 0
		}
		sampleDays := maxInt(daysSince, period) * 2
		if sampleDays < 2 {
			sampleDays = 2
		}
		if sampleDays > stockingSampleDaysCap {
			sampleDays = stockingSampleDaysCap
		}
		plans = append(plans, skuPlan{pid: pid, sampleDays: sampleDays, period: period, mult: c.Multiplier, firstReceipt: firstReceipt})
	}
	if len(plans) == 0 {
		return nil
	}
	// 按采样天数归组：一组一次 cube 查询（最多 8 组，超出用最长窗口合并）。
	sort.Slice(plans, func(i, j int) bool { return plans[i].sampleDays < plans[j].sampleDays })
	groups := make(map[int][]string)
	windowDays := make(map[string]int, len(plans))
	for _, p := range plans {
		groups[p.sampleDays] = append(groups[p.sampleDays], p.pid)
		windowDays[p.pid] = p.sampleDays
	}
	windows := make([]int, 0, len(groups))
	for d := range groups {
		windows = append(windows, d)
	}
	sort.Ints(windows)
	for len(windows) > stockingMaxSampleGroups {
		// 合并最长的两个窗口（取更长的那一个 ⇒ 日均偏低 ⇒ 只少拦不多拦）。
		last := windows[len(windows)-1]
		prev := windows[len(windows)-2]
		groups[last] = append(groups[last], groups[prev]...)
		for _, pid := range groups[prev] {
			windowDays[pid] = last
		}
		delete(groups, prev)
		windows = windows[:len(windows)-2]
		windows = append(windows, last)
	}

	salesByProduct := make(map[string]float64, len(plans))
	// lastSaleByProduct 是窗口内**最后一次有销量**的日期 —— 售罄日的估计
	//（已有库存时不用它，见 inStockDays）。
	lastSaleByProduct := make(map[string]string, len(plans))
	for days, pids := range groups {
		from := today.AddDate(0, 0, -days+1).Format("2006-01-02")
		to := today.AddDate(0, 0, 1).Format("2006-01-02")
		rows, err := sq.ListDailySales(ctx, branchID, pids, from, to)
		if err != nil {
			s.logger.Warn("settlement-auditor: 取备货采样窗口销量失败,该组跳过",
				"branch_id", branchID, "sample_days", days, "err", err)
			continue
		}
		for _, r := range rows {
			q, _ := r.Quantity.Float64()
			salesByProduct[r.ProductID] += q
			if r.Quantity.GreaterThan(decimal.Zero) {
				if d := normalizeCubeDate(r.BusinessDate); d > lastSaleByProduct[r.ProductID] {
					lastSaleByProduct[r.ProductID] = d
				}
			}
		}
	}

	facts := make([]StockingFact, 0, len(plans))
	for _, p := range plans {
		days := windowDays[p.pid]
		// 日平均销量的分母是**有货天数**，不是采样天数(2026-11-01 用户口径)。
		stockQty, stockKnown := stocks[p.pid]
		inStock := inStockDays(today.AddDate(0, 0, -days+1), days, stockQty, stockKnown,
			p.firstReceipt, lastSaleByProduct[p.pid], today)
		avg := 0.0
		if inStock > 0 {
			avg = salesByProduct[p.pid] / float64(inStock)
		}
		periodSales := avg * float64(p.period)
		mult, _ := p.mult.Float64()
		if mult <= 0 {
			mult = 1
		}
		reasonable := periodSales * mult
		basis := purchase[p.pid]
		f := StockingFact{
			ProductID:      p.pid,
			ProductName:    names[p.pid],
			Multiplier:     p.mult,
			PeriodDays:     p.period,
			SampleDays:     days,
			InStockDays:    inStock,
			AvgDailySales:  roundDecimal(avg, 3),
			PeriodSalesQty: roundDecimal(periodSales, 3),
			StockQty:       stocks[p.pid],
			// 净进货量(已扣退货);多日时另给 PurchaseDays/DailyAvgQty,
			// 比较用 CompareQty()(见 purchase_basis.go)。
			ReceivedQty:   basis.NetQty,
			ReturnedQty:   basis.ReturnedQty,
			PurchaseDays:  basis.Days,
			FirstDate:     basis.FirstDate,
			LastDate:      basis.LastDate,
			ReasonableQty: roundDecimal(reasonable, 3),
		}
		if basis.Days > 1 {
			f.DailyAvgQty = basis.CompareQty()
		} else if f.PurchaseDays <= 0 {
			f.PurchaseDays = 1
		}
		if avg > 0 {
			stock, _ := f.StockQty.Float64()
			recv, _ := f.ReceivedQty.Float64()
			f.CoverageDays = roundDecimal((stock+recv)/avg, 1)
		} else {
			f.CoverageDays = decimal.Zero
		}
		if _, ok := estimated[p.pid]; ok {
			f.Estimated = true
		}
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].ProductID < facts[j].ProductID })
	return facts
}

// stockingFactsMap 把事实列表转成 货号 → 事实 的 map（checkQuantity 判过量用）。
func stockingFactsMap(facts []StockingFact) map[string]StockingFact {
	if len(facts) == 0 {
		return nil
	}
	out := make(map[string]StockingFact, len(facts))
	for _, f := range facts {
		out[f.ProductID] = f
	}
	return out
}

// ---- 管理后台 CRUD ----

// StockingUpdateInput 是人工修改备货系数的入参。
type StockingUpdateInput struct {
	Multiplier decimal.Decimal
	PeriodDays int
	Note       string
	OperatorID string
}

// StockingRegenerateResult 是「重新生成」的结果。
type StockingRegenerateResult struct {
	// Regenerated 是本次重算成功的 SKU 数。
	Regenerated int `json:"regenerated"`
	// SkippedManual 是**人工修改过**、按规则跳过不覆盖的 SKU 数。
	SkippedManual int `json:"skipped_manual"`
	// Failed 是取数/落库失败的 SKU 数。
	Failed int `json:"failed"`
}

// ListStockingCoefficients 管理后台列表（按产品/名称包含匹配 + 分页）。
func (s *Service) ListStockingCoefficients(ctx context.Context, branchID, query string, limit, offset int) ([]model.StockingCoefficient, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Model(&model.StockingCoefficient{}).Where("branch_id = ?", branchID)
	if qs := strings.TrimSpace(query); qs != "" {
		like := "%" + qs + "%"
		q = q.Where("product_id ILIKE ? OR product_name ILIKE ?", like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.StockingCoefficient
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// UpdateStockingCoefficient 人工修改倍数/周期（Source 置 manual，重算不再覆盖）。
func (s *Service) UpdateStockingCoefficient(ctx context.Context, branchID, productID string, in StockingUpdateInput) (*model.StockingCoefficient, error) {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil, fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	period := in.PeriodDays
	if period < stockingMinPeriodDays || period > stockingMaxPeriodDays {
		return nil, fmt.Errorf("%w: 备货周期必须在 %d~%d 天之间", ErrInvalidInput, stockingMinPeriodDays, stockingMaxPeriodDays)
	}
	mult := in.Multiplier
	if mult.LessThan(decimal.NewFromFloat(stockingMinMultiplier)) || mult.GreaterThan(decimal.NewFromFloat(stockingMaxMultiplier)) {
		return nil, fmt.Errorf("%w: 备货倍数必须在 %s~%s 之间", ErrInvalidInput,
			decimal.NewFromFloat(stockingMinMultiplier), decimal.NewFromFloat(stockingMaxMultiplier))
	}
	var row model.StockingCoefficient
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrStockingNotFound
	}
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		"multiplier":  mult,
		"period_days": period,
		"source":      model.StockingSourceManual,
		"note":        truncate(in.Note, 256),
		"operator_id": in.OperatorID,
		// 人工改过之后，历史统计快照已不代表当前参数口径 ⇒ 标为本次修改时刻。
		"computed_at": s.now(),
	}
	if err := s.db.WithContext(ctx).Model(&row).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).First(&row, "id = ?", row.ID).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// DeleteStockingCoefficient 删除一个 SKU 的备货系数（下次审批会自动重算）。
func (s *Service) DeleteStockingCoefficient(ctx context.Context, branchID, productID string) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Delete(&model.StockingCoefficient{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStockingNotFound
	}
	return nil
}

// RegenerateStockingCoefficients 重新生成备货系数。
//
// productIDs 为空 ⇒ 重算本门店**已有系数**的全部 SKU（不主动扫全店商品：
// 全店上万 SKU 的逐日销量查询会打爆源库，也没必要给从不进货的商品建系数）。
//
// ⚠️ Source=manual 的行**跳过**（人工判断不被批量重算抹掉）。
func (s *Service) RegenerateStockingCoefficients(ctx context.Context, branchID string, productIDs []string, operatorID string) (*StockingRegenerateResult, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	if _, ok := s.cube.(stockingQuerier); !ok {
		return nil, fmt.Errorf("%w: cube 客户端不支持备货系数取数", ErrCubeUnavailable)
	}
	targets := dedupeStrings(productIDs)
	var skippedManual int
	if len(targets) == 0 {
		var rows []model.StockingCoefficient
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND source <> ?", branchID, model.StockingSourceManual).
			Limit(2000).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			targets = append(targets, r.ProductID)
		}
	}
	if len(targets) == 0 {
		return &StockingRegenerateResult{}, nil
	}
	// 人工行过滤（显式传了 product_id 也不覆盖 manual）。
	keep := make([]string, 0, len(targets))
	var manualIDs []string
	if err := s.db.WithContext(ctx).Model(&model.StockingCoefficient{}).
		Where("branch_id = ? AND product_id IN ? AND source = ?", branchID, targets, model.StockingSourceManual).
		Pluck("product_id", &manualIDs).Error; err != nil {
		return nil, err
	}
	manualSet := make(map[string]struct{}, len(manualIDs))
	for _, id := range manualIDs {
		manualSet[strings.TrimSpace(id)] = struct{}{}
	}
	for _, pid := range targets {
		if _, ok := manualSet[pid]; ok {
			skippedManual++
			continue
		}
		keep = append(keep, pid)
	}
	if len(keep) == 0 {
		return &StockingRegenerateResult{SkippedManual: skippedManual}, nil
	}

	names, err := s.cube.ListProductNames(ctx, branchID, keep)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取商品名失败,备货系数里名称留空", "err", err)
		names = nil
	}
	estimated := s.ensureStockingCoefficients(ctx, branchID, keep, names, s.now())
	res := &StockingRegenerateResult{SkippedManual: skippedManual}
	for _, pid := range keep {
		if _, ok := estimated[pid]; ok {
			res.Regenerated++
		} else {
			res.Failed++
		}
	}
	_ = operatorID // 系统重算不覆盖 operator_id（保留原来的维护留痕）。
	return res, nil
}

// ---- 小工具 ----

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// roundDecimal 把 float64 四舍五入成指定小数位的 decimal（展示/落库用）。
func roundDecimal(v float64, places int32) decimal.Decimal {
	return decimal.NewFromFloat(v).Round(places)
}

// newUUID 生成 36 位 uuid（与其它模型主键口径一致）。
func newUUID() string { return uuid.NewString() }
