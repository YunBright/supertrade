// Package service / subscribe.go —— Dapr pub/sub 订阅回调实现。
//
// 监听领域:
//   - sale.completed   → OnSaleCompleted 落 line_sales_by_pig
//   - auth.user.access_changed → OnAccessChanged 失效 scope cache(已在 service.go 实现)
//
// 关键设计:
//   - 只对 fresh_type=meat 的行落 line_sales_by_pig(produce 行丢弃,非本仓职责)。
//   - 退货 R 行(负 qty/amount):POS 已自翻符号,本服务直存 OrderStatus="R";毛利计算时排除。
//   - line 缺 pig_id 但有 cube_product_id 时,新规则 resolvePigForSaleLine:
//     1. 若 payload 含 sale_time → 在 sale_time 之前到达的当日 pig 候选;
//     2. 按 purchase_group_id 圈定:同 group 的猪按 arrived_at 升序分配;
//     LLM advice 含目标 cut 的猪优先;LLM 该 cut 售罄后轮到下个 pig;
//     3. 无 group → 选当日到达时间最早的 pig。
//   - 同一 (branch_id, pos_line_id) 二次投递 → idempotent(UNIQUE 约束 + DO NOTHING)。
//   - 时区:用 service.bizTZ 划日界,4:30 北京时间场内的猪触发当日 in_kg > 0。
package service

import (
	"context"
	"encoding/json"
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

// SaleLine 是 sale 事件内 lines[] 的元素。
//
// # ⚠️ **两种事件形状都要认**(2026-10-24 生产实测踩出来的)
//
//   - `sale.completed`(EVENT-CATALOG §2.3 的老契约):行里有 `sku_id` + `fresh_type`
//     (+ 可选 `pig_id` / `cut_type`)。
//   - `sale.paid`(**POS 真正发的那条**,见 internal/pos/service/events.go):
//     行里有 `product_id`(= `catalog.products.id` = **思迅货号**,已用生产核对:
//     `catalog /products/03808` → {"id":"03808","name":"水油","category_id":"1201"}),
//     **而且没有 `fresh_type`**。
//
// ⇒ 以前只订阅 `sale.completed` ⇒ **POS 的销售一条都没进来过**
// (生产日志实测:到 2026-10-24 为止,零条真实销售事件)。
type SaleLine struct {
	LineID string `json:"line_id"` // pos_line_id,idempotent 键
	// SKUID 是 sale.completed 的字段(= cube product id = 思迅货号)。
	SKUID string `json:"sku_id"`
	// ProductID 是 sale.paid 的字段(= catalog.products.id = cube product id = 思迅货号)。
	ProductID     string          `json:"product_id"`
	Qty           decimal.Decimal `json:"qty"` // kg
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	AmountYuan    decimal.Decimal `json:"amount_yuan"`
	// FreshType 是 sale.completed 的字段("none" | "produce" | "meat")。
	//
	// ⚠️ sale.paid **没有**这个字段 ⇒ 为空时按"这个货号在我们自己的表里"
	// 判定(部位映射 / 特价档 / 大宗品项),见 classifySaleLines。
	FreshType string        `json:"fresh_type"`
	PigID     string        `json:"pig_id,omitempty"`
	CutType   model.CutType `json:"cut_type,omitempty"`
}

// cubeSKU 这行的 cube 货号(两种事件形状都给得出)。
func (l SaleLine) cubeSKU() string {
	if s := strings.TrimSpace(l.SKUID); s != "" {
		return s
	}
	return strings.TrimSpace(l.ProductID)
}

// SaleCompletedPayload 是 sale 事件的 data(EVENT-CATALOG §2.3 + sale.paid 形状)。
//
// branch_id 必填(从 envelope.data 或 ?branch_id query 取,subscribe handler 已校验)。
type SaleCompletedPayload struct {
	// OpID 是 sale.paid 的幂等键(uuid);本域不用它(用 pos_line_id 去重)。
	OpID string `json:"op_id"`
	// SaleID 是销售单号。
	SaleID string `json:"sale_id"`
	// BranchID 是门店。
	BranchID string `json:"branch_id"`
	// MemberID 是会员号(sale.paid);本域不用。
	MemberID string `json:"member_id"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
	// TotalAmountYuan 是应收合计。
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	// CompletedAt 是 sale.completed 的成交时刻。
	CompletedAt time.Time `json:"completed_at"`
	// PaidAt 是 sale.paid 的成交时刻(RFC3339,+08:00)。
	PaidAt time.Time `json:"paid_at"`
	// OrderStatus "S"=销售 "R"=退货。
	OrderStatus string     `json:"order_status"`
	Lines       []SaleLine `json:"lines"`
}

// soldAt 成交时刻(两种事件形状都给得出)。
func (p SaleCompletedPayload) soldAt() time.Time {
	if !p.CompletedAt.IsZero() {
		return p.CompletedAt
	}
	return p.PaidAt
}

// OnSaleCompleted 落 line_sales_by_pig(本域的猪肉销售行)。
//
// 流程:
//  1. 遍历 lines,判断"这一行是不是本域的猪肉":
//     - 老事件(sale.completed)自带 fresh_type ⇒ 以它为准;
//     - 新事件(sale.paid)**没有 fresh_type** ⇒ 按我们自己的表判定
//     (部位映射 / 特价档 / 大宗品项),见 classifySaleLines。
//  2. **幂等检查**:这笔原始销售行落过库了就**整段跳过**
//     (dapr 是 at-least-once ⇒ 重投很常见;不跳过 ⇒ FIFO 被吃两遍)。
//  3. 行级补全 pig_id:
//     - **FIFO 归摊**(2026-10-24,优先):按「结转先后顺序」把重量摊到产出它的那几头猪
//     ⇒ 一笔销售可能拆成**多行**(每头猪一行,pos_line_id 按 (原 line_id, pig_id) 派生);
//     - 没有可摊的结转记录 ⇒ resolvePigForSaleLine(找"今天碰巧在场的猪");
//     - 找不到 → 丢该行(可观察事件总线补发)。
//  4. 行级 idempotent INSERT(UNIQUE(branch_id, pos_line_id) DO NOTHING)
//     + 扣掉被吃掉的 FIFO 重量(同一事务)。
//
// 返回:成功插入的行数(用于日志 / 测试断言)。
func (s *Service) OnSaleCompleted(ctx context.Context, p SaleCompletedPayload) (int, error) {
	if p.BranchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if len(p.Lines) == 0 {
		return 0, nil
	}

	now := s.now()
	saleDate := datatypes.Date(now)
	saleTime := now
	if t := p.soldAt(); !t.IsZero() {
		saleTime = t.In(s.bizTZ)
		saleDate = datatypes.Date(saleTime)
	}

	// 订单状态(销售/退货)。POS 已自翻符号(R 行 qty/amount 负数)。
	status := strings.ToUpper(strings.TrimSpace(p.OrderStatus))
	if status == "" {
		status = "S"
	}
	if status != "S" && status != "R" {
		// 未知状态:warn 后按 S 处理(防 POS 字段错位丢全部事件)。
		s.pubLogger.Warn("OnSaleCompleted: unknown order_status, fallback to S",
			"sale_id", p.SaleID, "status", p.OrderStatus)
		status = "S"
	}

	// ★ **批量**判定这一批行"是不是本域的猪肉"。
	//
	// ⚠️ sale.paid **没有 fresh_type** ⇒ 不能靠事件自带的字段判 ⇒
	// 只能靠**我们自己的表**:货号在部位映射里 ⇒ 猪肉(顺带拿到部位);
	// 在特价档里 ⇒ 特价桶。
	//
	// ⇒ 一次事件**一次查询**(不是每行一次):POS 一单几十行,
	// 逐行查会把 DB 打成筛子(而且是**每笔销售**都打一次)。
	skus := make([]string, 0, len(p.Lines))
	seenSKU := map[string]bool{}
	for _, line := range p.Lines {
		sku := line.cubeSKU()
		if sku == "" || seenSKU[sku] {
			continue
		}
		seenSKU[sku] = true
		skus = append(skus, sku)
	}
	cutOf, promoOf, bulkLineOf, cerr := s.classifySaleLines(ctx, p.BranchID, skus)
	if cerr != nil {
		return 0, cerr
	}

	// 逐行处理:幂等检查 → FIFO 归摊(可能拆行)→ 落库 → 扣 FIFO。
	//
	// ⚠️ 全部在**一个事务**里:落库与扣 FIFO 必须同时成功或同时失败
	// (只落库不扣 ⇒ 这批货会被再摊一次;只扣不落 ⇒ 收入丢了)。
	inserted := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, line := range p.Lines {
			sku := line.cubeSKU()
			if sku == "" {
				continue
			}
			// ★ 是不是本域该收的行?
			//
			// 老事件(sale.completed)自带 fresh_type ⇒ 以发件方的判断为准;
			// 新事件(sale.paid)没有 ⇒ 按我们自己的表判定。
			if ft := strings.TrimSpace(line.FreshType); ft != "" {
				if !strings.EqualFold(ft, "meat") {
					continue
				}
			} else if cutOf[sku] == "" && !promoOf[sku] {
				continue // 不是我们的货(可乐/大米/…)⇒ 本域不管
			}
			// ★ **大宗线的 POS 货号 ⇒ 不按零售算**(2026-10-25 门店口径)。
			//
			// 那是「攒着卖给酒店」的大宗货(单笔几百公斤)：
			// 若按零售收 ⇒ 收入归到「当日某一头猪」⇒ 那头猪毛利虚高。
			// ⇒ 这里**整行跳过**(不进 line_sales_by_pigs、不吃 FIFO)，
			//   改由门店在「统收结算」里**手工登记**(领用冷库重量 + 记收入)。
			//
			// ⇒ 安全网:那批货还挂在冷库里 ⇒ 结存页的「可领用」一直显示它
			//   ⇒ 门店看结存就知道「还有 N kg 没结」。
			if bulkLineOf[sku] {
				s.pubLogger.Info("OnSaleCompleted: 大宗线的 POS 货号,不按零售收(请在统收结算里手工登记)",
					"branch_id", p.BranchID, "sku_id", sku, "pos_line_id", line.LineID,
					"qty_kg", line.Qty.String())
				continue
			}
			if strings.TrimSpace(line.LineID) == "" {
				continue
			}

			// ⚠️ **幂等**:这笔原始销售行落过库了就**整段跳过**。
			//
			// ⇒ 为什么要先查:dapr 是 at-least-once ⇒ 同一笔销售会重投;
			// 而重投时 FIFO 的状态已经变了(上次扣过 consumed_kg)
			// ⇒ 再摊一次会把同一批货摊两遍 ⇒ 收入翻倍。
			recorded, err := s.saleLineRecorded(ctx, tx, p.BranchID, line.LineID)
			if err != nil {
				return err
			}
			if recorded {
				s.pubLogger.Info("OnSaleCompleted: 该销售行已落过库,跳过(幂等)",
					"branch_id", p.BranchID, "pos_line_id", line.LineID)
				continue
			}

			lineRows, consumed, err := s.buildSaleLineRows(
				ctx, tx, p, line, sku, cutOf[sku], saleDate, saleTime, status, now)
			if err != nil {
				return err
			}
			if len(lineRows) == 0 {
				continue // drop 该行(反查不到猪/不是我们的货)
			}

			// 行级 idempotent INSERT(UNIQUE(branch_id, pos_line_id) DO NOTHING)。
			res := tx.WithContext(ctx).Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "pos_branch_id"}, {Name: "pos_line_id"},
				},
				DoNothing: true,
			}).Create(&lineRows)
			if res.Error != nil {
				return fmt.Errorf("insert line_sales_by_pig: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				// 已经在库里了 ⇒ **不扣 FIFO**(否则这批货会被吃两遍)。
				continue
			}
			inserted += int(res.RowsAffected)

			// 扣 FIFO(**落库**)⇒ 下次不会把同一批货再摊一次。
			if err := s.consumeFIFO(ctx, tx, consumed); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// buildSaleLineRows 算出一行销售要落哪些行(一头猪一行)+ 要扣掉多少 FIFO。
//
// # 两条路
//
// ① **FIFO 归摊**(2026-10-24):按「结转先后顺序」把这笔销售的重量摊到产出它的那几头猪
//
//	⇒ 一笔销售可能拆成**多行**(每头猪一行)。
//	⇒ ⚠️ FIFO 的归属**优先于**"今天碰巧在场的猪"
//	(这正是"毛利回归到具体猪"的意义)。
//
// ② **老路**:归到事件自带的 pig_id,或 resolvePigByCubeProduct 解析出来的猪 —— 一行。
//
// ⚠️ 退货(R 行,qty 为负)**不走 FIFO**:负重量不该吃 FIFO。
func (s *Service) buildSaleLineRows(
	ctx context.Context, tx *gorm.DB,
	p SaleCompletedPayload, line SaleLine, sku string, mappedCut model.CutType,
	saleDate datatypes.Date, saleTime time.Time, status string, now time.Time,
) ([]model.LineSalesByPig, map[string]decimal.Decimal, error) {
	cutType := line.CutType
	if cutType == "" {
		cutType = mappedCut
	}

	// ---- ① FIFO 归摊 ----
	var alloc *fifoAlloc
	if status == "S" && line.Qty.IsPositive() {
		a, err := s.allocateSaleByFIFO(ctx, tx, p.BranchID, sku, saleDate, line.Qty)
		if err != nil {
			// FIFO 归摊失败**不丢销售**(它只是归属增强)⇒ 退回老路。
			s.pubLogger.Warn("OnSaleCompleted: FIFO 归摊失败,退回老路",
				"branch_id", p.BranchID, "sku_id", sku, "pos_line_id", line.LineID, "err", err)
			a = nil
		}
		alloc = a
	}
	if alloc != nil && len(alloc.KgByPig) > 0 {
		// FIFO 只覆盖了一部分(剩下的货是**今天新分割**的)⇒ 归给当天的猪。
		attributed := decimal.Zero
		for _, kg := range alloc.KgByPig {
			attributed = attributed.Add(kg)
		}
		if remainder := line.Qty.Sub(attributed); remainder.IsPositive() {
			// ⚠️ 事务里**只能用 tx** —— 用 s.db 会与写锁互等 ⇒ 死锁。
			pig, _, err := s.resolvePigByCubeProduct(ctx, tx, p.BranchID, sku, saleTime)
			if err != nil {
				s.pubLogger.Warn("OnSaleCompleted: 兜底归猪失败",
					"branch_id", p.BranchID, "sku_id", sku, "err", err)
			}
			if pig == "" {
				// 摊不满又找不到兜底的猪 ⇒ **放弃 FIFO**(不消耗它)⇒ 走老路。
				alloc = nil
			} else {
				alloc.KgByPig[pig] = alloc.KgByPig[pig].Add(remainder)
			}
		}
	}
	if alloc != nil && len(alloc.KgByPig) > 0 {
		split := splitSaleByPig(line.Qty, line.AmountYuan, alloc.KgByPig, sortedPigIDs(alloc.KgByPig))
		if len(split) > 0 {
			rows := make([]model.LineSalesByPig, 0, len(split))
			for _, sp := range split {
				posLineID := line.LineID
				if len(split) > 1 {
					// 摊到多头猪 ⇒ 必须按 (原始 line_id, pig_id) **确定性派生**
					// (UNIQUE(branch_id, pos_line_id) ⇒ 不派生的话后面的猪会被静默丢掉)。
					posLineID = fifoSaleLineID(line.LineID, sp.PigID)
				}
				rows = append(rows, model.LineSalesByPig{
					BranchID:      p.BranchID,
					SaleDate:      saleDate,
					PigID:         sp.PigID,
					CutType:       cutType,
					QtyKg:         sp.QtyKg,
					AmountYuan:    sp.AmountYuan,
					UnitPriceYuan: line.UnitPriceYuan,
					Source:        "pos",
					PosLineID:     posLineID,
					PosBranchID:   p.BranchID,
					CubeProductID: sku,
					OrderStatus:   status,
					CreatedAt:     now,
				})
			}
			return rows, alloc.Consumed, nil
		}
		alloc = nil
	}

	// ---- ② 老路 ----
	pigID := strings.TrimSpace(line.PigID)
	if pigID == "" {
		resolvedPig, resolvedCut, err := s.resolvePigByCubeProduct(ctx, tx, p.BranchID, sku, saleTime)
		if err != nil {
			s.pubLogger.Warn("OnSaleCompleted: resolvePigByCubeProduct failed",
				"branch_id", p.BranchID,
				"sku_id", sku,
				"err", err)
			return nil, nil, nil // drop 该行,后续监控告警
		}
		pigID = resolvedPig
		if cutType == "" {
			cutType = resolvedCut
		}
	}
	if pigID == "" {
		// 反查失败 → drop 行
		return nil, nil, nil
	}
	return []model.LineSalesByPig{{
		BranchID:      p.BranchID,
		SaleDate:      saleDate,
		PigID:         pigID,
		CutType:       cutType,
		QtyKg:         line.Qty,
		AmountYuan:    line.AmountYuan,
		UnitPriceYuan: line.UnitPriceYuan,
		Source:        "pos",
		PosLineID:     line.LineID,
		PosBranchID:   p.BranchID,
		CubeProductID: sku,
		OrderStatus:   status,
		CreatedAt:     now,
	}}, nil, nil
}

// classifySaleLines 批量判定一批货号"是不是本域的猪肉",并给出部位。
//
// 返回:
//   - cutOf:货号 → 部位(在 branch_cut_mappings 里的那些)
//   - promoOf:货号 → 是不是**特价档**(已登记且启用)
//   - bulkLineOf:货号 → 是不是**大宗线的 POS 货号**(大宗品项上装配的)
//
// # 为什么需要它
//
// ⚠️ `sale.paid`(**POS 真正发的那条**)的行里**没有 fresh_type**
// ⇒ 不能靠事件自带字段判"这是不是猪肉行" ⇒ 只能靠**我们自己的表**:
//
//	在部位映射里 ⇒ 猪肉(顺带拿到部位)
//	在特价档里   ⇒ 特价桶(不属于具体部位)
//	都不在       ⇒ 不是本域的事(可乐/大米/…)⇒ 丢掉
//
// # 为什么是**批量**
//
// POS 一单几十行,而这是**每笔销售**都要做的事 ⇒ 逐行查会把 DB 打成筛子。
// ⇒ 一次事件两次查询(IN 批量)。
func (s *Service) classifySaleLines(ctx context.Context, branchID string, skus []string) (map[string]model.CutType, map[string]bool, map[string]bool, error) {
	cutOf := map[string]model.CutType{}
	promoOf := map[string]bool{}
	bulkLineOf := map[string]bool{}
	if len(skus) == 0 {
		return cutOf, promoOf, bulkLineOf, nil
	}
	var mappings []model.BranchCutMapping
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id IN ?", branchID, skus).
		Find(&mappings).Error; err != nil {
		return nil, nil, nil, fmt.Errorf("query branch_cut_mappings: %w", err)
	}
	for _, m := range mappings {
		cutOf[m.CubeProductID] = m.CutType
	}
	var promos []model.PromoSku
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id IN ? AND active = ?", branchID, skus, true).
		Find(&promos).Error; err != nil {
		return nil, nil, nil, fmt.Errorf("query promo_skus: %w", err)
	}
	for _, pr := range promos {
		promoOf[pr.CubeProductID] = true
	}
	// 大宗线的 POS 货号(2026-10-25):这些货号在 POS 上卖时**不按零售算**
	// ⇒ 由调用方跳过(改走「统收结算」手工登记)。
	var bulkLines []model.BulkProduct
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND pos_cube_product_id IN ?", branchID, skus).
		Find(&bulkLines).Error; err != nil {
		return nil, nil, nil, fmt.Errorf("query bulk_products(bulk line): %w", err)
	}
	for _, bl := range bulkLines {
		if bl.PosCubeProductID != "" {
			bulkLineOf[bl.PosCubeProductID] = true
		}
	}
	return cutOf, promoOf, bulkLineOf, nil
}

// resolvePigByCubeProduct 通过 cube_product_id 反查 (branch, biz-day) 候选 pig。
//
// ⚠️ **db 参数**:调用方在事务里时**必须传 tx** —— 事务里碰 `s.db`
// (另一条连接)会与写锁互等 ⇒ 死锁(本仓踩过的坑,2026-10-24)。
//
// 算法(2026-10-01 优化):
//
//  1. branch_cut_mapping 找 cube_product_id 对应的 cut_type;
//
//  2. 候选 pig 圈选:
//     - 限定 bizTZ 当日窗口(默认 Asia/Shanghai);
//     - 若 line 有 purchase_group_id(本服务外推):先按 group 匹配;
//     - 否则:所有当日 whole_pig 按 arrived_at ASC(早到仓的优先)。
//
//  3. 在候选集里:LLM advice 含该 cut_type 的 pig 优先(精匹配);
//
//     兜底:取 arrived_at 最早的候选 pig(场景一/二的单头/半头猪)。
//
// 找不到 pig 时返 empty(""), nil — 由 caller 决定 drop 该行。
func (s *Service) resolvePigByCubeProduct(ctx context.Context, db *gorm.DB, branchID, cubeProductID string, saleTime time.Time) (string, model.CutType, error) {
	var m model.BranchCutMapping
	err := db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id = ?", branchID, cubeProductID).
		Order("cut_type ASC"). // 确定性:DB 侧有 UNIQUE(branch_id, cube_product_id),这里只是兜底
		First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// ⚠️ 查不到部位映射 ⇒ **原来直接丢整行**(金额和重量一起丢)。
			// 现在先看它是不是**特价档货号**:是 ⇒ 归到 promo 桶,金额与重量进得来。
			isPromo, perr := s.isPromoSku(ctx, db, branchID, cubeProductID)
			if perr != nil {
				return "", "", perr
			}
			if !isPromo {
				return "", "", nil // 真不认识 ⇒ caller drop
			}
			pig, perr := s.findEarliestPigOfDay(ctx, db, branchID, saleTime)
			if perr != nil {
				return "", "", perr
			}
			return pig, model.CutPromo, nil
		}
		return "", "", err
	}

	// 候选集:当日窗口内 + arrived_at <= sale_time(sale 之前的猪才可能贡献该 cut)。
	startOfDay, endOfDay := s.businessDayBounds(saleTime)
	if saleTime.After(endOfDay) {
		saleTime = endOfDay
	}

	// 优先 LLM 精匹配:findPigByCutInAdvice 找当窗口内 llm_advice_json 含该 cut 的 pig。
	candidate, err := s.findPigByCutInAdvice(ctx, db, branchID, m.CutType, startOfDay, saleTime)
	if err == nil && candidate != "" {
		return candidate, m.CutType, nil
	}

	// Fallback:当日窗口内最早一头 pig(arrived_at ASC;默认场景一/二)。
	pig, err := s.findEarliestPigOfDay(ctx, db, branchID, saleTime)
	if err != nil {
		return "", "", err
	}
	return pig, m.CutType, nil
}

// findEarliestPigOfDay 找当日窗口内最早进场的一头猪(没有 ⇒ 空串,nil)。
func (s *Service) findEarliestPigOfDay(ctx context.Context, db *gorm.DB, branchID string, saleTime time.Time) (string, error) {
	startOfDay, endOfDay := s.businessDayBounds(saleTime)
	if saleTime.After(endOfDay) {
		saleTime = endOfDay
	}
	var pig model.WholePig
	if err := db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Order("arrived_at ASC").
		First(&pig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil // 当日没 pig → caller drop
		}
		return "", err
	}
	return pig.ID, nil
}

// isPromoSku 判断某货号是不是**特价档**(已登记且启用)。
func (s *Service) isPromoSku(ctx context.Context, db *gorm.DB, branchID, cubeProductID string) (bool, error) {
	var n int64
	if err := db.WithContext(ctx).
		Model(&model.PromoSku{}).
		Where("branch_id = ? AND cube_product_id = ? AND active = ?", branchID, cubeProductID, true).
		Count(&n).Error; err != nil {
		return false, fmt.Errorf("count promo_skus: %w", err)
	}
	return n > 0, nil
}

// findPigByCutInAdvice 在 llm_advice_json.cuts 数组里找含目标 cut_type 的 pig。
//
// PG 用 jsonb_array_elements + ->>'cut_type';SQLite 用 json_each + json_extract。
// 跨 dial 兼容:用 GORM raw SQL,根据驱动名分两套查询。
func (s *Service) findPigByCutInAdvice(ctx context.Context, db *gorm.DB, branchID string, cutType model.CutType, start, end time.Time) (string, error) {
	dialect := db.Dialector.Name()
	cutStr := string(cutType)

	var row struct {
		ID string
	}
	var query string
	switch dialect {
	case "postgres":
		// jsonb 数组元素 ->> 字段
		query = `
SELECT id FROM whole_pigs
WHERE branch_id = ?
  AND arrived_at >= ? AND arrived_at < ?
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements(llm_advice_json) elem
    WHERE elem->>'cut_type' = ?
  )
ORDER BY arrived_at DESC
LIMIT 1`
	case "sqlite":
		// json_extract(elem, '$.cut_type')
		query = `
SELECT id FROM whole_pigs
WHERE branch_id = ?
  AND arrived_at >= ? AND arrived_at < ?
  AND EXISTS (
    SELECT 1 FROM json_each(llm_advice_json) je,
                 json_extract(je.value, '$.cut_type') AS cut
    WHERE cut = ?
  )
ORDER BY arrived_at DESC
LIMIT 1`
	default:
		return "", nil // 不支持的 dialect → fallback 到 by string match
	}
	err := db.WithContext(ctx).Raw(query, branchID, start, end, cutStr).Scan(&row).Error
	if err != nil {
		return "", err
	}
	if row.ID == "" {
		return "", nil
	}
	return row.ID, nil
}

// OnAccessChanged 处理 auth.user.access_changed 事件,失效 scope cache。
//
// branchID 空 → 失效该 user 全部缓存(InvalidateScopeCache 内部实现)。
func (s *Service) OnAccessChanged(_ context.Context, userID, branchID string) {
	s.InvalidateScopeCache(userID, branchID)
}

// 静默引用 encoding/json(若用户剥离部分代码)。
var _ = json.Marshal
