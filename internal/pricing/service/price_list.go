// Package service / price_list.go —— 售价的读写。
//
// # 业务规则
//
//   - 一行 = 一个商品在一个价目表里的当前价;PK (id, branch_id, product_id)
//   - 重复导入是 **upsert**(幂等):同样的数据导两次,结果一样
//   - 软删的行在 upsert 时**复活**(deleted_at 置 NULL)——
//     否则"删过一次的价格"再也导不回来,而症状是"导入报告成功、查价却查不到"
//   - **没维护价 = 该商品没有行**,而不是"有一行、价是 0"
//     (POS 要印「未维护售价」,印 ¥0.00 会被顾客当真价)
//
// # 金额一律 decimal,永不用 double
//
// 入参用**字符串**:JSON number 到了 Dart 侧会被 jsonDecode 变成 double,
// 精度当场丢掉(9.90 → 9.899999999999999)。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/pricing/model"
)

// DefaultPriceListID 是默认价目表编码。
//
// ⚠️ 预留"会员价目表""团购价目表"这类多价目表能力,
// 但**当前只有一套** —— 门店自营 POS 的价就是这一套。
// 查价不传价目表编码时用它。
const DefaultPriceListID = "default"

// 导入模式。
const (
	// ModeIncremental 已存在则跳过(只补新的)。
	ModeIncremental = "incremental"
	// ModeOverwrite 已存在则覆盖可变列。
	ModeOverwrite = "overwrite"
)

// WriteCounts 是写入的三态计数。
type WriteCounts struct {
	Received int `json:"received"`
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Deleted  int `json:"deleted"`
	// Warnings 是**不阻断但必须让人知道**的事
	// (例如"跳过 N 行对基准日无效的促销")。
	//
	// ⚠️ 归并成一条汇总消息,而不是逐行报 —— 3 万行历史逐行报会把报告刷爆,
	// 反而没人看。
	Warnings []string `json:"warnings,omitempty"`
}

// PriceInput 是售价的写入输入。
//
// 金额是**字符串**:调用方(导入器 / HTTP 层)拿到的就是字符串,
// 中间不经过 float64 ⇒ 精度不会在中转里丢掉。
type PriceInput struct {
	// ListID 是价目表编码;空 = DefaultPriceListID。
	ListID string
	// ProductID 是商品货号。
	ProductID string
	// PriceYuan 是零售价字符串。**空 = 该商品没维护价**。
	PriceYuan string
	// MemberPriceYuan 是会员价字符串。空 = 没有会员价。
	MemberPriceYuan string
	// CostYuan 是成本价字符串。空 = 不知道成本。
	CostYuan string
	// EffectiveAt 是生效时刻;nil = 现在。
	EffectiveAt *time.Time
	// ExpiredAt 是失效时刻;nil = 长期有效。
	ExpiredAt *time.Time
	// Status 是 active / inactive;空 = active。
	Status string
}

// PriceListService 是售价服务。
type PriceListService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewPriceListService 构造 PriceListService。
func NewPriceListService(db *gorm.DB) *PriceListService {
	return &PriceListService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *PriceListService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// actor 是写入时的 created_by / updated_by。
//
// ⚠️ 导入写入的 actor 是 "import"(与 catalog 的导入一致):
// 来源不是"某个人",而是"一次导入"。
const actor = "import"

// UpsertPrices 幂等写入一批售价。
//
// mode:
//   - ModeIncremental —— 已存在**跳过**(只补新的)
//   - ModeOverwrite   —— 已存在则**覆盖可变列**
//
// ⚠️ **没维护价的行会被跳过**(不建行、也不删已有行)——
//
//	"源库没给这个商品的价"与"这个商品该被清价"是两件事,
//	混为一谈的症状是"导入后一批商品突然没价了",而报告只写"成功 N 条"。
//	真正的"清价"由独立的 DeletePrice 负责。
//
// errors 返回**逐行错误**,不中断整体 —— 一批 27565 行里有一条脏数据就整体
// 回滚,等于没导。
func (s *PriceListService) UpsertPrices(ctx context.Context, branchID, mode string, rows []PriceInput) (WriteCounts, []model.ImportRowError, error) {
	out := WriteCounts{Received: len(rows)}
	if branchID == "" {
		return out, nil, model.ErrInvalidInput
	}
	if mode == "" {
		mode = ModeIncremental
	}
	if mode != ModeIncremental && mode != ModeOverwrite {
		return out, nil, fmt.Errorf("%w: mode=%q 非法(只能是 %s / %s)",
			model.ErrInvalidInput, mode, ModeIncremental, ModeOverwrite)
	}
	if len(rows) == 0 {
		return out, nil, nil
	}

	// 先把已有的键读出来 —— 才能分清"新增"与"更新"。
	existing, err := s.existingKeys(ctx, branchID)
	if err != nil {
		return out, nil, err
	}

	now := s.now()
	var toInsert []model.PriceList
	var toUpdate []model.PriceList
	var rowErrs []model.ImportRowError

	for _, in := range rows {
		pid := strings.TrimSpace(in.ProductID)
		if pid == "" {
			out.Skipped++
			continue
		}
		price, err := parseMoney(in.PriceYuan)
		if err != nil {
			// ⚠️ 金额解析失败**不能**当成 0 —— 那会把一个脏数据变成"售价 0 元",
			// POS 会照印。必须报出来。
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "prices", ID: pid, Error: "售价解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		if in.PriceYuan == "" {
			// 没维护价 ⇒ 不建行。
			out.Skipped++
			continue
		}
		member, err := parseMoneyOrZero(in.MemberPriceYuan)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "prices", ID: pid, Error: "会员价解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		cost, err := parseMoneyOrZero(in.CostYuan)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "prices", ID: pid, Error: "成本价解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}

		listID := strings.TrimSpace(in.ListID)
		if listID == "" {
			listID = DefaultPriceListID
		}
		status := strings.TrimSpace(in.Status)
		if status == "" {
			status = "active"
		}
		effectiveAt := startOfDay(now)
		if in.EffectiveAt != nil {
			// ⚠️ **一律取当天零点** —— 显式传入的带时刻值也如此。
			//
			// 查价的基准时刻是"基准日的 00:00:00"(日粒度),
			// 若 effective_at 带时分,则同一天里"生效"与"基准"的口径不一致
			// ⇒ 当天生效的价当天查不到。
			effectiveAt = startOfDay(*in.EffectiveAt)
		}

		row := model.PriceList{
			ID:              listID,
			BranchID:        branchID,
			ProductID:       pid,
			PriceYuan:       price,
			MemberPriceYuan: member,
			CostYuan:        cost,
			EffectiveAt:     effectiveAt,
			ExpiredAt:       in.ExpiredAt,
			Status:          status,
			CreatedBy:       actor,
			UpdatedBy:       actor,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if existing[priceKey(listID, branchID, pid)] {
			if mode == ModeIncremental {
				// 增量模式:已存在就跳过。
				out.Skipped++
				continue
			}
			// 已存在 ⇒ 保留 created_by / created_at(那是审计线索)。
			row.CreatedAt = now
			toUpdate = append(toUpdate, row)
			out.Updated++
			continue
		}
		toInsert = append(toInsert, row)
		out.Inserted++
	}

	if len(toInsert) > 0 {
		if err := s.db.WithContext(ctx).Create(&toInsert).Error; err != nil {
			return out, rowErrs, err
		}
	}
	if len(toUpdate) > 0 {
		// ⚠️ **deleted_at 也在更新列里** ——
		//		否则软删过的行 upsert 后仍然是"已删除",查价永远查不到,
		// 而导入报告说"成功 N 条"。
		err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "id"}, {Name: "branch_id"}, {Name: "product_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"price_yuan", "member_price_yuan", "cost_yuan",
				"effective_at", "expired_at", "status",
				"updated_by", "updated_at", "deleted_at",
			}),
		}).Create(&toUpdate).Error
		if err != nil {
			return out, rowErrs, err
		}
	}
	return out, rowErrs, nil
}

// DeletePrice 软删一个商品在某门店的售价。
//
// ⚠️ 与"没维护价"分开:本方法表达"**这个商品不再有价**",
// 而"没维护价"只表达"源库没给"。两者混用 ⇒ 一批商品突然没价。
func (s *PriceListService) DeletePrice(ctx context.Context, branchID, listID, productID string) (bool, error) {
	if listID == "" {
		listID = DefaultPriceListID
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ? AND product_id = ?", branchID, listID, productID).
		Delete(&model.PriceList{})
	return res.RowsAffected > 0, res.Error
}

// ActivePrice 取一个商品在某门店、某时刻的**生效售价**。
//
// 返回 (nil, nil) = 该商品此刻没有生效价(没维护 / 未生效 / 已失效 / 已删)。
func (s *PriceListService) ActivePrice(ctx context.Context, branchID, listID, productID string, at time.Time) (*model.PriceList, error) {
	if listID == "" {
		listID = DefaultPriceListID
	}
	var row model.PriceList
	// ⚠️ 用 Find 而不是 First ——
	// First 在查不到时会返回 gorm.ErrRecordNotFound,而 GORM 默认会把它**打进错误日志**
	// ("record not found")。而"这个商品没维护价"是**常态**(绝大多数商品),
	// 每查一次打一条错误日志 ⇒ 日志被正常业务刷满,真故障反而看不见。
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ? AND product_id = ?", branchID, listID, productID).
		Where("status = ?", "active").
		Where("effective_at <= ?", at).
		Where("expired_at IS NULL OR expired_at >= ?", at).
		Limit(1).
		Find(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ProductID == "" {
		return nil, nil
	}
	return &row, nil
}

// ActivePrices 取一批商品在某门店、某时刻的生效售价。
//
// 返回 map[product_id]PriceList。**没有生效价的商品不在 map 里**
// (调用方据此区分"没维护价"与"售价 0 元")。
func (s *PriceListService) ActivePrices(ctx context.Context, branchID, listID string, productIDs []string, at time.Time) (map[string]model.PriceList, error) {
	out := map[string]model.PriceList{}
	if len(productIDs) == 0 {
		return out, nil
	}
	if listID == "" {
		listID = DefaultPriceListID
	}
	var rows []model.PriceList
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, listID).
		Where("product_id IN ?", productIDs).
		Where("status = ?", "active").
		Where("effective_at <= ?", at).
		Where("expired_at IS NULL OR expired_at >= ?", at).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ProductID] = r
	}
	return out, nil
}

// ActivePricesAll 取该门店某价目表在基准时刻**全部**生效的售价。
//
// # ⚠️ WHERE 条件与 ActivePrices **逐字相同**(只少了 product_id IN 过滤)
//
//	口径不能各发明一套 —— 否则"批量查价"与"导出价"对同一时刻
//	会给出不同结论(查价说没价、导出说有价 ⇒ 客户端取哪个都不对)。
//
// # 返回值是**切片**而不是 map
//
// ActivePrices 返回 map 是因为它按 product_id 查;这里要的是"全量",
// 切片保留 DB 的行序,交给调用方自己归组。
//
// ⚠️ 排序按 product_id —— 一个商品在一个价目表里**只有一行**(PK 保证)
// ⇒ 这是**全序** ⇒ 两次导出结果顺序一致(客户端拿两次导出做 diff 才靠谱)。
func (s *PriceListService) ActivePricesAll(ctx context.Context, branchID, listID string, at time.Time) ([]model.PriceList, error) {
	if listID == "" {
		listID = DefaultPriceListID
	}
	var rows []model.PriceList
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, listID).
		Where("status = ?", "active").
		Where("effective_at <= ?", at).
		Where("expired_at IS NULL OR expired_at >= ?", at).
		Order("product_id").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// HasAnyPrices 该门店本地是否有价格数据。
//
// # 这是"回落 cube"的判据
//
// 门店 00 / 01(有 cube 源)本地没有价格 ⇒ 回落 cube(过渡期)。
// 门店 10(自营,没有 cube 源)本地**有**价格 ⇒ **不回落** ——
// 此时某个商品查不到价是**数据缺口**,必须显性暴露,
// 而不是回落 cube 拿一个必然 503 的错误盖过去。
func (s *PriceListService) HasAnyPrices(ctx context.Context, branchID string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&model.PriceList{}).
		Where("branch_id = ?", branchID).
		Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// existingKeys 读出该门店已存在的 (list_id, branch_id, product_id) 集合。
//
// ⚠️ **必须 Unscoped** ——
// GORM 的软删作用域会自动加 `deleted_at IS NULL`,
// 于是软删过的行**看不见** ⇒ 判定成"不存在" ⇒ 走 INSERT 分支
// ⇒ 撞 UNIQUE 约束(软删的行物理上还在,还占着主键)。
//
// 症状是"删过一次的价格再也导不回来",而报错是 UNIQUE constraint failed ——
// 一个与真因(软删作用域)毫无关系的错误。
func (s *PriceListService) existingKeys(ctx context.Context, branchID string) (map[string]bool, error) {
	type keyRow struct {
		ID        string
		BranchID  string
		ProductID string
	}
	var rows []keyRow
	err := s.db.WithContext(ctx).Unscoped().Model(&model.PriceList{}).
		Select("id", "branch_id", "product_id").
		Where("branch_id = ?", branchID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[priceKey(r.ID, r.BranchID, r.ProductID)] = true
	}
	return out, nil
}

// priceKey 拼出售价行的唯一键。
func priceKey(listID, branchID, productID string) string {
	return listID + "\x00" + branchID + "\x00" + productID
}

// startOfDay 取某个时刻所在那天的零点。
//
// ⚠️ 价格与促销都是**日粒度**的:价签按天印,
// 查价的基准时刻也是"基准日的零点"。⇒ 生效/失效都按天判。
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// parseMoney 解析金额字符串。
//
// ⚠️ 空串**不算错**(调用方自己判"没维护价"),但**非法字符串算错** ——
//
//	非法值当 0 会把脏数据变成"售价 0 元",POS 会照印。
func parseMoney(s string) (decimal.Decimal, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%q 不是合法金额", s)
	}
	return d, nil
}

// parseMoneyOrZero 解析金额字符串,空 = 0,非法 = 错。
func parseMoneyOrZero(s string) (decimal.Decimal, error) {
	d, err := parseMoney(s)
	if err != nil {
		return decimal.Zero, err
	}
	return d, nil
}
