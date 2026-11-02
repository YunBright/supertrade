package service

// internal/fresh-meat/service/pig_fifo.go —— **毛利按 FIFO 回归到具体猪**。
//
// # 目标(2026-10-24 门店口径,用户原话)
//
// 「做毛利回归到具体猪(如无标签,则按结转(日终盘点)的先后顺序(盘点所属日期)归摊。)」
//
// ⇒ 一笔销售卖掉的货,要归回**产出它的那头猪**,而不是"今天碰巧在场的猪"。
//
// # 队列是什么
//
// FIFO 队列 = **按日结转记录**(stock_bags 表):
//
//   - 一行 = 一天一个 SKU 的结转(归属日 + SKU + 重量 + 猪归属快照)。
//
// ⚠️ 2026-11-02 owner:标签号玩法下线 ⇒ 队列里不再区分"装袋的"与"散货的"。
//
// 排序:origin_date 升序(同一天按 id 稳定排序)⇒ **先进先出**。
//
// # 怎么摊
//
//   - 记录带**归属快照**(PigAlloc,盘点那一刻钉死的)⇒ 按快照里的猪份额摊
//     —— 这就是"按这批货的归属快照摊给那些猪";
//   - 记录**没有**归属快照(盘点那天没补录分割 ⇒ 算不出归属)
//     ⇒ **兜底**:按那天各猪**毛重比例**摊。
//
// # 消耗必须落库
//
// 每卖一笔就扣掉对应结转记录的"还没卖掉的重量"(stock_bags.consumed_kg)
// ⇒ 下次不会把同一批货再摊一次(否则收入会被摊好几遍)。
//
// # 幂等
//
// 同一笔销售重复投递 ⇒ **整段跳过**(不摊、不扣 FIFO)。
//
// ⚠️ 为什么不能靠"重算后看主键冲不冲突"来判重:重投时 FIFO 的状态已经变了
// (上一次已经扣过 consumed_kg)⇒ 再算一次会得到**不同的**摊派结果
// ⇒ 必须**先查原始 line_id 有没有落过库**,落过就整段跳过。

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
	"gorm.io/gorm"
)

// fifoShare 一条结转记录里的**一头猪的份额**。
type fifoShare struct {
	pigID string
	kg    decimal.Decimal
}

// fifoAlloc 一笔销售按 FIFO 摊出来的结果。
type fifoAlloc struct {
	// KgByPig 是"摊到每头猪的重量"(key = pig_id)。
	KgByPig map[string]decimal.Decimal
	// Consumed 是"这次从哪几条结转记录吃掉了多少"(结转记录 ID → kg)。
	Consumed map[string]decimal.Decimal
}

// fifoSaleLineID 拆行后的**确定性** pos_line_id。
//
// ⚠️ 为什么必须派生:`line_sales_by_pigs` 上有 UNIQUE(branch_id, pos_line_id)
// ⇒ 同一笔销售摊到多头猪时**不能**都用原始 line_id
// ⇒ 否则后面的猪会被 ON CONFLICT DO NOTHING **静默丢掉**(收入少记)。
//
// ⇒ 按 (原始 line_id, pig_id) 派生 ⇒ 短(1+16+1+16 = 34 字符,varchar(64) 装得下)
// 且确定(同一次投递永远算出同一个 id ⇒ 幂等)。
func fifoSaleLineID(originalLineID, pigID string) string {
	return "F" + hashKey(originalLineID) + "-" + hashKey(pigID)
}

// fifoSaleLinePrefix 同一笔原始销售行的所有拆行共用的前缀。
//
// ⇒ 幂等检查用:原始行可能以"原始 line_id"落一行(只摊到一头猪),
// 也可能以 "F<hash>-" 开头的多行落(摊到多头猪)⇒ 两种都要认。
func fifoSaleLinePrefix(originalLineID string) string {
	return "F" + hashKey(originalLineID) + "-"
}

// saleLineRecorded 这笔**原始**销售行是否已经落过库。
//
// ⚠️ 幂等的关键(见文件头):重投时 FIFO 状态已变 ⇒ 不能靠"重算 + 看主键冲突"判重。
func (s *Service) saleLineRecorded(ctx context.Context, tx *gorm.DB, branchID, lineID string) (bool, error) {
	if strings.TrimSpace(lineID) == "" {
		return false, nil
	}
	prefix := fifoSaleLinePrefix(lineID)
	var n int64
	err := tx.WithContext(ctx).Model(&model.LineSalesByPig{}).
		Where("pos_branch_id = ? AND (pos_line_id = ? OR pos_line_id LIKE ?)",
			branchID, lineID, prefix+"%").
		Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("check line_sales_by_pig(recorded): %w", err)
	}
	return n > 0, nil
}

// allocateSaleByFIFO 按 FIFO 把这笔销售的重量摊到结转记录(再摊到猪)。
//
// 返回 nil ⇒ **没有可摊的结转记录**(队列空 / 全卖完了 / 都摊不出归属)
// ⇒ 调用方走老路(归到"今天碰巧在场的猪")。
func (s *Service) allocateSaleByFIFO(
	ctx context.Context, tx *gorm.DB,
	branchID, cubeProductID string, saleDate datatypes.Date, qtyKg decimal.Decimal,
) (*fifoAlloc, error) {
	if !qtyKg.IsPositive() {
		return nil, nil
	}
	// FIFO 队列:该 SKU 的全部结转记录,按产生日(归属日)升序。
	//
	// ⚠️ origin_date <= saleDate:只能吃"卖出之前就已经结转出来"的货
	// (今天的货不该被昨天的销售吃掉)。
	var records []model.StockBag
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id = ? AND origin_date <= ?",
			branchID, cubeProductID, saleDate).
		Order("origin_date ASC, id ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("query stock_bags(fifo): %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	alloc := &fifoAlloc{
		KgByPig:  map[string]decimal.Decimal{},
		Consumed: map[string]decimal.Decimal{},
	}
	need := qtyKg
	pigCache := map[string][]model.WholePig{}
	for _, rec := range records {
		if !need.IsPositive() {
			break
		}
		remaining := rec.QtyKg.Sub(rec.ConsumedKg)
		if !remaining.IsPositive() {
			continue // 这批已经卖完了
		}
		shares, err := s.fifoRecordShares(ctx, tx, rec, pigCache)
		if err != nil {
			return nil, err
		}
		if len(shares) == 0 {
			// ⚠️ 这条记录**算不出归属**(那天没有猪)⇒ 跳过它,**不吃**这批货
			// (摊不出去的货不能白吃 FIFO ⇒ 否则收入会凭空消失)。
			continue
		}
		take := remaining
		if take.GreaterThan(need) {
			take = need
		}
		total := decimal.Zero
		for _, sh := range shares {
			total = total.Add(sh.kg)
		}
		if !total.IsPositive() {
			continue
		}
		for _, sh := range shares {
			alloc.KgByPig[sh.pigID] = alloc.KgByPig[sh.pigID].Add(take.Mul(sh.kg).Div(total))
		}
		alloc.Consumed[rec.ID] = alloc.Consumed[rec.ID].Add(take)
		need = need.Sub(take)
	}
	if len(alloc.KgByPig) == 0 {
		return nil, nil
	}
	return alloc, nil
}

// fifoRecordShares 这条结转记录的**猪份额**。
//
// ① 归属快照(PigAlloc):盘点那一刻钉死的"这批货是哪些猪产出的、各多少 kg";
// ② **兜底**:快照为空(盘点那天没补录分割 ⇒ 算不出归属)
//
//	⇒ 按那天各猪**毛重比例**摊(与 applyProducedFallback 同一套口径 —— 只能到猪这一层)。
func (s *Service) fifoRecordShares(
	ctx context.Context, tx *gorm.DB, rec model.StockBag, pigCache map[string][]model.WholePig,
) ([]fifoShare, error) {
	// ⚠️ **期初批次没有产出猪**（上线前就在冷库里的货）⇒ 不摊给任何猪。
	//
	// ⇒ 它的重量照样能被消耗（货确实卖掉了）⇒ 归到「**未归属**」(pigID="")：
	//   钱不丢（结算金额/毛利对得上），但也不硬塞给某头猪（那会让单猪毛利失真）。
	if rec.Source == BagSourceOpening {
		return []fifoShare{{pigID: "", kg: rec.QtyKg}}, nil
	}
	if len(rec.PigAlloc) > 0 {
		var items []bagPigAllocItem
		if err := json.Unmarshal(rec.PigAlloc, &items); err == nil {
			out := make([]fifoShare, 0, len(items))
			for _, it := range items {
				pigID := strings.TrimSpace(it.PigID)
				if pigID == "" {
					continue
				}
				kg, err := decimal.NewFromString(strings.TrimSpace(it.QtyKg))
				if err != nil || !kg.IsPositive() {
					continue
				}
				out = append(out, fifoShare{pigID: pigID, kg: kg})
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	// ---- 兜底:按当天各猪毛重比例 ----
	pigs, err := s.pigsOfBizDay(ctx, tx, rec.BranchID, rec.OriginDate, pigCache)
	if err != nil {
		return nil, err
	}
	totalW := decimal.Zero
	for _, p := range pigs {
		totalW = totalW.Add(p.GrossWeightKg)
	}
	if !totalW.IsPositive() {
		return nil, nil
	}
	out := make([]fifoShare, 0, len(pigs))
	for _, p := range pigs {
		out = append(out, fifoShare{pigID: p.ID, kg: p.GrossWeightKg})
	}
	return out, nil
}

// pigsOfBizDay 某营业日进场的猪(兜底摊派用)。
func (s *Service) pigsOfBizDay(
	ctx context.Context, tx *gorm.DB, branchID string, day datatypes.Date, cache map[string][]model.WholePig,
) ([]model.WholePig, error) {
	key := branchID + "|" + dateKey(day)
	if pigs, ok := cache[key]; ok {
		return pigs, nil
	}
	start, end := s.businessDayBounds(time.Time(day))
	var pigs []model.WholePig
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Order("arrived_at ASC, id ASC").
		Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs(fifo fallback): %w", err)
	}
	cache[key] = pigs
	return pigs, nil
}

// consumeFIFO 把这次卖掉的重量从对应结转记录上扣掉(**落库**)。
//
// ⚠️ 必须落库:不落库 ⇒ 每次重算都把同一批货再摊一次。
func (s *Service) consumeFIFO(ctx context.Context, tx *gorm.DB, consumed map[string]decimal.Decimal) error {
	now := s.now()
	for id, kg := range consumed {
		if !kg.IsPositive() {
			continue
		}
		if err := tx.WithContext(ctx).Model(&model.StockBag{}).
			Where("id = ?", id).
			Updates(map[string]any{
				"consumed_kg": gorm.Expr("consumed_kg + ?", kg),
				"updated_at":  now,
			}).Error; err != nil {
			return fmt.Errorf("consume stock_bag %s: %w", id, err)
		}
	}
	return nil
}

// splitSaleRow 一笔销售拆开后的一行(一头猪一行)。
type splitSaleRow struct {
	PigID      string
	QtyKg      decimal.Decimal
	AmountYuan decimal.Decimal
}

// splitSaleByPig 把一笔销售按"摊到各猪的重量"拆成多行(一头猪一行)。
//
// ⚠️ **守恒**:Σ 拆后重量 == 原重量;Σ 拆后金额 == 原金额
// (余数归最后一行 —— 保证总量守恒)。
func splitSaleByPig(qtyKg, amountYuan decimal.Decimal, kgByPig map[string]decimal.Decimal, order []string) []splitSaleRow {
	pigs := make([]string, 0, len(order))
	for _, pigID := range order {
		if kgByPig[pigID].IsPositive() {
			pigs = append(pigs, pigID)
		}
	}
	rows := make([]splitSaleRow, 0, len(pigs))
	accKg := decimal.Zero
	accAmt := decimal.Zero
	for i, pigID := range pigs {
		if i == len(pigs)-1 {
			// 最后一行拿余数(保证总量守恒)。
			rows = append(rows, splitSaleRow{
				PigID:      pigID,
				QtyKg:      qtyKg.Sub(accKg),
				AmountYuan: amountYuan.Sub(accAmt),
			})
			break
		}
		q := kgByPig[pigID].Round(3)
		a := decimal.Zero
		if qtyKg.IsPositive() {
			a = amountYuan.Mul(q).Div(qtyKg).Round(2)
		}
		rows = append(rows, splitSaleRow{PigID: pigID, QtyKg: q, AmountYuan: a})
		accKg = accKg.Add(q)
		accAmt = accAmt.Add(a)
	}
	return rows
}

// sortedPigIDs 把 pig_id 列表**确定性**排序(输出稳定,便于断言与幂等)。
func sortedPigIDs(kgByPig map[string]decimal.Decimal) []string {
	out := make([]string, 0, len(kgByPig))
	for pigID := range kgByPig {
		out = append(out, pigID)
	}
	sort.Strings(out)
	return out
}
