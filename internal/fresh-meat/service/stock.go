// Package service / stock.go —— 库存位置(猪肉台 / 冷库)与 SKU 成本状态。
//
// # 位置模型(2026-10-23 与门店确认)
//
//	counter    —— 猪肉台:当日在售
//	cold_store —— 冷库:过夜存放 / 分割副产(板油) / 待统收
//
// 「全店」是**查询口径**(= counter + cold_store),**不提供"只查冷库"**。
//
// # 位置流水
//
//	搬上台(冷库→柜台)   冷库 −T ; 柜台 +T      ← 全店不变
//	副产入冷库          冷库 +kg                ← 全店增加
//	酒店统收(冷库→出)   冷库 −kg                ← 全店减少(变成收入)
//	日终过夜(柜台→冷库) 柜台 −P ; 冷库 +P       ← 全店不变
//
// ⇒ **全店 = 柜台 + 冷库 恒等**;毛利用全店口径,转库不影响成本
// (成本跟着货走,移动加权单价是全店一个)。
//
// # 冷库余额的口径(2026-10-24 门店口径)
//
//	冷库余额 = 上期冷库
//	         + 本期**结转**(日终盘点盘出来的量 —— 盘点只盘**猪肉台**)
//	         − 本期**搬上台**
//	         − 本期**大宗结算出库**(大宗可以直接从冷库出,不必先搬上台)
//
// ⇒ 「**搬上台**」是**可选动作**:门店可以只把一部分搬上台零售,剩下的压冷库
//
//	(冷库里的货不参与盘点、盘点管不到它)⇒ **"没搬上台"不是异常**。
//
// # 为什么"搬上台"必须记录
//
// 门店可以**只搬一部分,甚至不上**(货在冷库压着,过几天再卖或走统收)。
// ⇒ 冷库余额是**累加**的,只有记录了搬台量才算得准
// ⇒ 否则冷库余额会只增不减,全店库存虚高。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// datatypesDate 把 time.Time 转成 datatypes.Date(只存日期部分)。
func datatypesDate(t time.Time) datatypes.Date { return datatypes.Date(t) }

// dateKey 把 datatypes.Date 格式化成 "YYYY-MM-DD"(用于 map key / 展示)。
func dateKey(d datatypes.Date) string { return time.Time(d).Format("2006-01-02") }

// BulkPositionKeyPrefix 大宗品项在**位置表**里的 key 前缀。
//
// ⚠️ **已废弃(2026-10-23)**:大宗库存不再用伪 key。
//
// 原来大宗记在 "bulk:<code>" 上(大宗没有思迅货号),但伪 key 不在部位映射里
// ⇒ 没有入库金额 ⇒ 移动加权单价恒为 0 ⇒ **统收成本 0 ⇒ 毛利率 100%**
// (实测:卖 10kg 板油收 60 元,成本 0、毛利 +60;真实成本 1000 元、毛利应是 −940)。
//
// ⇒ 大宗库存与收入现在记在**部位主 SKU** 上(见 service/bulk_carryover.go)。
// 本常量只保留给**清理历史残留**用(位置表里可能还有旧的 bulk:* 行)。
const BulkPositionKeyPrefix = "bulk:"

// BulkPositionKey 大宗品项的**旧**位置 key(仅供清理历史行用)。
func BulkPositionKey(productCode string) string { return BulkPositionKeyPrefix + productCode }

// CounterLoadLine 搬上猪肉台的一行。
//
// ⚠️ 2026-11-02 owner:标签号玩法下线 ⇒ **只有「按 SKU + kg」一种搬法**
// (不再支持「按标签号整批搬」)。
type CounterLoadLine struct {
	// ID 是这一行的**行号**(可选)。
	//
	// ⚠️ 给了 id ⇒ **覆盖**那一行(幂等:同一份重复提交不会多搬一次);
	//    不给 ⇒ **新增**一行 —— 门店"早上一批,卖完了中午再加一批"就是这么用。
	ID            string          `json:"id,omitempty"`
	CubeProductID string          `json:"cube_product_id"`
	QtyKg         decimal.Decimal `json:"qty_kg"`
	// Label 是**老客户端探测器**(2026-11-02 owner:标签号玩法下线)。
	//
	// ⚠️ **不实现任何标签语义** —— 只是为了认出「还在发 label 的老 APK」并当场报错:
	// 老包发的是 `{"label":"BAG-001"}`(没有货号、没有重量)
	// ⇒ 若静默忽略,这一行会被当成「0 kg = 今天不搬台」的标记 ⇒ **搬台被悄悄吞掉**。
	// ⇒ 认出它 ⇒ 明确告诉门店「这套玩法没了,请升级 App」。
	//
	// ⚠️ 新客户端**不要**下发这个字段。
	Label string `json:"label,omitempty"`
}

// CounterLoadInput POST /stock/counter-load 入参。
type CounterLoadInput struct {
	Lines []CounterLoadLine `json:"lines" binding:"required"`
	// TakenDate 是这次搬台的**归属日**(YYYY-MM-DD;空 = 今天)。
	//
	// ⚠️ 2026-10-24:搬台**有归属日**(与盘点一致)⇒ 补录往期的搬台记在**那一天**,
	// 冷库也从那一天扣(不是从今天扣)⇒ 门店口径的
	// 「冷库余额 = 上期冷库 + 本期结转 − 本期搬上台 − 本期大宗出库」才成立。
	//
	// ⚠️ 归属日必须 ≤ 今天(不能把货搬到未来)。
	TakenDate string `json:"taken_date,omitempty"`
}

// CounterLoadUpdateInput PUT /stock/counter-load/:id 入参(改一行的重量)。
type CounterLoadUpdateInput struct {
	QtyKg decimal.Decimal `json:"qty_kg" binding:"required"`
}

// StockLine 库存的一行(SKU 级)。
type StockLine struct {
	CubeProductID   string          `json:"cube_product_id"`
	CubeProductName string          `json:"cube_product_name,omitempty"`
	CutType         model.CutType   `json:"cut_type"`
	CounterKg       decimal.Decimal `json:"counter_kg"`
	ColdStoreKg     decimal.Decimal `json:"cold_store_kg"`
	TotalKg         decimal.Decimal `json:"total_kg"`
	// MovingAvgUnitCost 是全店移动加权单价(元/kg)。
	MovingAvgUnitCost decimal.Decimal `json:"moving_avg_unit_cost"`
}

// StockSnapshot 某店某范围的库存快照。
type StockSnapshot struct {
	BranchID string          `json:"branch_id"`
	Scope    string          `json:"scope"` // counter | all
	Lines    []StockLine     `json:"lines"`
	TotalKg  decimal.Decimal `json:"total_kg"`
}

// isCounterLoadMarkerOnly 判断这次搬台提交是不是**纯标记**（"今天不搬台"）。
//
// ⚠️ 纯标记 = **每一行都是 0 kg**(没搬任何货)。
//
//	⇒ 数值上等于"没搬"⇒ 不动货、不改余额链
//	⇒ 所以它**不需要**"前一个营业日已盘点"这条前置条件
//	（那条校验是为真搬台服务的:搬货要拿昨天的日终当基线）。
//
//	⚠️ 只要有一行 > 0 ⇒ 就是真搬台 ⇒ 校验照走。
func isCounterLoadMarkerOnly(lines []CounterLoadLine) bool {
	if len(lines) == 0 {
		return false
	}
	for _, ln := range lines {
		if !ln.QtyKg.IsZero() {
			return false
		}
	}
	return true
}

// CounterLoad 把冷库的货搬上猪肉台(开市准备)。
//
// 校验:qty_kg > 0;**新增**的行冷库余额必须够(不够 → 400,提示还剩多少);
// **前一个营业日必须已盘点**(否则 400,提示要补哪天,见 business_day.go);
// 归属日(taken_date)必须 ≤ 今天。
//
// # 一行一个动作(2026-10-24 门店口径)
//
// 「一个 sku 在当天搬台里可以有多条记录,早上一批,卖完了,中午加一批,
//
//	可以随时修改,甚至过一天修改。」
//
// ⇒ **一行 = 一次搬台动作**:
//   - 行里**带 id** ⇒ **覆盖**那一行(幂等:同一份重复提交不会多搬一次);
//   - 行里**不带 id** ⇒ **新增**一行(中午加一批)—— 序号自动接续;
//   - 改一行重量 ⇒ PUT /stock/counter-load/:id;
//   - 撤掉一行 ⇒ DELETE /stock/counter-load/:id;
//   - 改/撤**过往**某天的一行也允许(过一天修改)⇒ 只影响那一天的余额链。
//
// ⚠️ **改/撤造成的负库存不拦**(门店口径:要在每日毛利的告警里红字显示)
//
//	⇒ 见 service/gross_margin.go 的 MarginAlert。
//
// ⚠️ 搬台**只有「按 SKU + kg」一种搬法**(2026-11-02 owner:标签号玩法下线)
// ⇒ 但两种搬法都要留流水(counter_loads 表):(归属日, SKU, 重量, 操作人)
// ⇒ 休市判定与"按营业日推导的冷库余额"才有依据。
func (s *Service) CounterLoad(ctx context.Context, branchID string, in CounterLoadInput, operatorID string) (*StockSnapshot, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if len(in.Lines) == 0 {
		return nil, fmt.Errorf("%w: lines 不能为空", ErrInvalidInput)
	}
	// ⚠️ 老客户端探测器:还在发 label ⇒ 那是「按标签整批搬」的老玩法
	//   (2026-11-02 已下线)⇒ 当场报错,**别静默忽略**。
	//
	//   为什么必须报错而不是忽略:老包发的是 `{"label":"BAG-001"}`
	//   (没有货号、没有重量)⇒ 静默忽略的话,这一行会被当成
	//   「0 kg = 今天不搬台」的标记 ⇒ **门店以为搬了台,其实啥也没搬**。
	//
	//   ⇒ 这个检查放在**最前面**:老客户端的问题是"版本太旧",
	//     比"昨天没盘点"更该先说。
	for i, ln := range in.Lines {
		if strings.TrimSpace(ln.Label) != "" {
			return nil, fmt.Errorf("%w: lines[%d].label —— 搬台的「标签 / 整批搬」玩法已于 2026-11-02 下线,"+
				"请升级 App 后按 SKU + 重量搬台", ErrInvalidInput, i)
		}
	}
	now := s.now()
	// ---- 归属日(与盘点一致)----
	takenDate, _, err := s.resolveTakenDate(in.TakenDate, now)
	if err != nil {
		return nil, err
	}
	// ---- 链的起点:归属日不得早于期初结转的生效日(见 service.go 同名调用)----
	if err := s.assertAfterChainStart(ctx, branchID, takenDate, "搬台"); err != nil {
		return nil, err
	}
	// ---- 跨日顺序校验:前一个营业日必须已盘点 ----
	//
	// ⚠️ 2026-10-30 owner:「今天不搬台」的**纯标记**请求
	//   (每一行都是 0 kg)⇒ **跳过**这条校验。
	//   理由:它不动任何货、不改余额链 ⇒ 不需要"昨天的盘点"做基线
	//   ⇒ 不能因为昨天没盘点,就把门店"记一笔今天没搬台"这件事也挡住。
	if !isCounterLoadMarkerOnly(in.Lines) {
		if err := s.assertPrevBizDayStocktaken(ctx, branchID, takenDate); err != nil {
			return nil, err
		}
	}
	// ⚠️ 2026-11-01 owner：审计结转下线 ⇒ 冻结机制一并下线
	//   ⇒ 搬台(含补录)不再被「截止日已冻结」拦截。

	// ---- 解析每一行 ----
	//
	// ⚠️ 每行都带**部位**(cut_type):库存/结余按部位记(门店口径),
	//   SKU 只是那一批实物的溯源属性。
	cutOfSku, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	lines := make([]counterLoadLine, 0, len(in.Lines))
	for i, ln := range in.Lines {
		sku := strings.TrimSpace(ln.CubeProductID)
		// ⚠️ 2026-10-30 owner：「今天整日都不搬台」也是一个**合法的动作**
		//   ⇒ 允许 qty_kg = 0(之前是"必须 > 0" ⇒ 门店根本没法表达"今天不搬台")。
		//
		//   0 kg 的行 = 「**门店确认今天没有搬任何货上台**」:
		//   - **数值上等于"没搬"**(所有计算都是加减 0)⇒ 不影响毛利/成本链;
		//   - 但把"这一步已确认"记下来了 ⇒ 每日作业页第②步不再永远显示"未搬台"。
		//
		// ⚠️ 0 kg 的行**没有货号**(没搬任何货)⇒ 允许为空。
		zeroKg := ln.QtyKg.IsZero()
		if ln.QtyKg.IsNegative() {
			return nil, fmt.Errorf("%w: lines[%d].qty_kg 必须 >= 0", ErrInvalidInput, i)
		}
		if sku == "" && !zeroKg {
			return nil, fmt.Errorf("%w: lines[%d].cube_product_id 必填", ErrInvalidInput, i)
		}
		lines = append(lines, counterLoadLine{
			id: strings.TrimSpace(ln.ID), sku: sku, kg: ln.QtyKg,
			cut: cutOfSku[sku],
		})
	}

	// ---- 覆盖已有行:取旧重量(算位置差额)+ 校验归属日一致 ----
	for i := range lines {
		if lines[i].id == "" {
			continue
		}
		var cur model.CounterLoadRecord
		err := s.db.WithContext(ctx).
			Where("branch_id = ? AND id = ?", branchID, lines[i].id).
			First(&cur).Error
		if err == nil {
			curDate := time.Time(cur.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02")
			reqDate := takenDate.In(s.bizTZOrUTC()).Format("2006-01-02")
			if curDate != reqDate {
				return nil, fmt.Errorf("%w: 搬台流水 %s 的归属日是 %s,与请求的归属日 %s 不一致",
					ErrInvalidInput, lines[i].id, curDate, reqDate)
			}
			lines[i].prevKg = cur.QtyKg
			// ⚠️ SKU 不可改(那批货已经搬走了)⇒ 一律沿用原行的 SKU。
			lines[i].sku = cur.CubeProductID
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("query counter_load %s: %w", lines[i].id, err)
		}
		// 查不到 ⇒ 当成**新增**(客户端自带的幂等键:第一次提交时它还不存在)。
	}

	// ---- 冷库余额**只告警不拦截**(门店口径)----
	//
	// 「负库存只告警不拦截就可以」⇒ 搬超了也允许(门店可能补录/改单),
	// 负数由每日毛利的 alert 红字显示(见 service/gross_margin.go 的 MarginAlert)。
	_, cold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, err
	}
	addKg := map[string]decimal.Decimal{}
	for _, nl := range lines {
		if nl.id != "" {
			continue // 覆盖已有行 ⇒ 差额在事务里算
		}
		addKg[nl.sku] = addKg[nl.sku].Add(nl.kg)
	}
	for sku, v := range addKg {
		if v.GreaterThan(cold[sku]) {
			s.pubLogger.Warn("CounterLoad: 搬台量超过当前冷库余额(负库存,只告警不拦截)",
				"branch_id", branchID, "sku", sku, "cold_kg", cold[sku].String(), "move_kg", v.String())
		}
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ① 流水:带 id 的覆盖、不带 id 的新增(序号接续)。
		for _, nl := range lines {
			if err := s.upsertCounterLoadLine(ctx, tx, branchID, takenDate, nl, operatorID); err != nil {
				return err
			}
		}
		// ② 位置:按**每行的差额**调整(冷库 −、柜台 +)⇒ 幂等。
		for _, nl := range lines {
			delta := nl.kg.Sub(nl.prevKg)
			if delta.IsZero() {
				continue
			}
			if err := s.addPosition(ctx, tx, branchID, nl.sku, model.StockLocationColdStore, delta.Neg()); err != nil {
				return err
			}
			if err := s.addPosition(ctx, tx, branchID, nl.sku, model.StockLocationCounter, delta); err != nil {
				return err
			}
		}
		// 推进营业日状态(已搬台)。
		return s.markBusinessDayCounterLoaded(ctx, tx, branchID, takenDate)
	})
	if err != nil {
		return nil, err
	}
	_ = now
	_ = operatorID
	// 按营业日推导的冷库余额变了(本期搬上台)⇒ 重算那天的结余快照。
	if err := s.recomputeCostSnapshots(ctx, branchID, takenDate); err != nil {
		s.pubLogger.Error("CounterLoad: 结余快照重算失败",
			"branch_id", branchID, "taken_date", dateKey(datatypesDate(takenDate.In(s.bizTZOrUTC()))), "err", err)
	}
	return s.ListStock(ctx, branchID, model.StockScopeCounter)
}

// ListStock 查库存。
//
// scope:默认 counter(猪肉台);all = 猪肉台 + 冷库。
// ⚠️ **不提供"只查冷库"**(门店明确:没有查冷库的说法)。
func (s *Service) ListStock(ctx context.Context, branchID string, scope model.StockScope) (*StockSnapshot, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	switch scope {
	case "":
		scope = model.StockScopeCounter
	case model.StockScopeCounter, model.StockScopeAll:
	default:
		return nil, fmt.Errorf("%w: scope 只支持 counter | all(没有'只查冷库'这个口径)", ErrInvalidInput)
	}
	counter, cold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, err
	}
	cutOf, nameOf, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	// 移动加权单价(**部位级**,2026-10-27)。
	//
	// ⚠️ 成本链改部位级 ⇒ `sku_cost_states` **停止写入** ⇒ 不能再按 SKU 查
	//   (那样查出来恒为 0 ⇒ 门店在「库存查询」里看到的单价永远是 0)。
	// ⇒ 改成按**部位**读 `cut_cost_states`(部位级成本状态,每次盘点后刷新)。
	//   同一部位的货号共用该部位的单价 —— 成本本来就是部位口径
	//   (门店口径:「鲜肉只管部位数据,不管 SKU」)。
	unitCostByCut := map[model.CutType]decimal.Decimal{}
	var states []model.CutCostState
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&states).Error; err != nil {
		return nil, fmt.Errorf("query cut_cost_states: %w", err)
	}
	for _, st := range states {
		if st.MovingAvgUnitCost.IsPositive() {
			unitCostByCut[st.CutType] = st.MovingAvgUnitCost
		}
	}

	skuSet := map[string]struct{}{}
	for sku := range counter {
		skuSet[sku] = struct{}{}
	}
	for sku := range cold {
		skuSet[sku] = struct{}{}
	}
	lines := make([]StockLine, 0, len(skuSet))
	total := decimal.Zero
	for sku := range skuSet {
		c := counter[sku]
		cd := cold[sku]
		if scope == model.StockScopeCounter {
			cd = decimal.Zero
		}
		t := c.Add(cd)
		lines = append(lines, StockLine{
			CubeProductID:     sku,
			CubeProductName:   nameOf[sku],
			CutType:           cutOf[sku],
			CounterKg:         c,
			ColdStoreKg:       cd,
			TotalKg:           t,
			MovingAvgUnitCost: unitCostByCut[cutOf[sku]],
		})
		total = total.Add(t)
	}
	sortStockLines(lines)
	return &StockSnapshot{BranchID: branchID, Scope: string(scope), Lines: lines, TotalKg: total}, nil
}

// sortStockLines 按 cube_product_id 升序(输出稳定)。
func sortStockLines(lines []StockLine) {
	for i := 1; i < len(lines); i++ {
		j := i
		for j > 0 && lines[j-1].CubeProductID > lines[j].CubeProductID {
			lines[j-1], lines[j] = lines[j], lines[j-1]
			j--
		}
	}
}

// addPosition 给某位置的余额加 delta(自动 upsert)。
//
// # ⚠️ PG 专属坑(2026-10-23 修):upsert 的 SET 子句里**不能写不带表名的列名**
//
// GORM 把 `clause.Assignments` 渲染成:
//
//	ON CONFLICT (...) DO UPDATE SET "qty_kg" = qty_kg + $1
//	                                                  ↑ PG 报
//	                                                    ERROR: column reference "qty_kg" is ambiguous
//
// 原因:PG 的 ON CONFLICT DO UPDATE 里,**目标表与 `excluded` 都有这一列**,
// 不带表名就分不清指的是哪个 ⇒ 必须写成 `"sku_stock_positions"."qty_kg" + $1`。
// (SQLite 等其它方言的裸列名会解析到目标表 ⇒ 不加限定。)
//
// # 为什么这个坑一直没被发现
//
// fresh-meat 域到 2026-10-23 为止**零运营数据**(没有整猪/分割/盘点记录)
// ⇒ 位置表从来没被写过 ⇒ 搬上台 / 日终过夜搬运 / 大宗出入库
// **全部静默失败**(盘点那步的错误被当成 warning 吞掉,只是日志里有痕迹)。
// ⇒ 症状是"库存查询永远是空的",而服务 200 正常、零报错。
func (s *Service) addPosition(ctx context.Context, tx *gorm.DB, branchID, sku string, loc model.StockLocation, delta decimal.Decimal) error {
	rec := model.SkuStockPosition{
		ID:            StockPositionID(branchID, sku, string(loc)),
		BranchID:      branchID,
		CubeProductID: sku,
		Location:      loc,
		QtyKg:         delta,
		UpdatedAt:     s.now(),
	}
	// 累加表达式:PG 要带表名限定,其它方言用裸列名。
	expr := "qty_kg + ?"
	if tx.Dialector.Name() == "postgres" {
		expr = `"sku_stock_positions"."qty_kg" + ?`
	}
	// ⚠️ **必须把 ctx 绑到这条语句上** —— 溯源记账回调读的就是
	// `Statement.Context` 里的 run_id。
	//
	// 之前这里是 `tx.Create(rec)`,而**有的调用方传的是 `s.db`**(不带 ctx)
	// ⇒ 那条写入没有 run_id ⇒ 旁表不记账 ⇒ 收尾 sweep 清不掉 ⇒
	// 生产库里一天天攒下"0 kg 的幽灵行",而 sweep 报的是 OK。
	//
	// ⇒ 统一在这里 `tx.WithContext(ctx)`,调用方传什么 tx 都不会漏。
	stmt := tx.WithContext(ctx)
	// upsert:冲突时 qty_kg = qty_kg + delta
	return stmt.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}, {Name: "cube_product_id"}, {Name: "location"}},
		DoUpdates: clause.Assignments(map[string]any{
			"qty_kg":     gorm.Expr(expr, delta),
			"updated_at": s.now(),
		}),
	}).Create(&rec).Error
}

// applyOvernightMove 日终盘点后执行"过夜搬运":猪肉台 → 冷库。
//
// counterActual 是今日盘点的**柜台实盘**(per-SKU)。
// ⚠️ 全店余额不变(只是位置变),所以这一步**不影响毛利**,只影响位置与压货天数。
//
// ⚠️ 柜台是**置零**不是"减掉实盘":账面柜台可能小于实盘(盘盈)或大于实盘(盘亏),
// 减法会留下负数余额 ⇒ 全店 = 柜台 + 冷库 就算错。
// 正确做法:柜台绝对置零,冷库 += 今日实盘。
func (s *Service) applyOvernightMove(ctx context.Context, branchID string, counterActual map[string]decimal.Decimal) error {
	counter, _, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return err
	}
	// 要搬进冷库的量:盘到的 SKU 用实盘,没盘到的用账面。
	toMove := map[string]decimal.Decimal{}
	for sku, v := range counter {
		toMove[sku] = v
	}
	for sku, v := range counterActual {
		toMove[sku] = v
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for sku, cur := range counter {
			if cur.IsZero() {
				continue
			}
			// 柜台置零(可能是负数余额,一并归零)
			if err := s.addPosition(ctx, tx, branchID, sku, model.StockLocationCounter, cur.Neg()); err != nil {
				return err
			}
		}
		for sku, qty := range toMove {
			if !qty.IsPositive() {
				continue
			}
			if err := s.addPosition(ctx, tx, branchID, sku, model.StockLocationColdStore, qty); err != nil {
				return err
			}
		}
		return nil
	})
}

// PersistCutCostDaily 落当日**部位级**成本快照(幂等 upsert),并刷新 cut_cost_states。
//
// 触发点:**日终盘点提交后** —— 那是"这一天算完了"的自然时点。
// 快照同时是**次日期初的来源**(链式一致,不会因为某天漏算而断链)。
//
// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 落的是 `cut_cost_daily` / `cut_cost_states`。
//
//	旧的 `sku_cost_daily` / `sku_cost_states` **停止写入**(历史保留只读)。
func (s *Service) PersistCutCostDaily(ctx context.Context, branchID string, day time.Time) (int, error) {
	return s.persistCutCostDaily(ctx, branchID, day, true, priceRecomputeOn)
}

// persistCutCostDaily 落某营业日的**部位级**成本快照(幂等 upsert)。
//
// # updateState 的含义(2026-10-24)
//
// true  —— 同时刷新 **cut_cost_states**(「当前」成本状态)⇒ 只用于**今天**那次盘点;
// false —— **只写 cut_cost_daily**(按营业日的成本快照)⇒ 用于**补录往日**时的重算:
//
//	补录不能把"当前成本状态"改成历史某天的值(那会把今天的毛利带歪)。
//
// # closing_amount 记的是**结转口径**(下一期的期初金额来源)
//
// ⚠️ 有**预售结转**的那天:期末金额按**预期售价口径**记
//
//	(成本口径金额 + 预售重估)⇒ 下一期的期初单价自动 = 预期销售单价
//	⇒ 两期总毛利 = 实际毛利(不多算、不少算)。
//
//	成本口径本身仍然保留在快照里:closing_kg × moving_avg_unit_cost。
//
// # ⚠️ 成本状态(**单价**)只从"**上一营业日有冻结快照**"的那天刷新
//
// 为什么:成本状态里的单价会被引擎拿来给**期初定价**(见 cut_cost.go 的
// loadCutOpening:② cut_cost_states 的部位移动加权单价)。
// 若把**当天自己的**单价写进状态 ⇒ 当天被重算时,期初会被**自己昨天的单价**
// 重新定价 ⇒ 门店给的期初进价被覆盖 ⇒ 报告的数字与冻结快照对不上
// ⇒ 而且每重算一次就漂一次(自引用)。
//
// ⇒ 只在"上一营业日有冻结快照"时刷新状态(= 链真的从冻结快照接续过来);
//
//	链的起点那天(上一日没有快照)不刷新 ⇒ 期初由**门店给的进价**定价。
//
// (根因在 cut_cost.go 的定价优先级:它没有比较状态的 last_biz_date;
//
//	这边用"刷新时机"规避,不改引擎。)
//
// # recomputePrice 的含义(2026-11-02)
//
//	true  —— 采集完单价样本后**顺手重算**单价基线物化表
//	         ⇒ 单日写事实路径(录猪/盘点/搬台/大宗/删猪)走这条
//	         ⇒ 门店界面上"基线样本数/单价"当场就跟上,不用手动点「重算」。
//	false —— 只采集样本,**不**重算 ⇒ **批量**路径(全量物化成本链)走这条,
//	         逐日采集、**循环结束后统一重算一次**。
//
//	⚠️ 挂在**这里**而不是各写入口 ⇒ 因为所有"写事实"的路都会走到这一层
//	  ⇒ 不用逐个入口去记得触发(2026-11-02 的教训:触发点漏接 ⇒ 界面一直停在旧数字)。
func (s *Service) persistCutCostDaily(ctx context.Context, branchID string, day time.Time,
	updateState bool, recomputePrice bool) (int, error) {
	rows, _, _, _, err := s.computeCutDailyCosts(ctx, branchID, day)
	if err != nil {
		return 0, err
	}
	bizDate := day.In(s.bizTZOrUTC())
	bizKey := bizDate.Format("2006-01-02")
	now := s.now()
	// 上一营业日**有冻结快照**的部位(⇒ 状态可以刷新,见上面的说明)。
	frozenCuts, err := s.cutsWithFrozenPrevDay(ctx, branchID, day)
	if err != nil {
		return 0, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			// 结转金额 = 成本口径期末金额 + 预售重估(有预售结转的那天)。
			// ⇒ 下一期的期初金额从这张表读 ⇒ 自动按预期售价进来(链式)。
			closingAmount := r.ClosingAmount.Add(r.PresaleMarginYuan)
			snap := model.CutCostDaily{
				ID:                         CutCostDailyID(branchID, bizKey, string(r.CutType)),
				BranchID:                   branchID,
				BizDate:                    datatypesDate(bizDate),
				CutType:                    r.CutType,
				OpeningKg:                  r.OpeningKg,
				OpeningAmount:              r.OpeningAmount,
				ProducedKg:                 r.ProducedKg,
				ProducedAmount:             r.ProducedAmount,
				SoldKg:                     r.SoldKg,
				SoldAmount:                 r.SoldAmount,
				BulkSoldKg:                 r.BulkSoldKg,
				BulkSoldAmount:             r.BulkSoldAmount,
				WasteKg:                    r.WasteKg,
				WasteAmount:                r.WasteAmount,
				ExpectedClosingKg:          r.ExpectedClosingKg,
				ClosingKg:                  r.ClosingKg,
				ClosingAmount:              closingAmount,
				MovingAvgUnitCost:          r.MovingAvgUnitCost,
				SalesCostYuan:              r.SalesCostYuan,
				BulkCostYuan:               r.BulkCostYuan,
				WasteCostYuan:              r.WasteCostYuan,
				RevenueYuan:                r.RevenueYuan,
				GrossMarginYuan:            r.GrossMarginYuan,
				GrossMarginPct:             r.GrossMarginPct,
				PresaleMarginYuan:          r.PresaleMarginYuan,
				PresaleExpectedRevenueYuan: r.PresaleExpectedRevenueYuan,
				DataSource:                 r.DataSource,
				CostUncomputable:           r.CostUncomputable,
				// ---- 短卖(2026-10-30 owner 口径)----
				//
				// ⚠️ 这三列是**链的累计短卖能跨日传下去**的唯一载体:
				//   逐日物化(本函数)是"一次算一天、不带链状态"的
				//   ⇒ 次日只能从**上一营业日的快照**把累计短卖读回来
				//   (见 service/cut_chain.go::carriedShortSellFromSnapshot)。
				//   不落这三列 ⇒ 累计短卖每天断链 ⇒ 门店只看得到"当天短卖",
				//   看不到"这段时间一共短卖了多少"。
				//
				// ⚠️ short_sell_kg 存的是**当日总量短卖分摊到各部位**的值
				//   ⇒ Σ(各行) = 当日短卖(读的时候要按行求和)。
				// ⚠️ carried_short_sell_kg 每行都存**同一个**累计值
				//   ⇒ 读的时候只取**第一行**即可(读法见上面那个函数)。
				ShortSellKg:        r.ShortSellKg,
				CarriedShortSellKg: r.CarriedShortSellKg,
				CutShortSellKg:     r.CutShortSellKg,
				CreatedAt:          now,
				UpdatedAt:          now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "branch_id"}, {Name: "biz_date"}, {Name: "cut_type"}},
				DoUpdates: clause.Assignments(map[string]any{
					"opening_kg":                    r.OpeningKg,
					"opening_amount":                r.OpeningAmount,
					"produced_kg":                   r.ProducedKg,
					"produced_amount":               r.ProducedAmount,
					"sold_kg":                       r.SoldKg,
					"sold_amount":                   r.SoldAmount,
					"bulk_sold_kg":                  r.BulkSoldKg,
					"bulk_sold_amount":              r.BulkSoldAmount,
					"waste_kg":                      r.WasteKg,
					"waste_amount":                  r.WasteAmount,
					"expected_closing_kg":           r.ExpectedClosingKg,
					"closing_kg":                    r.ClosingKg,
					"closing_amount":                closingAmount,
					"moving_avg_unit_cost":          r.MovingAvgUnitCost,
					"sales_cost_yuan":               r.SalesCostYuan,
					"bulk_cost_yuan":                r.BulkCostYuan,
					"waste_cost_yuan":               r.WasteCostYuan,
					"revenue_yuan":                  r.RevenueYuan,
					"gross_margin_yuan":             r.GrossMarginYuan,
					"gross_margin_pct":              r.GrossMarginPct,
					"presale_margin_yuan":           r.PresaleMarginYuan,
					"presale_expected_revenue_yuan": r.PresaleExpectedRevenueYuan,
					"data_source":                   r.DataSource,
					"cost_uncomputable":             r.CostUncomputable,
					// ⚠️ 短卖三列也要跟着更新(见上面 Create 那处的说明):
					//   不在 DoUpdates 里 ⇒ 重算时列会留旧值 ⇒ 累计短卖越算越乱。
					"short_sell_kg":         r.ShortSellKg,
					"carried_short_sell_kg": r.CarriedShortSellKg,
					"cut_short_sell_kg":     r.CutShortSellKg,
					"updated_at":            now,
				}),
			}).Create(&snap).Error; err != nil {
				return err
			}
			// 成本状态:期末 = 新的移动加权单价
			//
			// ⚠️ 只在"今天那次盘点"时刷新 —— 补录往日重算**不许**改当前状态。
			if !updateState {
				continue
			}
			// ⚠️ 只在"上一营业日有冻结快照"时刷新(见函数头的说明):
			//   否则当天自己的单价会反过来给当天的期初定价(自引用)⇒
			//   门店给的期初进价被历史单价覆盖 ⇒ 报告与冻结快照对不上。
			if !frozenCuts[string(r.CutType)] {
				continue
			}
			state := model.CutCostState{
				ID:                CutCostStateID(branchID, string(r.CutType)),
				BranchID:          branchID,
				CutType:           r.CutType,
				QtyKg:             r.ClosingKg,
				AmountYuan:        closingAmount,
				MovingAvgUnitCost: r.MovingAvgUnitCost,
				LastBizDate:       datatypesDate(bizDate),
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "branch_id"}, {Name: "cut_type"}},
				DoUpdates: clause.Assignments(map[string]any{
					"qty_kg":               r.ClosingKg,
					"amount_yuan":          closingAmount,
					"moving_avg_unit_cost": r.MovingAvgUnitCost,
					"last_biz_date":        datatypesDate(bizDate),
				}),
			}).Create(&state).Error; err != nil {
				return err
			}
		}
		// ---- 删除本轮**不再产出**的旧行(2026-10-30)----
		//
		// ⚠️ **只 upsert 不 delete ⇒ 脏行会永远留在表里**,
		//   而且会被次日"读前一日快照接链"当成**真的期初**接回去 ⇒ 负数凭空复活。
		//   (生产实测 店 00:10-28 的 ear / lung / bone_spine / skin 四行是**旧链残留**,
		//    updated_at 还停在 10-29 16:42(那一刻算过)⇒ 10-29 读成 −1.279 / −17.64 …)
		//
		// ⇒ 重算某天 = "这天的快照**就是**引擎这一轮产出的那几行"
		//   ⇒ 产出集合之外的行 = 脏行 ⇒ **同事务**删掉。
		//
		// ⚠️ **只删这一天**(branch_id + biz_date)⇒ 不碰别的日子。
		//
		// ⚠️ **查询必须用"纯日期字符串"区间**,不能用 time.Time / datatypes.Date 等值:
		//   - PG:date 列与 **date 字面量**比较是精确的(与 DateStyle、会话时区都无关);
		//     与 timestamp 比较会按会话 TimeZone 折算 ⇒ 不可靠(仓库里踩过)。
		//   - SQLite:datatypes.Date 落库**保留时区表示**
		//     (业务时区写 "2026-10-06T00:00:00+08:00"、UTC 写 "2026-10-06T00:00:00Z")
		//     ⇒ 纯日期串做**字典序**区间能同时罩住两种形态
		//     (两者都以 "2026-10-06" 开头 ⇒ 都落在 ["2026-10-06","2026-10-07") 内)。
		//   ⇒ 再在 Go 里按**业务时区**的日期精确过滤一道(防御)。
		produced := make(map[string]bool, len(rows))
		for _, r := range rows {
			produced[string(r.CutType)] = true
		}
		var existing []model.CutCostDaily
		if err := tx.WithContext(ctx).
			Where("branch_id = ? AND biz_date >= ? AND biz_date < ?",
				branchID, bizKey, startDatePlusOne(bizKey)).
			Find(&existing).Error; err != nil {
			return err
		}
		var staleIDs []string
		for _, e := range existing {
			if produced[string(e.CutType)] {
				continue
			}
			if time.Time(e.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02") != bizKey {
				continue // 防御:不是这一天 ⇒ 不动
			}
			staleIDs = append(staleIDs, e.ID)
		}
		if len(staleIDs) > 0 {
			if err := tx.WithContext(ctx).
				Where("id IN ?", staleIDs).
				Delete(&model.CutCostDaily{}).Error; err != nil {
				return err
			}
			s.pubLogger.Info("成本日快照: 已删除本轮不再产出的脏行",
				"branch_id", branchID, "biz_date", bizKey, "rows", len(staleIDs))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// ---- 估计样本(2026-10-30 owner 选的 A 方案)----
	//
	// 这一天算完了 ⇒ 当日逐部位销量定了 ⇒ 把"销量按毛重比例摊到每头猪"的**估计样本**落库
	// ⇒ 之后「重算基线」就能用它 ⇒ 建议分割开始反映**本店真实出肉率**(而不是 LLM 通用先验)。
	//
	// ⚠️ **失败不阻断**:成本快照已经落好了 ⇒ 估计样本只是"喂基线"的材料
	//   ⇒ 挂了就挂了(下次重算/物化时再补)⇒ 不能让盘点提交整个失败。
	//
	// ⚠️⚠️ **overwrite=false**(2026-11-01 owner D8「采集即固化」):
	//   这是**引擎自动路径** ⇒ **已存在的样本一律不动**
	//   ⇒ 事后改搬台 / 改猪重 ⇒ 再物化 ⇒ 样本库**原样不动**。
	//   ⇒ 想让样本跟上最新数据 ⇒ 门店显式「按一天加入样本」/「重采」。
	if _, estErr := s.rebuildEstimatedSplitSamples(ctx, branchID, day, rows, false); estErr != nil {
		s.pubLogger.Warn("persistCutCostDaily: 估计样本重建失败(不影响成本快照)",
			"branch_id", branchID, "biz_date", bizKey, "err", estErr)
	}
	// ---- 单价日样本(2026-10-31 owner:「分割基线加入单价」)----
	//
	// 这一天算完了 ⇒ 当日的**整体毛利率**与各部位的**零售单价**都定了
	// ⇒ 倒推出的**部位进货单价**落库 ⇒ 之后基线就能用它。
	//
	// ⚠️ **失败不阻断**:单价样本只是"喂基线"的材料
	//   ⇒ 挂了就挂了(下次重算/物化时再补)⇒ 不能让盘点提交整个失败。
	//
	// ⚠️ 这里用**不自动重算**的内部版 ⇒ 重算由 recomputePrice 开关决定
	//   (批量路径要攒到最后统一重算一次;见 persistCutCostDaily 的说明)。
	priceRes, priceErr := s.collectCutPriceSamples(ctx, branchID, day, rows)
	if priceErr != nil {
		s.pubLogger.Warn("persistCutCostDaily: 单价样本采集失败(不影响成本快照)",
			"branch_id", branchID, "biz_date", bizKey, "err", priceErr)
	} else if recomputePrice {
		// ⚠️ **自动重算单价基线物化表**(2026-11-02 owner 拍板)
		//   ⇒ 失败不阻断(样本已经是事实)⇒ 但会记日志 + 写进 result。
		s.applyPriceBaselineRecompute(ctx, branchID, priceRes)
	}
	return len(rows), nil
}

// cutsWithFrozenPrevDay 取"上一营业日**有冻结成本快照**"的部位集合。
//
// ⚠️ 这是成本状态刷新时机的判据(见 persistCutCostDaily 的说明):
//
//	上一日有冻结快照 ⇒ 链真的从冻结快照接续 ⇒ 当天的期初金额来自快照
//	⇒ 刷新状态里的单价不会反过来影响当天的期初定价。
func (s *Service) cutsWithFrozenPrevDay(ctx context.Context, branchID string, day time.Time) (map[string]bool, error) {
	out := map[string]bool{}
	prevStart, prevEnd := s.previousBizDayBounds(day.In(s.bizTZOrUTC()))
	var prev []model.CutCostDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, prevStart, prevEnd).
		Find(&prev).Error; err != nil {
		return nil, fmt.Errorf("query cut_cost_daily(prev, frozen): %w", err)
	}
	for _, r := range prev {
		out[string(r.CutType)] = true
	}
	return out, nil
}
