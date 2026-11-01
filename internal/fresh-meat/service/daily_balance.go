package service

// internal/fresh-meat/service/daily_balance.go —— **按营业日推导的结余**。
//
// # 为什么需要(2026-10-24 门店口径)
//
// 「冷库余额 = 上期冷库 + 本期**结转** − 本期**搬上台** − 本期**大宗结算出库**」
//
// ⇒ 位置表(sku_stock_positions)是「**当前余额**」口径 ⇒ 补录往日的盘点会打乱今天
// ⇒ 改成:每个营业日有一份**结余快照**(sku_stock_daily),
//   sku_cost_daily(成本日快照)也按营业日重算 ⇒ 补录往日 ⇒ 重算那天 ⇒ 后面的日期跟着变。
//
// # 推导链
//
//	冷库(D) = 冷库(D−1) + 结转(D) − 搬上台(D) − 大宗出库(D) + 大宗进冷库(D)
//
//   - 结转(D)     = 当天**盘点盘出来的柜台实盘**(门店口径:「盘点盘的是猪肉台」)
//   - 搬上台(D)   = 当天搬台流水(counter_loads,**两种搬法都记**)
//   - 大宗出库(D) = 当天**交付口径**的统收结算(bulk_settlement_allocations,
//                   且只算**从冷库领用**的那部分)
//   - 大宗进冷库(D) = 当天**手工**补录的大宗产出(stock_bags,source='bulk_manual')
//
// ⚠️ **日终盘点自动结转**不进冷库余额
//   (applyOvernightMove 已经把柜台实盘搬进冷库了 ⇒ 再加一次就是同一批货记两次)。
//
// # 期初(base)
//
// ⚠️ **期初按 0 起算**(冷库从空开始)。
// ⇒ 门店若在本功能之前已有冷库余额,首次上线要做一次"期初结转"或人工核对
//   (交付报告「需要拍板」一节)。
//
// # 与位置表的关系
//
// 位置表**不改**(它还被 ListStock 等查询用)⇒ 仍是"当前余额";
// 按营业日的结余放在 **sku_stock_daily**,成本日快照放在 **sku_cost_daily**。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// dailyBalanceTerms 某个营业日的冷库余额变动项(按 SKU)。
type dailyBalanceTerms struct {
	// hasStocktake 当天**有没有盘点单**。
	//
	// ⚠️ 关键:盘点日夜里会做"过夜搬运"(柜台 → 冷库)⇒ 柜台的账面余额
	//   会回到冷库;**没有盘点的那天不做过夜搬运** ⇒ 搬上台的货还留在柜台上,
	//   冷库**不**加回。
	hasStocktake bool
	// carryover 是**结转**(当天盘点盘出来的柜台实盘)⇒ +。
	carryover map[string]decimal.Decimal
	// countedCuts 是"当天**盘到了**哪些部位"(与 carryover 分开记)。
	//
	// ⚠️ 为什么必须分开(2026-10-24 负库存清零时发现):
	//   carryover 只记**正数**重量(0 kg 的部位不进 map)⇒ 不能拿它判断
	//   "这个部位盘没盘" ⇒ **盘了 0 kg** 的部位会被误判成"没盘"
	//   ⇒ 余额链把"账面柜台余额"加回冷库,再减掉搬台量 ⇒ **净 0**
	//   ⇒ 实际搬超了(负库存)看不出来,冷库余额偏高。
	//
	// ⇒ 用 countedCuts 判"盘没盘",carryover 只管重量。
	countedCuts map[string]bool
	// counterLoad 是**搬上台** ⇒ −。
	counterLoad map[string]decimal.Decimal
	// bulkOut 是**大宗结算出库**(交付口径)⇒ −。
	bulkOut map[string]decimal.Decimal
	// bulkIn 是**大宗手工进冷库** ⇒ +。
	bulkIn map[string]decimal.Decimal
	// giftOut 是**搭赠出库**(2026-10-31)⇒ −。
	//
	// ⚠️ 为什么搭赠要进**冷库链**(链是"店里还剩多少货"的物理账):
	//	搭赠的货**真的离店了** ⇒ 冷库余额必须减
	//	⇒ 不减 ⇒ 链上一直留着这批货 ⇒ **次日期初虚高** ⇒ 成本链被污染
	//	  (那批送出去的货会一直"存在"店里,越滚越大)。
	//
	// ⚠️ **裁 0**(与 R1「链上永远是 ≥0」一致):
	//	链**不记分割产出** ⇒ 刚切出来就送掉的货(猪肝)在链上本来就没有
	//	⇒ 减完会是负数 ⇒ 裁 0(不制造"冷库余额为负"的假告警)。
	giftOut map[string]decimal.Decimal
}

func newDailyBalanceTerms() *dailyBalanceTerms {
	return &dailyBalanceTerms{
		carryover:   map[string]decimal.Decimal{},
		countedCuts: map[string]bool{},
		counterLoad: map[string]decimal.Decimal{},
		bulkOut:     map[string]decimal.Decimal{},
		bulkIn:      map[string]decimal.Decimal{},
		giftOut:     map[string]decimal.Decimal{},
	}
}

// add 往某个**部位**的变动项里加重量。
func (t *dailyBalanceTerms) add(m map[string]decimal.Decimal, cut model.CutType, kg decimal.Decimal) {
	if cut == "" || !kg.IsPositive() {
		return
	}
	m[string(cut)] = m[string(cut)].Add(kg)
}

// loadDailyTerms 载入某店**全部营业日**的冷库余额变动项(**按部位**)。
//
// ⚠️ 粒度是**部位**(门店口径:「这个模块只管部位数据,不管 SKU」):
// 盘点单的每一行(SKU 级)按 cut_type 汇总、搬台流水按 cut_type、大宗按 cut_type。
func (s *Service) loadDailyTerms(ctx context.Context, branchID string) (map[string]*dailyBalanceTerms, error) {
	out := map[string]*dailyBalanceTerms{}
	get := func(key string) *dailyBalanceTerms {
		t, ok := out[key]
		if !ok {
			t = newDailyBalanceTerms()
			out[key] = t
		}
		return t
	}

	// ---- ① 结转:盘点单里的**柜台实盘**(按部位汇总) ----
	//
	// ⚠️ 2026-10-24 起 CutSnapshot.ActualRemainKg 是**柜台实盘**(不含冷库)
	// ⇒ 它就是门店口径里的"本期结转"。
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&takes).Error; err != nil {
		return nil, fmt.Errorf("query stocktakes(daily terms): %w", err)
	}
	for _, st := range takes {
		key := st.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02")
		t := get(key)
		t.hasStocktake = true
		var snaps []model.CutSnapshot
		if len(st.Cuts) == 0 {
			continue
		}
		if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
			// 盘点单的 cuts 解析不了 ⇒ 跳过这一天(不能猜)。
			s.pubLogger.Warn("daily balance: 盘点单 cuts 解析失败,跳过该天的结转",
				"branch_id", branchID, "stocktake_id", st.ID, "err", err)
			continue
		}
		for _, sn := range snaps {
			// 盘到了这个部位(**即使实盘是 0**)⇒ 记进 countedCuts。
			if sn.CutType != "" {
				t.countedCuts[string(sn.CutType)] = true
			}
			t.add(t.carryover, sn.CutType, sn.ActualRemainKg)
		}
	}

	// ---- ② 搬上台流水(按部位) ----
	var loads []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&loads).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads(daily terms): %w", err)
	}
	for _, l := range loads {
		t := get(dateKey(l.BizDate))
		t.add(t.counterLoad, l.CutType, l.QtyKg)
	}

	// ---- ③ 大宗结算出库(交付口径,按部位)----
	//
	// ⚠️ 只有 mode='delivery'(交付口径)才真的从冷库扣货
	// (mode='price_diff' 只记账不动库存,见 service/bulk.go)。
	//
	// ⚠️ allocation 上的 cube_product_id **可能是空的**(历史实现没写)
	//   ⇒ 直接用 allocation 的 **cut_type**(大宗本来就按部位记)。
	//
	// ⚠️ 2026-10-25:**只有从冷库领用的**才减冷库链。
	//   从猪肉台领用的那批货,冷库余额在"搬上台"那一步就已经减过了
	//   ⇒ 这里再减一次就是减重(冷库会越算越低)。
	//   (location 为空 = 老数据 ⇒ 按冷库算。)
	type bulkOutRow struct {
		SettleDate datatypes.Date
		CutType    model.CutType
		QtyKg      decimal.Decimal
		Location   string
	}
	var bulkOuts []bulkOutRow
	if err := s.db.WithContext(ctx).
		Table("bulk_settlement_allocations AS a").
		Select("s.settle_date AS settle_date, a.cut_type AS cut_type, a.qty_kg AS qty_kg, a.location AS location").
		Joins("JOIN bulk_settlements AS s ON s.id = a.settlement_id").
		Where("s.branch_id = ? AND s.mode = ?", branchID, BulkModeDelivery).
		Scan(&bulkOuts).Error; err != nil {
		return nil, fmt.Errorf("query bulk settlement allocations(daily terms): %w", err)
	}
	for _, r := range bulkOuts {
		if r.Location != "" && r.Location != string(model.StockLocationColdStore) {
			continue
		}
		t := get(dateKey(r.SettleDate))
		t.add(t.bulkOut, r.CutType, r.QtyKg)
	}

	// ---- ④ 大宗手工进冷库(按部位)----
	//
	// ⚠️ 2026-10-25:大宗台账(bulk_productions)已**停写** ⇒ 改从**结转记录(袋子)**读
	//   (source='bulk_manual' 的行 = 手工补录进冷库的货)。
	//
	// ⚠️ 只取**进冷库那一刻的重量**(qty_kg),不是"现在还剩多少"
	//   —— 剩多少由出库项(大宗出库 / 搬上台)去减。
	type bulkInRow struct {
		OriginDate    datatypes.Date
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var bulkIns []bulkInRow
	if err := s.db.WithContext(ctx).
		Table("stock_bags").
		Select("origin_date AS origin_date, cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND source = ? AND location = ?",
			branchID, BagSourceBulkManual, string(model.StockLocationColdStore)).
		Scan(&bulkIns).Error; err != nil {
		return nil, fmt.Errorf("query stock_bags(daily terms): %w", err)
	}
	cutOfSku, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range bulkIns {
		t := get(dateKey(r.OriginDate))
		t.add(t.bulkIn, cutOfSku[r.CubeProductID], r.QtyKg)
	}

	// ---- ⑤ **搭赠出库**(2026-10-31)----
	//
	// ⚠️ 只取 waste_type='gift' 的行(报损**不进链** —— 报损是损耗,
	//   链的余额由**下一次盘点**的实盘纠正,不需要单独扣)。
	//
	// ⚠️ 按**部位**记(链的 terms 一律以 cut_type 为键)⇒ 只给货号的搭赠
	//   用 sku → cut 映射反查(RecordWasteLog 已经把部位补齐了,这里兜底)。
	var gifts []model.WasteLog
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND waste_type = ?", branchID, model.WasteTypeGift).
		Find(&gifts).Error; err != nil {
		return nil, fmt.Errorf("query waste_logs(gift, daily terms): %w", err)
	}
	for _, g := range gifts {
		cut := g.CutType
		if cut == "" {
			cut = cutOfSku[g.CubeProductID]
		}
		t := get(g.RecordedAt.In(s.bizTZOrUTC()).Format("2006-01-02"))
		t.add(t.giftOut, cut, g.QtyKg)
	}

	return out, nil
}

// stockOpening 一条期初结转(某个**部位**在某个生效日的冷库期初余额)。
type stockOpening struct {
	dateKey string
	cutType model.CutType
	kg      decimal.Decimal
	// unitPrice 是**门店给的**单价(元/kg)—— 期初这批货当时的进价(2026-10-25)。
	//
	// ⚠️ 没有它,成本链的第一天期初金额就是 0 ⇒ 那批货**零成本** ⇒ 毛利虚高。
	unitPrice decimal.Decimal
}

// loadOpenings 取该店的期初结转(per 部位)。
//
// ⚠️ 一个部位一条(upsert 覆盖)⇒ 改了期初就是改链的起点。
func (s *Service) loadOpenings(ctx context.Context, branchID string) (map[string]stockOpening, error) {
	return s.loadOpeningsFrom(ctx, nil, branchID)
}

// loadOpeningsFrom 同 loadOpenings,但可指定事务(读**事务内**已改动的状态)。
//
// ⚠️ 改期初的事务里要用它:删掉一个部位的期初后,链的起点可能后移
//   ⇒ 算边界时必须看到"事务内已经删掉"的状态,否则会拿旧边界去清东西。
func (s *Service) loadOpeningsFrom(ctx context.Context, tx *gorm.DB, branchID string) (map[string]stockOpening, error) {
	if tx == nil {
		tx = s.db
	}
	var rows []model.SkuStockOpening
	if err := tx.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query sku_stock_openings: %w", err)
	}
	out := map[string]stockOpening{}
	for _, r := range rows {
		out[string(r.CutType)] = stockOpening{
			dateKey:   time.Time(r.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02"),
			cutType:   r.CutType,
			kg:        r.ColdStoreKg,
			unitPrice: r.UnitPriceYuan,
		}
	}
	return out, nil
}

// chainBase 余额链的**起点**:上次审计结转的快照(= 该截止日的日终冷库余额)。
//
// 返回 (基准日 "YYYY-MM-DD", 各 SKU 的日终冷库余额)。
//
// ⚠️ 没有审计过 ⇒ 基准日为空、余额为空 ⇒ 链从**期初结转**起算。
//
// ⇒ 链的长度 = 两次审计之间的天数(而不是开业以来所有天)⇒ 实时算的成本有上界。
func (s *Service) chainBase(ctx context.Context, branchID string) (string, map[string]decimal.Decimal, error) {
	cutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return "", nil, err
	}
	if cutoff == "" {
		return "", nil, nil
	}
	counter, cold, ok, err := s.dailyClosing(ctx, branchID, mustParseDateInLoc(cutoff, s.bizTZOrUTC()))
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, nil
	}
	// 快照的 total = 柜台 + 冷库 = 那天的**日终冷库余额**(链的值)。
	base := map[string]decimal.Decimal{}
	for sku, v := range cold {
		base[sku] = v.Add(counter[sku])
	}
	return cutoff, base, nil
}

// deriveColdChain 推导某店**每个营业日的日终冷库余额**(**按部位**)。
//
// key = 归属日("YYYY-MM-DD");value = **部位(cut_type)** → 日终冷库余额(kg)。
//
// # 起点
//
// ① **上次审计结转的快照**(基准日)⇒ 基准日之前的天已经冻结,不再重推;
// ② 没有审计过 ⇒ 从**期初结转**(按部位)起算。
//
// # overrideDate / overrideCarryover
//
// 盘点那一刻**当天**的结转还没落库(盘点单正在写)⇒ 用调用方给的实盘覆盖
// ⇒ 这样"盘点那一刻的冷库余额"也能算准(补录往日时尤其重要)。
//
// ⚠️ overrideCarryover 的 key 是**部位**(与 terms 一致)。
func (s *Service) deriveColdChain(ctx context.Context, branchID string,
	overrideDate string, overrideCarryover map[string]decimal.Decimal) (map[string]map[string]decimal.Decimal, error) {
	terms, err := s.loadDailyTerms(ctx, branchID)
	if err != nil {
		return nil, err
	}
	openings, err := s.loadOpenings(ctx, branchID)
	if err != nil {
		return nil, err
	}
	baseDay, baseCold, err := s.chainBase(ctx, branchID)
	if err != nil {
		return nil, err
	}
	dates := make([]string, 0, len(terms))
	for d := range terms {
		dates = append(dates, d)
	}
	for _, op := range openings {
		if _, ok := terms[op.dateKey]; !ok {
			terms[op.dateKey] = newDailyBalanceTerms()
			dates = append(dates, op.dateKey)
		}
	}
	if overrideDate != "" {
		if _, ok := terms[overrideDate]; !ok {
			terms[overrideDate] = newDailyBalanceTerms()
			dates = append(dates, overrideDate)
		}
		// 用调用方给的实盘覆盖当天的结转。
		terms[overrideDate].carryover = map[string]decimal.Decimal{}
		for sku, v := range overrideCarryover {
			terms[overrideDate].carryover[sku] = v
		}
	}
	sort.Strings(dates)

	out := map[string]map[string]decimal.Decimal{}
	prev := map[string]decimal.Decimal{}
	for cut, v := range baseCold {
		prev[cut] = v
	}
	// ⚠️ **链的起点 = 该店期初结转的最早生效日**(2026-10-30 加)。
	//
	//	这一天**整天重置**:门店重定义了基线日的全部库存 ⇒
	//	链外(上线前)的余额**不许**顺着"没录期初的部位"漏进来。
	//	实测(店 00):10-02 有一笔链外的搬台(猪手 9.2kg,上线后才补录的),
	//	旧实现只重置了门店录过期初的部位 ⇒ 猪手 −9.2 kg 一路带进基线日。
	chainStartDay := ""
	for _, op := range openings {
		if chainStartDay == "" || op.dateKey < chainStartDay {
			chainStartDay = op.dateKey
		}
	}
	for _, d := range dates {
		// 基准日之前的天已冻结 ⇒ 链不再重推(基准快照里已经含它们)。
		if baseDay != "" && d <= baseDay {
			continue
		}
		t := terms[d]
		cur := make(map[string]decimal.Decimal, len(prev))
		for cut, v := range prev {
			cur[cut] = v
		}
		// ⚠️ 链的起点这一天**整天重置**:链外的余额不许漏进来。
		if chainStartDay != "" && d == chainStartDay {
			cur = map[string]decimal.Decimal{}
		}
		// 期初结转:生效日 ⇒ 该**部位**的余额**重新从期初起算**。
		for _, op := range openings {
			if op.dateKey == d {
				cur[string(op.cutType)] = op.kg
			}
		}
		// 结转(+):盘到的部位用**柜台实盘**;没盘到的部位用**账面柜台余额**
		// (= 当天搬上台量)—— 与 applyOvernightMove 的行为一致
		// (柜台置零,冷库 += 柜台余额;盘到的部位用实盘覆盖账面值)。
		for cut, v := range t.carryover {
			cur[cut] = cur[cut].Add(v)
		}
		for cut, v := range t.counterLoad {
			// ⚠️ 只有**盘点日**才做过夜搬运 ⇒ 柜台的账面余额才回到冷库。
			//   没有盘点的那天,搬上台的货还留在柜台上 ⇒ 冷库不加回。
			//
			// ⚠️ "盘没盘"用 countedCuts 判(不是 carryover):
			//   盘了 **0 kg** 的部位也是"盘过"⇒ 它的柜台实盘就是 0
			//   ⇒ 不能把账面搬台量加回冷库(否则搬超了/盘亏了会被算没发生过)。
			if t.hasStocktake {
				if !t.countedCuts[cut] {
					cur[cut] = cur[cut].Add(v) // 账面柜台余额回冷库
				}
			}
			cur[cut] = cur[cut].Sub(v) // 搬上台:冷库 −
		}
		for cut, v := range t.bulkOut {
			cur[cut] = cur[cut].Sub(v)
		}
		for cut, v := range t.bulkIn {
			cur[cut] = cur[cut].Add(v)
		}
		// **搭赠出库**(2026-10-31):货真的离店了 ⇒ 冷库余额 −。
		//
		// ⚠️ **裁 0**(与 R1「链上永远是 ≥0」一致):链不记分割产出
		//   ⇒ 刚切出来就送掉的货(猪肝)在链上本来就没有 ⇒ 减完会负 ⇒ 裁 0。
		for cut, v := range t.giftOut {
			cur[cut] = cur[cut].Sub(v)
			if cur[cut].IsNegative() {
				cur[cut] = decimal.Zero
			}
		}
		out[d] = cur
		prev = cur
	}
	return out, nil
}

// chainValueAt 取链上**某天(含之前最近一天)**的日终冷库余额。
//
// ⚠️ 链上只有"有流水的日子"⇒ 没有流水的那天,余额 = 之前最近那天的值。
func chainValueAt(chain map[string]map[string]decimal.Decimal, key string) map[string]decimal.Decimal {
	if v, ok := chain[key]; ok {
		return v
	}
	best := ""
	for d := range chain {
		if d > key {
			continue
		}
		if best == "" || d > best {
			best = d
		}
	}
	if best == "" {
		return map[string]decimal.Decimal{}
	}
	return chain[best]
}

// coldBalanceAtStocktake 某个营业日**盘点那一刻**的冷库余额(**按部位**)。
//
// # 口径(门店口径)
//
//	冷库余额 = 上期冷库 + 本期结转 − 本期搬上台 − 本期大宗结算出库
//
// ⇒ 盘点那一刻的冷库 = **日终冷库 − 本期结转**(柜台那份在夜里才搬进冷库)。
//
// # carryover
//
// 当天的**柜台实盘**(本期结转,**按部位**)—— 盘点单还没落库时由调用方给
// (盘点落库后重算会得到同样的值)。
func (s *Service) coldBalanceAtStocktake(ctx context.Context, branchID string, bizDate time.Time,
	carryover map[string]decimal.Decimal) (map[string]decimal.Decimal, error) {
	key := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")
	chain, err := s.deriveColdChain(ctx, branchID, key, carryover)
	if err != nil {
		return nil, err
	}
	endOfDay := chain[key]
	out := map[string]decimal.Decimal{}
	for cut, v := range endOfDay {
		out[cut] = v.Sub(carryover[cut])
	}
	// 冷库里只有货、柜台没盘的部位:结转是 0 ⇒ 冷库余额 = 日终冷库。
	for cut := range carryover {
		if _, ok := out[cut]; !ok {
			out[cut] = decimal.Zero
		}
	}
	return out, nil
}

// dailyClosing 某个营业日的**日终结余**(**按部位**):日终柜台 + 日终冷库。
//
// 返回 (柜台, 冷库, 是否有该营业日的快照)。
//
// ⚠️ 没有快照 ⇒ ok=false(调用方回退到实时算的余额链)。
func (s *Service) dailyClosing(ctx context.Context, branchID string, bizDate time.Time) (
	counter, cold map[string]decimal.Decimal, ok bool, err error) {
	start, end := s.businessDayBounds(bizDate)
	var rows []model.CutStockDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Find(&rows).Error; err != nil {
		return nil, nil, false, fmt.Errorf("query cut_stock_daily: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, false, nil
	}
	counter = map[string]decimal.Decimal{}
	cold = map[string]decimal.Decimal{}
	for _, r := range rows {
		counter[string(r.CutType)] = r.CounterKg
		cold[string(r.CutType)] = r.ColdStoreKg
	}
	return counter, cold, true, nil
}

// closingForDay 某个营业日的**日终结余**(per-SKU):日终柜台 + 日终冷库。
//
// # 口径
//
// 日终(夜里"过夜搬运"之后)⇒ 柜台已清空、货都在冷库 ⇒
//
//	counter_kg = 0
//	cold_store_kg = 链的日终冷库余额(= 门店口径的「冷库余额」)
//	total_kg     = cold_store_kg
//
// ⚠️ 盘点**那一刻**的分摊(柜台实盘 / 当时冷库里多少)在盘点单里给
//
//	(actual_remain_kg / cold_store_kg)⇒ 两处口径不混。
//
// # ① 已审计结转 ⇒ 直接读快照(冻结的、快)
//
// # ② 没审计 ⇒ **实时算**余额链
//
// ⇒ 审计前**不快照** ⇒ 补录往日的盘点 ⇒ **后续日期的结余与毛利跟着变**
//
//	(门店口径:「任何日期补录都会导致后续日期的数据发生变化」—— 这是特性,不是 bug)。
//
// ⇒ 审计后那些天被冻结 ⇒ 读快照即可,不用再实时推链 ⇒ 实时算的成本有上界。
func (s *Service) closingForDay(ctx context.Context, branchID string, bizDate time.Time) (
	counter, cold map[string]decimal.Decimal, ok bool, err error) {
	key := bizDate.In(s.bizTZOrUTC()).Format("2006-01-02")
	cutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, nil, false, err
	}
	if cutoff != "" && key <= cutoff {
		return s.dailyClosing(ctx, branchID, bizDate)
	}
	// 实时算:链的日终冷库(夜里柜台清空 ⇒ 柜台余额为 0)。
	chain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return nil, nil, false, err
	}
	endOfDay := chainValueAt(chain, key)
	counter = map[string]decimal.Decimal{}
	cold = map[string]decimal.Decimal{}
	for cut, v := range endOfDay {
		cold[cut] = v
	}
	return counter, cold, true, nil
}

// recomputeCostSnapshots 重算**某个营业日起**的成本日快照(**部位级**:cut_cost_daily)。
//
// # 做什么
//
// 重算这些天里**有盘点单**的天的成本日快照 ⇒ 补录往日 ⇒ 那天的快照变了,
// 后面的日期跟着变(链式:次日期初 = 上日期末)。
//
// # 不做什么
//
// ⚠️ **不落按营业日的结余快照**(cut_stock_daily)—— 那是**审计结转**的产物
//
//	(审计前实时算、不快照;见 service/biz_audit.go)。
//
// ⚠️ **不动位置表**(sku_stock_positions)⇒ 今天的"当前余额"不会被补录写脏。
// ⚠️ **不动 cut_cost_states**(当前成本状态)⇒ 它只由"今天那次盘点"刷新。
func (s *Service) recomputeCostSnapshots(ctx context.Context, branchID string, fromDate time.Time) error {
	chain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return err
	}
	fromKey := fromDate.In(s.bizTZOrUTC()).Format("2006-01-02")

	// 每天**有没有盘点单**(只有有盘点单的日子才有成本快照)。
	_, hasTake, err := s.loadCarryoverByDay(ctx, branchID)
	if err != nil {
		return err
	}

	dates := make([]string, 0, len(chain))
	for d := range chain {
		if d >= fromKey {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)

	for _, d := range dates {
		// 只有**当天有盘点单**的日子才重算成本快照(没盘点的那天算出来也是"推演值")。
		if !hasTake[d] {
			continue
		}
		// ⚠️ 这里**逐日重算**单价基线(priceRecomputeOn)⇒ 门店界面的基线当场跟上
		//   (通常只有一天有盘点单 ⇒ 重算次数 = 1,可接受)。
		if _, err := s.persistCutCostDaily(ctx, branchID, mustParseDateInLoc(d, s.bizTZOrUTC()), false,
			priceRecomputeOn); err != nil {
			s.pubLogger.Error("recomputeCostSnapshots: 成本日快照重算失败",
				"branch_id", branchID, "biz_date", d, "err", err)
		}
	}
	// ⚠️ **链是逐日接续的** ⇒ 变更会向下传播(今天的产出变了 ⇒ 明天的期初也变)。
	//   上面的快路径只重算"有盘点单"的天 ⇒ **后面的天会过期**。
	//   ⇒ 后台把整条链物化一遍(不绑盘点),让门店看到的成本是新的。
	s.ScheduleChainMaterialize(ctx, branchID, fromDate)
	return nil
}

// materializeDailyBalances 落**按营业日的结余快照**(sku_stock_daily)—— 审计结转的产物。
//
// # 为什么只在审计时落
//
// 审计前**实时算**(不快照)⇒ 补录往日的盘点 ⇒ 后续日期的结余跟着变(门店口径)。
// 审计时把截止日期及之前的每一天**固化**成快照 ⇒ 之后那些天被冻结、读快照即可。
//
// ⚠️ 快照的口径:**日终**(夜里"过夜搬运"之后)⇒ 柜台 0、货都在冷库:
//
//	counter_kg = 0;cold_store_kg = 链的日终冷库余额;total_kg = cold_store_kg。
//
// ⚠️ 盘点**那一刻**的分摊(柜台实盘 / 当时冷库里多少)在盘点单里给。
func (s *Service) materializeDailyBalances(ctx context.Context, branchID string, cutoff time.Time) error {
	chain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return err
	}
	_, hasTake, err := s.loadCarryoverByDay(ctx, branchID)
	if err != nil {
		return err
	}
	cutoffKey := cutoff.In(s.bizTZOrUTC()).Format("2006-01-02")
	now := s.now()
	for d, cold := range chain {
		if d > cutoffKey || !hasTake[d] {
			continue
		}
		bizDate := datatypesDate(mustParseDateInLoc(d, s.bizTZOrUTC()))
		for cut, v := range cold {
			rec := &model.CutStockDaily{
				ID:          CutStockDailyID(branchID, d, cut),
				BranchID:    branchID,
				BizDate:     bizDate,
				CutType:     model.CutType(cut),
				CounterKg:   decimal.Zero, // 日终柜台已清空
				ColdStoreKg: v,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			rec.TotalKg = rec.ColdStoreKg
			if err := s.upsertCutStockDaily(ctx, rec); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadCarryoverByDay 取每天的**柜台实盘**(结转,**按部位**)与"当天有没有盘点单"。
func (s *Service) loadCarryoverByDay(ctx context.Context, branchID string) (
	map[string]map[string]decimal.Decimal, map[string]bool, error) {
	carryover := map[string]map[string]decimal.Decimal{}
	hasTake := map[string]bool{}
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&takes).Error; err != nil {
		return nil, nil, fmt.Errorf("query stocktakes(carryover): %w", err)
	}
	for _, st := range takes {
		key := st.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02")
		hasTake[key] = true
		if _, ok := carryover[key]; !ok {
			carryover[key] = map[string]decimal.Decimal{}
		}
		var snaps []model.CutSnapshot
		if len(st.Cuts) == 0 {
			continue
		}
		if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
			continue
		}
		for _, sn := range snaps {
			if sn.CutType == "" {
				continue
			}
			cut := string(sn.CutType)
			carryover[key][cut] = carryover[key][cut].Add(sn.ActualRemainKg)
		}
	}
	return carryover, hasTake, nil
}

// upsertCutStockDaily 写一行按营业日的结余快照(幂等)。
func (s *Service) upsertCutStockDaily(ctx context.Context, rec *model.CutStockDaily) error {
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}, {Name: "biz_date"}, {Name: "cut_type"}},
		DoUpdates: clause.Assignments(map[string]any{
			"counter_kg":    rec.CounterKg,
			"cold_store_kg": rec.ColdStoreKg,
			"total_kg":      rec.TotalKg,
			"updated_at":    rec.UpdatedAt,
		}),
	}).Create(rec).Error; err != nil {
		return fmt.Errorf("upsert cut_stock_daily %s: %w", rec.ID, err)
	}
	return nil
}

// mustParseDateInLoc 解析 "YYYY-MM-DD"(内部用,失败退化成零值)。
//
// ⚠️ **必须用营业日界时区**解析:gorm.io/datatypes 的 Date 落库时保留**时区表示**
// ⇒ 写("+08:00 的零点")与读(同一构造)必须逐字一致,否则等值查询查不到。
func mustParseDateInLoc(key string, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02", key, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}
