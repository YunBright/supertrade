// Package service / promotion.go —— 促销的读写。
//
// # 业务规则
//
//   - PK (id, branch_id, product_id):id 是促销单号(思迅 plan_no)
//   - 重复导入是 **upsert**(幂等)
//   - **同一商品多个促销单 ⇒ 取促销价最低的那条**
//     (与 cube promotion model 的 `min_promo_price` 语义一致:
//     「用户实际能买到的最低价」)
//   - 生效判定:`begin_at <= 基准日 <= end_at` 且 status='active'
//
// # 促销是**时效数据**:导入按基准日(as_of)过滤
//
// 思迅源表 t_pub_plan_flow 实测 31371 行,覆盖 2021-09-16 ~ 2026-10-21,
// 其中对 2026-10-21 有效的只有 741 行。
// ⇒ 导入时只写"对基准日有效"的行,历史行**不落库**;
// ⇒ overwrite 模式再把库里已有的历史行清掉(见 deleteOutOfScopePromotions)。
//
// # 金额一律 decimal,永不用 double(见 price_list.go 的说明)
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/pricing/model"
)

// PromotionInput 是促销的写入输入。
type PromotionInput struct {
	// ID 是促销单号(思迅 plan_no)。空 = 跳过该行。
	ID string
	// ProductID 是商品货号。
	ProductID string
	// PlanName 是促销单名称。
	PlanName string
	// PriceYuan 是促销价字符串(必填)。
	PriceYuan string
	// OrigPriceYuan 是原价字符串;空 = 没有原价。
	OrigPriceYuan string
	// BeginDay / EndDay 是活动起止日(ISO yyyy-mm-dd)。
	//
	// ⚠️ 用**日期**而不是时刻:价签是按天判定的,
	// 而思迅的 begin_date/end_date 带时刻(实测到 12:00AM)。
	// 统一取日期部分 ⇒ 活动当天的价签能印出来。
	BeginDay string
	EndDay   string
	// Status 是 active / inactive;空 = active。
	Status string
}

// PromotionService 是促销服务。
type PromotionService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewPromotionService 构造 PromotionService。
func NewPromotionService(db *gorm.DB) *PromotionService {
	return &PromotionService{db: db, now: func() time.Time { return time.Now() }}
}

// SetClock 注入时钟(测试用)。
func (s *PromotionService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// UpsertPromotions 幂等写入一批促销。
//
// # as_of(基准日):只写"对基准日有效"的行
//
// 促销是**时效数据**:思迅 t_pub_plan_flow 实测 31371 行
// (2021-09-16 ~ 2026-10-21),其中对 2026-10-21 有效的只有 741 行。
// ⇒ 写进库的历史行永远用不上,却让"库里躺着 3 万多条促销"变成常态,
// 而且查价时按基准日过滤后只剩几百条能用。
//
// ⇒ **过滤在服务端做**(不能指望每个调用方都记得传 as_of)。
// 无效的行计入 Skipped,并给一条 warning(不逐行报错 ——
// 3 万行历史逐行报错会把导入报告刷爆,反而没人看)。
//
// # replaceScope(overwrite 模式):清掉该门店的历史行
//
// ⚠️ 只有 mode==overwrite **且** replaceScope=true 时才清。
//
//	replaceScope 由调用方给出:"本次导入的 scope 里到底有没有 promotions"。
//	一次只导 prices 的局部导入**不能**顺手把促销清了 ——
//	症状是"同步跑完之后 POS 上的特价全没了",而报告里 promotions 一行都没有。
//
// ⚠️ **起止日期解析不出来 ⇒ 跳过该行**并记逐行错误:
//
//	促销没有起止日期就等于"永远生效",那会让一个源库的脏行
//	变成"这个商品永远在打折" ⇒ POS 印出的价低于真价。
func (s *PromotionService) UpsertPromotions(ctx context.Context, branchID, mode, asOf string,
	rows []PromotionInput, replaceScope bool) (WriteCounts, []model.ImportRowError, error) {

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
	// 基准日:空 = 今天(服务端本地日期);非法 ⇒ 报错(调用方返 400)。
	at, err := parseAsOfDay(asOf)
	if err != nil {
		return out, nil, fmt.Errorf("%w: %s", model.ErrInvalidInput, err.Error())
	}

	existing, err := s.existingKeys(ctx, branchID)
	if err != nil {
		return out, nil, err
	}

	now := s.now()
	var toInsert []model.Promotion
	var toUpdate []model.Promotion
	var rowErrs []model.ImportRowError
	outOfWindow := 0

	for _, in := range rows {
		planNo := strings.TrimSpace(in.ID)
		pid := strings.TrimSpace(in.ProductID)
		if planNo == "" || pid == "" {
			out.Skipped++
			continue
		}
		begin, err := parseISODay(in.BeginDay)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "promotions", ID: planNo + "/" + pid,
				Error: "活动起始日解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		end, err := parseISODay(in.EndDay)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "promotions", ID: planNo + "/" + pid,
				Error: "活动截止日解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		price, err := parseMoney(in.PriceYuan)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "promotions", ID: planNo + "/" + pid,
				Error: "促销价解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		if in.PriceYuan == "" {
			out.Skipped++
			continue
		}
		orig, err := parseMoneyOrZero(in.OrigPriceYuan)
		if err != nil {
			rowErrs = append(rowErrs, model.ImportRowError{
				Scope: "promotions", ID: planNo + "/" + pid,
				Error: "原价解析失败:" + err.Error(),
			})
			out.Skipped++
			continue
		}
		// ⚠️ **时效过滤**:只写"对基准日有效"的行。
		//
		//	判定与查价口径一致(ActivePromotions):begin_at <= 基准日 <= end_at。
		//	过期 / 尚未开始的行 ⇒ 跳过(计入 Skipped),**不落库**。
		//
		//	⚠️ 跳过**不是错误**:过期促销是预期内的 ⇒ 不逐行报错,
		//	只给一条汇总 warning(否则 3 万行历史会把报告刷爆)。
		if begin.After(at) || end.Before(at) {
			outOfWindow++
			out.Skipped++
			continue
		}
		status := strings.TrimSpace(in.Status)
		if status == "" {
			status = "active"
		}

		row := model.Promotion{
			ID:            planNo,
			BranchID:      branchID,
			ProductID:     pid,
			PlanName:      in.PlanName,
			PriceYuan:     price,
			OrigPriceYuan: orig,
			BeginAt:       begin,
			EndAt:         end,
			Status:        status,
			CreatedBy:     actor,
			UpdatedBy:     actor,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if existing[promoKey(planNo, branchID, pid)] {
			if mode == ModeIncremental {
				out.Skipped++
				continue
			}
			toUpdate = append(toUpdate, row)
			out.Updated++
			continue
		}
		toInsert = append(toInsert, row)
		out.Inserted++
	}

	if outOfWindow > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"促销:跳过 %d 行(对基准日 %s 无效:已过期或尚未开始)",
			outOfWindow, at.Format("2006-01-02")))
	}

	// ⚠️ 写入与清理必须在**同一个事务**里 ——
	//
	//	先写后删、中间进程被杀 ⇒ 新数据写进去了、旧的没删干净;
	//	先删后写、中间被杀 ⇒ 促销全空(客户按零售价结账)。
	//	两种都是"同步成功但数据不对",而且报告里看不出来。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(toInsert) > 0 {
			if err := tx.Create(&toInsert).Error; err != nil {
				return err
			}
		}
		if len(toUpdate) > 0 {
			// ⚠️ deleted_at 也在更新列里(软删过的行必须能复活)。
			err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "id"}, {Name: "branch_id"}, {Name: "product_id"},
				},
				DoUpdates: clause.AssignmentColumns([]string{
					"plan_name", "price_yuan", "orig_price_yuan",
					"begin_at", "end_at", "status",
					"updated_by", "updated_at", "deleted_at",
				}),
			}).Create(&toUpdate).Error
			if err != nil {
				return err
			}
		}
		if mode == ModeOverwrite && replaceScope {
			deleted, err := deleteOutOfScopePromotions(tx, branchID, at)
			if err != nil {
				return err
			}
			out.Deleted = int(deleted)
		}
		return nil
	}); err != nil {
		return out, rowErrs, err
	}
	return out, rowErrs, nil
}

// deleteOutOfScopePromotions 软删该门店**不在有效集合内**的促销行。
//
// # 判据(哪些行会被删)
//
//	branch_id = 该门店
//	AND NOT (status = 'active' AND begin_at <= 基准日 AND end_at >= 基准日)
//
// ⇒ 被删的正是**历史行**(已过期)与**尚未开始**的行。
//
// ⚠️ **只删同一 branch_id 的行** —— 别的门店不动。
// ⚠️ **只碰 promotions 表** —— price_lists 不受影响。
// ⚠️ 判定口径与查价(ActivePromotions)一致 ⇒ 被删的行本来就查不出来。
//
// ⚠️ **软删**(gorm.DeletedAt,与 price_lists 的 DeletePrice 一致):
//
//	物理行还在表里,但对所有查询不可见。
//	⇒ 想物理清除需要另开一条 purge 路径(不在本次改动范围内)。
//
// ⚠️ **不删"还有效但这次没导进来"的行** ——
//
//	删掉一条还在生效的促销 ⇒ POS 改印零售价 ⇒ 客户被多收钱。
//	留着一条过期促销 ⇒ 只是占地方,查价时按基准日过滤掉了,不会印错价。
//	⇒ 宁可多留,不可少印。
func deleteOutOfScopePromotions(tx *gorm.DB, branchID string, at time.Time) (int64, error) {
	res := tx.
		Where("branch_id = ?", branchID).
		Where("NOT (status = ? AND begin_at <= ? AND end_at >= ?)", "active", at, at).
		Delete(&model.Promotion{})
	return res.RowsAffected, res.Error
}

// ActivePromotions 取一批商品在某门店、某基准日**当前生效**的促销。
//
// ⚠️ 同一商品有多个生效促销时,返回**促销价最低**的那条
// (与 cube promotion model 的 min_promo_price 语义一致)。
//
// 返回 map[product_id]Promotion。
func (s *PromotionService) ActivePromotions(ctx context.Context, branchID string, productIDs []string, at time.Time) (map[string]model.Promotion, error) {
	out := map[string]model.Promotion{}
	if len(productIDs) == 0 {
		return out, nil
	}
	var rows []model.Promotion
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Where("product_id IN ?", productIDs).
		Where("status = ?", "active").
		Where("begin_at <= ?", at).
		Where("end_at >= ?", at).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		cur, ok := out[r.ProductID]
		if !ok || r.PriceYuan.LessThan(cur.PriceYuan) {
			out[r.ProductID] = r
		}
	}
	return out, nil
}

// ActivePromotion 取一个商品在某门店、某基准日当前生效的促销。
//
// 返回 (nil, nil) = 该商品此刻没有生效促销(绝大多数商品的常态)。
func (s *PromotionService) ActivePromotion(ctx context.Context, branchID, productID string, at time.Time) (*model.Promotion, error) {
	m, err := s.ActivePromotions(ctx, branchID, []string{productID}, at)
	if err != nil {
		return nil, err
	}
	p, ok := m[productID]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

// existingKeys 读出该门店已存在的 (plan_no, branch_id, product_id) 集合。
//
// ⚠️ **必须 Unscoped** —— 理由同 price_list.go 的同名函数:
// 软删过的行物理上还在、还占着主键,看不见它就会走 INSERT 分支撞 UNIQUE 约束。
func (s *PromotionService) existingKeys(ctx context.Context, branchID string) (map[string]bool, error) {
	type keyRow struct {
		ID        string
		BranchID  string
		ProductID string
	}
	var rows []keyRow
	err := s.db.WithContext(ctx).Unscoped().Model(&model.Promotion{}).
		Select("id", "branch_id", "product_id").
		Where("branch_id = ?", branchID).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[promoKey(r.ID, r.BranchID, r.ProductID)] = true
	}
	return out, nil
}

// promoKey 拼出促销行的唯一键。
func promoKey(id, branchID, productID string) string {
	return id + "\x00" + branchID + "\x00" + productID
}

// parseISODay 解析 "yyyy-mm-dd" → 当天 00:00:00(本地时区)。
//
// ⚠️ **只接受 ISO 日期** —— 思迅的 begin_date/end_date 是 datetime,
//
//	但导出侧已经用 CONVERT(varchar(10), ..., 23) 转成 "2026-07-01"
//	(cube promotion model 的 begin_day / end_day 维度)。
//	拿到别的格式(比如 "Jul  1 2026 12:00AM")时**必须报错**,
//	绝不能"尽力解析"成一个大概的日期 ——
//	那会让促销在错误的日期生效,印出低于真价的价签。
func parseISODay(s string) (time.Time, error) {
	t := strings.TrimSpace(s)
	if len(t) != 10 || t[4] != '-' || t[7] != '-' {
		return time.Time{}, fmt.Errorf("%q 不是 yyyy-mm-dd", s)
	}
	for i, c := range t {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return time.Time{}, fmt.Errorf("%q 不是 yyyy-mm-dd", s)
		}
	}
	return time.ParseInLocation("2006-01-02", t, time.Local)
}

// parseAsOfDay 解析基准日 "yyyy-mm-dd" → 当天 00:00:00(本地时区)。
//
// ⚠️ 空 = 今天(服务端本地日期)—— 与 archivedto.NormalizeAsOf 同一口径。
// ⚠️ 非法格式 ⇒ 报错(调用方返 400)—— 绝不"尽力解析"。
func parseAsOfDay(s string) (time.Time, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		now := time.Now()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
	}
	return parseISODay(t)
}
