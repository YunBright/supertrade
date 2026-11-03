package service

// internal/fresh-meat/service/counter_derive.go —— **按记录层推导猪肉台(柜台)余额**。
//
// # 为什么不用位置表(2026-11-04)
//
// 位置表(sku_stock_positions)的**柜台侧**是**只累加、不扣减**的:
//
//	搬上台 += / 外采 += / 大宗领用 −= / 过夜搬运置零。
//
// ⇒ 它**从来没有扣过零售卖出** ⇒ 柜台余额**只涨不跌**。
//
// 生产实测(店 00,2026-11-03):11-02 外采了 11.4kg 肋排并全部卖掉,
// 而位置表 `sku_stock_positions` 的 01401/counter 停在 **44.350 kg**
// (= 10-30/10-31/11-01/11-02 四笔外采之和)⇒ **全是幽灵货**:
// 门店在「库存」页会看到"猪肉台还有 44.35 kg 肋排",可那 44.35 kg 早就卖掉了。
//
// # 幽灵余额是怎么攒出来的(生产实测,店 00)
//
// 位置表柜台侧只在**过夜搬运**时置零(盘点夜:柜台清空、实盘进冷库)。
// 而**补录的外采**是在那之后写进位置表的:
//
//	四笔外采的 created_at 全是 11-03 09:30~09:32(+08)
//	(门店 11-03 上午一次性补录了 10-30/10-31/11-01/11-02 四天的外采)
//	⇒ 而 11-02 的收盘盘点是 11-03 00:27 提交的(柜台当时已置零)
//	⇒ 四笔外采全部写在置零之后 ⇒ 位置表 = 0 + 44.35 ⇒ 再也不掉。
//
// ⇒ **归属日 ≤ 锚点日的事件必须整体排除**:那一批货已经被当晚的实盘吸收了。
//
// # 口径(与冷库推导对称,见 cold_store_derive.go)
//
//	柜台(SKU, 现在) = Σ_{d ∈ 窗口}[ 搬上台(d) + 外采(d) + 分割产出(d)
//	                          − 零售卖出(d) − 大宗柜台领用(d) − 报损(d) − 搭赠(d) ]
//	                ⇒ **裁 0**(柜台上不可能有负的货)
//
//	窗口 = (最近一次日终盘点的归属日, 今天]
//
// # 为什么窗口从「最近一次盘点日之后」开始
//
// 柜台**每天夜里被清零**(过夜搬运:柜台置零、实盘进冷库)⇒ 柜台余额是
// **当天之内**的量,不是"当前余额"。
//
// ⇒ 上一次盘点之后柜台上有什么 = 上次盘点夜(柜台=0) + 之后搬上来多少
//   + 之后切出来多少 − 之后卖掉多少 − 之后大宗/报损/搭赠走了多少。
//
// ⚠️ **实盘不进柜台**:盘点那一刻数出来的柜台实盘,在夜里被**搬进冷库**
//   ⇒ 它是冷库链的"结转",不是柜台的起点(柜台那晚被置零了)。
//
// ⚠️ **窗口用「归属日」而不是「提交时刻」**:
//
//	门店补录往日的盘点(生产实测:11-02 的盘点 created_at = 11-03 00:27)
//	⇒ 按提交时刻取会把"刚补录的旧盘点"当成起点 ⇒ 柜台余额被算成一个历史值。
//	⇒ 按归属日取 ⇒ 起点永远是**最新的那一次日终**。
//
// # 「分割产出」为什么必须有
//
// 猪切出来的肉**上柜台** ⇒ 不计它 ⇒ 柜台上卖出量会超过搬上台的量
// ⇒ 差值被裁 0 ⇒ **柜台恒显示 0**,门店看不到自己刚切出来的货。
//
// ⇒ 产出取**引擎同一套口径**(`computeProducedBySku`:pig_cuts 实录 →
//   统计基线 p50 → LLM 兜底)⇒ 库存页与毛利看板讲的**是同一批肉**。
//
// # 裁 0 的理由
//
// 产出没补录时,卖出量可能超过锚点+台账 ⇒ 差值不是"柜台欠货",
// 而是"当天切出来的货已经卖掉了"⇒ 裁 0(与链上「永远是 ≥0」一致)。
//
// # 推不出来的情况
//
// 这家店**一张盘点单都没有** ⇒ 没有归零的起点 ⇒ 返回 errCounterNotDerivable,
// 调用方退回位置表(旧口径),并**大声**记日志。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// errCounterNotDerivable 柜台余额**推不出来**(这家店没有任何盘点单 ⇒ 没有归零的起点)。
//
// ⚠️ 与冷库的 errColdStoreNotDerivable 是**同一种降级**:
//   调用方退回位置表(旧口径),并**大声**记日志 —— 失败必须可见。
var errCounterNotDerivable = fmt.Errorf("柜台余额按记录层推不出来")

// counterDeriveMaxDays 窗口最多跨多少天。
//
// ⚠️ 窗口起点是「最近一次盘点」⇒ 正常只差 0~1 天。
//   差很多天(门店很久没盘点了)⇒ 起点早已不代表柜台上现在的货
//   ⇒ 与其给一个不可信的数字,不如退回位置表并记日志。
const counterDeriveMaxDays = 31

// counterDeriveDisabled 柜台侧推导的**开关**(判别力实测用)。
//
// ⚠️ **必须是运行期变量**(包级 var):写成 `if false {...}` 会被 Go 编译器
//   把整个分支消掉 ⇒ "去掉实现"其实没去掉 ⇒ 判别力实测是假的。
var counterDeriveDisabled = false

// counterWindowIncludeAnchor 窗口**包含锚点当天**(判别力实测用)。
//
// ⚠️ 同样是**运行期变量**(见上面的说明)。
//   打开它 ⇒ 窗口变成 (锚点日−1, 今天] ⇒ 盘点日当天的搬台会被**双算**
//   ⇒ 用例 TestCounter_StocktakeDayEvents_AreAbsorbed 应当变红。
var counterWindowIncludeAnchor = false

// deriveCounterBySku 推导某店**当前**的猪肉台(柜台)余额(**按 SKU**)。
//
// # 口径
//
//	柜台(SKU) = max(0, Σ_{d ∈ (最近一次盘点日, 今天]}(台账净额))
//	台账净额 = 搬上台 + 外采 + 分割产出 − 零售卖出 − 大宗柜台领用 − 报损 − 搭赠
//
// # 起点
//
// **最近一次日终盘点**的归属日(那一晚柜台被置零 ⇒ 柜台余额从 0 开始重新累计)。
//
// # 推不出来的情况
//
// 没有盘点单 ⇒ 没有归零的起点 ⇒ errCounterNotDerivable(调用方退回位置表)。
func (s *Service) deriveCounterBySku(ctx context.Context, branchID string) (map[string]decimal.Decimal, error) {
	if counterDeriveDisabled {
		// 判别力实测用的退路:整个推导绕开 ⇒ 退回位置表(旧口径)。
		counter, _, err := s.loadPositionBalances(ctx, branchID)
		return counter, err
	}
	days, err := s.counterWindowDays(ctx, branchID)
	if err != nil {
		return nil, err
	}
	ledger, err := s.loadCounterLedger(ctx, branchID, days)
	if err != nil {
		return nil, err
	}
	out := map[string]decimal.Decimal{}
	for sku, v := range ledger {
		if v.IsNegative() {
			// ⚠️ 柜台上不可能有负的货(见包注释「裁 0 的理由」)。
			out[sku] = decimal.Zero
			continue
		}
		out[sku] = v
	}
	return out, nil
}

// counterWindowDays 取柜台推导的**窗口**(营业日列表)。
//
// = (最近一次日终盘点的归属日, 今天]
//
// ⚠️ 起点按**归属日**取最大(不按提交时刻)—— 见包注释「窗口用归属日」。
func (s *Service) counterWindowDays(ctx context.Context, branchID string) ([]string, error) {
	startDay, err := s.loadCounterWindowStart(ctx, branchID)
	if err != nil {
		return nil, err
	}
	start, err := time.ParseInLocation("2006-01-02", startDay, s.bizTZOrUTC())
	if err != nil {
		return nil, fmt.Errorf("parse counter window start %q: %w", startDay, err)
	}
	if counterWindowIncludeAnchor {
		// 判别力实测:窗口多含一天 ⇒ 盘点日当天的搬台被双算。
		start = start.AddDate(0, 0, -1)
	}
	today := s.now().In(s.bizTZOrUTC())
	days := []string{}
	for d := start.AddDate(0, 0, 1); !d.After(today); d = d.AddDate(0, 0, 1) {
		if len(days) >= counterDeriveMaxDays {
			return nil, fmt.Errorf("%w: 起点(%s)离今天超过 %d 天", errCounterNotDerivable,
				startDay, counterDeriveMaxDays)
		}
		days = append(days, d.Format("2006-01-02"))
	}
	return days, nil
}

// loadCounterWindowStart 取柜台推导的**窗口起点**:最近一次日终盘点的归属日。
//
// ⚠️ 「最近一次」按**归属日**取最大(不是按提交时刻):
//
//	门店补录往日的盘点(生产实测:11-02 的盘点 created_at = 11-03 00:27)
//	⇒ 按提交时刻取会把「刚补录的旧盘点」当成起点 ⇒ 柜台余额被算成一个历史值。
//	⇒ 按归属日取 ⇒ 起点永远是**最新的那一次日终**。
func (s *Service) loadCounterWindowStart(ctx context.Context, branchID string) (string, error) {
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&takes).Error; err != nil {
		return "", fmt.Errorf("query stocktakes(counter window): %w", err)
	}
	startDay := ""
	for _, st := range takes {
		key := st.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02")
		if startDay == "" || key > startDay {
			startDay = key
		}
	}
	if startDay == "" {
		return "", fmt.Errorf("%w: 这家店没有任何盘点单", errCounterNotDerivable)
	}
	return startDay, nil
}

// loadCounterLedger 取窗口内每一天的**台账净额**(per SKU)。
//
// ⚠️ 逐项与位置表的柜台侧**同源**(同一批记录、同一套加减),
//   再补上位置表**从来没有**的两项:分割产出(+)、零售卖出(−)。
//
// ⚠️ **大宗只扣从柜台领用的那部分**:从冷库领用的货走冷库链
//   (见 cold_store_derive.go 的 bulkOut),扣在柜台上是重复扣。
func (s *Service) loadCounterLedger(ctx context.Context, branchID string, days []string) (
	map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	inWindow := map[string]bool{}
	for _, d := range days {
		inWindow[d] = true
	}
	add := func(sku string, kg decimal.Decimal) {
		sku = strings.TrimSpace(sku)
		if sku == "" {
			return
		}
		out[sku] = out[sku].Add(kg)
	}

	// ---- ① 搬上台(+) ----
	var loads []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&loads).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads(counter ledger): %w", err)
	}
	for _, l := range loads {
		if !inWindow[dateKey(l.BizDate)] {
			continue
		}
		add(l.CubeProductID, l.QtyKg)
	}

	// ---- ② 外采(+)(直接上猪肉台,见 external_purchase.go) ----
	var exts []model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&exts).Error; err != nil {
		return nil, fmt.Errorf("query external_purchases(counter ledger): %w", err)
	}
	for _, r := range exts {
		if !inWindow[dateKey(r.BizDate)] {
			continue
		}
		add(r.CubeProductID, r.QtyKg)
	}

	// ---- ③ 分割产出(+):猪切出来的肉上柜台 ----
	produced, err := s.loadCounterProducedInWindow(ctx, branchID, days)
	if err != nil {
		return nil, err
	}
	for sku, v := range produced {
		add(sku, v)
	}

	// ---- ④ 大宗从**柜台**领用(−)(交付口径) ----
	type bulkOutRow struct {
		SettleDate    datatypes.Date
		CutType       model.CutType
		CubeProductID string
		QtyKg         decimal.Decimal
		Location      string
	}
	var bulkOuts []bulkOutRow
	if err := s.db.WithContext(ctx).
		Table("bulk_settlement_allocations AS a").
		Select("s.settle_date AS settle_date, a.cut_type AS cut_type, "+
			"a.cube_product_id AS cube_product_id, a.qty_kg AS qty_kg, a.location AS location").
		Joins("JOIN bulk_settlements AS s ON s.id = a.settlement_id").
		Where("s.branch_id = ? AND s.mode = ?", branchID, BulkModeDelivery).
		Scan(&bulkOuts).Error; err != nil {
		return nil, fmt.Errorf("query bulk settlement allocations(counter ledger): %w", err)
	}
	if len(bulkOuts) > 0 {
		primary, err := s.primarySkuPerCut(ctx, branchID)
		if err != nil {
			return nil, err
		}
		for _, r := range bulkOuts {
			// ⚠️ location 为空 = 老数据 ⇒ 按**冷库**领用算(与冷库链同一判据)。
			if r.Location != string(model.StockLocationCounter) {
				continue
			}
			if !inWindow[dateKey(r.SettleDate)] {
				continue
			}
			sku := strings.TrimSpace(r.CubeProductID)
			if sku == "" {
				sku = primary[r.CutType]
			}
			add(sku, r.QtyKg.Neg())
		}
	}

	// ---- ⑤ 报损 / 搭赠(−)(货真的离店了) ----
	var wastes []model.WasteLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&wastes).Error; err != nil {
		return nil, fmt.Errorf("query waste_logs(counter ledger): %w", err)
	}
	for _, w := range wastes {
		if !inWindow[w.RecordedAt.In(s.bizTZOrUTC()).Format("2006-01-02")] {
			continue
		}
		add(w.CubeProductID, w.QtyKg.Neg())
	}

	// ---- ⑥ 零售卖出(−)—— **位置表从来没有扣的这一项** ----
	sold, err := s.loadCounterSoldInWindow(ctx, branchID, days)
	if err != nil {
		return nil, err
	}
	for sku, v := range sold {
		add(sku, v.Neg())
	}

	return out, nil
}

// loadCounterProducedInWindow 取窗口内**切出来的货**(per SKU)。
//
// ⚠️ 用**引擎同一套口径**(`computeProducedBySku`):
//   pig_cuts 实录 → 统计基线 p50 → LLM 兜底。
//   ⇒ 库存页的柜台量与毛利看板的入库量讲的是**同一批肉**。
//
// ⚠️ 归属日取**猪进场的营业日**(与引擎同一口径)⇒ 补录分割明细不改归属日。
func (s *Service) loadCounterProducedInWindow(ctx context.Context, branchID string, days []string) (
	map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	for _, day := range days {
		start, end := s.businessDayBounds(mustParseDateInLoc(day, s.bizTZOrUTC()))
		var pigs []model.WholePig
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
			Find(&pigs).Error; err != nil {
			return nil, fmt.Errorf("query whole_pigs(counter produced %s): %w", day, err)
		}
		if len(pigs) == 0 {
			continue
		}
		pigCost := make(map[string]decimal.Decimal, len(pigs))
		for _, p := range pigs {
			pigCost[p.ID] = p.PurchaseCostYuan
		}
		producedKg, _, _, _, err := s.computeProducedBySku(ctx, branchID, start, end, pigs, pigCost)
		if err != nil {
			return nil, fmt.Errorf("compute produced(counter, %s): %w", day, err)
		}
		for sku, v := range producedKg {
			if !v.IsPositive() {
				continue
			}
			out[sku] = out[sku].Add(v)
		}
	}
	return out, nil
}

// loadCounterSoldInWindow 取窗口内每一天的**零售卖出**(per SKU)。
//
// ⚠️ **只扣零售**:大宗出库走台账(见 loadCounterLedger ④)⇒ 这里再扣就是重复扣。
//
// ⚠️ **有源/无源分流**(与 loadSkuFlows 同一判据):
//   有源门店读 cube(思迅日汇总);无源门店读本地收银事件(line_sales_by_pigs)。
//   统收(source='bulk')**不扣**——它走大宗台账。
func (s *Service) loadCounterSoldInWindow(ctx context.Context, branchID string, days []string) (
	map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	if len(days) == 0 {
		return out, nil
	}
	skus, _, err := s.mappedSkus(ctx, branchID)
	if err != nil {
		return nil, err
	}
	useCube, err := s.salesReadPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	for _, key := range days {
		if useCube {
			rows, err := s.cubeSalesRowsForDay(ctx, branchID, key, skus)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if !r.QtyKg.IsPositive() {
					continue
				}
				out[r.SKU] = out[r.SKU].Add(r.QtyKg)
			}
			continue
		}
		var sales []model.LineSalesByPig
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND "+s.fmSaleDateExpr(), branchID, key).
			Find(&sales).Error; err != nil {
			return nil, fmt.Errorf("query line_sales_by_pigs(counter sold): %w", err)
		}
		for _, sa := range sales {
			if sa.OrderStatus != "S" || sa.Source == "bulk" || !sa.QtyKg.IsPositive() {
				continue
			}
			sku := strings.TrimSpace(sa.CubeProductID)
			if sku == "" {
				continue
			}
			out[sku] = out[sku].Add(sa.QtyKg)
		}
	}
	return out, nil
}

// loadCounterBalances 取**对外口径**的柜台余额(per SKU)。
//
// # 优先推导值,失败退回位置表
//
// ⚠️ **失败必须可见**:推不出来时**大声**记日志 ——
//   悄悄给一个旧口径的数 ⇒ 门店看到的还是那个 44.35kg 的幽灵余额,而没人知道。
//
// ⚠️ 退回的是**位置表**的柜台侧(只累加、不扣卖出)⇒ 它**可能偏大**
//   ⇒ 调用方要把它当"台账"看,不能当"库存"看。
func (s *Service) loadCounterBalances(ctx context.Context, branchID string) (
	map[string]decimal.Decimal, error) {
	derived, err := s.deriveCounterBySku(ctx, branchID)
	if err == nil {
		return derived, nil
	}
	s.pubLogger.Warn("loadCounterBalances: 柜台余额按记录层推导失败,退回位置表(旧口径)",
		"branch_id", branchID, "err", err)
	counter, _, perr := s.loadPositionBalances(ctx, branchID)
	if perr != nil {
		return nil, perr
	}
	return counter, nil
}
