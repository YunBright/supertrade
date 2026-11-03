package service

// internal/fresh-meat/service/cold_store_derive.go —— **按 SKU 推导冷库余额**。
//
// # 为什么不用位置表(2026-11-03 生产事故)
//
// 位置表(sku_stock_positions)的冷库侧是**累加**出来的:
// 过夜搬运 += / 搬台 -= / 大宗 += / 期初 +=。
//
// 而「过夜搬运」只在 `isToday`(归属日 == 日历日)时才跑 ⇒
// 门店的日终盘点**经常在午夜后提交**(生产实测:11-02 的盘点 created_at = 11-03 00:27,
// 归属日 11-02)⇒ 被判成「补录往日」⇒ **过夜搬运被跳过**
// ⇒ 那批货**永远进不了位置表的冷库**⇒ 搬台页只看到「冷库共 1 个 SKU / 2.40 kg」
// ⇒ 门店 11-03 要搬的 3 斤排骨(11-02 日终进冷库的 1.5kg)**搬不了**。
//
// ⇒ 冷库余额改成**按记录层推导**(盘点结转 + 搬台流水 + 大宗 + 搭赠 + 期初):
//   - 提交得再晚也算得对(补录往日同样生效);
//   - 历史数据自动修好(不用手工修数);
//   - 幂等(推导,不是累加)⇒ 同一张盘点重复提交不会翻倍。
//
// # 口径与按部位的链一致
//
// 逐项对齐 deriveColdChain(按部位,见 daily_balance.go)⇒ 同一批数据下
// Σ(某部位的 SKU 余额) == 链上该部位的余额。
//
// ⚠️ **两者必须一致**:毛利看板用的是按部位的链,搬台页用的是按 SKU 的推导
//   ⇒ 不一致 ⇒ 门店对不上账 ⇒ 「库存查询 70kg、看板 65kg」这种脏事。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// errColdStoreNotDerivable 冷库余额**推不出来**(有审计结转的店)。
//
// ⚠️ 这类店的链起点是**按部位**的审计快照 ⇒ 按 SKU 推不出来
//   ⇒ 调用方退回位置表(旧口径),并**大声**记日志。
var errColdStoreNotDerivable = fmt.Errorf("冷库余额按记录层推不出来")

// coldSkuTerms 某个营业日的冷库余额变动项(**按 SKU**)。
//
// ⚠️ 与 daily_balance.go 的 dailyBalanceTerms(**按部位**)逐项对应,
//   只是键从部位换成货号 ⇒ 口径不会走样。
type coldSkuTerms struct {
	// hasStocktake 当天**有没有盘点单**。
	//
	// ⚠️ 关键:盘点日夜里会做「过夜搬运」(柜台 → 冷库)⇒ 柜台的账面余额
	//   会回到冷库;**没有盘点的那天不做过夜搬运** ⇒ 搬上台的货还留在柜台上,
	//   冷库**不**加回。
	hasStocktake bool
	// countedCuts 当天**盘到了**哪些部位(键 = 部位)。
	//
	// ⚠️ 为什么按部位记(不是按 SKU):链的口径就是部位的 ——
	//   「盘了 0 kg」也算「盘过」⇒ 它的柜台实盘就是 0
	//   ⇒ 不能把账面搬台量加回冷库(否则搬超了/盘亏了会被算没发生过)。
	countedCuts map[string]bool
	// carryover **结转**(当天盘点盘出来的柜台实盘)⇒ +。
	carryover map[string]decimal.Decimal
	// counterLoad **搬上台** ⇒ −。
	counterLoad map[string]decimal.Decimal
	// bulkOut **大宗结算出库**(交付口径、从冷库领用的那部分)⇒ −。
	bulkOut map[string]decimal.Decimal
	// bulkIn **大宗手工进冷库** ⇒ +。
	bulkIn map[string]decimal.Decimal
	// giftOut **搭赠出库** ⇒ −(裁 0)。
	//
	// ⚠️ 报损(loss)**不进链** —— 报损是损耗,链的余额由**下一次盘点**的实盘纠正。
	giftOut map[string]decimal.Decimal
}

func newColdSkuTerms() *coldSkuTerms {
	return &coldSkuTerms{
		countedCuts: map[string]bool{},
		carryover:   map[string]decimal.Decimal{},
		counterLoad: map[string]decimal.Decimal{},
		bulkOut:     map[string]decimal.Decimal{},
		bulkIn:      map[string]decimal.Decimal{},
		giftOut:     map[string]decimal.Decimal{},
	}
}

// addSku 往某个变动项里加重量(只记**正数** —— 与 dailyBalanceTerms.add 一致)。
func (t *coldSkuTerms) addSku(m map[string]decimal.Decimal, sku string, kg decimal.Decimal) {
	sku = strings.TrimSpace(sku)
	if sku == "" || !kg.IsPositive() {
		return
	}
	m[sku] = m[sku].Add(kg)
}

// deriveColdStoreBySku 推导某店**当前**冷库余额(**按 SKU**)。
//
// # 口径(与按部位的链逐项一致)
//
//	冷库(D) = 冷库(D−1) + 结转(D) − 搬上台(D) − 大宗出库(D) + 大宗进库(D) − 搭赠(D)
//
//	- 结转(D)     = 当天盘点盘出来的**柜台实盘**(per SKU)
//	- 搬上台(D)   = 当天搬台流水(per SKU)
//	- 大宗出库(D) = 交付口径的统收结算里**从冷库领用**的那部分
//	- 大宗进库(D) = **手工**补录的大宗产出(source='bulk_manual')
//	- 搭赠(D)     = 搭赠出库(货真的离店了 ⇒ 冷库余额 −,裁 0)
//
// # 起点
//
// 期初结转的**生效日**(整日重置:链外的余额不许漏进来)。
//
// # 推不出来的情况
//
// 这家店有**审计结转** ⇒ 链的起点是按部位的审计快照 ⇒ 按 SKU 推不出来
// ⇒ 返回 errColdStoreNotDerivable,调用方退回位置表。
func (s *Service) deriveColdStoreBySku(ctx context.Context, branchID string) (map[string]decimal.Decimal, error) {
	// ① 这家店有审计结转吗?(有 ⇒ 链的起点不是期初 ⇒ 按 SKU 推不出来)
	cutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if cutoff != "" {
		return nil, fmt.Errorf("%w: 该店有审计结转(截止日 %s)⇒ 链的起点是按部位的快照", errColdStoreNotDerivable, cutoff)
	}

	// ② 变动项(按天、按 SKU)+ 部位映射
	terms, cutOfSku, err := s.loadColdTermsBySku(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// ③ 期初(按 SKU)
	openings, err := s.loadColdOpeningsBySku(ctx, branchID)
	if err != nil {
		return nil, err
	}

	// 链的**起点** = 最早一期初的生效日。
	chainStartDay := ""
	for day := range openings {
		if chainStartDay == "" || day < chainStartDay {
			chainStartDay = day
		}
	}

	// 有流水的日子 + 期初的日子,按时间排序。
	dates := make([]string, 0, len(terms)+len(openings))
	seen := map[string]bool{}
	for d := range terms {
		if !seen[d] {
			seen[d] = true
			dates = append(dates, d)
		}
	}
	for d := range openings {
		if !seen[d] {
			seen[d] = true
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)

	prev := map[string]decimal.Decimal{}
	for _, d := range dates {
		t := terms[d]
		cur := make(map[string]decimal.Decimal, len(prev))
		for sku, v := range prev {
			cur[sku] = v
		}
		// 链的起点这一天**整天重置**:链外的余额不许漏进来。
		if chainStartDay != "" && d == chainStartDay {
			cur = map[string]decimal.Decimal{}
		}
		// 期初:生效日 ⇒ 该货号的余额**重新从期初起算**。
		for sku, kg := range openings[d] {
			cur[sku] = kg
		}
		if t != nil {
			// 结转(+):盘到的货号用**柜台实盘**。
			for sku, v := range t.carryover {
				cur[sku] = cur[sku].Add(v)
			}
			// 搬上台(−):盘点日、且**没盘到的部位** ⇒ 账面柜台余额回冷库
			//   (柜台被置零 ⇒ 那批货其实还在冷库里)。
			for sku, v := range t.counterLoad {
				if t.hasStocktake {
					cut, ok := cutOfSku[sku]
					if !ok || !t.countedCuts[string(cut)] {
						cur[sku] = cur[sku].Add(v)
					}
				}
				cur[sku] = cur[sku].Sub(v)
			}
			for sku, v := range t.bulkOut {
				cur[sku] = cur[sku].Sub(v)
			}
			for sku, v := range t.bulkIn {
				cur[sku] = cur[sku].Add(v)
			}
			// 搭赠出库:货真的离店了 ⇒ 冷库余额 −(裁 0,与链同口径)。
			for sku, v := range t.giftOut {
				cur[sku] = cur[sku].Sub(v)
				if cur[sku].IsNegative() {
					cur[sku] = decimal.Zero
				}
			}
		}
		prev = cur
	}
	return prev, nil
}

// loadColdTermsBySku 载入某店**全部营业日**的冷库余额变动项(**按 SKU**)。
//
// ⚠️ 与 loadDailyTerms(按部位)逐项对应 ⇒ 口径一致。
func (s *Service) loadColdTermsBySku(ctx context.Context, branchID string) (
	map[string]*coldSkuTerms, map[string]model.CutType, error) {
	out := map[string]*coldSkuTerms{}
	get := func(day string) *coldSkuTerms {
		t, ok := out[day]
		if !ok {
			t = newColdSkuTerms()
			out[day] = t
		}
		return t
	}

	// ---- ① 盘点单:柜台实盘(结转)+ 当天盘过哪些部位 ----
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&takes).Error; err != nil {
		return nil, nil, fmt.Errorf("query stocktakes(cold terms): %w", err)
	}
	for _, st := range takes {
		t := get(st.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02"))
		t.hasStocktake = true
		if len(st.Cuts) == 0 {
			continue
		}
		var snaps []model.CutSnapshot
		if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
			// 解析不了 ⇒ 跳过这一天的结转(不能猜)。
			s.pubLogger.Warn("cold terms: 盘点单 cuts 解析失败,跳过该天的结转",
				"branch_id", branchID, "stocktake_id", st.ID, "err", err)
			continue
		}
		for _, sn := range snaps {
			if sn.CutType != "" {
				t.countedCuts[string(sn.CutType)] = true
			}
			t.addSku(t.carryover, sn.CubeProductID, sn.ActualRemainKg)
		}
	}

	// ---- ② 搬台流水(per SKU) ----
	var loads []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&loads).Error; err != nil {
		return nil, nil, fmt.Errorf("query counter_loads(cold terms): %w", err)
	}
	for _, l := range loads {
		t := get(dateKey(l.BizDate))
		// ⚠️ 「今天不搬台」那行是 0 kg 且**没有货号** ⇒ 自然被跳过。
		t.addSku(t.counterLoad, l.CubeProductID, l.QtyKg)
	}

	// ---- ③ 大宗结算出库(交付口径、且从冷库领用的那部分) ----
	//
	// ⚠️ allocation 上的 cube_product_id **可能是空的**(历史实现没写)
	//   ⇒ 归到该部位的**主 SKU**(大宗库存本来就记在主 SKU 上)。
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
		return nil, nil, fmt.Errorf("query bulk settlement allocations(cold terms): %w", err)
	}
	if len(bulkOuts) > 0 {
		primary, err := s.primarySkuPerCut(ctx, branchID)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range bulkOuts {
			// 只有**从冷库领用**的才减冷库链;从猪肉台领用的,搬上台那步已经减过了。
			if r.Location != "" && r.Location != string(model.StockLocationColdStore) {
				continue
			}
			sku := strings.TrimSpace(r.CubeProductID)
			if sku == "" {
				sku = primary[r.CutType]
			}
			t := get(dateKey(r.SettleDate))
			t.addSku(t.bulkOut, sku, r.QtyKg)
		}
	}

	// ---- ④ 大宗手工进冷库(结转台账 source='bulk_manual') ----
	type bulkInRow struct {
		OriginDate    datatypes.Date
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var bulkIns []bulkInRow
	if err := s.db.WithContext(ctx).
		Table("carryover_records").
		Select("origin_date AS origin_date, cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND source = ? AND location = ?",
			branchID, CarryoverSourceBulkManual, string(model.StockLocationColdStore)).
		Scan(&bulkIns).Error; err != nil {
		return nil, nil, fmt.Errorf("query carryover_records(bulk manual, cold terms): %w", err)
	}
	for _, r := range bulkIns {
		t := get(dateKey(r.OriginDate))
		t.addSku(t.bulkIn, r.CubeProductID, r.QtyKg)
	}

	// ---- ⑤ 搭赠出库(waste_type='gift';报损不进链) ----
	var gifts []model.WasteLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND waste_type = ?", branchID, model.WasteTypeGift).
		Find(&gifts).Error; err != nil {
		return nil, nil, fmt.Errorf("query waste_logs(gift, cold terms): %w", err)
	}
	if len(gifts) > 0 {
		primary, err := s.primarySkuPerCut(ctx, branchID)
		if err != nil {
			return nil, nil, err
		}
		for _, g := range gifts {
			sku := strings.TrimSpace(g.CubeProductID)
			if sku == "" {
				sku = primary[g.CutType]
			}
			t := get(g.RecordedAt.In(s.bizTZOrUTC()).Format("2006-01-02"))
			t.addSku(t.giftOut, sku, g.QtyKg)
		}
	}

	// 部位映射(SKU → 部位):判「盘没盘」要用。
	cutOfSku, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, nil, err
	}
	return out, cutOfSku, nil
}

// loadColdOpeningsBySku 载入**期初结转**(按 SKU、按生效日分组)。
//
// ⚠️ 期初在 `sku_stock_openings` 里是**按部位**记的;
//   按 SKU 记的那一份在**结转台账**里(source='opening' 的行:
//   货号 = 该部位的主 SKU、重量 = 该部位的期初量、产生日 = 期初生效日)
//   ⇒ 直接用它 ⇒ 与链上「期初生效日 ⇒ 该部位余额 = 期初量」完全一致。
func (s *Service) loadColdOpeningsBySku(ctx context.Context, branchID string) (
	map[string]map[string]decimal.Decimal, error) {
	type row struct {
		OriginDate    datatypes.Date
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var rows []row
	if err := s.db.WithContext(ctx).
		Table("carryover_records").
		Select("origin_date AS origin_date, cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND source = ? AND location = ?",
			branchID, CarryoverSourceOpening, string(model.StockLocationColdStore)).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query carryover_records(opening, cold terms): %w", err)
	}
	out := map[string]map[string]decimal.Decimal{}
	for _, r := range rows {
		sku := strings.TrimSpace(r.CubeProductID)
		if sku == "" || !r.QtyKg.IsPositive() {
			continue
		}
		day := dateKey(r.OriginDate)
		if out[day] == nil {
			out[day] = map[string]decimal.Decimal{}
		}
		out[day][sku] = r.QtyKg
	}
	return out, nil
}

// closingWindowHours 收盘窗口的宽限小时数(2026-11-03)。
//
// 门店的日终盘点**经常在午夜后提交**(生产实测:11-02 的盘点 created_at = 11-03 00:27,
// 归属日 11-02)⇒ 用「归属日 == 日历日」判「是不是今天的盘点」会把它判成**补录往日**
// ⇒ 过夜搬运(柜台 → 冷库)被跳过 ⇒ 冷库余额缺这一批(门店 11-03 搬不了那 3 斤排骨)。
//
// ⇒ 归属日 D 的盘点只要在 **D+1 04:00 之前**提交,就算「D 那天的收盘盘点」。
//
// ⚠️ 为什么不是「隔多久都算收盘」:真正**补录往日**(隔天才补录)时,
//   柜台已经摆着今天的货了 ⇒ 置零柜台会把今天的货也抹掉。
const closingWindowHours = 4

// isClosingStocktake 这张盘点是不是「归属日那天的**收盘盘点**」。
//
// 判定:提交时刻落在 [归属日 00:00, 归属日+1 00:00 + 宽限) 内。
//
// ⚠️ 与「归属日 == 日历日」**不是一回事**:门店 00:27 录的是**昨天**的收盘。
func (s *Service) isClosingStocktake(takenDate time.Time, now time.Time) bool {
	_, end := s.businessDayBounds(takenDate)
	deadline := end.Add(time.Duration(closingWindowHours) * time.Hour)
	return now.In(s.bizTZOrUTC()).Before(deadline)
}
