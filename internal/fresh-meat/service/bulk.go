// Package service / bulk.go —— 大宗台账(冷库里的"不是零售、攒着统一结算"的东西)。
//
// # 业务场景(2026-10-23 与门店确认)
//
//	分割产生板油 8kg —— 它**不上猪肉台**(不零售),直接进冷库;
//	攒几天后酒店(统购方)一次性收走,**几天后才结账、才知道单价**。
//
// ⇒ 三个动作:
//	① 录产出(板油当日重量)   → 冷库 +
//	② 冷库 → 酒店统收结算     → 冷库 −,收入 +(单价这时才确定)
//	③ 结存查询               → 还剩多少没卖出去
//
// # 猪归属
//
// 录入**不指定猪**(门店决策:最省事)。猪归属在**结算时**算:
// 该产出日期的各头猪按 `gross_weight_kg` 比例分摊
// (板油产量与猪的重量正相关)。
//
// # 收入落点
//
// 结算收入写进 `line_sales_by_pigs`(source="bulk"),**记在产出日**而不是结算日 ⇒
// 产出它的那头猪当天就拿到收入,单猪毛利在正确的日子闭合。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 大宗结算模式。
const (
	// BulkModeDelivery 交付离店:货交给酒店,结算 = 收入,库存减少(默认)。
	BulkModeDelivery = "delivery"
	// BulkModePriceDiff 价差结算:货已零售,只结价差(库存不变)。
	BulkModePriceDiff = "price_diff"
)

// 大宗产出的来源(2026-10-23)。
const (
	// BulkSourceStocktake 日终盘点**自动结转**(主路径,门店不用手工录)。
	BulkSourceStocktake = "stocktake"
	// BulkSourceManual 手工兜底(盘点没录该部位时)。
	BulkSourceManual = "manual"
)

// BulkProductInput POST /bulk-products 入参。
//
// ⚠️ 2026-10-24(门店口径):大宗品项**只针对部位** —— 用户**只选一个部位**,
// 名称 / 编码 / 单位 / POS 货号**都不需要填**(服务端自动填)。
//
// 大宗台账的目的只是"协助每日毛利计算与部位毛利分析":
// 看结存 + 结算时选部位(顺带显示参考成本单价) ⇒ 越简单越好。
type BulkProductInput struct {
	// CutType 必填:这个大宗品项对应哪个部位。
	CutType model.CutType `json:"cut_type" binding:"required"`
	Note    string        `json:"note"`
}

// CreateBulkProduct 登记大宗品项(**只选部位**,其余服务端自动填)。
func (s *Service) CreateBulkProduct(ctx context.Context, branchID string, in BulkProductInput) (*model.BulkProduct, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if !in.CutType.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举内", ErrInvalidInput, in.CutType, len(model.AllCutTypes))
	}
	// 编码 = 部位 wire 值;名称 = 部位中文名;单位 = kg。
	//
	// ⚠️ 为什么自动填而不是让用户填:门店口径是"大宗品项就是某个部位",
	// 让用户再编一个编码 ⇒ 同一个部位出现两个"品项"(编码不同)⇒ 结存与结算各算一半。
	code := string(in.CutType)
	name := model.CutLabel(in.CutType)
	unit := "kg"
	// 部位**不可重复**(门店要求:大宗品项从部位里选,同一部位只能登记一个)。
	//
	// ⚠️ 先查再报,而不是靠唯一索引冲突:
	// 冲突时 PG 只说"某处唯一键冲突",操作者看不懂是 code 撞了还是部位撞了。
	var sameCut model.BulkProduct
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type = ?", branchID, in.CutType).
		First(&sameCut).Error
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: 部位 %s 已经登记过大宗品项「%s」,不能重复登记",
			ErrBulkProductConflict, in.CutType, sameCut.Name)
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, fmt.Errorf("query bulk_products: %w", err)
	}
	now := s.now()
	rec := &model.BulkProduct{
		ID:        generateID(now, "BKP"),
		BranchID:  branchID,
		Code:      code,
		Name:      name,
		CutType:   in.CutType,
		Unit:      unit,
		Note:      strings.TrimSpace(in.Note),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(rec).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: (%s, %s) 已存在", ErrBulkProductConflict, branchID, code)
		}
		return nil, fmt.Errorf("create bulk_product: %w", err)
	}
	return rec, nil
}

// ListBulkProducts 列某店的大宗品项。
func (s *Service) ListBulkProducts(ctx context.Context, branchID string) ([]model.BulkProduct, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.BulkProduct
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("code ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query bulk_products: %w", err)
	}
	return rows, nil
}

// BulkProductionInput POST /bulk-productions 入参(录当日产出)。
type BulkProductionInput struct {
	ProductCode string          `json:"product_code" binding:"required"`
	BizDate     string          `json:"biz_date"` // YYYY-MM-DD,空 = 当日
	QtyKg       decimal.Decimal `json:"qty_kg" binding:"required"`
	Note        string          `json:"note"`
}

// RecordBulkProduction 录当日大宗产出(进冷库)。
//
// ⚠️ **手工兜底路径**。**主路径是"日终盘点自动结转"**
// (盘点会把剩余的货按 SKU 登记成**结转记录/袋子**,见 registerStockBags):
// 手工录 = 同一个数字记两处,迟早不一致。
// 与自动结转**互斥**:当天该部位已有盘点结转 ⇒ 400。
//
// # 2026-10-25:它记的是**袋子**,不是台账
//
// ⇒ 大宗结算的 FIFO 队列 = 袋子队列(见 service/bulk_fifo.go)
// ⇒ 手工补录的货**也必须进队列**,否则它结不出去(冷库里有、账上没有)。
//
// ⇒ 返回体保持 BulkProduction 的形状(前端不用改),
// 但数据源已经是袋子了 —— 里面的 ID 是**这张手工单**的 ID。
func (s *Service) RecordBulkProduction(ctx context.Context, branchID string, in BulkProductionInput, operatorID string) (*model.BulkProduction, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if !in.QtyKg.IsPositive() {
		return nil, fmt.Errorf("%w: qty_kg 必须 > 0", ErrInvalidInput)
	}
	prod, err := s.getBulkProduct(ctx, branchID, strings.TrimSpace(in.ProductCode))
	if err != nil {
		return nil, err
	}
	now := s.now()
	bizDate := datatypesDate(now.In(s.bizTZOrUTC()))
	if d := strings.TrimSpace(in.BizDate); d != "" {
		parsed, perr := time.ParseInLocation("2006-01-02", d, s.bizTZOrUTC())
		if perr != nil {
			return nil, fmt.Errorf("%w: biz_date 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		bizDate = datatypesDate(parsed)
	}
	// 冻结检查:已审计结转的期间不能再改(含补录)。
	if err := s.assertNotAudited(ctx, branchID, time.Time(bizDate), "大宗产出"); err != nil {
		return nil, err
	}
	// 大宗库存记在**大宗品项自己绑的货号**上(没绑定才回退到部位主 SKU)。
	//
	// ⚠️ 全链路(进冷库 / 出冷库 / 结存)必须记在**同一个 SKU** 上,
	// 否则库存对不上(进记 A、出记 B ⇒ 冷库凭空多出一批)。
	//
	// ⚠️ 不用 `bulk:<code>` 伪 SKU:它没有入库金额 ⇒ 移动加权单价 0
	// ⇒ 统收成本 0 ⇒ 毛利率 100%。
	sku, err := s.bulkStorageSKU(ctx, branchID, prod)
	if err != nil {
		return nil, err
	}
	// 与自动结转互斥:当天该部位已有**盘点结转记录** ⇒ 不要再手工录。
	//
	// ⚠️ 2026-10-25:判据从"台账 bulk_productions"改成"**结转记录(袋子)**"
	//   —— 台账已停写,判据必须跟队列一致,否则同一个数字记两处。
	startOfDay, endOfDay := s.businessDayBounds(time.Time(bizDate))
	var autoCnt int64
	if err := s.db.WithContext(ctx).Model(&model.StockBag{}).
		Where("branch_id = ? AND cube_product_id = ? AND source = ? AND qty_kg > ?",
			branchID, sku, BagSourceStocktake, decimal.Zero).
		Where("origin_date >= ? AND origin_date < ?", startOfDay, endOfDay).
		Count(&autoCnt).Error; err != nil {
		return nil, fmt.Errorf("query stock_bags(bulk manual guard): %w", err)
	}
	if autoCnt > 0 {
		return nil, fmt.Errorf("%w: %s 当天的日终盘点已经登记过结转记录了,不用再手工录", ErrInvalidInput, prod.Name)
	}
	// 成本快照:入冷库那一刻的移动加权单价 —— 这就是"记录分摊成本"。
	//
	// ⚠️ 2026-10-27:成本链改部位级 ⇒ 单价按**部位**取(大宗本来就按部位记)。
	unitCost := s.bulkUnitCost(ctx, branchID, prod.CutType, time.Time(bizDate))
	productionID := generateID(now, "BKP")
	// 归属快照:这批货是当天哪几头猪的产出(按分割补录;没有就空 ⇒ 结算时按毛重兜底)。
	alloc, err := s.bagPigAlloc(ctx, branchID, prod.CutType, sku, time.Time(bizDate))
	if err != nil {
		return nil, err
	}
	// 建**结转记录(袋子)** —— 它才是 FIFO 队列的一员。
	bag := model.StockBag{
		ID:            StockBulkManualBagID(branchID, productionID),
		BranchID:      branchID,
		Label:         "", // 手工补录没有标签号 ⇒ 是"不装袋的散货结转记录"
		CubeProductID: sku,
		CutType:       prod.CutType,
		QtyKg:         in.QtyKg,
		OriginDate:    bizDate,
		Location:      string(model.StockLocationColdStore),
		Status:        BagStatusActive,
		Source:        BagSourceBulkManual,
		PigAlloc:      alloc,
		UnitCostYuan:  unitCost,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.db.WithContext(ctx).Create(&bag).Error; err != nil {
		return nil, fmt.Errorf("create bulk manual stock_bag: %w", err)
	}
	// 冷库 +=(记在**部位主 SKU** 上 ⇒ 与零售/盘点共用同一个 SKU,成本跟着货走)
	if err := s.addPosition(ctx, s.db, branchID, sku, model.StockLocationColdStore, in.QtyKg); err != nil {
		return nil, err
	}
	// 按营业日推导的冷库余额变了 ⇒ 重算那天的结余快照。
	if err := s.recomputeCostSnapshots(ctx, branchID, time.Time(bizDate)); err != nil {
		s.pubLogger.Error("RecordBulkProduction: 结余快照重算失败",
			"branch_id", branchID, "biz_date", dateKey(bizDate), "err", err)
	}
	// 返回体保持 BulkProduction 的形状(前端/契约测试不用改)。
	return &model.BulkProduction{
		ID:            productionID,
		BranchID:      branchID,
		ProductCode:   prod.Code,
		BizDate:       bizDate,
		QtyKg:         in.QtyKg,
		SettledQtyKg:  decimal.Zero,
		Source:        BulkSourceManual,
		CubeProductID: sku,
		UnitCostYuan:  unitCost,
		Note:          strings.TrimSpace(in.Note),
		OperatorID:    operatorID,
		CreatedAt:     now,
	}, nil
}

// ListBulkProductions 列某店的大宗结转记录。
//
// ⚠️ 2026-10-25:台账表(bulk_productions)已**停写** —— 同一事实不再记两处
//   (台账记一份、袋子记一份 ⇒ 结算按台账扣、零售按袋子扣 ⇒ 同一批货卖两遍)。
//   ⇒ 这里返回的是**从结转记录(袋子)派生**的视图,**字段形状不变**
//   (前端与契约测试不用改):
//   - 一行 = 一个批次(一个袋子 / 一行散货结转记录);
//   - qty_kg = 那批货的重量;
//   - settled_qty_kg = 那批货被**大宗结算**领用了多少(按分摊凭据聚合);
//   - source = stocktake(日终盘点) | manual(手工补录)。
func (s *Service) ListBulkProductions(ctx context.Context, branchID string) ([]model.BulkProduction, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	products, err := s.ListBulkProducts(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// SKU → 大宗品项编码(大宗库存记在**部位主 SKU** 上)。
	skuToCode := map[string]string{}
	for i := range products {
		p := &products[i]
		sku, skuErr := s.bulkStorageSKU(ctx, branchID, p)
		if skuErr != nil {
			// 该部位没映射 SKU ⇒ 这一品项没有可结的货(跳过,别猜)。
			s.pubLogger.Warn("ListBulkProductions: 部位没有映射 SKU,跳过该品项",
				"branch_id", branchID, "product_code", p.Code, "cut_type", p.CutType, "err", skuErr)
			continue
		}
		skuToCode[sku] = p.Code
	}
	if len(skuToCode) == 0 {
		return []model.BulkProduction{}, nil
	}
	skus := make([]string, 0, len(skuToCode))
	for sku := range skuToCode {
		skus = append(skus, sku)
	}
	var bags []model.StockBag
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id IN ?", branchID, skus).
		Order("origin_date ASC, id ASC").
		Find(&bags).Error; err != nil {
		return nil, fmt.Errorf("query stock_bags(bulk ledger): %w", err)
	}
	settled, err := s.bulkSettledKgByBag(ctx, branchID)
	if err != nil {
		return nil, err
	}
	out := make([]model.BulkProduction, 0, len(bags))
	for _, bag := range bags {
		code := skuToCode[bag.CubeProductID]
		if code == "" {
			continue
		}
		src := BulkSourceStocktake
		note := "日终盘点结转"
		if bag.Source == BagSourceBulkManual {
			src = BulkSourceManual
			note = "手工补录"
		}
		out = append(out, model.BulkProduction{
			ID:            bag.ID,
			BranchID:      branchID,
			ProductCode:   code,
			BizDate:       bag.OriginDate,
			QtyKg:         bag.QtyKg,
			SettledQtyKg:  settled[bag.ID],
			Source:        src,
			CubeProductID: bag.CubeProductID,
			UnitCostYuan:  bag.UnitCostYuan,
			Note:          note,
			CreatedAt:     bag.CreatedAt,
		})
	}
	return out, nil
}

// bulkSettledKgByBag 每个**结转记录(袋子)**被大宗结算领用了多少(按分摊凭据聚合)。
//
// ⚠️ 只认**交付口径**(delivery):价差结算(price_diff)不动库存。
func (s *Service) bulkSettledKgByBag(ctx context.Context, branchID string) (map[string]decimal.Decimal, error) {
	type row struct {
		BagID string
		QtyKg string
	}
	var rows []row
	if err := s.db.WithContext(ctx).
		Table("bulk_settlement_allocations AS a").
		Select("a.bag_id AS bag_id, CAST(SUM(a.qty_kg) AS TEXT) AS qty_kg").
		Joins("JOIN bulk_settlements AS s ON s.id = a.settlement_id").
		Where("s.branch_id = ? AND s.mode = ? AND a.bag_id <> ''", branchID, BulkModeDelivery).
		Group("a.bag_id").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query bulk allocations(by bag): %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		kg, perr := decimal.NewFromString(r.QtyKg)
		if perr != nil {
			continue
		}
		out[r.BagID] = kg
	}
	return out, nil
}

// BulkSettlementInput POST /bulk-settlements 入参(**表头 + 明细**)。
//
// 门店原话(2026-10-23):「整个统收结算设计为表头,明细模式,
// 表头有结算日期、对方、备注,明细为大宗品项、结算数量、单价」。
//
// ⇒ 一次可以把板油、水油**一起**交给酒店,金额也归到同一张单上。
type BulkSettlementInput struct {
	// SettleDate 是**结算日期**(YYYY-MM-DD)。空 = 当天。
	SettleDate string `json:"settle_date"`
	// Counterparty 是**对方**(酒店 / 购货方)。
	Counterparty string `json:"counterparty"`
	Mode         string `json:"mode"`
	Note         string `json:"note"`
	// Items 是结算明细(至少一条)。
	Items []BulkSettlementItemInput `json:"items" binding:"required,min=1,dive"`
}

// BulkSettlementItemInput 一条明细:哪个大宗品项、多少 kg、什么单价。
type BulkSettlementItemInput struct {
	ProductCode   string          `json:"product_code" binding:"required"`
	QtyKg         decimal.Decimal `json:"qty_kg" binding:"required"`
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan" binding:"required"`
}

// BulkSettlementView 是结算单(表头 + 明细)—— 列表与详情都用它。
//
// ⚠️ **显式字段,而不是内嵌 model.BulkSettlement**:
// 内嵌会把 `settle_date` 以 datatypes.Date 的 JSON 输出 ⇒ 是**完整时间戳**
// ("2026-10-23T19:24:39.55+08:00")⇒ 前端拿它跟输入的 "2026-10-23" 对不上。
// ⇒ 这里输出干净的 "YYYY-MM-DD"。
type BulkSettlementView struct {
	ID              string                     `json:"id"`
	BranchID        string                     `json:"branch_id"`
	SettleDate      string                     `json:"settle_date"`
	TotalAmountYuan decimal.Decimal            `json:"total_amount_yuan"`
	Counterparty    string                     `json:"counterparty,omitempty"`
	Mode            string                     `json:"mode"`
	Note            string                     `json:"note,omitempty"`
	SettledAt       time.Time                  `json:"settled_at"`
	OperatorID      string                     `json:"operator_id"`
	CreatedAt       time.Time                  `json:"created_at"`
	Items           []model.BulkSettlementItem `json:"items"`
}

// newBulkSettlementView 把模型行转成 API 视图(日期转 "YYYY-MM-DD")。
func newBulkSettlementView(row model.BulkSettlement, items []model.BulkSettlementItem) BulkSettlementView {
	if items == nil {
		items = []model.BulkSettlementItem{}
	}
	return BulkSettlementView{
		ID:              row.ID,
		BranchID:        row.BranchID,
		SettleDate:      dateKey(row.SettleDate),
		TotalAmountYuan: row.TotalAmountYuan,
		Counterparty:    row.Counterparty,
		Mode:            row.Mode,
		Note:            row.Note,
		SettledAt:       row.SettledAt,
		OperatorID:      row.OperatorID,
		CreatedAt:       row.CreatedAt,
		Items:           items,
	}
}

// CreateBulkSettlement 酒店统收结算(交付离店口径),**表头 + 明细**。
//
// # 流程(2026-10-25 改:FIFO 队列统一到**袋子**)
//
//  1. 校验表头(结算日期/对方)与每条明细(品项/数量/单价);
//  2. 建表头(拿到**单号**);
//  3. **逐条明细** FIFO 吃「结算日期之前(含当天)」的**结转记录(袋子)**;
//  4. 每袋的 kg 按**袋子的归属快照**(PigAlloc)摊到猪(没有快照才按毛重兜底);
//  5. 写 line_sales_by_pigs(source="bulk", 记在**产生日**)⇒ 产出它的猪当天就有收入;
//  6. 扣这袋的 consumed_kg(与零售**共用同一个水位**)⇒ 同一批货不会被卖两遍;
//     交付口径再按**领用时所在位置**扣库存;
//  7. 存明细行 + allocations(结算 → 明细 → **袋子** → 猪 的凭据)。
//
// ⚠️ 全部在**一个事务**里:中途某条明细的可领用不足 ⇒ 整单回滚
// (不能出现"半张单已出库")。
func (s *Service) CreateBulkSettlement(ctx context.Context, branchID string, in BulkSettlementInput, operatorID string) (*BulkSettlementView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: 结算明细不能为空", ErrInvalidInput)
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = BulkModeDelivery
	}
	if mode != BulkModeDelivery && mode != BulkModePriceDiff {
		return nil, fmt.Errorf("%w: mode 只支持 %s | %s", ErrInvalidInput, BulkModeDelivery, BulkModePriceDiff)
	}
	now := s.now()
	settleDate := datatypesDate(now.In(s.bizTZOrUTC()))
	if d := strings.TrimSpace(in.SettleDate); d != "" {
		parsed, perr := time.ParseInLocation("2006-01-02", d, s.bizTZOrUTC())
		if perr != nil {
			return nil, fmt.Errorf("%w: settle_date 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		settleDate = datatypesDate(parsed)
	}
	// 冻结检查:已审计结转的期间不能再改(含补录)。
	if err := s.assertNotAudited(ctx, branchID, time.Time(settleDate), "大宗结算"); err != nil {
		return nil, err
	}
	// 明细预校验(失败要在开事务前就报,免得白开一次)
	for i, it := range in.Items {
		if !it.QtyKg.IsPositive() {
			return nil, fmt.Errorf("%w: items[%d].qty_kg 必须 > 0", ErrInvalidInput, i)
		}
		if !it.UnitPriceYuan.IsPositive() {
			return nil, fmt.Errorf("%w: items[%d].unit_price_yuan 必须 > 0(单价在结算时才确定)", ErrInvalidInput, i)
		}
	}

	total := decimal.Zero
	settlement := &model.BulkSettlement{
		ID:           generateID(now, "BKS"),
		BranchID:     branchID,
		SettleDate:   settleDate,
		Counterparty: strings.TrimSpace(in.Counterparty),
		Mode:         mode,
		Note:         strings.TrimSpace(in.Note),
		SettledAt:    now,
		OperatorID:   operatorID,
		CreatedAt:    now,
	}

	var createdItems []model.BulkSettlementItem

	// ⚠️ **品项与主 SKU 必须在事务外解析**。
	//
	// getBulkProduct / bulkCutSku 都走 `s.db`(**另一条连接**)⇒ 在事务里调它们
	// 会与本事务的写锁互等 ⇒ SQLite 直接 **database is locked**(实测挂死 600s)。
	// ⇒ 先全部解析好,事务里只做写。
	type resolvedItem struct {
		in   BulkSettlementItemInput
		prod *model.BulkProduct
		sku  string
	}
	resolved := make([]resolvedItem, 0, len(in.Items))
	for _, it := range in.Items {
		prod, err := s.getBulkProduct(ctx, branchID, strings.TrimSpace(it.ProductCode))
		if err != nil {
			return nil, err
		}
		// 大宗库存/收入记在**部位主 SKU** 上(不是 `bulk:<code>` 伪 SKU)。
		//
		// ⚠️ 2026-10-23 修正(实测):记在伪 SKU 上时,伪 SKU 没有入库金额
		// ⇒ 移动加权单价恒为 0 ⇒ **统收成本 = 0** ⇒ 毛利率 100%。
		sku, err := s.bulkStorageSKU(ctx, branchID, prod)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, resolvedItem{in: it, prod: prod, sku: sku})
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(settlement).Error; err != nil {
			return fmt.Errorf("create bulk_settlement: %w", err)
		}
		for i, r := range resolved {
			it := r.in
			prod := r.prod
			sku := r.sku
			_ = i
			// 明细行**先建**(拿到 item.ID)—— 下面的 allocations 要引用它。
			//
			// ⚠️ 不要在最后用 "UPDATE ... WHERE settlement_id=? AND item_id IS NULL"
			// 回填:那样第二条明细的更新会把**第一条**明细的分摊也改掉
			// (它们的 item_id 也是 NULL)⇒ 分摊归到错误的明细。
			item := model.BulkSettlementItem{
				ID:            generateID(now, "BKI"),
				BranchID:      branchID,
				SettlementID:  settlement.ID,
				ProductCode:   prod.Code,
				QtyKg:         it.QtyKg,
				UnitPriceYuan: it.UnitPriceYuan,
				AmountYuan:    it.QtyKg.Mul(it.UnitPriceYuan),
				CreatedAt:     now,
			}
			if err := tx.Create(&item).Error; err != nil {
				return fmt.Errorf("create bulk_settlement_item: %w", err)
			}
			// ---- FIFO:从**结转记录(袋子)**队列领用 ----
			//
			// ⚠️ 2026-10-25 口径修正:队列不再吃 bulk_productions 台账,
			//   而是吃**袋子**(与零售 FIFO 同一套队列、共用 consumed_kg 水位)
			//   ⇒ 「结算的领用」与「袋子」是同一批货 ⇒ 不会被卖两遍。
			takes, short, err := s.allocateBulkByBIFO(ctx, tx, branchID, sku, settleDate, it.QtyKg)
			if err != nil {
				return err
			}
			if short.IsPositive() {
				return fmt.Errorf("%w: %s 可领用量不足,还差 %s kg(结算日期 %s)",
					ErrInvalidInput, prod.Name, short.String(), dateKey(settleDate))
			}
			for _, tk := range takes {
				// 猪份额:袋子的归属快照 → 当天各猪毛重 → 结算当天各猪毛重
				// (见 bulkBagShares)。
				kgByPig := map[string]decimal.Decimal{}
				for _, sh := range tk.Shares {
					kgByPig[sh.pigID] = kgByPig[sh.pigID].Add(sh.kg)
				}
				amount := tk.QtyKg.Mul(it.UnitPriceYuan)
				split := splitSaleByPig(tk.QtyKg, amount, kgByPig, sortedPigIDs(kgByPig))
				if len(split) == 0 {
					// ⚠️ 这批货**摊不出归属**(那天没有猪)⇒ 记一行"无归属猪"的收入:
					//   钱不能凭空消失(结算单金额必须与损益对得上),
					//   但也不能硬塞给某头猪(那会让单猪毛利失真)。
					split = []splitSaleRow{{PigID: "", QtyKg: tk.QtyKg, AmountYuan: amount}}
				}
				for _, sp := range split {
					// ⚠️ PosLineID 必须**短**:它进 `line_sales_by_pigs` 的唯一键
					// (varchar(64))。用"结算单+明细+袋子+猪"的 id **拼**会超长
					// ⇒ 改成**哈希**:短、定长、且天然带上全部四个维度。
					posLineID := "BULK" + hashKey(settlement.ID, item.ID, tk.Bag.ID, sp.PigID)
					row := model.LineSalesByPig{
						BranchID:      branchID,
						SaleDate:      tk.Bag.OriginDate,
						PigID:         sp.PigID,
						CutType:       prod.CutType,
						CubeProductID: sku,
						QtyKg:         sp.QtyKg,
						AmountYuan:    sp.AmountYuan,
						UnitPriceYuan: it.UnitPriceYuan,
						Source:        "bulk",
						PosLineID:     posLineID,
						PosBranchID:   branchID,
						OrderStatus:   "S",
						CreatedAt:     now,
					}
					if err := tx.Clauses(clause.OnConflict{
						Columns:   []clause.Column{{Name: "pos_branch_id"}, {Name: "pos_line_id"}},
						DoNothing: true,
					}).Create(&row).Error; err != nil {
						return err
					}
					alloc := model.BulkSettlementAllocation{
						ID:             generateID(now, "BKA"),
						SettlementID:   settlement.ID,
						ItemID:         item.ID,
						ProductionID:   "", // 2026-10-25:不再指向台账(台账停写)
						BagID:          tk.Bag.ID,
						Location:       tk.Bag.Location,
						CubeProductID:  sku,
						CutType:        prod.CutType,
						PigID:          sp.PigID,
						QtyKg:          sp.QtyKg,
						AmountYuan:     sp.AmountYuan,
						ProductionDate: tk.Bag.OriginDate,
						CreatedAt:      now,
					}
					if err := tx.Create(&alloc).Error; err != nil {
						return err
					}
				}
				// ★ 扣这袋的"已消耗" —— 与零售**共用同一个水位**
				// ⇒ 同一批货不可能既被零售卖掉、又被大宗结一遍。
				if err := s.consumeFIFO(ctx, tx, map[string]decimal.Decimal{tk.Bag.ID: tk.QtyKg}); err != nil {
					return err
				}
				// 交付口径:按**领用时那批货所在的位置**扣库存
				// (冷库里的扣冷库、搬上台过的扣柜台)⇒ 位置表与日余额链都对得上。
				if mode == BulkModeDelivery {
					if err := s.addPosition(ctx, tx, branchID, sku, bulkBagLocation(tk.Bag), tk.QtyKg.Neg()); err != nil {
						return err
					}
				}
			}
			createdItems = append(createdItems, item)
			total = total.Add(item.AmountYuan)
		}
		settlement.TotalAmountYuan = total
		if err := tx.Model(settlement).Update("total_amount_yuan", total).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	view := newBulkSettlementView(*settlement, createdItems)
	// 按营业日推导的冷库余额变了(大宗可以直接从冷库出)⇒ 重算那天的结余快照。
	if err := s.recomputeCostSnapshots(ctx, branchID, time.Time(settleDate)); err != nil {
		s.pubLogger.Error("CreateBulkSettlement: 结余快照重算失败",
			"branch_id", branchID, "settle_date", dateKey(settleDate), "err", err)
	}
	return &view, nil
}

// ListBulkSettlements 列某店的统收结算单(表头 + 明细)。
func (s *Service) ListBulkSettlements(ctx context.Context, branchID string) ([]BulkSettlementView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.BulkSettlement
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("settle_date DESC, settled_at DESC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query bulk_settlements: %w", err)
	}
	out := make([]BulkSettlementView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	var items []model.BulkSettlementItem
	if err := s.db.WithContext(ctx).
		Where("settlement_id IN ?", ids).
		Order("created_at ASC").
		Find(&items).Error; err != nil {
		return nil, fmt.Errorf("query bulk_settlement_items: %w", err)
	}
	bySettlement := map[string][]model.BulkSettlementItem{}
	for _, it := range items {
		bySettlement[it.SettlementID] = append(bySettlement[it.SettlementID], it)
	}
	for _, r := range rows {
		list := bySettlement[r.ID]
		if list == nil {
			list = []model.BulkSettlementItem{}
		}
		out = append(out, newBulkSettlementView(r, list))
	}
	return out, nil
}

// BulkBalance 单个品项的结存。
type BulkBalance struct {
	ProductCode string          `json:"product_code"`
	ProductName string          `json:"product_name"`
	CutType     model.CutType   `json:"cut_type"`
	Unit        string          `json:"unit"`
	ProducedKg  decimal.Decimal `json:"produced_kg"`
	SettledKg   decimal.Decimal `json:"settled_kg"`
	// BalanceKg 是**还能领用多少**(结转队列里还没被消耗的重量)。
	//
	// ⚠️ 2026-10-25 口径变更:以前是"台账结余"(产出 − 已结,另一份账),
	//   现在是**队列剩余** —— 与结算 FIFO 吃的是**同一个数**
	//   ⇒ 门店在结算页填的量不会超过它(超了服务端会 400)。
	BalanceKg decimal.Decimal `json:"balance_kg"`
	// ColdStoreKg 是**冷库实际余额**(位置表)—— 物理真相。
	//
	// ⚠️ 结存以它为准:队列是"我们记账领用了多少",冷库是"实际有多少"。
	// 两者对不上时,报出来(见 LedgerVsColdDiffKg),不要挑一个信。
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`
	// ColdStoreCostYuan 是冷库结存的**成本金额**(冷库 kg × 部位主 SKU 的移动加权单价)
	// —— 这就是"板油压在冷库里的钱"。
	ColdStoreCostYuan decimal.Decimal `json:"cold_store_cost_yuan"`
	// LedgerVsColdDiffKg = 队列剩余 − 冷库实际余额。
	//
	// 正常应为 0。不为 0 的常见原因:
	//   - 冷库里有**期初**的货(没经过盘点结转 ⇒ 不在队列里)⇒ 差值为负;
	//   - 按 kg 搬上台(没按标签搬)⇒ 货在台上、袋子还在冷库 ⇒ 差值为正。
	//
	// ⇒ **显式给出差额**,不藏:这类差额藏起来就变成"库存说不清"。
	LedgerVsColdDiffKg decimal.Decimal `json:"ledger_vs_cold_diff_kg"`
	// UnitCostYuan 是该部位主 SKU 当前的移动加权单价(元/kg)。
	UnitCostYuan decimal.Decimal `json:"unit_cost_yuan"`
}

// ListBulkBalances 查某店各大宗品项的结存。
//
// # 2026-10-25:三条口径都从**结转记录(袋子)**派生
//
//   - 还能领用(队列剩余)—— 结算 FIFO 吃它
//   - 冷库实际余额(位置表)—— **物理真相**
//   - 两者的差额 —— 对不上就说出来
func (s *Service) ListBulkBalances(ctx context.Context, branchID string) ([]BulkBalance, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	products, err := s.ListBulkProducts(ctx, branchID)
	if err != nil {
		return nil, err
	}
	settledByBag, err := s.bulkSettledKgByBag(ctx, branchID)
	if err != nil {
		return nil, err
	}
	_, cold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]BulkBalance, 0, len(products))
	for i := range products {
		p := &products[i]
		sku, skuErr := s.bulkStorageSKU(ctx, branchID, p)
		coldKg := decimal.Zero
		unitCost := decimal.Zero
		produced := decimal.Zero
		available := decimal.Zero
		settled := decimal.Zero
		if skuErr == nil {
			coldKg = cold[sku]
			unitCost = s.bulkUnitCost(ctx, branchID, p.CutType, now)
			// 该 SKU 的**全部结转记录**(袋子 + 散货行)⇒ 产出与队列剩余。
			var bags []model.StockBag
			if err := s.db.WithContext(ctx).
				Where("branch_id = ? AND cube_product_id = ?", branchID, sku).
				Find(&bags).Error; err != nil {
				return nil, fmt.Errorf("query stock_bags(bulk balance): %w", err)
			}
			for _, bag := range bags {
				produced = produced.Add(bag.QtyKg)
				settled = settled.Add(settledByBag[bag.ID])
				if bag.Status != BagStatusActive {
					continue // 已搬上台的袋子不再是"可领用"的货
				}
				remain := bag.QtyKg.Sub(bag.ConsumedKg)
				if remain.IsPositive() {
					available = available.Add(remain)
				}
			}
		}
		out = append(out, BulkBalance{
			ProductCode:        p.Code,
			ProductName:        p.Name,
			CutType:            p.CutType,
			Unit:               p.Unit,
			ProducedKg:         produced,
			SettledKg:          settled,
			BalanceKg:          available,
			ColdStoreKg:        coldKg,
			ColdStoreCostYuan:  coldKg.Mul(unitCost),
			LedgerVsColdDiffKg: available.Sub(coldKg),
			UnitCostYuan:       unitCost,
		})
	}
	return out, nil
}

// DeleteBulkProduct **物理删除**一个大宗品项(2026-10-24 门店口径)。
//
// ⚠️ 门店原话:「可直接删除大宗品项而不是停用,统收结算就不再可选,
// 后续可重新添加,已产生的结算记录可保留。」
//
// ⇒ 物理删(不是 active=false):
//   - 删除后该部位**可以重新登记**;
//   - **已产生的结算记录保留**(结算单/明细引用的是 product_code 文本,
//     不是外键 ⇒ 品项删了,历史结算单照样查得到、金额不变)。
//
// ⚠️ 只有"还没产生过结算/产出"的品项才适合删;
// 带台账的品项删了 ⇒ 结存页会少一条,历史台账行的 product_code 变孤儿
// (记录仍在,但查不到对应的品项名)。
func (s *Service) DeleteBulkProduct(ctx context.Context, branchID, id string) error {
	var rec model.BulkProduct
	if err := s.db.WithContext(ctx).First(&rec, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBranchCutMappingNotFound
		}
		return err
	}
	if rec.BranchID != branchID {
		return ErrBranchMismatch
	}
	if err := s.db.WithContext(ctx).Delete(&model.BulkProduct{}, "id = ?", id).Error; err != nil {
		return fmt.Errorf("delete bulk_product: %w", err)
	}
	return nil
}

// getBulkProduct 取大宗品项(不存在 → 400)。
func (s *Service) getBulkProduct(ctx context.Context, branchID, code string) (*model.BulkProduct, error) {
	var p model.BulkProduct
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND code = ?", branchID, code).
		First(&p).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: 大宗品项 %q 未登记(先 POST /bulk-products)", ErrInvalidInput, code)
		}
		return nil, err
	}
	return &p, nil
}
