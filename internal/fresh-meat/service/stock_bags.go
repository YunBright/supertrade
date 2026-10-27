package service

// internal/fresh-meat/service/stock_bags.go —— **袋子 / 标签号**。
//
// # 门店口径(2026-10-24)
//
// 「盘点时将剩余的鲜肉商品按 sku 打包过秤然后登记,每个袋子夹上标签,
// 方便次日搬上猪肉台继续售卖和毛利回归。」
//
// ⇒ 一次盘点 = 若干**袋**(每袋:SKU + 过秤重量 + 一个**标签号**)。
//
// # 袋子解决的三件事
//
// ① **整袋搬上台**:门店按袋搬,不是按 kg 搬 ⇒ 搬完冷库和猪肉台都对得上。
// ② **搬上台留下流水**:位置表只记余额、不留痕 ⇒ "当日搬上台了多少"查不到
//    ⇒ 而每日毛利的"分摊重量"需要它(搬上台的是**老货**,不能再摊一次猪成本)。
// ③ **每袋有归属**:这袋货是当天哪几头猪的产出 ⇒ 卖掉时毛利能**回归**
//    到产出它的那头猪(而不是今天碰巧在场的猪)。
//
// # 标签号可复用
//
// 袋子搬上猪肉台后标签号**释放**(status=released)⇒ 同一个标签号下次贴在别的
// 袋子上 ⇒ 唯一性只约束**在用中**的袋子(部分唯一索引,migrate.go)。

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

// 袋子的状态。
const (
	// BagStatusActive 在用(标签号被占用)。
	BagStatusActive = "active"
	// BagStatusReleased 标签号已释放(袋子已搬上猪肉台,标签可以再贴别处)。
	BagStatusReleased = "released"
)

// 结转记录(袋子)的**来源**(2026-10-25)。
//
// ⚠️ 为什么袋子要记来源:「同一天重提交盘点 ⇒ 清掉这次没出现的旧行」这条清理
// 只能清**盘点来的**行 ⇒ 手工补录的冷库货必须活着(货还在冷库里)。
const (
	// BagSourceStocktake 日终盘点登记(主路径)。
	BagSourceStocktake = "stocktake"
	// BagSourceBulkManual 大宗手工兜底产出(盘点没录该部位时)。
	BagSourceBulkManual = "bulk_manual"
	// BagSourceOpening **期初结转**(上线前冷库里就有的货)。
	//
	// ⚠️ 它**没有产出猪** ⇒ 归属为空 ⇒ FIFO 领用它时收入记「未归属」。
	BagSourceOpening = "opening"
)

// StockBagID 袋子的**确定性**代理主键(带标签号的行)。
//
// ⇒ 同一 (门店, 产生日, 标签号) 永远是同一行 ⇒ 重复提交同一天的盘点 = 覆盖,
// 不会"多出一袋"(多一袋 ⇒ 冷库余额虚高)。
func StockBagID(branchID string, origin datatypes.Date, label string) string {
	return surrogateID("SBG", branchID, dateKey(origin), label)
}

// StockCarryOverID **不装袋的散货结转记录**的确定性代理主键。
//
// ⚠️ 为什么不能沿用 StockBagID:无 label 的行没有标签号可参与主键
// ⇒ 同一天同 SKU 的两行会**撞成一行** ⇒ 一行被悄悄覆盖。
//
// ⇒ 改用 (门店, 产生日, SKU) 参与哈希 ⇒
//   - 同一 (归属日, SKU) 只有**一行**无 label;
//   - 重复提交同一天的盘点 = 覆盖同一行(ID 相同 ⇒ upsert)。
//
// (同一天同 SKU 想多袋 ⇒ 前端**分行**并各自带 label ⇒ 各有各的标签号 ⇒ 各有各的行。)
func StockCarryOverID(branchID string, origin datatypes.Date, cubeProductID string) string {
	return surrogateID("SCO", branchID, dateKey(origin), cubeProductID)
}

// StockBulkManualBagID **大宗手工兜底产出**那一行结转记录的确定性代理主键。
//
// ⚠️ 为什么单独一套 ID:手工产出**不属于**"某天的盘点结转" ⇒ 不能用
// (门店, 产生日, SKU) 参与哈希 —— 否则它会跟当天同 SKU 的散货结转行**撞成一行**
// ⇒ 一行被覆盖 ⇒ 冷库里的货凭空少一截(或多一截)。
//
// ⇒ 把手工产出的**单据 ID** 掺进哈希 ⇒ 同一张单永远同一行,不同单各占一行。
func StockBulkManualBagID(branchID string, productionID string) string {
	return surrogateID("SBM", branchID, productionID)
}

// bagPigAllocItem 袋子归属快照里的一项(一头猪的份额)。
type bagPigAllocItem struct {
	PigID string `json:"pig_id"`
	QtyKg string `json:"qty_kg"`
}

// registerStockBags 盘点时登记**按日结转记录**(袋子 + 不装袋的散货)。
//
// # 2026-10-24 的门店口径
//
// 「盘点时将剩余的鲜肉商品按 sku 打包过秤然后登记,每个袋子夹上标签,
//
//	方便次日搬上猪肉台继续售卖和毛利回归。」
//
// 「每袋有自己的产生日,这个产生日就是盘点归属日;
//
//	不装袋的 sku+kg 也根据盘点归属日做记录。」
//
// ⇒ **每一行**盘点明细都要落一行:
//   - 带 label ⇒ **一袋**(可整袋搬上台、标签号搬完释放可复用);
//   - 无 label ⇒ **不装袋的散货结转记录**(只有归属日 + SKU + 重量)。
//
// ⇒ 两者都是 **FIFO 队列**的一员:毛利按 FIFO 回归到猪时按产生日先后摊
// (见 service/pig_fifo.go)。
//
// # 幂等与"重提交"的清理
//
//   - ID 是确定性的 ⇒ 同一天重复提交同一份盘点 = **覆盖**同一批行;
//   - 同一天重提交时**这次没出现的行要清掉**(同一 SKU 下、且还没搬上台的)
//     ⇒ 否则 FIFO 队列里留着上一版的行 ⇒ 同一批货被摊两次。
func (s *Service) registerStockBags(ctx context.Context, branchID string, origin time.Time, cuts []model.CutInput) error {
	originDate := datatypesDate(origin.In(s.bizTZOrUTC()))
	now := s.now()

	// 同一 (归属日, SKU) 只允许**一行**无 label(前端多袋请分行并各自带标签号)。
	noLabelSeen := map[string]bool{}
	for _, c := range cuts {
		if strings.TrimSpace(c.Label) != "" {
			continue
		}
		sku := strings.TrimSpace(c.CubeProductID)
		if noLabelSeen[sku] {
			return fmt.Errorf("%w: 归属日 %s 的 SKU %s 只能有一行不装袋的记录(多袋请分行并各自带标签号)",
				ErrInvalidInput, dateKey(originDate), sku)
		}
		noLabelSeen[sku] = true
	}

	// 本次提交涉及的行(用于最后清掉"这次没出现的旧行")。
	submittedIDs := make([]string, 0, len(cuts))
	submittedSKUs := map[string]bool{}

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

	for _, c := range cuts {
		label := strings.TrimSpace(c.Label)
		alloc, err := s.bagPigAlloc(ctx, branchID, c.CutType, c.CubeProductID, origin)
		if err != nil {
			return err
		}
		unitCost := unitCosts[c.CutType]
		if label == "" {
			// ---- 不装袋的散货结转记录 ----
			bag := model.StockBag{
				ID:            StockCarryOverID(branchID, originDate, c.CubeProductID),
				BranchID:      branchID,
				Label:         "",
				CubeProductID: c.CubeProductID,
				CutType:       c.CutType,
				QtyKg:         c.ActualRemainKg,
				OriginDate:    originDate,
				Location:      string(model.StockLocationColdStore),
				Status:        BagStatusActive,
				Source:        BagSourceStocktake,
				PigAlloc:      alloc,
				UnitCostYuan:  unitCost,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := s.upsertStockBag(ctx, bag); err != nil {
				return err
			}
			submittedIDs = append(submittedIDs, bag.ID)
			submittedSKUs[c.CubeProductID] = true
			continue
		}

		// ---- 一袋(带标签号)----
		bagID := StockBagID(branchID, originDate, label)
		if err := s.assertLabelUsable(ctx, branchID, label, bagID, c.CubeProductID, originDate); err != nil {
			return err
		}
		bag := model.StockBag{
			ID:            bagID,
			BranchID:      branchID,
			Label:         label,
			CubeProductID: c.CubeProductID,
			CutType:       c.CutType,
			QtyKg:         c.ActualRemainKg,
			OriginDate:    originDate,
			Location:      string(model.StockLocationColdStore),
			Status:        BagStatusActive,
			Source:        BagSourceStocktake,
			PigAlloc:      alloc,
			UnitCostYuan:  unitCost,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.upsertStockBag(ctx, bag); err != nil {
			return err
		}
		submittedIDs = append(submittedIDs, bag.ID)
		submittedSKUs[c.CubeProductID] = true
	}

	// 同一天重提交 ⇒ 把"这次没出现"的旧行清掉(**只限本次涉及的 SKU**,
	// 且只清 active 的 —— 已搬上台(released)的行保留:那是"搬上台流水")。
	return s.dropStaleStockBags(ctx, branchID, originDate, submittedSKUs, submittedIDs)
}

// upsertStockBag 写一行结转记录(确定性 ID ⇒ 幂等覆盖)。
//
// ⚠️ **不覆盖 consumed_kg**:已卖掉的重量不会因为重提交盘点而"复活"
// (否则同一批货会被 FIFO 再摊一次)。
func (s *Service) upsertStockBag(ctx context.Context, bag model.StockBag) error {
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"cube_product_id", "cut_type", "qty_kg", "location", "status", "source",
			"pig_alloc", "unit_cost_yuan", "updated_at",
		}),
	}).Create(&bag).Error; err != nil {
		return fmt.Errorf("upsert stock_bag %s: %w", bag.ID, err)
	}
	return nil
}

// assertLabelUsable 校验标签号可用(没被别的在用袋子占用)。
//
// ⚠️ 为什么在 service 层查:DB 侧那是**部分唯一索引**(`WHERE status='active'`),
// 是手写 DDL 建的 ⇒ **只在 Postgres 上存在** ⇒ SQLite 单测库里没有
// ⇒ 行为会不一致 ⇒ service 层查一遍,两个库行为一样。
func (s *Service) assertLabelUsable(ctx context.Context, branchID, label, bagID, cubeProductID string, originDate datatypes.Date) error {
	var clashes []model.StockBag
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND label = ? AND status = ?", branchID, label, BagStatusActive).
		Find(&clashes).Error
	if err != nil {
		return fmt.Errorf("query stock_bags(label clash): %w", err)
	}
	for _, cl := range clashes {
		if cl.ID == bagID {
			// 同一行(同一天同一标签号)⇒ 是覆盖,不是冲突;
			// 但**不能挂到别的 SKU 上**(主键是 (门店, 产生日, 标签号) ⇒ 会被覆盖)。
			if cl.CubeProductID != cubeProductID {
				return fmt.Errorf("%w: 归属日 %s 的标签号 %s 已经挂在 SKU %s 上,不能再挂 %s",
					ErrInvalidInput, dateKey(originDate), label, cl.CubeProductID, cubeProductID)
			}
			continue
		}
		return fmt.Errorf("%w: 标签号 %s 已经用在 %s 的袋子上了(搬上台释放后才能复用)",
			ErrInvalidInput, label, dateKey(cl.OriginDate))
	}
	return nil
}

// dropStaleStockBags 清掉"同一天重提交时这次没出现"的旧行。
//
// ⚠️ 作用域严格限定:**本次提交涉及的 SKU** + **active**(没搬上台的)
//   - **source='stocktake'**(盘点来的)。
//   - 别的 SKU 的行不动(盘点可能是部分盘点 is_complete=false);
//   - released(已搬上台)的行不动 —— 那是"搬上台流水",毛利要靠它;
//   - **大宗手工兜底**(source='bulk_manual')的行不动 ——
//     那不是"这次盘点没出现的行",而是**另一笔单据**记的冷库货
//     (删了就等于把还在冷库里的货抹掉)。
func (s *Service) dropStaleStockBags(ctx context.Context, branchID string, originDate datatypes.Date, submittedSKUs map[string]bool, submittedIDs []string) error {
	if len(submittedSKUs) == 0 || len(submittedIDs) == 0 {
		return nil
	}
	skus := make([]string, 0, len(submittedSKUs))
	for sku := range submittedSKUs {
		skus = append(skus, sku)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND origin_date = ? AND cube_product_id IN ? AND status = ? AND source = ?",
			branchID, originDate, skus, BagStatusActive, BagSourceStocktake).
		Where("id NOT IN ?", submittedIDs).
		Delete(&model.StockBag{})
	if res.Error != nil {
		return fmt.Errorf("delete stale stock_bags: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		s.pubLogger.Info("registerStockBags: 清掉同一天重提交时消失的结转行",
			"branch_id", branchID, "origin_date", dateKey(originDate), "rows", res.RowsAffected)
	}
	return nil
}

// bagPigAlloc 这袋货的**归属快照**:当日该 SKU 由哪几头猪产出、各多少 kg。
//
// ⚠️ 为什么在盘点那一刻就把归属**钉死**:后来猪被改名/归档/补录都不该影响
// 这袋货的毛利归属 ⇒ 存快照,而不是实时去查。
//
// ⚠️ 没补录分割(pig_cuts 为空)时 ⇒ 归属为空 ⇒ 这袋货的毛利回归不到具体猪
// (退化成"按当日各猪毛重比例"的兜底,见 gross_margin 侧)。
func (s *Service) bagPigAlloc(ctx context.Context, branchID string, cutType model.CutType, cubeProductID string, day time.Time) (datatypes.JSON, error) {
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
		return nil, fmt.Errorf("query pig_cuts(bag alloc): %w", err)
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
	out := make([]bagPigAllocItem, 0, len(order))
	for _, pigID := range order {
		out = append(out, bagPigAllocItem{PigID: pigID, QtyKg: byPig[pigID].StringFixed(3)})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal bag pig_alloc: %w", err)
	}
	return datatypes.JSON(raw), nil
}

// ListStockBags 列某店的袋子(可按位置/标签过滤)。
//
// ⚠️ 2026-10-24:这张表现在也装**不装袋的散货结转记录**(label 为空)
// ⇒ 列表里会出现 label="" 的行:它们**不是袋子**,不能按标签搬上台,
//
//	只是"这一天的结转 + 猪归属"的台账行(毛利 FIFO 回归靠它)。
//
// ListStockBags 列某店的袋子(可按位置/标签/产生日过滤)。
//
// ⚠️ `originDate` 过滤是给前端「载入并编辑」用的:
// 盘点单的快照是**按 SKU 聚合**的 ⇒ 一个 SKU 拆成多袋时,快照里只有一个 label
// ⇒ **还原不了多袋结构** ⇒ 前端要按 `origin_date` 把当天的袋子全查出来才能还原。
func (s *Service) ListStockBags(ctx context.Context, branchID, location, label, originDate string) ([]model.StockBag, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if loc := strings.TrimSpace(location); loc != "" {
		q = q.Where("location = ?", loc)
	}
	if l := strings.TrimSpace(label); l != "" {
		q = q.Where("label = ?", l)
	}
	if d := strings.TrimSpace(originDate); d != "" {
		day, err := time.ParseInLocation("2006-01-02", d, s.bizTZOrUTC())
		if err != nil {
			return nil, fmt.Errorf("%w: origin_date 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		start, end := s.businessDayBounds(day)
		q = q.Where("origin_date >= ? AND origin_date < ?", start, end)
	}
	var rows []model.StockBag
	if err := q.Order("origin_date ASC, label ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query stock_bags: %w", err)
	}
	return rows, nil
}

// moveBagToCounter 按**标签号**整袋搬上猪肉台。
//
// ⇒ 袋子的位置改成猪肉台、**标签号释放**(可复用)、moved_at = 归属日。
//
// # ⚠️ 这里**不动位置表**
//
// 位置差额由 CounterLoad 按「当天搬台集合的新旧差额」统一调整
// ⇒ 这样"同一归属日重复提交同一份"才是幂等的(余额不会翻倍)。
//
// # movedAt = **归属日**(2026-10-24)
//
// 搬台有归属日 ⇒ 补录往期的搬台,moved_at 记在**那一天**
// ⇒ 「当日搬上台了多少」(每日毛利的分摊口径)按归属日归集,不是按提交那天。
//
// # 幂等
//
// 同一归属日重复提交**同一袋** ⇒ 第二次直接返回(不重复搬)。
// (换一天再搬同一袋 ⇒ 400:这袋已经在别的日子搬走了。)
func (s *Service) moveBagToCounter(ctx context.Context, tx *gorm.DB, branchID, label string, movedAt time.Time) (model.StockBag, error) {
	var bag model.StockBag
	// ⚠️ 空标签号 = **不装袋的散货结转记录** ⇒ 它不是袋子,搬不了
	// (否则会把"今天新结转的散货"当成"昨天的一袋"搬上台)。
	if strings.TrimSpace(label) == "" {
		return bag, fmt.Errorf("%w: 标签号不能为空(没有标签的结转记录不能整袋搬)", ErrInvalidInput)
	}
	err := tx.WithContext(ctx).
		Where("branch_id = ? AND label = ? AND status = ?", branchID, label, BagStatusActive).
		First(&bag).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return bag, fmt.Errorf("query stock_bag %s: %w", label, err)
		}
		// 没有在用的袋子 ⇒ 可能是"同一归属日已经搬过"(幂等)⇒ 找出来核对。
		var done model.StockBag
		qErr := tx.WithContext(ctx).
			Where("branch_id = ? AND label = ? AND status = ?", branchID, label, BagStatusReleased).
			Order("origin_date DESC").
			First(&done).Error
		if qErr != nil {
			if errors.Is(qErr, gorm.ErrRecordNotFound) {
				return bag, fmt.Errorf("%w: 标签号 %s 没有在用的袋子", ErrInvalidInput, label)
			}
			return bag, fmt.Errorf("query stock_bag %s: %w", label, qErr)
		}
		if done.MovedAt != nil && s.sameBizDay(*done.MovedAt, movedAt) {
			return done, nil // 同一归属日已搬过 ⇒ 幂等返回
		}
		movedKey := ""
		if done.MovedAt != nil {
			movedKey = done.MovedAt.In(s.bizTZOrUTC()).Format("2006-01-02")
		}
		return bag, fmt.Errorf("%w: 标签号 %s 已经于 %s 搬上猪肉台", ErrInvalidInput, label, movedKey)
	}
	now := s.now()
	if err := tx.WithContext(ctx).Model(&bag).Updates(map[string]any{
		"location":   string(model.StockLocationCounter),
		"status":     BagStatusReleased,
		"moved_at":   movedAt,
		"updated_at": now,
	}).Error; err != nil {
		return bag, fmt.Errorf("update stock_bag %s: %w", label, err)
	}
	return bag, nil
}

// sameBizDay 两个时间点是不是同一个**营业日**(营业日界时区)。
func (s *Service) sameBizDay(a, b time.Time) bool {
	loc := s.bizTZOrUTC()
	la, lb := a.In(loc), b.In(loc)
	return la.Year() == lb.Year() && la.YearDay() == lb.YearDay()
}

// bagOriginKgBySku 某天**结转**的重量(按 SKU):当天产生的袋子的重量之和。
//
// ⚠️ 这就是门店口径里的"**日终盘点重量**":盘点盘出来的、留在台面/进冷库的那部分。
//
// ⚠️ 2026-10-24:**只算带标签号的袋子**(label <> ”)。
//
// ⇒ 为什么不装袋的散货结转行不算:这个口径是**移动加权成本引擎**的"入库量兜底"
// (applyProducedFallback)的输入,而那条兜底路径的**前提就是"门店贴了标签"**
// (没贴标签就没有袋子 ⇒ 不兜底,避免凭空造入库量)。
// ⇒ 本轮需求要求"不影响现有的移动加权成本引擎" ⇒ 这里保持原口径不变。
// (FIFO 归摊用的是**全部**结转行,见 service/pig_fifo.go。)
func (s *Service) bagOriginKgBySku(ctx context.Context, branchID string, day time.Time) (map[string]decimal.Decimal, error) {
	startOfDay, endOfDay := s.businessDayBounds(day)
	type row struct {
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var rows []row
	err := s.db.WithContext(ctx).
		Model(&model.StockBag{}).
		Select("cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND origin_date >= ? AND origin_date < ? AND label <> ''",
			branchID, startOfDay, endOfDay).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query bag origin: %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		out[r.CubeProductID] = out[r.CubeProductID].Add(r.QtyKg)
	}
	return out, nil
}

// applyProducedFallback **入库量兜底**:某个 SKU 当天**没有分割补录**时,
// 用门店口径倒推它当天分割出了多少:
//
//	当日分割量 = 销售重量 + 日终盘点重量 − 搬上台重量
//
// # 为什么是这个式子
//
//   - 「销售 + 日终盘点」= 当日**台面**的总流量(卖掉的 + 剩着的)
//   - 减掉「搬上台」:搬上台的是**前几天就分割出来、成本早就摊过**的老货
//     ⇒ 不减 ⇒ 同一批成本会被摊两次。
//
// ⚠️ 「日终盘点重量」与「搬上台重量」都来自**袋子表**(标签号):
// 结转 = 今天产生的袋子的重量;搬上台 = 今天被搬上台的袋子的重量。
//
// # 为什么只在"没有补录"时兜底
//
// 有分割补录(pig_cuts)时,**按补录的 kg 逐猪摊**更精确(能落到具体猪)
// ⇒ 不用兜底。兜底只在"门店没补录"时救场(否则那天成本就摊不出去)。
func (s *Service) applyProducedFallback(
	ctx context.Context, branchID string, bizDate time.Time,
	producedKg map[string]decimal.Decimal,
	producedByPigKg map[string]map[string]decimal.Decimal,
	pigs []model.WholePig,
	soldKg map[string]decimal.Decimal,
) error {
	stocktakeKg, err := s.bagOriginKgBySku(ctx, branchID, bizDate)
	if err != nil {
		return err
	}
	if len(stocktakeKg) == 0 {
		return nil // 门店没贴标签 ⇒ 没有袋子 ⇒ 不兜底(避免凭空造入库量)
	}
	movedKg, err := s.movedToCounterKgBySku(ctx, branchID, bizDate)
	if err != nil {
		return err
	}
	totalW := decimal.Zero
	for _, p := range pigs {
		totalW = totalW.Add(p.GrossWeightKg)
	}
	if !totalW.IsPositive() {
		return nil // 当日没猪 ⇒ 没有成本可摊
	}
	for sku, kg := range stocktakeKg {
		if producedKg[sku].IsPositive() {
			continue // 有分割补录 ⇒ 用补录的(更精确)
		}
		qty := soldKg[sku].Add(kg).Sub(movedKg[sku])
		if !qty.IsPositive() {
			continue
		}
		producedKg[sku] = qty
		// 按当日各猪的**毛重比例**分摊(没有分割记录 ⇒ 只能到猪这一层)
		for _, p := range pigs {
			if producedByPigKg[p.ID] == nil {
				producedByPigKg[p.ID] = map[string]decimal.Decimal{}
			}
			share := p.GrossWeightKg.Div(totalW)
			producedByPigKg[p.ID][sku] = producedByPigKg[p.ID][sku].Add(qty.Mul(share))
		}
	}
	return nil
}

// movedToCounterKgBySku 当日**搬上台**的重量(按 SKU),用于每日毛利的分摊口径。
//
// ⚠️ 为什么需要它:搬上台的是**老货**(前几天分割的、成本早就摊过)
// ⇒ 当日分摊猪成本时必须**减掉**它,否则同一批成本会被摊两次。
//
// ⇒ 数据来源 = 袋子表(标签号搬上台时记了 moved_at)。
func (s *Service) movedToCounterKgBySku(ctx context.Context, branchID string, day time.Time) (map[string]decimal.Decimal, error) {
	startOfDay, endOfDay := s.businessDayBounds(day)
	type row struct {
		CubeProductID string
		QtyKg         decimal.Decimal
	}
	var rows []row
	err := s.db.WithContext(ctx).
		Model(&model.StockBag{}).
		Select("cube_product_id AS cube_product_id, qty_kg AS qty_kg").
		Where("branch_id = ? AND moved_at >= ? AND moved_at < ?", branchID, startOfDay, endOfDay).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("query moved bags: %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		out[r.CubeProductID] = out[r.CubeProductID].Add(r.QtyKg)
	}
	return out, nil
}
