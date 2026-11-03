package service

// internal/fresh-meat/service/external_purchase.go —— **外采**(2026-11-03 owner)。
//
// # 是什么
//
// 「鲜肉 · 每日作业」在「① 录入猪」与「② 搬上台」之间加一步「**录入外采**」:
// 门店**额外采购某个部位的货**。
//
// owner 原话:「11 月 1 日采购了 20 斤货号 01401 对应部位肋排/排骨」。
//
// # 与「进猪」的区别(这是外采这个概念的**全部**要点)
//
// 进猪:一整头猪进来 ⇒ 分割出各部位 ⇒ 成本按**猪价**摊到各部位。
// 外采:**已经切好的某个部位**的一笔额外进货 ⇒ 没有产出猪、没有分割。
//
// ⇒ 所以外采的成本是**门店给的单价**(与期初结转同一口径),
//   不是猪成本摊出来的 ⇒ 单价 0 会让这批货**零成本**、毛利虚高 ⇒ 必须 > 0。
//
// # 它怎么进成本链(口径的核心)
//
// 移动加权平均(见 moving_average.go 的包注释):
//
//	可售数量 = 期初 + 本期入库
//	可售金额 = 期初金额 + 本期入库金额
//	本期移动加权单价 = 可售金额 / 可售数量
//	本期销售成本 = 可售金额 − 期末存货金额 − 统收成本 − 报损成本
//	本期毛利     = 本期销售收入 − 本期销售成本
//
// ⇒ 外采就是「本期入库」的**另一个来源**(与猪的分割产出并列):
//
//	本期入库量   += 外采 kg(按部位)
//	本期入库金额 += 外采金额(kg × 门店给的单价)
//
// ⇒ **卖掉的那些外采** ⇒ 成本进销售成本 ⇒ 毛利下降
//   (owner:「进货成本变高了」);**还没卖掉的** ⇒ 留在期末存货里 ⇒ 不影响当天毛利。
//
// ⚠️ **必须在 applySplitWeights 之后叠加**:那个函数把入库量整体替换成
//   **分割权重 W**、并把入库金额按 W 的比例重分
//   ⇒ 外采放进去会被 W 顶掉、金额会被摊到别的部位。
//
// # 库存:跟录猪一样(owner 2026-11-03 拍板)
//
//	「跟录猪一样,卖不完会在日终盘点体现,随日终盘点一起进冷库,
//	  部位卖完,日终盘点为 0,就进不到冷库了。」
//
// ⇒ **外采的货直接上猪肉台**(位置表 counter += 外采量)—— 与录猪的分割产出同构:
//   录完就能卖,**不用再搬一次台**。
//
// ⇒ **不占冷库**:冷库余额链里**没有**外采这一项
//   ⇒ 卖不完的部分靠日终盘点的「柜台实盘」体现 ⇒ 过夜搬运(后端自动)进冷库;
//     部位卖完(盘点 0)⇒ 自然进不了冷库 ✔ 与 owner 的描述逐字吻合。
//
// ⇒ 与「进猪」唯一的差别:进猪的分割产出**不写位置表**
//   (链不记分割产出,见 daily_balance.go 的 giftOut 注释);
//   而外采**要**写位置表(记在猪肉台)⇒ 门店在「库存」页看得见这批货。
//
// ⚠️ 为什么不走「先进冷库、再搬一次台」那套(2026-11-03 拍板前的实现):
//   那套要求门店**必须再搬一次台**才准卖,而漏搬 ⇒ 冷库链虚高 ⇒ **假盘亏**
//   ⇒ 外采是**可选动作**,不该埋「漏一步就亏钱」的坑。
//
// # FIFO 归摊:外采的货**不落到猪身上**
//
// 外采没有产出猪 ⇒ 它的收入不能摊给某头猪(那会让单猪毛利失真)
// ⇒ 外采自己登记一行**结转记录**(source='external',归属快照为空)
// ⇒ FIFO 领用它时收入记「未归属」(与期初结转同一处理,见 pig_fifo.go)。
//
// ⚠️ 同时**日终盘点的结转记录要扣掉外采的那部分**
//   (盘点盘的是猪肉台上的货,其中可能就有外采没卖完的)
//   ⇒ 不扣 ⇒ 同一批货在队列里出现两次 ⇒ FIFO 会把收入摊两遍。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ExternalPurchaseInput POST /external-purchases 的入参。
type ExternalPurchaseInput struct {
	// CubeProductID 是**货号**(必填;必须是该店 branch_cut_mappings 里配过的货号)。
	//
	// ⚠️ 门店按**货号**录(owner 口径:「货号 01401 对应部位肋排/排骨」)
	//   ⇒ 部位由后端从映射表反查 ⇒ 门店不用自己挑部位、也不会挑错。
	CubeProductID string `json:"cube_product_id"`
	// QtyKg 是**采购重量**(kg,必填 > 0)。
	//
	// ⚠️ 单位统一 kg(前端可按「斤」录,换算在前端做)。
	QtyKg decimal.Decimal `json:"qty_kg"`
	// UnitPriceYuan 是**采购单价**(元/kg,必填 > 0)。
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	// TakenDate 是**归属营业日**("YYYY-MM-DD");空 = 今天(后端按 bizTZ 划日)。
	TakenDate string `json:"taken_date,omitempty"`
	// Note 是备注(如「XX 供应商送的」)。
	Note string `json:"note,omitempty"`
}

// ExternalPurchaseView 一行外采(对外 JSON)。
type ExternalPurchaseView struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
	// BizDate 是归属营业日("YYYY-MM-DD" 文本)。
	BizDate string `json:"biz_date"`
	// CubeProductID 是货号。
	CubeProductID string `json:"cube_product_id"`
	// CubeProductName 是商品名(映射表里查不到就留空 ⇒ 前端显示货号即可)。
	CubeProductName string `json:"cube_product_name,omitempty"`
	// CutType 是这个货号对应的部位。
	CutType model.CutType `json:"cut_type"`
	// CutLabel 是部位中文名。
	CutLabel string `json:"cut_label,omitempty"`
	// QtyKg 是采购重量(kg)。
	QtyKg decimal.Decimal `json:"qty_kg"`
	// UnitPriceYuan 是采购单价(元/kg)。
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	// TotalAmountYuan 是总价(元)= 重量 × 单价(服务端算)。
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	Note            string          `json:"note,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// ExternalPurchasesView 某营业日的外采清单(对外 JSON)。
type ExternalPurchasesView struct {
	BranchID string `json:"branch_id"`
	// BizDate 是归属营业日("YYYY-MM-DD" 文本)。
	BizDate string `json:"biz_date"`
	// TotalKg / TotalAmountYuan 是当天外采的合计。
	TotalKg         decimal.Decimal `json:"total_kg"`
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	// Items 是明细(按录入顺序)。
	Items []ExternalPurchaseView `json:"items"`
}

// ExternalPurchaseID 外采单的**确定性**代理主键。
//
// ⚠️ 为什么用确定性 id:溯源记账(core/purge)按 INSERT 里的主键值删行
//   ⇒ upsert 时 GORM 保留**旧行**的主键 ⇒ 主键必须是算出来的,不是随机的。
func ExternalPurchaseID(branchID, bizDate, sku string, seq int) string {
	return surrogateID("SEP", branchID, bizDate, sku, fmt.Sprintf("%03d", seq))
}

// ExternalPurchaseCarryoverID 外采那一行**结转记录**的确定性代理主键。
func ExternalPurchaseCarryoverID(externalID string) string {
	return surrogateID("SBE", externalID)
}

// RecordExternalPurchase 录一笔外采。
//
// # 做了什么(一个事务)
//
//	① external_purchases 行(事实);
//	② 位置表:**猪肉台** += 外采量(录完就在台面上 ⇒ 直接能卖,不用再搬台);
//	③ 结转记录(source='external'):进 FIFO 队列 ⇒ 大宗结算能领用、零售归摊不落到猪身上。
//
// # 之后(事务外)
//
// 重算成本链(那天起的结余快照 + 整条链物化)⇒ 当天与往后的毛利跟着变。
//
// ⚠️ **失败不阻断录入**:外采是"事实录入",成功就成功;
//   派生数据没跟上只影响"看数字",下次任何写动作都会自愈(与录猪同一套纪律)。
func (s *Service) RecordExternalPurchase(ctx context.Context, branchID string, in ExternalPurchaseInput,
	operatorID string) (*ExternalPurchaseView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	sku := strings.TrimSpace(in.CubeProductID)
	if sku == "" {
		return nil, fmt.Errorf("%w: cube_product_id 必填(按货号录,如 01401)", ErrInvalidInput)
	}
	if !in.QtyKg.IsPositive() {
		return nil, fmt.Errorf("%w: qty_kg 必须 > 0", ErrInvalidInput)
	}
	if !in.UnitPriceYuan.IsPositive() {
		return nil, fmt.Errorf("%w: unit_price_yuan 必填且要 > 0 —— "+
			"外采是已经付过钱的货,单价 0 会让这批货零成本、毛利虚高", ErrInvalidInput)
	}
	now := s.now()
	takenDate, _, err := s.resolveTakenDate(in.TakenDate, now)
	if err != nil {
		return nil, err
	}
	cutType, err := s.cutTypeOfPurchasedSku(ctx, branchID, sku)
	if err != nil {
		return nil, err
	}
	// 链起点 / 跨日顺序:与录猪、搬台同一套纪律(「不补录昨天,就无法录今天」)。
	if err := s.assertAfterChainStart(ctx, branchID, takenDate, "外采"); err != nil {
		return nil, err
	}
	if err := s.assertPrevBizDayStocktaken(ctx, branchID, takenDate); err != nil {
		return nil, err
	}

	bizDate := datatypesDate(takenDate)
	amount := in.QtyKg.Mul(in.UnitPriceYuan).Round(2)
	dayStart, dayEnd := s.businessDayBounds(takenDate)
	seq, err := s.nextExternalPurchaseSeq(ctx, branchID, dayStart, dayEnd, sku)
	if err != nil {
		return nil, err
	}
	row := model.ExternalPurchase{
		ID:              ExternalPurchaseID(branchID, dateKey(bizDate), sku, seq),
		BranchID:        branchID,
		BizDate:         bizDate,
		CubeProductID:   sku,
		CutType:         cutType,
		QtyKg:           in.QtyKg,
		UnitPriceYuan:   in.UnitPriceYuan,
		TotalAmountYuan: amount,
		Note:            strings.TrimSpace(in.Note),
		OperatorID:      operatorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&row).Error; err != nil {
			return fmt.Errorf("create external_purchase: %w", err)
		}
		// 位置表:**猪肉台** += 外采量(直接上台 ⇒ 录完就能卖,不用再搬台)。
		if err := s.addPosition(ctx, tx, branchID, sku, model.StockLocationCounter, in.QtyKg); err != nil {
			return err
		}
		// 结转记录(source='external'):FIFO 队列的一员(大宗可领用、零售归摊不落到猪)。
		return s.registerExternalCarryoverRecord(ctx, tx, row)
	})
	if err != nil {
		return nil, err
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, takenDate); err != nil {
		s.pubLogger.Error("RecordExternalPurchase: 成本链重算失败(派生数据会旧,下次写动作会自愈)",
			"branch_id", branchID, "id", row.ID, "err", err)
	}
	return s.externalPurchaseView(ctx, row)
}

// UpdateExternalPurchase 改一笔外采(数量 / 单价 / 备注)。
//
// ⚠️ 位置表按**差额**调整(猪肉台 += 新 − 旧)⇒ 幂等。
// ⚠️ 结转记录同步改(qty / 成本快照)。
func (s *Service) UpdateExternalPurchase(ctx context.Context, branchID, id string, in ExternalPurchaseInput) (
	*ExternalPurchaseView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	if !in.QtyKg.IsPositive() {
		return nil, fmt.Errorf("%w: qty_kg 必须 > 0", ErrInvalidInput)
	}
	if !in.UnitPriceYuan.IsPositive() {
		return nil, fmt.Errorf("%w: unit_price_yuan 必填且要 > 0 —— "+
			"外采是已经付过钱的货,单价 0 会让这批货零成本、毛利虚高", ErrInvalidInput)
	}
	var cur model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).First(&cur).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("%w: 外采单 %s 不存在", ErrInvalidInput, id)
		}
		return nil, fmt.Errorf("query external_purchase: %w", err)
	}
	now := s.now()
	amount := in.QtyKg.Mul(in.UnitPriceYuan).Round(2)
	updates := map[string]any{
		"qty_kg":           in.QtyKg,
		"unit_price_yuan":  in.UnitPriceYuan,
		"total_amount_yuan": amount,
		"note":             strings.TrimSpace(in.Note),
		"updated_at":       now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Model(&model.ExternalPurchase{}).
			Where("branch_id = ? AND id = ?", branchID, id).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update external_purchase: %w", err)
		}
		// 位置表:按差额(新 − 旧)。
		delta := in.QtyKg.Sub(cur.QtyKg)
		if !delta.IsZero() {
			if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationCounter, delta); err != nil {
				return err
			}
		}
		cur.QtyKg = in.QtyKg
		cur.UnitPriceYuan = in.UnitPriceYuan
		cur.TotalAmountYuan = amount
		cur.Note = strings.TrimSpace(in.Note)
		cur.UpdatedAt = now
		// 结转记录:重量与成本快照同步。
		return s.registerExternalCarryoverRecord(ctx, tx, cur)
	})
	if err != nil {
		return nil, err
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, time.Time(cur.BizDate)); err != nil {
		s.pubLogger.Error("UpdateExternalPurchase: 成本链重算失败(派生数据会旧,下次写动作会自愈)",
			"branch_id", branchID, "id", id, "err", err)
	}
	return s.externalPurchaseView(ctx, cur)
}

// DeleteExternalPurchase 撤一笔外采。
//
// ⚠️ 位置表按**反向差额**调整(猪肉台 −= 外采量)。
// ⚠️ 结转记录一并删掉(它只代表这批外采的货)。
func (s *Service) DeleteExternalPurchase(ctx context.Context, branchID, id string) error {
	if branchID == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var cur model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).First(&cur).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return fmt.Errorf("%w: 外采单 %s 不存在", ErrInvalidInput, id)
		}
		return fmt.Errorf("query external_purchase: %w", err)
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).
			Delete(&model.ExternalPurchase{}).Error; err != nil {
			return fmt.Errorf("delete external_purchase: %w", err)
		}
		// 位置表:猪肉台 −= 外采量。
		if err := s.addPosition(ctx, tx, branchID, cur.CubeProductID, model.StockLocationCounter, cur.QtyKg.Neg()); err != nil {
			return err
		}
		// 结转记录:删掉(只代表这批外采的货)。
		if err := tx.WithContext(ctx).Where("id = ?", ExternalPurchaseCarryoverID(cur.ID)).
			Delete(&model.CarryoverRecord{}).Error; err != nil {
			return fmt.Errorf("delete external carryover_record: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, time.Time(cur.BizDate)); err != nil {
		s.pubLogger.Error("DeleteExternalPurchase: 成本链重算失败(派生数据会旧,下次写动作会自愈)",
			"branch_id", branchID, "id", id, "err", err)
	}
	return nil
}

// ListExternalPurchases 查某营业日的外采清单。
//
// bizDate 空 = 今天。
func (s *Service) ListExternalPurchases(ctx context.Context, branchID, bizDate string) (*ExternalPurchasesView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	day := s.now().In(loc)
	if key := strings.TrimSpace(bizDate); key != "" {
		parsed, err := time.ParseInLocation("2006-01-02", key, loc)
		if err != nil {
			return nil, fmt.Errorf("%w: biz_date 格式应为 YYYY-MM-DD", ErrInvalidInput)
		}
		day = parsed
	}
	start, end := s.businessDayBounds(day)
	var rows []model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query external_purchases: %w", err)
	}
	out := &ExternalPurchasesView{
		BranchID: branchID,
		BizDate:  day.In(loc).Format("2006-01-02"),
		Items:    make([]ExternalPurchaseView, 0, len(rows)),
	}
	for _, r := range rows {
		v, err := s.externalPurchaseView(ctx, r)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, *v)
		out.TotalKg = out.TotalKg.Add(r.QtyKg)
		out.TotalAmountYuan = out.TotalAmountYuan.Add(r.TotalAmountYuan)
	}
	return out, nil
}

// externalPurchaseView 一行外采的对外视图(补商品名与部位中文名)。
func (s *Service) externalPurchaseView(ctx context.Context, r model.ExternalPurchase) (*ExternalPurchaseView, error) {
	_, nameOfSku, err := s.loadSkuCatalog(ctx, r.BranchID, nil)
	if err != nil {
		return nil, err
	}
	return &ExternalPurchaseView{
		ID:              r.ID,
		BranchID:        r.BranchID,
		BizDate:         time.Time(r.BizDate).In(s.bizTZOrUTC()).Format("2006-01-02"),
		CubeProductID:   r.CubeProductID,
		CubeProductName: nameOfSku[r.CubeProductID],
		CutType:         r.CutType,
		CutLabel:        model.CutLabel(r.CutType),
		QtyKg:           r.QtyKg,
		UnitPriceYuan:   r.UnitPriceYuan,
		TotalAmountYuan: r.TotalAmountYuan,
		Note:            r.Note,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}, nil
}

// cutTypeOfPurchasedSku 货号 → 部位(从该店的部位映射表反查)。
//
// ⚠️ 货号没配映射 ⇒ 400 ⇒ **不猜**(猜错部位 ⇒ 成本摊到错的部位上)。
func (s *Service) cutTypeOfPurchasedSku(ctx context.Context, branchID, sku string) (model.CutType, error) {
	cutOfSku, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return "", err
	}
	cut, ok := cutOfSku[sku]
	if !ok || cut == "" {
		return "", fmt.Errorf("%w: 货号 %s 没有配部位(鲜肉-设置里的「部位 ↔ 货号」映射里查不到)", ErrInvalidInput, sku)
	}
	return cut, nil
}

// nextExternalPurchaseSeq 当天该货号的下一个序号(主键要确定性 ⇒ 序号也得确定)。
//
// ⚠️ 日期用**区间**而不是等值:datatypes.Date 落库是"带时区的零点",
//   PG 里 `date = timestamp` 会按会话时区折算 ⇒ 等值不可靠。
func (s *Service) nextExternalPurchaseSeq(ctx context.Context, branchID string,
	startOfDay, endOfDay time.Time, sku string) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.ExternalPurchase{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ? AND cube_product_id = ?",
			branchID, startOfDay, endOfDay, sku).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("count external_purchases: %w", err)
	}
	return int(n) + 1, nil
}

// registerExternalCarryoverRecord 外采那一行**结转记录**(FIFO 队列的一员)。
//
// ⚠️ **归属快照为空** ⇒ FIFO 领用它时收入记「未归属」
//   (外采没有产出猪 ⇒ 不能摊给某头猪,见 pig_fifo.go 的期初处理)。
// ⚠️ 成本快照 = **门店给的**采购单价(不是 0,也不是倒算的移动加权单价)。
//
// ⚠️ location = **猪肉台**(2026-11-03 owner 拍板):外采录完直接上台、不占冷库
//   ⇒ 大宗从它领用时记在台面(不是冷库),见 bulk_fifo.go 的 location 判断。
func (s *Service) registerExternalCarryoverRecord(ctx context.Context, tx *gorm.DB, row model.ExternalPurchase) error {
	now := s.now()
	rec := model.CarryoverRecord{
		ID:            ExternalPurchaseCarryoverID(row.ID),
		BranchID:      row.BranchID,
		CubeProductID: row.CubeProductID,
		CutType:       row.CutType,
		QtyKg:         row.QtyKg,
		OriginDate:    row.BizDate,
		Location:      string(model.StockLocationCounter),
		Status:        CarryoverStatusActive,
		Source:        CarryoverSourceExternal,
		UnitCostYuan:  row.UnitPriceYuan,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	return s.upsertCarryoverRecord(ctx, tx, rec)
}

// loadExternalPurchaseTotals 某营业日的外采入库(按**部位**汇总:kg / 金额)。
//
// ⚠️ 这是成本链的入口(见 cut_cost.go 的注入点):
//   本期入库量 += 外采 kg;本期入库金额 += 外采金额。
//
// ⚠️ 日期用**区间**而不是等值:datatypes.Date 落库是"带时区的零点",
//   PG 里 `date = timestamp` 会按会话时区折算 ⇒ 等值不可靠。
func (s *Service) loadExternalPurchaseTotals(ctx context.Context, branchID string,
	startOfDay, endOfDay time.Time) (map[string]decimal.Decimal, map[string]decimal.Decimal, error) {
	kg := map[string]decimal.Decimal{}
	amount := map[string]decimal.Decimal{}
	var rows []model.ExternalPurchase
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, startOfDay, endOfDay).
		Find(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("query external_purchases(totals): %w", err)
	}
	for _, r := range rows {
		cut := string(r.CutType)
		if cut == "" {
			continue
		}
		kg[cut] = kg[cut].Add(r.QtyKg)
		amount[cut] = amount[cut].Add(r.TotalAmountYuan)
	}
	return kg, amount, nil
}
