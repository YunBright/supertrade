package service

// internal/fresh-meat/service/stock_openings.go —— **期初结转**(鲜肉-设置)。
//
// # 为什么需要(2026-10-24 门店口径)
//
// 「按营业日推导的冷库余额」是**链式**的:
//
//	冷库(D) = 冷库(D−1) + 结转(D) − 搬上台(D) − 大宗出库(D) + 大宗进库(D)
//
// ⇒ 链的**起点**(期初)必须由门店给:上线前冷库里已经有货,而那些货
// **不在本系统的流水里**(本系统 2026-10 才有搬台流水)⇒ 期初按 0 起算的话,
// 历史日的冷库会**偏低** ⇒ 毛利算不准。
//
// ⇒ 「鲜肉-设置」里录一次**期初结转**(哪个 SKU、冷库多少 kg、从哪天起算)
//   ⇒ 链从那天起算 ⇒ 每一天的余额都对得上。
//
// # 语义
//
// - 一个 SKU 一条(upsert 覆盖)⇒ 改了期初 = 改链的起点;
// - 生效日(BizDate)= 期初余额所属的那一天;
// - 柜台期初恒为 0(开市前柜台是空的)。
//
// # 改完会重算
//
// 保存后 ⇒ 重推冷库余额链 ⇒ 重写各营业日的结余快照 + 重算成本日快照
// ⇒ 历史日的余额/毛利跟着变(链式)。

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

// StockOpeningID 期初结转的**确定性**代理主键(按部位)。
func StockOpeningID(branchID, cutType string) string {
	return surrogateID("SSO", branchID, cutType)
}

// StockOpeningBagID 期初那一行**结转记录(袋子)**的确定性代理主键(按部位)。
//
// ⚠️ 不按生效日：生效日可以被改 ⇒ 主键跟着变就变成"多出一行"
// ⇒ 按 (门店, 部位) ⇒ 改期初 = 覆盖同一行。
func StockOpeningBagID(branchID, cutType string) string {
	return surrogateID("SBO", branchID, cutType)
}

// StockOpeningView 期初结转的一行(对外 JSON)。
type StockOpeningView struct {
	// CutType 是期初的**部位**(五花肉 / 梅花肉 …)⇒ 与每日毛利的部位口径一致。
	CutType model.CutType `json:"cut_type"`
	// BizDate 是期初生效日("YYYY-MM-DD")。
	BizDate     string          `json:"biz_date"`
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`
	// UnitPriceYuan 是**门店给的**单价(元/kg)—— 期初这批货当时的进价。
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	// TotalAmountYuan 是**总价**(元) = 重量 × 单价(服务端算,不可填)。
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	Note            string          `json:"note,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// StockOpeningInput PUT /stock/openings 入参的一行。
type StockOpeningInput struct {
	// CutType 是期初的**部位**(必填)。
	CutType model.CutType `json:"cut_type" binding:"required"`
	// BizDate 是期初生效日("YYYY-MM-DD");空 = 今天。
	BizDate string `json:"biz_date,omitempty"`
	// ColdStoreKg 是期初的冷库余额(kg)。
	ColdStoreKg decimal.Decimal `json:"cold_store_kg"`
	// UnitPriceYuan 是**单价**(元/kg)—— **门店给**。
	//
	// ⚠️ 重量 > 0 时**必填且 > 0**:期初是已经付过钱的货,单价 0 ⇒
	// 成本链的第一天期初金额为 0 ⇒ 那批货**零成本** ⇒ 毛利虚高。
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	Note           string          `json:"note,omitempty"`
}

// StockOpeningsView 某店的期初结转清单(对外 JSON)。
type StockOpeningsView struct {
	BranchID string `json:"branch_id"`
	// Openings 是期初结转明细(按部位升序)。
	Openings []StockOpeningView `json:"openings"`
}

// ListStockOpenings 查某店的期初结转(鲜肉-设置页读端)。
func (s *Service) ListStockOpenings(ctx context.Context, branchID string) (*StockOpeningsView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.SkuStockOpening
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cut_type ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query sku_stock_openings: %w", err)
	}
	out := make([]StockOpeningView, 0, len(rows))
	for _, r := range rows {
		out = append(out, StockOpeningView{
			CutType:         r.CutType,
			BizDate:         time.Time(r.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02"),
			ColdStoreKg:     r.ColdStoreKg,
			UnitPriceYuan:   r.UnitPriceYuan,
			TotalAmountYuan: r.TotalAmountYuan,
			Note:            r.Note,
			UpdatedAt:       r.UpdatedAt,
		})
	}
	return &StockOpeningsView{BranchID: branchID, Openings: out}, nil
}

// applyOpeningToStock 把**期初**落进「位置表(冷库)」与「结转记录(袋子)」。
//
// # 为什么两处都要落(2026-10-25 模拟一周时抓到的断链)
//
// 只写 `sku_stock_openings` 的话，期初的货**只活在链里**：
//
//   - 库存查询(位置表)看不到它 ⇒ 门店说"冷库是空的"，而报表说 100kg；
//   - 大宗结算的 FIFO 队列里没有它 ⇒ 那批货**结不出去**（可领用量不足）。
//
// ⇒ 期初必须是**链 + 位置表 + 队列**三处一致的一份货。
//
// # 参数
//
//	newKg —— 期初的**绝对值**（队列那一行按绝对值写：覆盖，不是累加）；
//	deltaKg —— 相对上次的**差额**（位置表按差额调整：链的起点是绝对值，位置表是增量）；
//	unitPrice —— **门店给的**单价（元/kg）⇒ 钉在队列那一行的成本快照上
//	（这批货结掉时按这个成本进损益，不是 0）。
//
// # 归属
//
// 期初是**上线前就在冷库里的货** ⇒ 没有产出猪 ⇒ 归属快照为空
// ⇒ FIFO 领用它时收入记「**未归属**」（钱不丢，但也不硬塞给某头猪）。
func (s *Service) applyOpeningToStock(
	ctx context.Context, branchID string, cutType model.CutType,
	bizDate time.Time, newKg, deltaKg, unitPrice decimal.Decimal,
) error {
	if !deltaKg.IsZero() {
		// 位置表：冷库 ← 差额。
		sku, err := s.bulkCutSku(ctx, branchID, cutType)
		if err != nil {
			// 该部位没映射 SKU ⇒ 位置表无处安放（链仍按部位算）⇒ 跳过，不猜。
			s.pubLogger.Warn("applyOpeningToStock: 部位没有映射 SKU,期初只进链",
				"branch_id", branchID, "cut_type", cutType, "err", err)
		} else if err := s.addPosition(ctx, s.db, branchID, sku, model.StockLocationColdStore, deltaKg); err != nil {
			return err
		}
	}
	// 结转记录(袋子)：source='opening' ⇒ 进 FIFO 队列（可领用/可结掉）。
	sku, err := s.bulkCutSku(ctx, branchID, cutType)
	if err != nil {
		s.pubLogger.Warn("applyOpeningToStock: 部位没有映射 SKU,期初不进队列",
			"branch_id", branchID, "cut_type", cutType, "err", err)
		return nil
	}
	now := s.now()
	if newKg.IsZero() {
		// 期初被清零/删掉 ⇒ 这一行不该留在队列里（留着会看到一条 0kg 的"袋"）。
		if err := s.db.WithContext(ctx).
			Where("id = ?", StockOpeningBagID(branchID, string(cutType))).
			Delete(&model.StockBag{}).Error; err != nil {
			return fmt.Errorf("delete opening stock_bag: %w", err)
		}
		return nil
	}
	bag := model.StockBag{
		ID:            StockOpeningBagID(branchID, string(cutType)),
		BranchID:      branchID,
		Label:         "", // 期初没有标签号 ⇒ 是"不装袋的散货结转记录"
		CubeProductID: sku,
		CutType:       cutType,
		QtyKg:         newKg,
		OriginDate:    datatypesDate(bizDate),
		Location:      string(model.StockLocationColdStore),
		Status:        BagStatusActive,
		Source:        BagSourceOpening,
		// 成本快照 = **门店给的**单价（不是 0,也不是倒算的移动加权单价）。
		UnitCostYuan: unitPrice,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.upsertStockBag(ctx, bag); err != nil {
		return err
	}
	return nil
}

// SaveStockOpenings 保存期初结转(鲜肉-设置页写端)⇒ 重推余额链。
//
// 幂等:同一 (门店, 部位) 一条 ⇒ 重复保存 = 覆盖。
//
// ⚠️ 已审计结转的期间**冻结** ⇒ 期初生效日落在冻结区 ⇒ 400。
func (s *Service) SaveStockOpenings(ctx context.Context, branchID string, items []StockOpeningInput) (*StockOpeningsView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	now := s.now()
	loc := s.bizTZOrUTC()
	today := now.In(loc)

	earliest := ""
	for i, it := range items {
		if !it.CutType.Valid() {
			return nil, fmt.Errorf("%w: openings[%d].cut_type %q 不在 %d 类枚举", ErrInvalidInput, i, it.CutType, len(model.AllCutTypes))
		}
		bizDate := today
		if key := strings.TrimSpace(it.BizDate); key != "" {
			parsed, err := time.ParseInLocation("2006-01-02", key, loc)
			if err != nil {
				return nil, fmt.Errorf("%w: openings[%d].biz_date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, i, key)
			}
			if parsed.After(today) {
				return nil, fmt.Errorf("%w: openings[%d].biz_date %s 不能是未来", ErrInvalidInput, i, key)
			}
			bizDate = parsed
		}
		// 冻结检查:期初生效日落在已结转的期间 ⇒ 不许改。
		//
		// ⚠️ **先于数值校验**:审计不可逆是硬规则 ⇒ 冻结期间的任何改动
		//   都要先说"已冻结"(而不是"你没填单价")—— 前者才是真正的拦路虎。
		if err := s.assertNotAudited(ctx, branchID, bizDate, "期初结转"); err != nil {
			return nil, err
		}
		if it.ColdStoreKg.IsNegative() {
			return nil, fmt.Errorf("%w: openings[%d].cold_store_kg 不能为负", ErrInvalidInput, i)
		}
		// ⚠️ **有货就要给单价**(2026-10-25 门店口径)。
		//
		// 期初是**上线前就在冷库里的货**,它的成本不在本系统流水里
		// ⇒ 单价 0 ⇒ 成本链的第一天期初金额 0 ⇒ 那批货**零成本**
		// ⇒ 卖出去全是利润(毛利虚高)⇒ 所以这里**必填且 > 0**。
		//
		// 重量 = 0 的行(明确的 0)⇒ 金额也是 0 ⇒ 单价可省。
		if it.ColdStoreKg.IsPositive() && !it.UnitPriceYuan.IsPositive() {
			return nil, fmt.Errorf("%w: openings[%d](%s)的 unit_price_yuan 必填且要 > 0 —— "+
				"期初是已经付过钱的货,单价 0 会让这批货零成本、毛利虚高",
				ErrInvalidInput, i, model.CutLabel(it.CutType))
		}
		if it.UnitPriceYuan.IsNegative() {
			return nil, fmt.Errorf("%w: openings[%d].unit_price_yuan 不能为负", ErrInvalidInput, i)
		}
		// ⚠️ 先读**旧值**:位置表与结转记录都要按**差额**调整
		//   (链的起点是"绝对值",位置表是"增量" ⇒ 改期初必须走差额,否则会叠两次)。
		oldKg := decimal.Zero
		var old model.SkuStockOpening
		if err := s.db.WithContext(ctx).
			Where("id = ?", StockOpeningID(branchID, string(it.CutType))).
			First(&old).Error; err == nil {
			oldKg = old.ColdStoreKg
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("query sku_stock_opening(old): %w", err)
		}
		rec := model.SkuStockOpening{
			ID:          StockOpeningID(branchID, string(it.CutType)),
			BranchID:    branchID,
			CutType:     it.CutType,
			ColdStoreKg: it.ColdStoreKg,
			// 单价 = 门店给的;总价 = 重量 × 单价(**服务端算,不可填**)。
			UnitPriceYuan:   it.UnitPriceYuan,
			TotalAmountYuan: it.ColdStoreKg.Mul(it.UnitPriceYuan),
			BizDate:         datatypesDate(bizDate),
			Note:            strings.TrimSpace(it.Note),
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"cold_store_kg":     rec.ColdStoreKg,
				"unit_price_yuan":   rec.UnitPriceYuan,
				"total_amount_yuan": rec.TotalAmountYuan,
				"biz_date":          rec.BizDate,
				"note":              rec.Note,
				"updated_at":        rec.UpdatedAt,
			}),
		}).Create(&rec).Error; err != nil {
			return nil, fmt.Errorf("upsert sku_stock_opening %s: %w", rec.ID, err)
		}
		key := bizDate.Format("2006-01-02")
		if earliest == "" || key < earliest {
			earliest = key
		}
		// ⚠️ **期初也要落进「位置表」与「结转记录(袋子)」** —— 2026-10-25 模拟一周时抓到的断链。
		//
		// 只写 `sku_stock_openings` 的话，期初的货**只活在链里**：
		//	- 库存查询(位置表)看不到它 ⇒ 门店说"冷库是空的"，报表说 100kg；
		//	- 大宗结算的 FIFO 队列里没有它 ⇒ 那批货**结不出去**（可领用量不足）。
		//
		// ⇒ 这里补上：① 位置表(冷库) 按**差额**调整；② 登记一行
		//   source='opening' 的结转记录（**没有产出猪** ⇒ 归属为空，收入记「未归属」）。
		if err := s.applyOpeningToStock(ctx, branchID, it.CutType, bizDate, it.ColdStoreKg,
			it.ColdStoreKg.Sub(oldKg), it.UnitPriceYuan); err != nil {
			return nil, err
		}
	}
	// 期初改了 ⇒ 链的起点变了 ⇒ 各营业日的成本日快照要重算
	// (结余快照是**审计结转**的产物,审计前实时算,这里不落)。
	if earliest != "" {
		if err := s.recomputeCostSnapshots(ctx, branchID, mustParseDateInLoc(earliest, loc)); err != nil {
			return nil, fmt.Errorf("重算按营业日的成本快照: %w", err)
		}
	}
	return s.ListStockOpenings(ctx, branchID)
}

// DeleteStockOpening 删掉某个部位的**期初结转**(把该部位从链的起点里去掉)。
//
// # 为什么需要
//
// `PUT /stock/openings` 只能"写 0"⇒ 门店想**显式删除**某个部位的期初时
// 只能写 0 ⇒ 界面上分不清"这个部位冷库里真的有 0"和"这个部位压根没有期初"
// ⇒ 列表里一直挂着一条 0 kg 的行,看起来像脏数据。
//
// ⚠️ 冻结检查:期初生效日落在已结转的期间 ⇒ 不许删(与保存同口径)。
func (s *Service) DeleteStockOpening(ctx context.Context, branchID string, cutType model.CutType) (*StockOpeningsView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if !cutType.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举", ErrInvalidInput, cutType, len(model.AllCutTypes))
	}
	var rec model.SkuStockOpening
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cut_type = ?", branchID, cutType).
		First(&rec).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: 部位 %s 没有期初结转记录", ErrInvalidInput, cutType)
		}
		return nil, fmt.Errorf("query sku_stock_opening: %w", err)
	}
	// 冻结检查:生效日落在已结转的期间 ⇒ 不许删。
	if err := s.assertNotAudited(ctx, branchID, time.Time(rec.BizDate).In(s.bizTZOrUTC()), "期初结转"); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Delete(&model.SkuStockOpening{}, "id = ?", rec.ID).Error; err != nil {
		return nil, fmt.Errorf("delete sku_stock_opening: %w", err)
	}
	// ⚠️ 删掉期初 ⇒ **位置表与队列**里的那批货也要一起撤
	//   （否则"链里没了、账上还在"⇒ 门店看到两本账）。
	if err := s.applyOpeningToStock(ctx, branchID, rec.CutType,
		time.Time(rec.BizDate).In(s.bizTZOrUTC()), decimal.Zero, rec.ColdStoreKg.Neg(), decimal.Zero); err != nil {
		return nil, err
	}
	// 链的起点变了 ⇒ 各营业日的成本日快照要重算。
	if err := s.recomputeCostSnapshots(ctx, branchID, time.Time(rec.BizDate).In(s.bizTZOrUTC())); err != nil {
		return nil, fmt.Errorf("重算按营业日的成本快照: %w", err)
	}
	return s.ListStockOpenings(ctx, branchID)
}
