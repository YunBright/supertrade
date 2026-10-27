package service

// internal/fresh-meat/service/biz_audit.go —— **审计结转**(2026-10-24 门店口径)。
//
// # 门店口径(原话)
//
// 「审计则进行结转,审计指定截止日期;审计前,所有数据从上期结转开始,实时计算,
//   不快照,任何日期补录都会导致后续日期的数据发生变化;审计后,产生结转(即快照),
//   审计截止日期和之前的都不能再修改(包括补录);审计条件是截止日期和之前的每日
//   日终盘点都要有,否则不予以审计。」
//
// # 三段语义
//
//	审计前: **实时算**(余额链不快照)⇒ 补录往日 ⇒ **后续日期的毛利跟着变**
//	        (这是特性,不是 bug —— 门店口径就是要"整条时间线"跟着修正)。
//	审计时: 截止日期及之前的**每日日终盘点必须齐全**⇒ 否则**拒绝**(列出缺哪几天);
//	        通过 ⇒ **落快照(=结转)**:把每一天的结余与成本固化成快照。
//	审计后: 截止日期及之前**冻结** ⇒ 不能再改(补录 / 改搬台 / 撤搬台 / 改大宗都不行)。
//
// # 为什么这样设计(它解决了什么)
//
// ① **性能**:余额链只从**上次审计的快照**往后推 ⇒ 链的长度 = 两次审计之间的天数
//    (而不是开业以来所有天)⇒ 实时算的成本有上界。
// ② **维护不积压**:审计前门店把该补的补完(系统实时重算,毛利永远是对的),
//    审计后冻结 ⇒ 门店不用一直惦记"哪天还没补" ⇒ 维护工作量有终点。
// ③ **快照是"冻结产物"而不是"缓存"**:审计后那些天再也不会变 ⇒ 不存在
//    "缓存失效要重算"的问题(这正是"实时算 + 随时快照"的老毛病)。
//
// # 冻结的实现
//
// 冻结 = **写操作前查审计状态**:任何作用在「截止日期及之前」的写入
// (盘点补录 / 搬台增改删 / 大宗结算 / 期初结转 / 进猪)⇒ 400 并说明已冻结。
// 读不受影响(快照照读)。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BizAuditID 审计记录的**确定性**代理主键。
func BizAuditID(branchID, cutoffDate string) string {
	return surrogateID("BA", branchID, cutoffDate)
}

// BizAuditView 审计结转的一行(对外 JSON)。
type BizAuditView struct {
	BranchID string `json:"branch_id"`
	// CutoffDate 是审计截止日("YYYY-MM-DD")。
	CutoffDate string `json:"cutoff_date"`
	// AuditedAt 是审计时间。
	AuditedAt time.Time `json:"audited_at"`
	OperatorID string  `json:"operator_id"`
	Note       string  `json:"note,omitempty"`
}

// BizAuditInput POST /stock/audit 入参。
type BizAuditInput struct {
	// CutoffDate 是审计截止日("YYYY-MM-DD")。
	CutoffDate string `json:"cutoff_date" binding:"required"`
	Note       string `json:"note,omitempty"`
}

// BizAuditStatus 某店的审计状态(对外 JSON)。
type BizAuditStatus struct {
	BranchID string `json:"branch_id"`
	// LastCutoffDate 是最近一次审计的截止日(空 = 还没审计过 ⇒ 全程实时算)。
	LastCutoffDate string `json:"last_cutoff_date,omitempty"`
	// Audited 是不是已经审计过。
	Audited bool `json:"audited"`
	// Audits 是历次审计(按截止日升序)。
	Audits []BizAuditView `json:"audits"`
	// FrozenBefore 是"截止日及之前不能再改"的提示文案。
	FrozenBefore string `json:"frozen_before,omitempty"`
	// Presale 是最近一次审计的**预期毛利(预售结转)**结果(nil = 那次审计没做结转)。
	Presale *PresaleView `json:"presale,omitempty"`
	// BalanceDiffs 是**审计结存差异表**(按部位)。
	//
	// ⚠️ **只列审计时"被强制改了数量"的部位**(目前只有负库存清零);
	//   没有这种改动时**整个字段不出现**(门店口径:「如没有则不出现审计结存差异表」)。
	BalanceDiffs []BalanceDiffRow `json:"balance_diffs,omitempty"`
	// Replay 是**本次**审计的"重放截止日之后的动作 + 校准当前余额"结果
	// (只在 POST /stock/audit 的响应里给;读端点不返)。
	Replay *AuditReplayResult `json:"replay,omitempty"`
}

// AuditReplayResult 审计后的"重放 + 校准"结果(对外 JSON)。
type AuditReplayResult struct {
	// CutoffDate 是审计截止日。
	CutoffDate string `json:"cutoff_date"`
	// ReplayedDays 是**截止日之后**被重算的营业日数(新口径生效的天数)。
	ReplayedDays int `json:"replayed_days"`
	// ReplayedKg 是截止日之后的动作涉及的重量(搬台 + 大宗出入库)。
	ReplayedKg decimal.Decimal `json:"replayed_kg"`
	// ReplayedCounterLoadKg 是**审计后的搬台**重量(它们一行不丢地参与了后续计算)。
	ReplayedCounterLoadKg decimal.Decimal `json:"replayed_counter_load_kg"`
	// Calibrations 是当前余额的校准明细(per SKU)。
	Calibrations []StockCalibrationRow `json:"calibrations,omitempty"`
	// Note 是给门店看的说明。
	Note string `json:"note,omitempty"`
}

// BalanceDiffRow 审计结存差异表的一行(按部位)。
//
// # 什么时候出现
//
// 审计对**结存数量**做过强制修改时才有:
//
//	负库存清零   —— 该部位结存是负数 ⇒ 审计时清零,**不带给下一期**;
//	历史差额校准 —— 位置表(当前余额)与按营业日推导的结余对不上 ⇒ 校准到审计口径。
//
// ⇒ 普通的、没有被动过的部位**不进这张表**。
type BalanceDiffRow struct {
	CutType   model.CutType `json:"cut_type"`
	CutLabel  string        `json:"cut_label"`
	// BeforeKg 是审计前的结存(kg)。
	BeforeKg decimal.Decimal `json:"before_kg"`
	// AfterKg 是审计后的结存(kg)。
	AfterKg decimal.Decimal `json:"after_kg"`
	// DiffKg 是差异(= AfterKg − BeforeKg)。
	DiffKg decimal.Decimal `json:"diff_kg"`
	// Reason:negative_zero(负库存清零)| history_calibration(历史差额校准)。
	Reason string `json:"reason"`
	// WriteoffAmountYuan 是这次清零对应的**核销金额**(元)。
	//
	// ⚠️ 负库存意味着"搬台超过了实际库存"⇒ 那部分成本在清零前被当成
	//   **已卖出的成本**计进了当期损益 ⇒ 清零 = 把它放回当期毛利。
	//   ⇒ 门店对账时用这个金额解释"审计当天毛利的变化"。
	WriteoffAmountYuan decimal.Decimal `json:"writeoff_amount_yuan,omitempty"`
	// Note 是给门店看的一句话。
	Note string `json:"note,omitempty"`
}

// 审计结存差异的原因。
const (
	// BalanceDiffReasonNegativeZero 负库存在审计时清零(不带给下一期)。
	BalanceDiffReasonNegativeZero = "negative_zero"
	// BalanceDiffReasonHistoryCalibration 位置表与按营业日推导的结余对不上 ⇒ 校准。
	BalanceDiffReasonHistoryCalibration = "history_calibration"
)

// StockCalibrationRow 一个 SKU 的余额校准明细(对外 JSON)。
type StockCalibrationRow struct {
	CubeProductID string        `json:"cube_product_id"`
	CutType       model.CutType `json:"cut_type"`
	// BeforeKg 是校准前的账面冷库余额。
	BeforeKg decimal.Decimal `json:"before_kg"`
	// AfterKg 是校准后的冷库余额(= 截止日结余 + 截止日之后的动作重放)。
	AfterKg decimal.Decimal `json:"after_kg"`
	// DeltaKg 是校准差额(= AfterKg − BeforeKg)。
	DeltaKg decimal.Decimal `json:"delta_kg"`
}

// BizAuditPrecheck 审计**前**的预检结果(对外 JSON)。
type BizAuditPrecheck struct {
	BranchID string `json:"branch_id"`
	// CutoffDate 是预检的截止日。
	CutoffDate string `json:"cutoff_date"`
	// Ready 表示条件是否满足(截止日合法 + 每日日终盘点齐全)。
	Ready bool `json:"ready"`
	// Error 是条件不满足的原因(Ready=false 时有值)。
	Error string `json:"error,omitempty"`
	// MissingStocktakeDays 是缺完整日终盘点的日子。
	MissingStocktakeDays []string `json:"missing_stocktake_days,omitempty"`
	// HasBalance 表示这次审计**会产生结余**(有没有结余 = 有没有预售结转)。
	HasBalance bool `json:"has_balance"`
	// Presale 是这次审计**将会**产生的预期毛利(预售结转)测算。
	Presale *PresaleView `json:"presale,omitempty"`
}

// lastAudit 取该店最近一次审计(按截止日倒序第一条)。
func (s *Service) lastAudit(ctx context.Context, branchID string) (*model.BizAudit, error) {
	var a model.BizAudit
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cutoff_date DESC").
		First(&a).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query biz_audits: %w", err)
	}
	return &a, nil
}

// auditCutoff 该店已冻结的截止日(空字符串 = 没冻结过)。
func (s *Service) auditCutoff(ctx context.Context, branchID string) (string, error) {
	a, err := s.lastAudit(ctx, branchID)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", nil
	}
	return time.Time(a.CutoffDate).In(s.bizTZOrUTC()).Format("2006-01-02"), nil
}

// assertNotAudited 冻结检查:作用在「截止日及之前」的写入 ⇒ 400。
//
// ⚠️ 这是"审计后不能再改(含补录)"的执行点 —— 所有会写**历史营业日**的入口都要调它。
func (s *Service) assertNotAudited(ctx context.Context, branchID string, day time.Time, what string) error {
	cutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return err
	}
	if cutoff == "" {
		return nil
	}
	key := day.In(s.bizTZOrUTC()).Format("2006-01-02")
	if key > cutoff {
		return nil
	}
	return fmt.Errorf("%w: %s 已审计结转(截止日 %s),不能再修改(含补录)", ErrInvalidInput, key, cutoff)
}

// GetBizAuditStatus 查某店的审计状态。
func (s *Service) GetBizAuditStatus(ctx context.Context, branchID string) (*BizAuditStatus, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.BizAudit
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cutoff_date ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query biz_audits: %w", err)
	}
	out := &BizAuditStatus{BranchID: branchID, Audits: []BizAuditView{}}
	for _, a := range rows {
		out.Audits = append(out.Audits, BizAuditView{
			BranchID:   a.BranchID,
			CutoffDate: time.Time(a.CutoffDate).In(s.bizTZOrUTC()).Format("2006-01-02"),
			AuditedAt:  a.AuditedAt,
			OperatorID: a.OperatorID,
			Note:       a.Note,
		})
	}
	if len(out.Audits) > 0 {
		last := out.Audits[len(out.Audits)-1]
		out.LastCutoffDate = last.CutoffDate
		out.Audited = true
		out.FrozenBefore = last.CutoffDate + " 及之前已结转冻结"
		// 最近一次审计的**预期毛利(预售结转)**结果。
		if pv, err := s.ListPresale(ctx, branchID,
			mustParseDateInLoc(last.CutoffDate, s.bizTZOrUTC())); err == nil && len(pv.Rows) > 0 {
			out.Presale = pv
		}
	}
	return out, nil
}

// PrecheckBizAudit 审计**前**的预检(GET /stock/audit/precheck)。
//
// # 为什么要有预检(而不是让门店直接按确认)
//
// 审计是**不可逆**的一刀(截止日及之前全部冻结)⇒ 门店在按下确认之前
// 应当看到"这一刀下去会发生什么":
//  ① 条件是否满足(缺哪几天的日终盘点);
//  ② 本次会产生的**结余**与**预期毛利(预售结转)**测算;
//  ③ 审计提示文案(门店原话)。
func (s *Service) PrecheckBizAudit(ctx context.Context, branchID string, cutoffDate string) (*BizAuditPrecheck, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	out := &BizAuditPrecheck{BranchID: branchID, CutoffDate: cutoffDate}
	cutoff, err := time.ParseInLocation("2006-01-02", cutoffDate, loc)
	if err != nil {
		out.Error = fmt.Sprintf("cutoff_date 必须是 YYYY-MM-DD(收到 %q)", cutoffDate)
		return out, nil
	}
	todayKey := s.now().In(loc).Format("2006-01-02")
	if cutoffDate > todayKey {
		out.Error = fmt.Sprintf("审计截止日 %s 不能是未来", cutoffDate)
		return out, nil
	}
	lastCutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if lastCutoff != "" && cutoffDate <= lastCutoff {
		out.Error = fmt.Sprintf("审计截止日 %s 必须晚于上次审计的截止日 %s", cutoffDate, lastCutoff)
		return out, nil
	}
	// 截止日之前必须**已经有营业数据** ⇒ 否则审计是空转(冻结了一个什么都没发生的日子)。
	firstDay, err := s.firstBizDay(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if firstDay == "" || cutoffDate < firstDay {
		out.Error = fmt.Sprintf("该店最早的营业数据是 %s,截止日 %s 之前没有任何数据", firstDay, cutoffDate)
		return out, nil
	}
	missing, err := s.missingStocktakeDays(ctx, branchID, lastCutoff, cutoffDate)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		out.MissingStocktakeDays = missing
		shown := missing
		if len(shown) > 10 {
			shown = shown[:10]
		}
		out.Error = fmt.Sprintf("审计截止日 %s 之前还有 %d 天没有完整的日终盘点(%s…),补齐后才能审计结转",
			cutoffDate, len(missing), joinDates(shown))
		return out, nil
	}
	out.Ready = true
	// 结余与预期毛利测算(不落库)。
	plan, err := s.buildPresalePlan(ctx, branchID, cutoff)
	if err != nil {
		return nil, err
	}
	if plan != nil && len(plan.Rows) > 0 {
		out.HasBalance = true
		out.Presale = plan
	}
	return out, nil
}

// AuditBusinessDays 审计结转(POST /stock/audit)。
//
// # 条件(门店口径)
//
// 「审计条件是截止日期和之前的每日日终盘点都要有,否则不予以审计。」
//
// ⇒ 检查范围 = **上次审计截止日 + 1** ~ 本次截止日(第一次审计 = 开业首日 ~ 截止日);
// ⇒ 缺哪几天就报哪几天(门店照着补)。
//
// # 做了什么
//
//  1. 校验截止日(≤ 今天、> 上次审计截止日);
//  2. 校验区间内**每一天都有完整的日终盘点**(is_complete=true)⇒ 缺 ⇒ 400;
//  3. 重推余额链 + 重算这些天的**成本日快照**;
//  4. 把这些天的**结余快照**(sku_stock_daily)落库(= 结转);
//  5. 写审计记录 ⇒ 该截止日及之前**冻结**。
func (s *Service) AuditBusinessDays(ctx context.Context, branchID string, in BizAuditInput, operatorID string) (*BizAuditStatus, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	now := s.now()
	loc := s.bizTZOrUTC()
	cutoff, err := time.ParseInLocation("2006-01-02", in.CutoffDate, loc)
	if err != nil {
		return nil, fmt.Errorf("%w: cutoff_date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, in.CutoffDate)
	}
	today := now.In(loc)
	todayKey := today.Format("2006-01-02")
	cutoffKey := cutoff.Format("2006-01-02")
	if cutoffKey > todayKey {
		return nil, fmt.Errorf("%w: 审计截止日 %s 不能是未来", ErrInvalidInput, cutoffKey)
	}

	// 上次审计截止日(往后审计才有效)。
	lastCutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if lastCutoff != "" && cutoffKey < lastCutoff {
		return nil, fmt.Errorf("%w: 审计截止日 %s 必须晚于上次审计的截止日 %s", ErrInvalidInput, cutoffKey, lastCutoff)
	}
	if lastCutoff != "" && cutoffKey == lastCutoff {
		// ⚠️ **幂等**:同一个截止日重复审计 ⇒ 直接返回**已经完成的结转结果**。
		//
		// 门店在手机上点了两次(网络慢 / 不确定有没有成功)是很常见的 ⇒
		// 第二次报"必须晚于上次审计的截止日"会让人以为审计失败了。
		//
		// ⇒ 同一个截止日 ⇒ 返回上一次的结果(不重复结转、不重复冻结)。
		return s.GetBizAuditStatus(ctx, branchID)
	}

	// ---- 条件:截止日之前必须有营业数据 + 每一天都要有**完整的日终盘点** ----
	firstDay, err := s.firstBizDay(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if firstDay == "" || cutoffKey < firstDay {
		return nil, fmt.Errorf("%w: 该店最早的营业数据是 %s,截止日 %s 之前没有任何数据",
			ErrInvalidInput, firstDay, cutoffKey)
	}
	missing, err := s.missingStocktakeDays(ctx, branchID, lastCutoff, cutoffKey)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		shown := missing
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return nil, fmt.Errorf("%w: 审计截止日 %s 之前还有 %d 天没有完整的日终盘点(%s…),补齐后才能审计结转",
			ErrInvalidInput, cutoffKey, len(missing), joinDates(shown))
	}

	// ---- 结转:重算成本日快照 + 落按营业日的结余快照(= 冻结产物)----
	//
	// ⚠️ 重算范围 = **上次审计之后**到本次截止日(上次已冻结的不用重算)。
	from := cutoff
	if lastCutoff != "" {
		from = mustParseDateInLoc(lastCutoff, loc).AddDate(0, 0, 1)
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, from); err != nil {
		return nil, fmt.Errorf("结转重算成本快照: %w", err)
	}
	if err := s.materializeDailyBalances(ctx, branchID, cutoff); err != nil {
		return nil, fmt.Errorf("结转落结余快照: %w", err)
	}

	// ---- 预期毛利(预售结转):把结余按当前售价结转掉 ----
	//
	// ⇒ 本期毛利含进这笔预期毛利;下一期期初单价改成预期售价。
	//
	// ⚠️ **取价服务不可用 ⇒ 审计直接失败**(不冻结):
	//   审计是不可逆的一刀 ⇒ 预售没算出来就冻结,门店再也补不了
	//   ⇒ 等价格服务恢复后重试,好过带着"少算的毛利"把数据定死。
	//   (某个 SKU **没维护售价**是另一回事:业务事实 ⇒ 跳过该 SKU 并列出来。)
	plan, err := s.buildPresalePlan(ctx, branchID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("结转测算预期毛利: %w", err)
	}
	if plan != nil && len(plan.Rows) > 0 {
		if err := s.settlePresale(ctx, branchID, cutoff, plan); err != nil {
			return nil, fmt.Errorf("结转预期毛利(预售): %w", err)
		}
		// ⚠️ 预售结转后,截止日的**结转金额**变了(改成**预期售价口径**)
		//   ⇒ 重落那天的成本日快照 ⇒ 下一期的期初单价 = 预期销售单价
		//   ⇒ 两期总毛利 = 实际毛利(不多算、不少算)。
		if _, err := s.persistCutCostDaily(ctx, branchID, cutoff, false); err != nil {
			return nil, fmt.Errorf("结转重落成本快照(预售口径): %w", err)
		}
	}

	// ---- 负库存清零:不把负数带给下一期 ----
	//
	// 门店口径(2026-10-24):「负库存部分要在审计的时候清零,不带给下一期」
	// ⇒ 截止日的结余快照里**负的**部位 ⇒ 置 0;
	// ⇒ 该部位在**成本日快照**里的负期末(kg/金额)也置 0
	//   (否则下一期的期初金额会带着负数进去 = 变相带给下一期);
	// ⇒ 改动记进**审计结存差异表**(门店要看到"哪些数量被强制改了")。
	//
	// ⚠️ 必须发生在**写审计记录之前**:链的基准就是这张快照 ⇒ 先清零,
	//   下一期才是从 0 起算(而不是从 −5 起算)。
	balanceDiffs, err := s.zeroNegativeBalances(ctx, branchID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("结转清理负库存: %w", err)
	}

	// ---- 写审计记录(冻结)----
	rec := model.BizAudit{
		ID:         BizAuditID(branchID, cutoffKey),
		BranchID:   branchID,
		CutoffDate: datatypesDate(cutoff),
		AuditedAt:  now,
		OperatorID: operatorID,
		Note:       in.Note,
		CreatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"audited_at":  rec.AuditedAt,
			"operator_id": rec.OperatorID,
			"note":        rec.Note,
		}),
	}).Create(&rec).Error; err != nil {
		return nil, fmt.Errorf("upsert biz_audit: %w", err)
	}

	// ---- 重放:把**截止日之后的动作**重放一遍 + 校准当前余额 ----
	//
	// 审计把截止日及之前冻结了 ⇒ 基准变成"截止日的结余快照";
	// 截止日之后的搬台/大宗/盘点**一行不丢**地重放一遍
	// ⇒ 后续那些天的毛利按新口径(期初 = 预期售价)重算。
	//
	// ⇒ 然后把**当前余额**(位置表)校准到"截止日结余 + 重放后的结果"
	//   —— 否则补录往日盘点留下的历史差额会一直挂在库存查询里。
	replay, err := s.replayAfterAudit(ctx, branchID, cutoff)
	if err != nil {
		s.pubLogger.Error("审计重放失败(余额未校准)",
			"branch_id", branchID, "cutoff_date", cutoffKey, "err", err)
		replay = nil
	}

	status, err := s.GetBizAuditStatus(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if plan != nil && len(plan.Rows) > 0 {
		status.Presale = plan
	}
	if len(balanceDiffs) > 0 {
		status.BalanceDiffs = balanceDiffs
	}
	status.Replay = replay
	return status, nil
}

// zeroNegativeBalances 审计时把**负库存**清零(**不带给下一期**)。
//
// # 改三处
//
// ① 截止日的**结余快照**(cut_stock_daily):cold_store_kg < 0 的部位 ⇒ 置 0;
// ② 同一营业日的**成本日快照**(sku_cost_daily):closing_kg / closing_amount < 0 的行 ⇒ 置 0
//   (下一期的期初金额从这张表读 ⇒ 不置 0 就等于把负数带进下一期);
// ③ 返回**审计结存差异表**(按部位):改了哪些数量、从多少改到多少、为什么改。
//
// ⚠️ 负库存本身是**账面问题**(搬台超过了实际库存)⇒ 清零意味着
//   这批不存在的货在**审计当期**被核销(成本进当期损益)⇒ 门店看得见这笔损失。
func (s *Service) zeroNegativeBalances(ctx context.Context, branchID string, cutoff time.Time) ([]BalanceDiffRow, error) {
	start, end := s.businessDayBounds(cutoff)
	var snaps []model.CutStockDaily
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Find(&snaps).Error; err != nil {
		return nil, fmt.Errorf("query cut_stock_daily(zero): %w", err)
	}
	out := []BalanceDiffRow{}
	now := s.now()
	for _, sn := range snaps {
		if !sn.ColdStoreKg.IsNegative() {
			continue
		}
		before := sn.ColdStoreKg
		rec := sn
		rec.ColdStoreKg = decimal.Zero
		rec.TotalKg = rec.CounterKg.Add(decimal.Zero)
		rec.UpdatedAt = now
		if err := s.upsertCutStockDaily(ctx, &rec); err != nil {
			return nil, err
		}
		// 成本日快照里这个部位的负期末也置 0。
		//
		// ⚠️ 2026-10-27:成本链改**部位级** ⇒ 查 `cut_cost_daily`(按部位),
		//   不再按 SKU 查 `sku_cost_daily`(那张表已停写)。
		writeoff := decimal.Zero
		var neg []model.CutCostDaily
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND cut_type = ? AND biz_date >= ? AND biz_date < ?",
				branchID, sn.CutType, start, end).
			Where("closing_kg < 0 OR closing_amount < 0").
			Find(&neg).Error; err != nil {
			return nil, fmt.Errorf("query cut_cost_daily(zero %s): %w", sn.CutType, err)
		}
		for _, n := range neg {
			writeoff = writeoff.Add(n.ClosingAmount.Abs())
		}
		if err := s.db.WithContext(ctx).Model(&model.CutCostDaily{}).
			Where("branch_id = ? AND cut_type = ? AND biz_date >= ? AND biz_date < ?",
				branchID, sn.CutType, start, end).
			Where("closing_kg < 0 OR closing_amount < 0").
			Updates(map[string]any{
				"closing_kg":     decimal.Zero,
				"closing_amount": decimal.Zero,
			}).Error; err != nil {
			return nil, fmt.Errorf("update cut_cost_daily(zero %s): %w", sn.CutType, err)
		}
		out = append(out, BalanceDiffRow{
			CutType:            sn.CutType,
			CutLabel:           model.CutLabel(sn.CutType),
			BeforeKg:           before,
			AfterKg:            decimal.Zero,
			DiffKg:             decimal.Zero.Sub(before),
			Reason:             BalanceDiffReasonNegativeZero,
			WriteoffAmountYuan: writeoff,
			Note:               "负库存在审计时清零(不带给下一期);这批不存在的货在审计当期核销。",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CutType < out[j].CutType })
	return out, nil
}

// replayAfterAudit 审计后的"重放 + 校准"。
//
// # ① 重放(成本口径)
//
// 截止日之后的营业日的**成本日快照**按新口径重算:
// 它们的期初 = 截止日的期末(已被预售结转改成**预期售价口径**)
// ⇒ 「下一期期初的该部位单价 = 预期销售单价」这条口径链式生效。
//
// # ② 校准(数量口径)
//
// 当前余额(位置表) := 截止日的结余快照 + 截止日之后的动作(搬台/大宗/盘点结转)
// ⇒ 补录往日盘点造成的历史差额被**一次性校准掉**
// ⇒ 库存查询与每日毛利看到的余额一致(黄字告警不再挂着)。
//
// ⚠️ **柜台余额不动**:今天搬上台的货是今天的动作,不受历史结转影响。
func (s *Service) replayAfterAudit(ctx context.Context, branchID string, cutoff time.Time) (*AuditReplayResult, error) {
	loc := s.bizTZOrUTC()
	cutoffKey := cutoff.In(loc).Format("2006-01-02")
	out := &AuditReplayResult{
		CutoffDate: cutoffKey,
		Note: "截止日之后的动作已按新口径重放;当前余额已校准到「截止日结余 + 重放后的结果」。",
	}
	// ---- ① 重放:重算截止日之后的成本日快照 ----
	chain, err := s.deriveColdChain(ctx, branchID, "", nil)
	if err != nil {
		return nil, err
	}
	for d := range chain {
		if d > cutoffKey {
			out.ReplayedDays++
		}
	}
	if err := s.recomputeCostSnapshots(ctx, branchID, cutoff.AddDate(0, 0, 1)); err != nil {
		return nil, fmt.Errorf("重放成本日快照: %w", err)
	}
	// ---- 截止日之后的搬台/大宗重量(给门店看"一行不丢")----
	terms, err := s.loadDailyTerms(ctx, branchID)
	if err != nil {
		return nil, err
	}
	for d, t := range terms {
		if d <= cutoffKey {
			continue
		}
		for _, v := range t.counterLoad {
			out.ReplayedCounterLoadKg = out.ReplayedCounterLoadKg.Add(v)
		}
		for _, v := range t.bulkOut {
			out.ReplayedKg = out.ReplayedKg.Add(v)
		}
		for _, v := range t.bulkIn {
			out.ReplayedKg = out.ReplayedKg.Add(v)
		}
	}
	// ---- ② 校准当前余额 ----
	rows, err := s.calibrateCurrentBalance(ctx, branchID, cutoff, chain)
	if err != nil {
		return nil, err
	}
	out.Calibrations = rows
	return out, nil
}

// calibrateCurrentBalance 把当前余额(位置表)校准到"截止日结余 + 重放后的结果"。
//
// # 当前余额 = 链上**最后一天**的值
//
// 审计之后,链的基准就是"截止日的结余快照",链上只覆盖**截止日之后**的日子:
//
//   - 链上有日子 ⇒ 最后一天的值 = 当前余额(= 基准 + 之后的动作);
//   - 链上没日子(截止日就是最新的一天)⇒ 当前余额 = **基准**(截止日的结余快照)。
//
// ⚠️ 只动**冷库**:柜台是今天的动作,不受历史结转影响。
// ⚠️ **只动链上有部位的 SKU**:没有部位映射的 SKU 不在链里 ⇒ 不碰它
//   (否则会把"链外"的余额清零)。
func (s *Service) calibrateCurrentBalance(ctx context.Context, branchID string, cutoff time.Time,
	chain map[string]map[string]decimal.Decimal) ([]StockCalibrationRow, error) {
	var cutCold map[string]decimal.Decimal
	if len(chain) > 0 {
		latest := ""
		for d := range chain {
			if latest == "" || d > latest {
				latest = d
			}
		}
		cutCold = chain[latest]
	} else {
		// 链上没日子 ⇒ 当前余额 = 截止日的结余快照(= 审计基准)。
		_, cold, ok, err := s.dailyClosing(ctx, branchID, cutoff)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		cutCold = cold
	}
	cutOf, _, err := s.loadSkuCatalog(ctx, branchID, nil)
	if err != nil {
		return nil, err
	}
	_, liveCold, err := s.loadSkuPositions(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// 部位 → SKU(按当前冷库余额占比拆;见 cut_split.go 的 splitCutClosingToSkus)。
	skuCold := s.splitCutClosingToSkus(ctx, branchID, cutCold)
	rows := []StockCalibrationRow{}
	for _, sku := range sortedKeys(skuCold) {
		after := skuCold[sku]
		before := liveCold[sku]
		delta := after.Sub(before)
		if delta.IsZero() {
			continue
		}
		if err := s.addPosition(ctx, s.db, branchID, sku, model.StockLocationColdStore, delta); err != nil {
			return nil, err
		}
		rows = append(rows, StockCalibrationRow{
			CubeProductID: sku,
			CutType:       cutOf[sku],
			BeforeKg:     before,
			AfterKg:      after,
			DeltaKg:      delta,
		})
	}
	return rows, nil
}

// sortedKeys map 的 key 升序(输出稳定,便于断言)。
func sortedKeys(m map[string]decimal.Decimal) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// missingStocktakeDays 找出 (fromExclusive, toInclusive] 区间内**没有完整日终盘点**的日子。
//
// fromExclusive 空 ⇒ 从该店**第一个营业日**开始。
func (s *Service) missingStocktakeDays(ctx context.Context, branchID, fromExclusive, toInclusive string) ([]string, error) {
	// 区间内**每一天**都要有完整的日终盘点(门店口径:「审计条件是截止日期和之前的
	// 每日日终盘点都要有,否则不予以审计」+「每天都有盘点单,没有任何剩余也要
	// 创建一个空的」)。
	//
	// ⚠️ 2026-10-24 修:原来只查"**有活动**的日子"(有猪/有搬台/有盘点)
	//   ⇒ **空档日**(那天没进猪也没搬台,只是忘了盘点)根本不在检查范围里
	//   ⇒ 审计照样放行 ⇒ 余额链在那一天是断的 ⇒ 后续所有天的结余都偏。
	//   ⇒ 现在按**日历日**逐天查:从该店第一个营业日起,到截止日为止,每一天都要有。
	firstDay, err := s.firstBizDay(ctx, branchID)
	if err != nil {
		return nil, err
	}
	// 已经有完整盘点的日子。
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND is_complete = ?", branchID, true).
		Find(&takes).Error; err != nil {
		return nil, fmt.Errorf("query stocktakes(audit): %w", err)
	}
	complete := map[string]bool{}
	for _, t := range takes {
		complete[t.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02")] = true
	}
	var missing []string
	if firstDay == "" {
		return missing, nil
	}
	loc := s.bizTZOrUTC()
	start, err := time.ParseInLocation("2006-01-02", firstDay, loc)
	if err != nil {
		return nil, fmt.Errorf("parse first biz day %q: %w", firstDay, err)
	}
	end, err := time.ParseInLocation("2006-01-02", toInclusive, loc)
	if err != nil {
		return nil, fmt.Errorf("parse cutoff %q: %w", toInclusive, err)
	}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if fromExclusive != "" && key <= fromExclusive {
			continue
		}
		if !complete[key] {
			missing = append(missing, key)
		}
	}
	return missing, nil
}

// firstBizDay 该店**第一个营业日**(有猪 / 有搬台 / 有盘点的最早那天)。
//
// 空 ⇒ 该店还没有任何营业数据(此时审计没有意义,调用方另行判断)。
func (s *Service) firstBizDay(ctx context.Context, branchID string) (string, error) {
	days, err := s.bizDaysWithActivity(ctx, branchID)
	if err != nil {
		return "", err
	}
	if len(days) == 0 {
		return "", nil
	}
	return days[0], nil
}

// bizDaysWithActivity 该店有过任何营业日痕迹的日期(升序)。
func (s *Service) bizDaysWithActivity(ctx context.Context, branchID string) ([]string, error) {
	set := map[string]bool{}
	var pigs []model.WholePig
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&pigs).Error; err != nil {
		return nil, fmt.Errorf("query whole_pigs(audit): %w", err)
	}
	for _, p := range pigs {
		set[p.ArrivedAt.In(s.bizTZOrUTC()).Format("2006-01-02")] = true
	}
	var takes []model.PorkCutsStocktake
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&takes).Error; err != nil {
		return nil, fmt.Errorf("query stocktakes(audit): %w", err)
	}
	for _, t := range takes {
		set[t.TakenAt.In(s.bizTZOrUTC()).Format("2006-01-02")] = true
	}
	var loads []model.CounterLoadRecord
	if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&loads).Error; err != nil {
		return nil, fmt.Errorf("query counter_loads(audit): %w", err)
	}
	for _, l := range loads {
		set[dateKey(l.BizDate)] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// joinDates 把日期拼成可读的一串(给报错用)。
func joinDates(days []string) string {
	out := ""
	for i, d := range days {
		if i > 0 {
			out += ", "
		}
		out += d
	}
	return out
}
