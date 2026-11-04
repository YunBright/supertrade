// Package service / stocking_group.go —— 「组级」备货充足度事实（2026-11-01 用户口径）。
//
// # 为什么从「单 SKU 进货量 vs 合理备货量」改成「组级补货后库存 vs 可售期需求」
//
// 旧口径生产失准（2026-11-01 复盘 202 条 qty_deviation block）：
//
//	58% 的合理备货量 < 1、85% 的周期销售数 < 2 —— 低频 SKU 的分母天然塌向 0，
//
// 一次正常补货就被判成 ×13 的超量。而且旧口径只看「这一单进了多少」，
// 不看「店里还剩多少」：旧货还躺在仓库里，账面库存明明撑得住，还是按进货量判超量。
//
// 新口径只回答一个问题：
//
//	把这批货补进去之后，组里**真正可售**的量够卖多少天？
//	—— 超过这个品类的可售上限，才是"补过头"。
//
// # 三条硬口径（2026-11-02 修订）
//
//  1. **组**：组合/拆分商品（整件进货、拆散零售）按商品档案的父子关系归并，
//     库存/销量/进货都换算到**组基准单位**（通常是整件）再相加（见 units.go）。
//  2. **已入账 ≠ 本次补货**：收货日 <= 该商品库存行最后变动日（oper_date）⇒
//     这批货已经体现在当前库存里，**不再重复叠加**；只有之后到的货才算未入账。
//     多日合并付款的累计量也不能当成一次备货量：单批量级按收货日拆开看。
//  3. **陈列保有量**：货架上的正常陈列不是可售库存 ⇒
//     可售量 = 补货后库存 − 陈列保有量（P50 自动生成，人工可修正）。
//     有陈列费/堆头费时陈列量 ×2（不再放大可售上限天数）。
//     ⚠️ 单批最小包装进货也由陈列保有量覆盖，不再单独维护「一次进货下限」。
//  4. **可售上限**按品类档位（生鲜 7 天 / 冷藏 20 / 冷冻 30 / 饮料 45 / 日用 90），
//     优先按思迅类别自动标记、品名关键词兜底、人工可修正（product_profiles）。
//
// # 性能
//
// 固定 180 天窗口（不按单扩窗）一次聚合，按商品缓存 12 小时
// （stocking_fact_cache）；库存不缓存（它每个小时都在变）。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 组级备货判定的常量。
const (
	// stockingHistoryDays 是历史窗口长度（固定值，见包注释的"性能"一节）。
	stockingHistoryDays = 180
	// stockingRecentSaleDays 是"最近有没有卖动"的观察窗口。
	stockingRecentSaleDays = 30
	// stockingNewReceiptDays 是"刚收货"的宽限期：到货不足 7 天的 SKU 不走无销售口径。
	stockingNewReceiptDays = 7
	// stockingLongNoSaleDays 是"长期没动"的判定天数（有库存 + 无销售）。
	stockingLongNoSaleDays = 30
	// stockingPriceRiseRatio 是进货均价高于历史均价的提示阈值（10%）。
	stockingPriceRiseRatio = 0.10
	// stockingExcessAmountYuan 是"超量金额"的阻断阈值（元）。
	stockingExcessAmountYuan = 300
	// stockingLedgerGapMinQty 是账面结存差异的最小绝对量（小于它不提示）。
	stockingLedgerGapMinQty = 3
	// stockingLedgerGapRatio 是账面结存差异的相对阈值（进货量的 20%）。
	stockingLedgerGapRatio = 0.2
	// stockingCacheTTL 是历史聚合缓存的存活时间。
	stockingCacheTTL = 12 * time.Hour
	// stockingQueryChunk 是单次 cube 查询的商品数上限（IN 子句保护）。
	stockingQueryChunk = 120
	// stockingDisplayAllowanceMultiplier 是"有陈列费/堆头费"时陈列保有量的倍数。
	//
	// 2026-11-02 用户口径：陈列费/堆头费放宽的是**陈列量**，不是可售天数。
	stockingDisplayAllowanceMultiplier = 2
)

// groupQuerier 是组级备货判定额外需要的 cube 能力（可选接口）。
//
// ⚠️ 与 stockingQuerier 同理：用**类型断言**而不是加进 CubeQuerier，
// 既有单测的 fake 不必全部补实现；生产注入的 DaprCubeQuerier 都有。
type groupQuerier interface {
	ListItemCombos(ctx context.Context, branchID string) ([]ComboRelation, error)
	ListProductUnits(ctx context.Context, branchID string, productIDs []string) (map[string]ProductUnitInfo, error)
	ListProductCategories(ctx context.Context, branchID string, categoryIDs []string) (map[string]CategoryInfo, error)
	ListProductPurchaseHistory(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) ([]PurchaseHistoryRow, error)
}

// StockingGroupFact 是给审批结论/LLM 看的**一个商品组**的备货事实。
//
// ⚠️ 只放事实与算出来的量，不放"建议退货多少"这类指令性内容
// （用户口径 2026-11-01：只给事实，不输出建议）。
type StockingGroupFact struct {
	GroupKey    string
	ProductID   string // 组内代表货号（本次进货金额最大的那个，纯展示/落库用）
	Label       string // 渲染好的组描述（「名称-货号」，多成员时注明组合商品）
	MemberCount int
	// MemberIDs 是组内全部货号（组口径覆盖了这些货号，单 SKU 老口径要跳过它们）。
	MemberIDs []string
	// BaseUnit 是组基准单位名（通常是父商品的采购单位，如 件/提）。
	BaseUnit string

	// ---- 历史（180 天窗口）----
	SampleDays     int
	InStockDays    int
	SalesQty       decimal.Decimal // 组销量（基准单位）
	RecentSalesQty decimal.Decimal // 近 30 天组销量（判"最近有没有卖动"）
	// LossQty 是近 180 天组盘损（已审核报损/盘亏，基准单位）。
	// 组消耗 = SalesQty + LossQty —— 盘损也是货的去向。
	LossQty       decimal.Decimal
	AvgDailySales decimal.Decimal // 日均消耗 = (销量 + 盘损) / InStockDays
	LastReceipt   string          // 最后一次收货日（YYYY-MM-DD，含本次）
	// HistoryReceiptCount 是历史进货次数（**不含本次审批涉及的单据**）。
	HistoryReceiptCount int
	// SingleMedian 是历史单次进货量中位数（基准单位，判断"本单是不是特别大"）。
	SingleMedian decimal.Decimal

	// ---- 本次审批 ----
	ReceivedQty        decimal.Decimal // 本次净进货（基准单位，PI − RO，含已入账批次）
	ReceivedGross      decimal.Decimal
	ReturnedQty        decimal.Decimal
	ReceivedAmountYuan decimal.Decimal
	PurchaseDays       int
	FirstDate          string
	LastDate           string
	// PendingQty 是**未入账**的本次收货（补货后库存才需要加上它）。
	//
	// 判定口径（2026-11-02）：收货日 <= 该商品库存行最后变动日 ⇒ 已入账，
	// 当前库存里已经包含它，不能再加一次；只有之后的收货才是未入账。
	PendingQty decimal.Decimal
	// BookedQty 是本次收货里**已入账**的部分（只做事实展示，不参与补货后库存）。
	BookedQty decimal.Decimal
	// BatchCount / MaxBatchQty / BatchMedian 是本次收货按**收货日**分批后的量级事实。
	//
	// ⚠️ 多日合并付款的累计量不能当成"一次备货量"去比下限/中位数
	// （2026-11-02 用户口径）：伊利八连杯 102 组跨 52 天，按单批中位数只有 12 组。
	BatchCount  int
	MaxBatchQty decimal.Decimal
	BatchMedian decimal.Decimal

	// ---- 库存与判定 ----
	StockQty decimal.Decimal
	// DisplayQty 是**生效的陈列保有量**（已乘陈列费/堆头费倍数）。
	//
	// 可售量 = 补货后库存 − DisplayQty —— 货架上的陈列不是可售库存。
	DisplayQty    decimal.Decimal
	DisplaySource string // auto / manual
	// SellableQty 是可售量 = max(0, 补货后库存 − 陈列保有量)。
	SellableQty decimal.Decimal
	// StockAboveDisplay 是当前库存减去陈列保有量（判"长期无销售"用）。
	StockAboveDisplay decimal.Decimal
	CapDays           int             // 可售上限天数（组内最严）
	CapQty            decimal.Decimal // 可售上限量 = 日均 × CapDays
	AfterQty          decimal.Decimal // 补货后总量 = 库存 + 未入账收货
	CoverageDays      decimal.Decimal // 可售天数 = SellableQty / 日均（0 = 无销量）
	ExcessQty         decimal.Decimal // 超过可售上限的量（> 0 才有意义）
	ExcessYuan        decimal.Decimal // 超过部分的金额（按本单均价估）

	// ---- 其它事实 ----
	PriceThis      decimal.Decimal // 本单组均价（元/基准单位）
	PriceHistory   decimal.Decimal // 近 180 天组加权均价
	PriceRisePct   decimal.Decimal // 本单比历史涨了多少（%）
	LedgerGap      decimal.Decimal // 账面结存差异 = 进货 − 销售 − 库存
	AllowanceBoost bool            // 是否因陈列费/堆头费放大了可售上限
	// ZeroSalesCase 是"组内 180 天无销量"时的分类：
	//	recent_receipt（刚收货）/ new_product（首单）/ long_stock（长期无销售）
	//	/ ledger_gap（其余）
	ZeroSalesCase string
	// Unconvertible 表示组里有采购行无法换算到基准单位（单位不一致）。
	// ⚠️ 这时**不做数量阻断** —— 无法比较不等于超量。
	Unconvertible bool
	UnitNote      string
}

// ---- 历史聚合（缓存实体）----

// productHistoryReceipt 是一条历史采购明细（PI/RO 各一行）。
type productHistoryReceipt struct {
	SheetID    string          `json:"sheet_id"`
	Date       string          `json:"date"`
	Doc        string          `json:"doc"` // PI / RO
	Qty        decimal.Decimal `json:"qty"`
	Amount     decimal.Decimal `json:"amount"`
	UnitNo     string          `json:"unit_no"`
	UnitFactor decimal.Decimal `json:"unit_factor"`
}

// productHistory 是单个商品近 180 天的历史聚合（落 stocking_fact_cache）。
type productHistory struct {
	ProductID      string          `json:"product_id"`
	SalesQty       decimal.Decimal `json:"sales_qty"`
	RecentSalesQty decimal.Decimal `json:"recent_sales_qty"`
	// LossQty 是窗口内已审核的盘损数量（报损/盘亏），已按口径收敛（见 aggregateLoss）。
	//
	// ⚠️ 组消耗 = 销售 + 盘损（2026-11-01 用户口径）：盘损也是货的去向，
	// 忽略它会让"进了 10、卖了 1、报损 8"这种正常情况被判成压货。
	LossQty  decimal.Decimal         `json:"loss_qty"`
	Receipts []productHistoryReceipt `json:"receipts"`
	Unit     ProductUnitInfo         `json:"unit"`
}

// groupStockingInput 是纯计算函数的入参（不碰 IO，便于单测）。
type groupStockingInput struct {
	Products  []string
	Combos    []ComboRelation
	Units     map[string]ProductUnitInfo
	Histories map[string]*productHistory
	Stock     map[string]decimal.Decimal
	// StockUpdatedAt 是货号 → 库存行最后变动日（YYYY-MM-DD）。
	//
	// 缺省/空串 ⇒ 无法判断是否已入账 ⇒ 本次收货按"未入账"处理（旧行为）。
	StockUpdatedAt map[string]string
	// SheetDates 是采购单 id → 实际收货日（PI/RO 都尽量给）。
	SheetDates map[string]string
	// Categories 是末级类别编码 → 类别名（品类档位判定用）。
	Categories map[string]CategoryInfo
	PILines    []SheetLineRow
	ROLines    []SheetLineRow
	Purchases  map[string]purchaseBasis
	Names      map[string]string
	// Profiles 是已有的商品经营属性（branch 内）。
	Profiles map[string]model.ProductProfile
	// Displays 是已有的陈列保有量（组键 → 行）。
	Displays map[string]model.StockingDisplay
	// AllowanceBoost 表示本次审批有陈列费/堆头费（陈列保有量 ×2）。
	AllowanceBoost bool
	Today          string
}

// groupStockingResult 是组级计算的结果（含建议落库的行）。
type groupStockingResult struct {
	Facts []StockingGroupFact
	// Profiles 是需要 upsert 的商品经营属性（Source 由调用方按本次是否阻断决定）。
	Profiles map[string]model.ProductProfile
	// Displays 是需要 upsert 的陈列保有量（同上）。
	Displays map[string]model.StockingDisplay
	Members  []string
}

// groupStockingFactsForAudit 组装备次审批的**组级备货事实**。
//
// 全部失败路径都只告警不阻断：备货事实是研判依据，不是审批结论的必要条件。
func (s *Service) groupStockingFactsForAudit(
	ctx context.Context,
	branchID string,
	products []string,
	piLines, roLines []SheetLineRow,
	purchases map[string]purchaseBasis,
	sheetDates map[string]string,
	names map[string]string,
	allowance Allowance,
	now time.Time,
) *groupStockingResult {
	if len(products) == 0 || s.cube == nil {
		return nil
	}
	gq, ok := s.cube.(groupQuerier)
	if !ok {
		return nil
	}
	today := now.Format("2006-01-02")
	combos, err := gq.ListItemCombos(ctx, branchID)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取组合拆分关系失败,按单商品口径判定",
			"branch_id", branchID, "err", err)
		combos = nil
	}
	idx := buildProductGroups(products, combos)
	members := groupMembers(idx)
	units, err := s.loadProductUnits(ctx, branchID, members)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取商品单位失败,单位换算按 1:1",
			"branch_id", branchID, "err", err)
		units = nil
	}
	histories, stocks, stockUpdatedAt, err := s.loadProductHistories(ctx, branchID, members, today, now)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取历史聚合失败,备货事实不完整",
			"branch_id", branchID, "err", err)
	}
	// 类别字典（品类档位优先按思迅类别判，品名关键词兜底）。
	categoryIDs := make([]string, 0, len(units))
	seenCategory := make(map[string]struct{}, len(units))
	for _, u := range units {
		id := strings.TrimSpace(u.CategoryID)
		if id == "" {
			continue
		}
		if _, ok := seenCategory[id]; ok {
			continue
		}
		seenCategory[id] = struct{}{}
		categoryIDs = append(categoryIDs, id)
	}
	// ⚠️ 取不到类别只告警：回退品名关键词，判定方向不变。
	// 单独包一层是为了让"cube 没有该 model"也能走兜底（fakes/降级）。
	var categories map[string]CategoryInfo
	if cq, ok := s.cube.(interface {
		ListProductCategories(context.Context, string, []string) (map[string]CategoryInfo, error)
	}); ok && len(categoryIDs) > 0 {
		categories, err = cq.ListProductCategories(ctx, branchID, categoryIDs)
		if err != nil {
			s.logger.Warn("settlement-auditor: 取商品类别失败,品类档位按品名兜底",
				"branch_id", branchID, "err", err)
		}
	}
	// 成员商品名（品类档位按品名判定；缺的补一次查询）。
	nameMap := make(map[string]string, len(names)+len(members))
	for k, v := range names {
		nameMap[k] = v
	}
	missingNames := make([]string, 0, len(members))
	for _, pid := range members {
		if strings.TrimSpace(nameMap[pid]) == "" {
			missingNames = append(missingNames, pid)
		}
	}
	if len(missingNames) > 0 {
		extra, err := s.cube.ListProductNames(ctx, branchID, missingNames)
		if err != nil {
			s.logger.Warn("settlement-auditor: 取组合成员商品名失败,按货号判定",
				"branch_id", branchID, "err", err)
		}
		for pid, n := range extra {
			nameMap[pid] = n
		}
	}

	profiles := s.productProfilesOf(ctx, branchID, members)
	groupKeys := make([]string, 0, len(members))
	seenKey := make(map[string]struct{}, len(members))
	for _, pid := range members {
		k := idx.group(pid)
		if _, ok := seenKey[k]; ok {
			continue
		}
		seenKey[k] = struct{}{}
		groupKeys = append(groupKeys, k)
	}
	displays := s.stockingDisplaysOf(ctx, branchID, groupKeys)

	return computeGroupStockingFacts(groupStockingInput{
		Products:       products,
		Combos:         combos,
		Units:          units,
		Histories:      histories,
		Stock:          stocks,
		StockUpdatedAt: stockUpdatedAt,
		SheetDates:     sheetDates,
		Categories:     categories,
		PILines:        piLines,
		ROLines:        roLines,
		Purchases:      purchases,
		Names:          nameMap,
		Profiles:       profiles,
		Displays:       displays,
		AllowanceBoost: allowance.HasDisplayOrPromotion(),
		Today:          today,
	})
}

// computeGroupStockingFacts 是组级事实的纯计算（单测直接喂数据）。
func computeGroupStockingFacts(in groupStockingInput) *groupStockingResult {
	idx := buildProductGroups(in.Products, in.Combos)
	res := &groupStockingResult{
		Profiles: make(map[string]model.ProductProfile),
		Displays: make(map[string]model.StockingDisplay),
		Members:  groupMembers(idx),
	}

	// 本次审批涉及的采购单（历史聚合要排除它们，避免重复计入）。
	currentSheets := make(map[string]struct{}, len(in.PILines)+len(in.ROLines))
	for _, ln := range in.PILines {
		currentSheets[ln.SheetID] = struct{}{}
	}
	for _, ln := range in.ROLines {
		currentSheets[ln.SheetID] = struct{}{}
	}

	// 本次进货按商品聚合（数量已换算到组基准单位，金额是元）。
	byProduct := make(map[string]*purchaseAgg, len(in.Products))
	aggOf := func(pid string) *purchaseAgg {
		a, ok := byProduct[pid]
		if !ok {
			a = &purchaseAgg{batches: make(map[string]*purchaseBatch)}
			byProduct[pid] = a
		}
		return a
	}
	batchOf := func(a *purchaseAgg, ln SheetLineRow) *purchaseBatch {
		date := strings.TrimSpace(in.SheetDates[ln.SheetID])
		key := date
		if key == "" {
			// 没有实际收货日 ⇒ 按单据单独算一批（不把不同单合并成一个"批次"）。
			key = "sheet:" + ln.SheetID
		}
		b, ok := a.batches[key]
		if !ok {
			b = &purchaseBatch{Date: date}
			a.batches[key] = b
		}
		return b
	}
	for _, ln := range in.PILines {
		if ln.ProductID == "" {
			continue
		}
		a := aggOf(ln.ProductID)
		gk := idx.group(ln.ProductID)
		f, ok := lineGroupFactor(in.Units[ln.ProductID], ln.UnitNo, ln.UnitFactor, idx, gk, ln.ProductID)
		if !ok {
			a.unconvert = true
			a.notes = append(a.notes, unitMismatchNote(in.Units[ln.ProductID], ln.UnitNo))
		}
		q := ln.ReceivedQuantity
		if q.IsNegative() {
			q = q.Neg()
		}
		lineQty := q.Mul(f)
		a.gross = a.gross.Add(lineQty)
		a.amount = a.amount.Add(ln.TotalAmountYuan)
		b := batchOf(a, ln)
		b.gross = b.gross.Add(lineQty)
		b.amount = b.amount.Add(ln.TotalAmountYuan)
	}
	for _, ln := range in.ROLines {
		if ln.ProductID == "" {
			continue
		}
		a := aggOf(ln.ProductID)
		gk := idx.group(ln.ProductID)
		f, ok := lineGroupFactor(in.Units[ln.ProductID], ln.UnitNo, ln.UnitFactor, idx, gk, ln.ProductID)
		if !ok {
			a.unconvert = true
			a.notes = append(a.notes, unitMismatchNote(in.Units[ln.ProductID], ln.UnitNo))
		}
		q := ln.ReceivedQuantity
		if q.IsNegative() {
			q = q.Neg()
		}
		lineQty := q.Mul(f)
		a.returned = a.returned.Add(lineQty)
		// 明细金额在退货单上同样是正数（见 purchase_sheet_line schema）⇒ 相减。
		a.amount = a.amount.Sub(ln.TotalAmountYuan)
		b := batchOf(a, ln)
		b.returned = b.returned.Add(lineQty)
		b.amount = b.amount.Sub(ln.TotalAmountYuan)
	}

	// 每个组算一次。
	groupKeys := make([]string, 0, len(in.Products))
	seen := make(map[string]struct{}, len(in.Products))
	for _, pid := range in.Products {
		gk := idx.group(pid)
		if _, ok := seen[gk]; ok {
			continue
		}
		seen[gk] = struct{}{}
		groupKeys = append(groupKeys, gk)
	}
	for _, gk := range groupKeys {
		fact, profs, display := computeOneGroup(in, idx, gk, currentSheets, byProduct)
		if fact == nil {
			continue
		}
		res.Facts = append(res.Facts, *fact)
		for pid, p := range profs {
			if p.ProductID == "" {
				continue
			}
			res.Profiles[pid] = p
		}
		if display != nil {
			res.Displays[display.GroupKey] = *display
		}
	}
	sort.Slice(res.Facts, func(i, j int) bool {
		a, b := res.Facts[i], res.Facts[j]
		if a.ReceivedAmountYuan.GreaterThan(b.ReceivedAmountYuan) {
			return true
		}
		if b.ReceivedAmountYuan.GreaterThan(a.ReceivedAmountYuan) {
			return false
		}
		return a.GroupKey < b.GroupKey
	})
	return res
}

// purchaseBatch 是本次收货按**收货日**聚合的一批（组基准单位）。
type purchaseBatch struct {
	Date     string
	gross    decimal.Decimal
	returned decimal.Decimal
	amount   decimal.Decimal
}

func (b *purchaseBatch) net() decimal.Decimal { return b.gross.Sub(b.returned) }

// purchaseAgg 是本次进货的商品级聚合（数量已换算到组基准单位）。
type purchaseAgg struct {
	gross     decimal.Decimal
	returned  decimal.Decimal
	amount    decimal.Decimal
	unconvert bool
	notes     []string
	// batches 是 收货日/单据 → 该批净进货与金额（拆"多日累计"用）。
	batches map[string]*purchaseBatch
}

// computeOneGroup 计算一个商品组的全部事实（组内本次无进货 ⇒ 返回 nil）。
func computeOneGroup(
	in groupStockingInput,
	idx *productGroupIndex,
	gk string,
	currentSheets map[string]struct{},
	byProduct map[string]*purchaseAgg,
) (*StockingGroupFact, map[string]model.ProductProfile, *model.StockingDisplay) {
	members := idx.members(gk)
	if len(members) == 0 {
		return nil, nil, nil
	}
	factors := idx.factorOf[gk]

	gross, returned, amount := decimal.Zero, decimal.Zero, decimal.Zero
	pendingQty := decimal.Zero
	unconvert := false
	var notes []string
	salesQty, recentSalesQty, lossQty, stockQty := decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero
	histQty, histAmount := decimal.Zero, decimal.Zero
	histReceipts := make(map[string]struct{})
	perDate := make(map[string]decimal.Decimal)
	var batchSizes []decimal.Decimal
	lastReceipt := ""
	capDays := 0
	profiles := make(map[string]model.ProductProfile, len(members))
	repProduct, repAmount := "", decimal.Zero
	// 组内最晚的库存变动日：给**没有库存行**的成员兜底（组合/拆分里整件父商品
	// 常常没有独立库存行，货实际记在子商品上）。
	groupStockUpdatedAt := ""
	for _, member := range members {
		if u := strings.TrimSpace(in.StockUpdatedAt[member]); u > groupStockUpdatedAt {
			groupStockUpdatedAt = u
		}
	}

	for _, member := range members {
		gf := factors[member]
		if gf.IsZero() {
			gf = decimal.NewFromInt(1)
		}
		if a, ok := byProduct[member]; ok {
			gross = gross.Add(a.gross)
			returned = returned.Add(a.returned)
			amount = amount.Add(a.amount)
			if a.unconvert {
				unconvert = true
				notes = append(notes, a.notes...)
			}
			if a.amount.GreaterThan(repAmount) {
				repAmount = a.amount
				repProduct = member
			}
			// 已入账判定：收货日 <= 该商品库存行最后变动日 ⇒ 这批货已经体现在
			// 当前库存里，不能再加一次（2026-11-02 用户口径）。
			stockUpdatedAt := strings.TrimSpace(in.StockUpdatedAt[member])
			if stockUpdatedAt == "" {
				// 该成员没有库存行（组合父商品常见）⇒ 用组内最晚变动日兜底。
				stockUpdatedAt = groupStockUpdatedAt
			}
			for _, b := range a.batches {
				if b == nil {
					continue
				}
				date := strings.TrimSpace(b.Date)
				net := b.net()
				// ⚠️ 两个日期都缺失 ⇒ 无法判断 ⇒ 按"未入账"处理（保守：宁多算不少算）。
				booked := date != "" && stockUpdatedAt != "" && date <= stockUpdatedAt
				if !booked {
					pendingQty = pendingQty.Add(net)
				}
				// 单批量级只统计**进货**批次（退货单独冲减，不参与"一次进多少"）。
				if b.gross.GreaterThan(decimal.Zero) {
					batchSizes = append(batchSizes, b.gross)
				}
			}
		}
		if h, ok := in.Histories[member]; ok {
			salesQty = salesQty.Add(h.SalesQty.Mul(gf).Round(3))
			recentSalesQty = recentSalesQty.Add(h.RecentSalesQty.Mul(gf).Round(3))
			// 盘损也是货的去向：报损/盘亏的数量按同一换算率并进组消耗。
			lossQty = lossQty.Add(h.LossQty.Mul(gf).Round(3))
			for _, r := range h.Receipts {
				if _, cur := currentSheets[r.SheetID]; cur {
					continue
				}
				f, ok := unitBaseFactor(h.Unit, r.UnitNo, r.UnitFactor)
				if !ok {
					unconvert = true
					notes = append(notes, unitMismatchNote(h.Unit, r.UnitNo))
					f = decimal.NewFromInt(1)
				}
				f = f.Mul(gf)
				q := r.Qty
				if q.IsNegative() {
					q = q.Neg()
				}
				sign := decimal.NewFromInt(1)
				if r.Doc == "RO" {
					sign = decimal.NewFromInt(-1)
				}
				net := q.Mul(f).Mul(sign)
				histQty = histQty.Add(net)
				histAmount = histAmount.Add(r.Amount.Mul(sign))
				if r.Doc == "PI" {
					histReceipts[r.SheetID] = struct{}{}
					perDate[r.Date] = perDate[r.Date].Add(net)
					if r.Date > lastReceipt {
						lastReceipt = r.Date
					}
				}
			}
		}
		stockQty = stockQty.Add(in.Stock[member].Mul(gf).Round(3))
		// 品类档位（组内取最严）：人工改过的用人工值；否则按思迅类别自动判，
		// 不用库里可能过期的 auto 值（类别优先是 2026-11-02 新增的口径）。
		baseCap := 0
		if p, ok := in.Profiles[member]; ok && p.Source == model.ProfileSourceManual && p.CapDays > 0 {
			baseCap = p.CapDays
		}
		if baseCap == 0 {
			var cat *CategoryInfo
			if c, ok := in.Categories[strings.TrimSpace(in.Units[member].CategoryID)]; ok {
				cat = &c
			}
			_, baseCap, _ = categoryCapDaysFor(in.Names[member], cat)
		}
		if capDays == 0 || baseCap < capDays {
			capDays = baseCap
		}
		profiles[member] = suggestedProfile(in, member, idx.group(member))
	}
	if repProduct == "" && len(members) > 0 {
		repProduct = members[0]
	}
	received := gross.Sub(returned)
	if received.IsNegative() {
		received = decimal.Zero
	}
	if !received.GreaterThan(decimal.Zero) {
		return nil, nil, nil
	}

	// 本次收货日跨度（组内所有成员的实际收货日）。
	first, last := "", ""
	for _, member := range members {
		b, ok := in.Purchases[member]
		if !ok {
			continue
		}
		if b.FirstDate != "" && (first == "" || b.FirstDate < first) {
			first = b.FirstDate
		}
		if b.LastDate != "" && (last == "" || b.LastDate > last) {
			last = b.LastDate
		}
	}
	purchaseDays := 1
	if first != "" && last != "" && first != last {
		if n, ok := dateSpanInclusive(first, last); ok {
			purchaseDays = n
		}
	}

	// ⚠️ 陈列费/堆头费不再放大"可售上限天数"（2026-11-02 用户口径），
	// 改成放大**陈列保有量**（货架/堆头占的量本来就不该算可售）。

	// 有货天数：起点 = 组最早收货日；终点 = 今天（已售罄且近 30 天无销量 ⇒ 往前 30 天）。
	inStockDays := inStockDaysFor(in.Today, perDate, maxString(lastReceipt, last), stockQty, recentSalesQty)
	// 日均**消耗** = (销量 + 盘损) / 有货天数（2026-11-01 用户口径）。
	//
	// ⚠️ 只看销量会把"进了 10、卖了 1、报损 8"判成压货 —— 盘损同样是货的去向。
	consumption := salesQty.Add(lossQty)
	avgDaily := decimal.Zero
	if inStockDays > 0 {
		avgDaily = consumption.Div(decimal.NewFromInt(int64(inStockDays))).Round(4)
	}
	capQty := avgDaily.Mul(decimal.NewFromInt(int64(capDays))).Round(3)
	// 补货后库存 = 当前库存 + **未入账**收货（已入账的不再重复加）。
	afterQty := stockQty.Add(pendingQty).Round(3)

	// 陈列保有量：人工行优先；否则按历史单批进货量 P50 自动生成。
	// 存的是**未乘倍数**的基数，陈列费/堆头费在运行时 ×2。
	displayBase := decimal.Zero
	displaySource := model.ProfileSourceAuto
	if d, ok := in.Displays[gk]; ok && d.Source == model.ProfileSourceManual && d.DisplayQty.GreaterThan(decimal.Zero) {
		displayBase = d.DisplayQty
		displaySource = model.ProfileSourceManual
	} else {
		displayBase = medianOf(perDate)
		if !displayBase.GreaterThan(decimal.Zero) {
			displayBase = decimal.NewFromInt(1)
		}
	}
	displayQty := displayBase
	if in.AllowanceBoost {
		displayQty = displayQty.Mul(decimal.NewFromInt(stockingDisplayAllowanceMultiplier))
	}
	displayQty = displayQty.Round(3)

	sellableQty := afterQty.Sub(displayQty)
	if sellableQty.IsNegative() {
		sellableQty = decimal.Zero
	}
	sellableQty = sellableQty.Round(3)
	stockAboveDisplay := stockQty.Sub(displayQty).Round(3)
	coverageDays := decimal.Zero
	if avgDaily.GreaterThan(decimal.Zero) {
		coverageDays = sellableQty.Div(avgDaily).Round(2)
	}
	excess := sellableQty.Sub(capQty)
	if excess.IsNegative() {
		excess = decimal.Zero
	}

	// 单批量级事实（多日合并付款看"每一批多大"，不看累计）。
	batchCount := len(batchSizes)
	maxBatch := decimal.Zero
	batchMedian := decimal.Zero
	if len(batchSizes) > 0 {
		sort.Slice(batchSizes, func(i, j int) bool { return batchSizes[i].LessThan(batchSizes[j]) })
		maxBatch = batchSizes[len(batchSizes)-1]
		batchMedian = medianOfValues(batchSizes)
	}

	priceThis := decimal.Zero
	if received.GreaterThan(decimal.Zero) {
		priceThis = amount.Div(received).Round(4)
	}
	histPrice := decimal.Zero
	if histQty.GreaterThan(decimal.Zero) {
		histPrice = histAmount.Div(histQty).Round(4)
	}
	rise := decimal.Zero
	if histPrice.GreaterThan(decimal.Zero) && priceThis.GreaterThan(decimal.Zero) {
		rise = priceThis.Div(histPrice).Sub(decimal.NewFromInt(1)).
			Mul(decimal.NewFromInt(100)).Round(1)
	}
	singleMedian := medianOf(perDate)

	// 下限已并入陈列保有量（2026-11-02 用户口径）：单批最小包装进货由
	// 「可售量 = 补货后库存 − 陈列保有量」覆盖，不再单独维护免判下限。

	// 账面结存差异：进货 − 销售 − 盘损 − 库存（理想情况 ≈ 0，差得远说明账实有缺口）。
	ledgerGap := histQty.Add(received).Sub(salesQty).Sub(lossQty).Sub(stockQty).Round(3)

	zeroCase := ""
	if !salesQty.GreaterThan(decimal.Zero) && !recentSalesQty.GreaterThan(decimal.Zero) {
		switch {
		case last != "" && daysSince(last, in.Today) >= 0 && daysSince(last, in.Today) < stockingNewReceiptDays:
			zeroCase = "recent_receipt"
		case len(histReceipts) == 0:
			zeroCase = "new_product"
		case stockAboveDisplay.GreaterThan(decimal.Zero) && lastReceipt != "" &&
			daysSince(lastReceipt, in.Today) > stockingLongNoSaleDays:
			zeroCase = "long_stock"
		default:
			zeroCase = "ledger_gap"
		}
	}

	baseUnit := ""
	for _, m := range members {
		if factors[m].Equal(decimal.NewFromInt(1)) {
			baseUnit = strings.TrimSpace(in.Units[m].Unit)
			break
		}
	}
	if baseUnit == "" {
		baseUnit = strings.TrimSpace(in.Units[repProduct].Unit)
	}
	if baseUnit == "" {
		baseUnit = "单位"
	}

	fact := &StockingGroupFact{
		GroupKey:            gk,
		ProductID:           repProduct,
		Label:               productGroupLabel(in.Names[repProduct], repProduct, len(members)),
		MemberCount:         len(members),
		MemberIDs:           members,
		BaseUnit:            baseUnit,
		SampleDays:          stockingHistoryDays,
		InStockDays:         inStockDays,
		SalesQty:            salesQty.Round(3),
		RecentSalesQty:      recentSalesQty.Round(3),
		LossQty:             lossQty.Round(3),
		AvgDailySales:       avgDaily,
		LastReceipt:         maxString(lastReceipt, last),
		HistoryReceiptCount: len(histReceipts),
		SingleMedian:        singleMedian.Round(3),
		ReceivedQty:         received.Round(3),
		ReceivedGross:       gross.Round(3),
		ReturnedQty:         returned.Round(3),
		ReceivedAmountYuan:  amount.Round(2),
		PurchaseDays:        purchaseDays,
		FirstDate:           first,
		LastDate:            last,
		PendingQty:          pendingQty.Round(3),
		BookedQty:           received.Sub(pendingQty).Round(3),
		BatchCount:          batchCount,
		MaxBatchQty:         maxBatch.Round(3),
		BatchMedian:         batchMedian.Round(3),
		StockQty:            stockQty.Round(3),
		DisplayQty:          displayQty,
		DisplaySource:       displaySource,
		SellableQty:         sellableQty,
		StockAboveDisplay:   stockAboveDisplay,
		CapDays:             capDays,
		CapQty:              capQty,
		AfterQty:            afterQty,
		CoverageDays:        coverageDays,
		ExcessQty:           excess.Round(3),
		ExcessYuan:          excess.Mul(priceThis).Round(2),
		PriceThis:           priceThis,
		PriceHistory:        histPrice,
		PriceRisePct:        rise,
		LedgerGap:           ledgerGap,
		AllowanceBoost:      in.AllowanceBoost,
		ZeroSalesCase:       zeroCase,
		Unconvertible:       unconvert,
		UnitNote:            summarizeUnitNotes(notes),
	}
	var display *model.StockingDisplay
	if displaySource != model.ProfileSourceManual {
		display = &model.StockingDisplay{
			GroupKey:     gk,
			ProductID:    repProduct,
			ProductName:  in.Names[repProduct],
			DisplayQty:   displayBase.Round(3),
			SampleDays:   stockingHistoryDays,
			ReceiptCount: len(histReceipts),
		}
	}
	return fact, profiles, display
}

// suggestedProfile 生成商品的经营属性建议（自动标记）。
//
// ⚠️ 人工改过的行（Source=manual）不生成建议 ⇒ 自动标记永远不覆盖人的判断。
func suggestedProfile(in groupStockingInput, pid, groupKey string) model.ProductProfile {
	if p, ok := in.Profiles[pid]; ok && p.Source == model.ProfileSourceManual {
		return model.ProductProfile{}
	}
	var cat *CategoryInfo
	if c, ok := in.Categories[strings.TrimSpace(in.Units[pid].CategoryID)]; ok {
		cat = &c
	}
	class, capDays, shelfLife := categoryCapDaysFor(in.Names[pid], cat)
	return model.ProductProfile{
		ProductID:     pid,
		ProductName:   in.Names[pid],
		Class:         class,
		CapDays:       capDays,
		ShelfLifeDays: shelfLife,
	}
}

// inStockDaysFor 返回"真正有货"的天数（日平均销量的分母）。
//
// 起点 = 组最早收货日（不认识就退回今天）；终点 = 今天；
// 已售罄且近 30 天无销量 ⇒ 终点往前 30 天（货早就卖光了，不可能"有货"到现在）。
//
// ⚠️ 拿不到精确的"最后销售日"（sale_day 没有 max(date) 这种 measure），
// 用"近 30 天有没有销量"近似判断终点 —— 少算天数会让日均偏大 ⇒ 上限偏高
// ⇒ 更宽松；这里取的是保守方向（终点尽早），把天数算多、上限算低。
func inStockDaysFor(today string, perDate map[string]decimal.Decimal, lastReceipt string,
	stockQty, recentSales decimal.Decimal) int {
	start := ""
	for d := range perDate {
		if start == "" || d < start {
			start = d
		}
	}
	if lastReceipt != "" && (start == "" || lastReceipt < start) {
		start = lastReceipt
	}
	// 本次收货日也要算（它在 perDate 里已被排除 ⇒ 单独补）。
	end := today
	if !stockQty.GreaterThan(decimal.Zero) && !recentSales.GreaterThan(decimal.Zero) {
		if t, ok := parseDateOnly(today); ok {
			end = t.AddDate(0, 0, -stockingRecentSaleDays).Format("2006-01-02")
		}
	}
	if start == "" || start > end {
		return 1
	}
	n, ok := dateSpanInclusive(start, end)
	if !ok {
		return 1
	}
	return clampInt(n, 1, stockingHistoryDays)
}

// lineGroupFactor 返回「采购行 → 组基准单位」的总换算率。
//
// 不可换算时**仍返回组换算率**（让数量至少量级可读），由调用方打 Unconvertible。
func lineGroupFactor(info ProductUnitInfo, unitNo string, unitFactor decimal.Decimal,
	idx *productGroupIndex, groupKey, productID string) (decimal.Decimal, bool) {
	base, ok := unitBaseFactor(info, unitNo, unitFactor)
	if !ok {
		return idx.convert(groupKey, productID), false
	}
	return base.Mul(idx.convert(groupKey, productID)), true
}

// unitBaseFactor 返回「采购单位 → 商品基本单位」的换算率。
//
// 实测本店 Unit_factor 恒为 1（进货基本按销售单位录入），所以单位名不一致
// 就等于口径不可换算：拿"瓶"的数量去比"罐"的销量，只会算出错误的倍数。
func unitBaseFactor(info ProductUnitInfo, unitNo string, unitFactor decimal.Decimal) (decimal.Decimal, bool) {
	if unitFactor.IsPositive() && !unitFactor.Equal(decimal.NewFromInt(1)) {
		return unitFactor, true
	}
	if strings.TrimSpace(unitNo) == "" || strings.TrimSpace(info.Unit) == "" {
		// 档案缺信息：按 1:1（与源表 Unit_factor 的默认口径一致）。
		return decimal.NewFromInt(1), true
	}
	if strings.TrimSpace(unitNo) == strings.TrimSpace(info.Unit) {
		return decimal.NewFromInt(1), true
	}
	return decimal.NewFromInt(1), false
}

// unitMismatchNote 生成中性的单位说明（不写"错误""违规"这类判断词）。
func unitMismatchNote(info ProductUnitInfo, unitNo string) string {
	if strings.TrimSpace(unitNo) == "" {
		return "采购行未记录采购单位"
	}
	if strings.TrimSpace(info.Unit) == "" {
		return "商品档案未记录单位"
	}
	return fmt.Sprintf("采购单位「%s」与商品档案单位「%s」不一致", unitNo, info.Unit)
}

// summarizeUnitNotes 合并去重单位说明（最多两条，避免消息过长）。
func summarizeUnitNotes(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	uniq := make([]string, 0, 2)
	seen := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		uniq = append(uniq, n)
		if len(uniq) == 2 {
			break
		}
	}
	return strings.Join(uniq, "；")
}

// medianOf 返回按日期净进货量的中位数（空 ⇒ 0）。
func medianOf(perDate map[string]decimal.Decimal) decimal.Decimal {
	if len(perDate) == 0 {
		return decimal.Zero
	}
	vals := make([]decimal.Decimal, 0, len(perDate))
	for _, q := range perDate {
		vals = append(vals, q)
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i].LessThan(vals[j]) })
	return medianOfValues(vals)
}

// medianOfValues 返回**已排序**切片的中位数（空 ⇒ 0）。
func medianOfValues(vals []decimal.Decimal) decimal.Decimal {
	n := len(vals)
	if n == 0 {
		return decimal.Zero
	}
	if n%2 == 1 {
		return vals[n/2]
	}
	return vals[n/2-1].Add(vals[n/2]).Div(decimal.NewFromInt(2))
}

// maxString 返回较大的日期字符串（空串不参与比较）。
func maxString(a, b string) string {
	if a > b {
		return a
	}
	return b
}

// ---- 数据加载 ----

// groupMembers 返回所有涉及组的成员货号（已排序去重）。
func groupMembers(idx *productGroupIndex) []string {
	set := make(map[string]struct{})
	for _, members := range idx.membersOf {
		for _, m := range members {
			set[m] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// loadProductUnits 分批取商品单位/规格（cube IN 子句有上限）。
func (s *Service) loadProductUnits(ctx context.Context, branchID string, products []string) (map[string]ProductUnitInfo, error) {
	gq, ok := s.cube.(groupQuerier)
	if !ok {
		return nil, nil
	}
	out := make(map[string]ProductUnitInfo, len(products))
	for _, chunk := range chunksOf(products, stockingQueryChunk) {
		rows, err := gq.ListProductUnits(ctx, branchID, chunk)
		if err != nil {
			return out, err
		}
		for pid, info := range rows {
			out[pid] = info
		}
	}
	return out, nil
}

// loadProductHistories 取商品近 180 天历史聚合（先读缓存，缺的再打源库）。
//
// 库存**不走缓存**：它每个小时都在变，缓存 12 小时的库存会让判定失真。
func (s *Service) loadProductHistories(
	ctx context.Context, branchID string, products []string, today string, now time.Time,
) (map[string]*productHistory, map[string]decimal.Decimal, map[string]string, error) {
	out := make(map[string]*productHistory, len(products))
	stocks := make(map[string]decimal.Decimal, len(products))
	stockUpdatedAt := make(map[string]string, len(products))
	if len(products) == 0 {
		return out, stocks, stockUpdatedAt, nil
	}
	from := windowFrom(today)
	to := tomorrow(today)

	for _, chunk := range chunksOf(products, stockingQueryChunk) {
		rows, err := s.cube.GetStocks(ctx, branchID, chunk)
		if err != nil {
			return out, stocks, stockUpdatedAt, err
		}
		for _, r := range rows {
			// 按商品归并：cube 可能按 oper_date 拆成多行（多分店/多变动），
			// 库存要相加，最后变动日取最晚。
			stocks[r.ProductID] = stocks[r.ProductID].Add(r.Quantity)
			if u := strings.TrimSpace(r.UpdatedAt); u > stockUpdatedAt[r.ProductID] {
				stockUpdatedAt[r.ProductID] = u
			}
		}
	}

	out = s.cachedProductHistories(ctx, branchID, from, products, now)
	missing := make([]string, 0, len(products))
	for _, pid := range products {
		if _, ok := out[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	if len(missing) == 0 {
		return out, stocks, stockUpdatedAt, nil
	}
	gq, ok := s.cube.(groupQuerier)
	if !ok {
		return out, stocks, stockUpdatedAt, nil
	}
	var firstErr error
	for _, chunk := range chunksOf(missing, stockingQueryChunk) {
		units, err := gq.ListProductUnits(ctx, branchID, chunk)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		rows, err := gq.ListProductPurchaseHistory(ctx, branchID, chunk, from, to)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		sales, err := s.cube.SumSalesByProduct(ctx, branchID, chunk, from, to)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		recent, err := s.cube.SumSalesByProduct(ctx, branchID, chunk,
			windowFrom(today, stockingRecentSaleDays), to)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		byProduct := make(map[string]*productHistory, len(chunk))
		for _, pid := range chunk {
			byProduct[pid] = &productHistory{ProductID: pid, Unit: units[pid]}
		}
		for _, r := range rows {
			h := byProduct[r.ProductID]
			if h == nil {
				continue
			}
			h.Receipts = append(h.Receipts, productHistoryReceipt{
				SheetID:    r.SheetID,
				Date:       r.SheetDate,
				Doc:        r.DocumentType,
				Qty:        r.Quantity,
				Amount:     r.AmountYuan,
				UnitNo:     r.UnitNo,
				UnitFactor: r.UnitFactor,
			})
		}
		for _, r := range sales {
			if h := byProduct[r.ProductID]; h != nil {
				h.SalesQty = r.NetSaleQuantity
			}
		}
		for _, r := range recent {
			if h := byProduct[r.ProductID]; h != nil {
				h.RecentSalesQty = r.NetSaleQuantity
			}
		}
		// 盘损也是货的去向（组消耗 = 销售 + 盘损）。
		//
		// ⚠️ 读不到盘损**只告警不失败**：少算盘损 ⇒ 日均偏小 ⇒ 可售上限偏低
		// ⇒ 判定偏严，方向是安全的（不会因此放过一单）。
		if s.loss != nil {
			losses, err := s.loss.SumLossByProduct(ctx, branchID, chunk, from, to)
			if err != nil {
				s.logger.Warn("settlement-auditor: 取盘损失败,组消耗按不含盘损计算",
					"branch_id", branchID, "err", err)
			} else {
				for pid, q := range losses {
					if h := byProduct[pid]; h != nil {
						h.LossQty = q
					}
				}
			}
		}
		for pid, h := range byProduct {
			out[pid] = h
		}
	}
	if firstErr != nil {
		return out, stocks, stockUpdatedAt, firstErr
	}
	s.storeProductHistories(ctx, branchID, from, out, now)
	return out, stocks, stockUpdatedAt, nil
}

// cachedProductHistories 读 12 小时内的历史聚合缓存。
func (s *Service) cachedProductHistories(
	ctx context.Context, branchID, from string, products []string, now time.Time,
) map[string]*productHistory {
	out := make(map[string]*productHistory, len(products))
	if s.db == nil || len(products) == 0 {
		return out
	}
	var rows []model.StockingFactCache
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND window_from = ? AND expires_at > ? AND product_id IN ?",
			branchID, from, now, products).
		Find(&rows).Error
	if err != nil {
		s.logger.Warn("settlement-auditor: 读备货历史缓存失败,本次直接查源库",
			"branch_id", branchID, "err", err)
		return out
	}
	for _, r := range rows {
		var h productHistory
		if err := json.Unmarshal([]byte(r.Payload), &h); err != nil {
			continue
		}
		h.ProductID = r.ProductID
		out[r.ProductID] = &h
	}
	return out
}

// storeProductHistories 把本次算出的历史聚合写进缓存（失败只告警）。
func (s *Service) storeProductHistories(
	ctx context.Context, branchID, from string, hists map[string]*productHistory, now time.Time,
) {
	if s.db == nil {
		return
	}
	expires := now.Add(stockingCacheTTL)
	for pid, h := range hists {
		payload, err := json.Marshal(h)
		if err != nil {
			continue
		}
		row := &model.StockingFactCache{
			BranchID:   branchID,
			ProductID:  pid,
			WindowFrom: from,
			Payload:    string(payload),
			ComputedAt: now,
			ExpiresAt:  expires,
		}
		var existing model.StockingFactCache
		err = s.db.WithContext(ctx).
			Where("branch_id = ? AND product_id = ? AND window_from = ?", branchID, pid, from).
			First(&existing).Error
		switch {
		case err == nil:
			if err := s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{
				"payload":     row.Payload,
				"computed_at": now,
				"expires_at":  expires,
			}).Error; err != nil {
				s.logger.Warn("settlement-auditor: 更新备货历史缓存失败", "product_id", pid, "err", err)
			}
		case err == gorm.ErrRecordNotFound:
			row.ID = newUUID()
			if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
				s.logger.Warn("settlement-auditor: 写入备货历史缓存失败", "product_id", pid, "err", err)
			}
		}
	}
}

// productProfilesOf 取商品经营属性（不存在的不返回）。
func (s *Service) productProfilesOf(ctx context.Context, branchID string, products []string) map[string]model.ProductProfile {
	out := make(map[string]model.ProductProfile, len(products))
	if s.db == nil || len(products) == 0 {
		return out
	}
	var rows []model.ProductProfile
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id IN ?", branchID, products).
		Find(&rows).Error
	if err != nil {
		s.logger.Warn("settlement-auditor: 读商品经营属性失败,按品名自动标记",
			"branch_id", branchID, "err", err)
		return out
	}
	for _, r := range rows {
		out[r.ProductID] = r
	}
	return out
}

// stockingDisplaysOf 取组的陈列保有量（不存在的不返回）。
func (s *Service) stockingDisplaysOf(ctx context.Context, branchID string, groupKeys []string) map[string]model.StockingDisplay {
	out := make(map[string]model.StockingDisplay, len(groupKeys))
	if s.db == nil || len(groupKeys) == 0 {
		return out
	}
	var rows []model.StockingDisplay
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND group_key IN ?", branchID, groupKeys).
		Find(&rows).Error
	if err != nil {
		s.logger.Warn("settlement-auditor: 读陈列保有量失败,按历史单批中位数自动计算",
			"branch_id", branchID, "err", err)
		return out
	}
	for _, r := range rows {
		out[r.GroupKey] = r
	}
	return out
}

// ---- 小工具 ----

// membersSet 把成员列表转成集合（checkQuantity 的 Skip 用）。
func membersSet(members []string) map[string]struct{} {
	out := make(map[string]struct{}, len(members))
	for _, m := range members {
		out[m] = struct{}{}
	}
	return out
}

// quantityBlockedProducts 取信号里的**阻断级**数量类商品（信号带的是组代表货号）。
func quantityBlockedProducts(drafts []signalDraft) map[string]struct{} {
	out := make(map[string]struct{}, len(drafts))
	for _, d := range drafts {
		if d.Level != model.SignalBlock || d.ProductID == "" {
			continue
		}
		if d.SignalType != SigQtyDeviation && d.SignalType != SigPurchaseWithoutSales {
			continue
		}
		out[d.ProductID] = struct{}{}
	}
	return out
}

// blockedGroupKeys 返回本次被阻断的组键（自动标记据此写 pending）。
func blockedGroupKeys(facts []StockingGroupFact, blocked map[string]struct{}) map[string]bool {
	out := make(map[string]bool, len(facts))
	for _, f := range facts {
		if _, ok := blocked[f.ProductID]; ok {
			out[f.GroupKey] = true
		}
	}
	return out
}

// storeStockingSuggestions 落审批过程中产生的自动标记（product_profiles / stocking_displays）。
//
// # 口径（2026-11-01 用户口径）
//
//	「通过审批付款过程中触发自动标记是更好的办法」——
//	被动触发:只在本次审批真的走到这一步时写,不做全量定时重算。
//
//	本次有数量类阻断的组 ⇒ Source=pending（等人工在「待人工确认」清单里修正）;
//	其余 ⇒ auto。人工改过的行（manual）**永不覆盖**。
//
// ⚠️ 全部失败只告警:自动标记写不进去不影响本次审批结论。
func (s *Service) storeStockingSuggestions(ctx context.Context, branchID string,
	res *groupStockingResult, blocked map[string]bool, now time.Time) {
	if s.db == nil || res == nil {
		return
	}
	groupOf := make(map[string]string, len(res.Members))
	for _, f := range res.Facts {
		for _, m := range f.MemberIDs {
			groupOf[m] = f.GroupKey
		}
	}
	for pid, p := range res.Profiles {
		if pid == "" || p.ProductID == "" {
			continue
		}
		src := model.ProfileSourceAuto
		if blocked[groupOf[pid]] {
			src = model.ProfileSourcePending
		}
		var existing model.ProductProfile
		err := s.db.WithContext(ctx).
			Where("branch_id = ? AND product_id = ?", branchID, pid).
			First(&existing).Error
		switch {
		case err == nil:
			if existing.Source == model.ProfileSourceManual {
				continue
			}
			if err := s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{
				"product_name":    p.ProductName,
				"class":           p.Class,
				"cap_days":        p.CapDays,
				"shelf_life_days": p.ShelfLifeDays,
				"source":          src,
				"updated_at":      now,
			}).Error; err != nil {
				s.logger.Warn("settlement-auditor: 更新商品经营属性失败", "product_id", pid, "err", err)
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			p.ID = newUUID()
			p.BranchID = branchID
			p.Source = src
			p.CreatedAt, p.UpdatedAt = now, now
			if err := s.db.WithContext(ctx).Create(&p).Error; err != nil {
				s.logger.Warn("settlement-auditor: 写入商品经营属性失败", "product_id", pid, "err", err)
			}
		default:
			s.logger.Warn("settlement-auditor: 读商品经营属性失败", "product_id", pid, "err", err)
		}
	}
	for gk, dp := range res.Displays {
		if gk == "" {
			continue
		}
		src := model.ProfileSourceAuto
		if blocked[gk] {
			src = model.ProfileSourcePending
		}
		var existing model.StockingDisplay
		err := s.db.WithContext(ctx).
			Where("branch_id = ? AND group_key = ?", branchID, gk).
			First(&existing).Error
		switch {
		case err == nil:
			// 人工改过的陈列量**永不覆盖**（与可售天数的自动学习完全分开）。
			if existing.Source == model.ProfileSourceManual {
				continue
			}
			if err := s.db.WithContext(ctx).Model(&existing).Updates(map[string]any{
				"product_id":    dp.ProductID,
				"product_name":  dp.ProductName,
				"display_qty":   dp.DisplayQty,
				"sample_days":   dp.SampleDays,
				"receipt_count": dp.ReceiptCount,
				"source":        src,
				"updated_at":    now,
			}).Error; err != nil {
				s.logger.Warn("settlement-auditor: 更新陈列保有量失败", "group_key", gk, "err", err)
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			dp.ID = newUUID()
			dp.BranchID = branchID
			dp.Source = src
			dp.CreatedAt, dp.UpdatedAt = now, now
			if err := s.db.WithContext(ctx).Create(&dp).Error; err != nil {
				s.logger.Warn("settlement-auditor: 写入陈列保有量失败", "group_key", gk, "err", err)
			}
		default:
			s.logger.Warn("settlement-auditor: 读陈列保有量失败", "group_key", gk, "err", err)
		}
	}
}

// windowFrom 返回窗口起点（含）：today - (days-1)；
// 不传 days 时用 stockingHistoryDays。
func windowFrom(today string, days ...int) string {
	n := stockingHistoryDays
	if len(days) > 0 && days[0] > 0 {
		n = days[0]
	}
	t, ok := parseDateOnly(today)
	if !ok {
		return today
	}
	return t.AddDate(0, 0, -n+1).Format("2006-01-02")
}

// tomorrow 返回窗口终点（开区间）：today + 1 天。
func tomorrow(today string) string {
	t, ok := parseDateOnly(today)
	if !ok {
		return today
	}
	return t.AddDate(0, 0, 1).Format("2006-01-02")
}

// daysSince 返回 from 距 today 的自然日数（同一天 0；解析失败 -1）。
func daysSince(from, today string) int {
	f, okFrom := parseDateOnly(from)
	t, okTo := parseDateOnly(today)
	if !okFrom || !okTo {
		return -1
	}
	return int(t.Sub(f).Hours() / 24)
}

// chunksOf 把列表按 size 分批（IN 子句保护）。
func chunksOf(in []string, size int) [][]string {
	if size <= 0 || len(in) == 0 {
		return nil
	}
	out := make([][]string, 0, (len(in)+size-1)/size)
	for i := 0; i < len(in); i += size {
		end := i + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}
