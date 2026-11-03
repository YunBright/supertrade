package service

// internal/fresh-meat/service/carryover_records.go —— **按日结转记录**(结转记录台账)。
//
// # ⚠️ 2026-11-02 owner:旧的整批搬台玩法整套下线
//
// 原来这张表装的是「批 + 批次标记」:门店盘点时给每批做一个批次标记,
// 次日「按批次整批搬」上猪肉台。搬台不再有批次标记这条路、盘点也不再填批次标记
// ⇒ 这张表退化成「**按日结转台账**」:一行 = (归属日 + SKU + 重量 + 猪归属快照)。
//
// # 这张表现在还解决三件事
//
// ① **每行要有归属**:这批货是当天哪几头猪的产出 ⇒ 卖掉时毛利能**回归**
//    到产出它的那头猪(而不是今天碰巧在场的猪)。
// ② **FIFO 队列**:按产生日排队 ⇒ 卖掉一笔货时,收入按「结转先后顺序」摊回猪。
// ③ **成本锚点**:每行钉着「入冷库那一刻的移动加权单价」⇒ 大宗隔几天才结账时
//    成本不会被"当日单价"摊歪。
//
// # 粒度:同一 (归属日, SKU) 一行
//
// 拆批(同一 SKU 分多批)是"每批一个批次标记"的衍生物 ⇒ 旧玩法下线后没有多批的身份
// ⇒ 前端不再拆批;老 APK 若仍拆着提交 ⇒ 后端**合并求和**(见 registerCarryoverRecords)。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 结转记录的状态。
const (
	// CarryoverStatusActive 在用。
	CarryoverStatusActive = "active"
	// CarryoverStatusReleased 已搬上猪肉台。
	//
	// ⚠️ 2026-11-02 owner:旧的整批搬台玩法下线 ⇒ 不再有"搬上台 ⇒ 释放批次标记"这条路径
	//   ⇒ 新行一律 active;released 只剩历史行(按批次整批搬时代写的)。
	CarryoverStatusReleased = "released"
)

// 结转记录的**来源**(2026-10-25)。
//
// ⚠️ 为什么结转记录要记来源:「同一天重提交盘点 ⇒ 清掉这次没出现的旧行」这条清理
// 只能清**盘点来的**行 ⇒ 手工补录的冷库货必须活着(货还在冷库里)。
const (
	// CarryoverSourceStocktake 日终盘点登记(主路径)。
	CarryoverSourceStocktake = "stocktake"
	// CarryoverSourceBulkManual 大宗手工兜底产出(盘点没录该部位时)。
	CarryoverSourceBulkManual = "bulk_manual"
	// CarryoverSourceOpening **期初结转**(上线前冷库里就有的货)。
	//
	// ⚠️ 它**没有产出猪** ⇒ 归属为空 ⇒ FIFO 领用它时收入记「未归属」。
	CarryoverSourceOpening = "opening"
	// CarryoverSourceExternal **外采**(2026-11-03 owner):额外采购某个部位的货。
	//
	// ⚠️ 它同样**没有产出猪** ⇒ 归属为空 ⇒ FIFO 领用它时收入记「未归属」
	//   (与期初同一处理,见 pig_fifo.go 的 fifoRecordShares)。
	//
	// ⚠️ FIFO 消费顺序:外采的行**先于**盘点结转的行被领用
	//   ⇒ 否则同一天的两行(外采 + 盘点结转)按 id 排序 ⇒ 谁先被领是随机的
	//   ⇒ 外采的收入可能摊到猪身上(那会让单猪毛利失真)。
	CarryoverSourceExternal = "external"
)

// StockCarryOverID **按日结转记录**的确定性代理主键。
//
// ⇒ 同一 (门店, 产生日, SKU) 永远是同一行 ⇒
//   - 重复提交同一天的盘点 = 覆盖同一行(ID 相同 ⇒ upsert);
//   - 同一天同 SKU 的两行会**撞成一行** ⇒ 所以同一 (归属日, SKU) 只允许一行
//     (校验见 registerCarryoverRecords)。
//
// ⚠️ 2026-11-02 owner:旧玩法下线 ⇒ 原本「带批次标记的结转记录」用
// (门店, 产生日, 批次标记) 参与哈希的那套 ID(带批次标记的那套)已删除。
func StockCarryOverID(branchID string, origin datatypes.Date, cubeProductID string) string {
	return surrogateID("SCO", branchID, dateKey(origin), cubeProductID)
}

// StockBulkManualCarryoverID **大宗手工兜底产出**那一行结转记录的确定性代理主键。
//
// ⚠️ 为什么单独一套 ID:手工产出**不属于**"某天的盘点结转" ⇒ 不能用
// (门店, 产生日, SKU) 参与哈希 —— 否则它会跟当天同 SKU 的散货结转行**撞成一行**
// ⇒ 一行被覆盖 ⇒ 冷库里的货凭空少一截(或多一截)。
//
// ⇒ 把手工产出的**单据 ID** 掺进哈希 ⇒ 同一张单永远同一行,不同单各占一行。
func StockBulkManualCarryoverID(branchID string, productionID string) string {
	return surrogateID("SBM", branchID, productionID)
}

// carryoverPigAllocItem 结转记录归属快照里的一项(一头猪的份额)。
type carryoverPigAllocItem struct {
	PigID string `json:"pig_id"`
	QtyKg string `json:"qty_kg"`
}

// registerCarryoverRecords 盘点时登记**按日结转记录**(归属日 + SKU + 重量 + 猪归属)。
//
// # 门店口径(2026-10-24,旧的分批搬台部分已于 2026-11-02 下线)
//
// 「盘点时将剩余的鲜肉商品按 sku 打包过秤然后登记」
//
// 「每批有自己的产生日,这个产生日就是盘点归属日」
//
// ⇒ **每一行**盘点明细都落一行结转记录 ⇒ 它是 **FIFO 队列**的一员:
// 毛利按 FIFO 回归到猪时按产生日先后摊(见 service/pig_fifo.go)。
//
// # 幂等与"重提交"的清理
//
//   - ID 是确定性的 ⇒ 同一天重复提交同一份盘点 = **覆盖**同一批行;
//   - 同一天重提交时**这次没出现的行要清掉**(同一 SKU 下、且还在冷库的)
//     ⇒ 否则 FIFO 队列里留着上一版的行 ⇒ 同一批货被摊两次。
func (s *Service) registerCarryoverRecords(ctx context.Context, branchID string, origin time.Time, cuts []model.CutInput) error {
	originDate := datatypesDate(origin.In(s.bizTZOrUTC()))
	now := s.now()

	// ⚠️ 2026-11-02 owner:旧玩法下线 ⇒ **拆批(同 SKU 分多行)在前端一起下线**。
	//
	//   但**老 APK** 仍可能拆着提交 ⇒ 这里**按 SKU 合并求和**,不报错、也不静默覆盖:
	//   - 静默覆盖(ID 按 SKU 定)⇒ "位置表 20kg / 结转记录 5kg" ⇒ FIFO 归摊用错重量;
	//   - 报错 ⇒ 门店当天**盘不了**(老 APK 没有新口径可用)。
	//   ⇒ 求和 = 与位置表(counterActualKg)的口径一致 ⇒ 数据自洽。
	type mergedCut struct {
		cutType model.CutType
		sku     string
		kg      decimal.Decimal
	}
	merged := make([]mergedCut, 0, len(cuts))
	idxOf := map[string]int{}
	for _, c := range cuts {
		sku := strings.TrimSpace(c.CubeProductID)
		if at, ok := idxOf[sku]; ok {
			merged[at].kg = merged[at].kg.Add(c.ActualRemainKg)
			continue
		}
		idxOf[sku] = len(merged)
		merged = append(merged, mergedCut{cutType: c.CutType, sku: sku, kg: c.ActualRemainKg})
	}

	// 本次提交涉及的行(用于最后清掉"这次没出现的旧行")。
	submittedIDs := make([]string, 0, len(cuts))
	submittedSKUs := map[string]bool{}

	// ---- 当天**外采**的剩余量(按货号)----
	//
	// ⚠️ 2026-11-03 owner:盘点盘的是**猪肉台**上的货,
	//   其中可能就有**外采**没卖完的那部分(外采录完直接上架、卖不完)。
	//
	// ⇒ 外采自己有一行结转记录(source='external')⇒ 这一部分**归它**
	// ⇒ 这里从盘点这一行里**扣掉** ⇒ 否则同一批货在 FIFO 队列里出现两次
	//   ⇒ FIFO 会把同一笔收入摊两遍(猪的毛利被摊空)。
	extRemaining := map[string]decimal.Decimal{}
	{
		type extRow struct {
			CubeProductID string
			Remaining     decimal.Decimal
		}
		var extRows []extRow
		if err := s.db.WithContext(ctx).
			Table("carryover_records").
			Select("cube_product_id AS cube_product_id, (qty_kg - consumed_kg) AS remaining").
			Where("branch_id = ? AND origin_date = ? AND source = ?", branchID, originDate, CarryoverSourceExternal).
			Scan(&extRows).Error; err != nil {
			return fmt.Errorf("query carryover_records(external remaining): %w", err)
		}
		for _, r := range extRows {
			if r.Remaining.IsPositive() {
				extRemaining[r.CubeProductID] = extRemaining[r.CubeProductID].Add(r.Remaining)
			}
		}
	}

	// 成本快照(按**部位**):入冷库那一刻的移动加权单价 —— 这就是"记录分摊成本"。
	//
	// ⚠️ **一次性**算齐(不能逐部位调):兜底路径会算一整天的成本
	//   ⇒ 逐个部位调 ⇒ 一次盘点几十倍开销。
	//
	// ⚠️ 2026-10-27:成本链改部位级 ⇒ 成本单价按**部位**取。
	cutSet := map[model.CutType]bool{}
	for _, c := range cuts {
		if c.CutType != "" {
			cutSet[c.CutType] = true
		}
	}
	cutList := make([]model.CutType, 0, len(cutSet))
	for cut := range cutSet {
		cutList = append(cutList, cut)
	}
	unitCosts := s.bulkUnitCostMap(ctx, branchID, origin, cutList)

	for _, mc := range merged {
		alloc, err := s.carryoverPigAlloc(ctx, branchID, mc.cutType, mc.sku, origin)
		if err != nil {
			return err
		}
		unitCost := unitCosts[mc.cutType]
		// ⚠️ 扣掉当天外采的那部分(它归外采自己的结转记录)⇒ 不重复入队。
		kg := mc.kg
		if ext := extRemaining[mc.sku]; ext.IsPositive() {
			kg = kg.Sub(ext)
			if kg.IsNegative() {
				kg = decimal.Zero
			}
		}
		// ---- 一行结转记录(归属日 + SKU + 重量 + 归属快照)----
		rec := model.CarryoverRecord{
			ID:            StockCarryOverID(branchID, originDate, mc.sku),
			BranchID:      branchID,
			CubeProductID: mc.sku,
			CutType:       mc.cutType,
			QtyKg:         kg,
			OriginDate:    originDate,
			Location:      string(model.StockLocationColdStore),
			Status:        CarryoverStatusActive,
			Source:        CarryoverSourceStocktake,
			PigAlloc:      alloc,
			UnitCostYuan:  unitCost,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.upsertCarryoverRecord(ctx, nil, rec); err != nil {
			return err
		}
		submittedIDs = append(submittedIDs, rec.ID)
		submittedSKUs[mc.sku] = true
	}

	// 同一天重提交 ⇒ 把"这次没出现"的旧行清掉(**只限本次涉及的 SKU**,
	// 且只清 active 的 —— released 的行是历史"搬上台流水",保留)。
	return s.dropStaleCarryoverRecords(ctx, branchID, originDate, submittedSKUs, submittedIDs)
}

// upsertCarryoverRecord 写一行结转记录(确定性 ID ⇒ 幂等覆盖)。
//
// ⚠️ **不覆盖 consumed_kg**:已卖掉的重量不会因为重提交盘点而"复活"
// (否则同一批货会被 FIFO 再摊一次)。
//
// ⚠️ tx 可空:传 nil 时用 s.db(非事务场景);
//
//	改期初那种"多张表一起动"的事务场景要传 tx ⇒ 期初/位置表/队列要么全成要么全滚。
func (s *Service) upsertCarryoverRecord(ctx context.Context, tx *gorm.DB, rec model.CarryoverRecord) error {
	if tx == nil {
		tx = s.db
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"cube_product_id", "cut_type", "qty_kg", "location", "status", "source",
			"pig_alloc", "unit_cost_yuan", "updated_at",
		}),
	}).Create(&rec).Error; err != nil {
		return fmt.Errorf("upsert carryover_record %s: %w", rec.ID, err)
	}
	return nil
}

// dropStaleCarryoverRecords 清掉"同一天重提交时这次没出现"的旧行。
//
// ⚠️ 作用域严格限定:**本次提交涉及的 SKU** + **active**
//   - **source='stocktake'**(盘点来的)。
//   - 别的 SKU 的行不动(盘点可能是部分盘点 is_complete=false);
//   - released 的行不动 —— 那是历史"搬上台流水"(旧的整批搬台时代写的);
//   - **大宗手工兜底**(source='bulk_manual')的行不动 ——
//     那不是"这次盘点没出现的行",而是**另一笔单据**记的冷库货
//     (删了就等于把还在冷库里的货抹掉)。
func (s *Service) dropStaleCarryoverRecords(ctx context.Context, branchID string, originDate datatypes.Date, submittedSKUs map[string]bool, submittedIDs []string) error {
	if len(submittedSKUs) == 0 || len(submittedIDs) == 0 {
		return nil
	}
	skus := make([]string, 0, len(submittedSKUs))
	for sku := range submittedSKUs {
		skus = append(skus, sku)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND origin_date = ? AND cube_product_id IN ? AND status = ? AND source = ?",
			branchID, originDate, skus, CarryoverStatusActive, CarryoverSourceStocktake).
		Where("id NOT IN ?", submittedIDs).
		Delete(&model.CarryoverRecord{})
	if res.Error != nil {
		return fmt.Errorf("delete stale carryover_records: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		s.pubLogger.Info("registerCarryoverRecords: 清掉同一天重提交时消失的结转行",
			"branch_id", branchID, "origin_date", dateKey(originDate), "rows", res.RowsAffected)
	}
	return nil
}

// carryoverPigAlloc 这批货的**归属快照**:当日该 SKU 由哪几头猪产出、各多少 kg。
//
// ⚠️ 为什么在盘点那一刻就把归属**钉死**:后来猪被改名/归档/补录都不该影响
// 这批货的毛利归属 ⇒ 存快照,而不是实时去查。
//
// ⚠️ 没补录分割(pig_cuts 为空)时 ⇒ 归属为空 ⇒ 这批货的毛利回归不到具体猪
// (退化成"按当日各猪毛重比例"的兜底,见 gross_margin 侧)。
func (s *Service) carryoverPigAlloc(ctx context.Context, branchID string, cutType model.CutType, cubeProductID string, day time.Time) (datatypes.JSON, error) {
	startOfDay, endOfDay := s.businessDayBounds(day)
	type row struct {
		PigID         string
		CubeProductID string
		WeightKg      decimal.Decimal
	}
	var rows []row
	err := s.db.WithContext(ctx).
		Table("pig_cuts AS pc").
		Select("pc.pig_id AS pig_id, pc.cube_product_id AS cube_product_id, pc.weight_kg AS weight_kg").
		Joins("JOIN whole_pigs AS p ON p.id = pc.pig_id").
		Where("pc.branch_id = ? AND p.arrived_at >= ? AND p.arrived_at < ? AND pc.cut_type = ?",
			branchID, startOfDay, endOfDay, cutType).
		Where("pc.cube_product_id = ? OR pc.cube_product_id = ''", cubeProductID).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query pig_cuts(carryover alloc): %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	byPig := map[string]decimal.Decimal{}
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, ok := byPig[r.PigID]; !ok {
			order = append(order, r.PigID)
		}
		byPig[r.PigID] = byPig[r.PigID].Add(r.WeightKg)
	}
	out := make([]carryoverPigAllocItem, 0, len(order))
	for _, pigID := range order {
		out = append(out, carryoverPigAllocItem{PigID: pigID, QtyKg: byPig[pigID].StringFixed(3)})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal carryover pig_alloc: %w", err)
	}
	return datatypes.JSON(raw), nil
}

// ListCarryoverRecords 列某店的**按日结转记录**(可按位置/产生日过滤)。
//
// ⚠️ 2026-11-02 owner:旧玩法下线 ⇒ 不再有"按批次标记过滤"这个维度。
// (库里 label 列的历史值只作留档,不再作为筛选口径。)
//
// ⚠️ `originDate` 过滤是给「按产生日核对结转台账」用的。
// (毛利 FIFO 归摊、大宗 FIFO 领用都按产生日排队。)
func (s *Service) ListCarryoverRecords(ctx context.Context, branchID, location, originDate string) ([]model.CarryoverRecord, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if loc := strings.TrimSpace(location); loc != "" {
		q = q.Where("location = ?", loc)
	}
	if d := strings.TrimSpace(originDate); d != "" {
		day, err := time.ParseInLocation("2006-01-02", d, s.bizTZOrUTC())
		if err != nil {
			return nil, fmt.Errorf("%w: origin_date 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		start, end := s.businessDayBounds(day)
		q = q.Where("origin_date >= ? AND origin_date < ?", start, end)
	}
	var rows []model.CarryoverRecord
	if err := q.Order("origin_date ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query carryover_records: %w", err)
	}
	return rows, nil
}

// ⚠️ 2026-11-02 owner:随「批次标记玩法」一起下线的是**入库量兜底**
// (applyProducedFallback / carryoverOriginKgBySku / movedToCounterKgBySku)。
//
// # 为什么它跟着下线
//
// 那条兜底是"门店没补录分割时,用日终盘点重量倒推当天分割了多少":
//
//	当日分割量 = 销售重量 + 日终盘点重量 − 搬上台重量
//
// 而它的两个输入**都来自"批次标记玩法"这套机制**:
//   - 「日终盘点重量」只认**带批次标记的结转记录**(label <> '');
//   - 「搬上台重量」只认**按批次整批搬**时写下的 moved_at。
//
// ⇒ 旧玩法下线 ⇒ 两个输入同时消失 ⇒ 这条兜底**恒不生效**
// (生产实测:除契约测试门店外,没有任何一批带标记 ⇒ 从未生效过)。
//
// ⇒ 把它**改成"算全部结转行"**看着像修 bug,实际是**启用一条从未跑过的路径**
//   ⇒ 会让"门店没补录分割"那几天的成本口径突变 ⇒ 不能这么干。
//
// ⇒ 所以按 owner 的口径**整条删掉**:它不是"规则",是"批次标记玩法"的附属品。
// (FIFO 归摊用的是**全部**结转行,不受影响 —— 见 service/pig_fifo.go。)
