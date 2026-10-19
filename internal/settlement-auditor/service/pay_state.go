// Package service / pay_state.go —— 结算单付款状态台账。
//
// 业务问题(2026-10-15):付款审批功能上线之前,门店已经在思迅里付掉大量供应商
// 货款。发起审批页从 cube 拉「已审核 DP/CP 结款单」时会把这些单一起列出来,
// 店长勾选时根本分不清哪张还欠着钱。
//
// 实测结论决定了本文件的设计(见 model.SettlementPayState 的注释):
//   - 两个思迅源库都没有可用的付款标记;
//   - supertrade 没有写回思迅的通路(无 SQL Server 驱动,cube 只读)。
//
// 所以"这张单付过没有"只能落在 supertrade PG 的 settlement_pay_states。
// 状态有两个来源(set_method 区分),入口就是本文件的 ApplySettlementInit 与
// markApprovalSettlementsPaid:
//
//	init     —— 管理员按筛选条件批量补录历史已付款(初始化记账)
//	approval —— 审批终审通过时由流水线写入
//
// # 筛选条件由服务端解析,不由客户端传 id 列表
//
// 一次可能涉及上千张单;客户端只能看到自己加载过的那几页。把 id 列表当参数
// 传过来,"全选搜索结果"就退化成"全选已加载的页",而且前端分页一变大就漏。
// 所以执行端收的是**筛选条件**,由服务端解析全集 —— 代价是必须如实告知
// 调用方全集有多大、是否触到拉取上限(见 SettlementSetPreview.Truncated)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// settlementInClauseChunk 是拼 IN 子句时每批的 id 数。
//
// PostgreSQL 对单个 IN 列表的参数个数有上限,而全集一次可能到几千;
// 分批让"id 数量"与"查询能不能发出去"解耦。
const settlementInClauseChunk = 500

// payStateUpsertBatch 是台账批量写入的单批行数。
const payStateUpsertBatch = 200

// payStateSampleRows 是预览/执行回显里带多少行样本。
const payStateSampleRows = 20

// ErrResultTruncated 表示筛选结果触到了 cube 拉取上限,可能不完整。
//
// 与 ErrInvalidInput 分开:调用方**收窄筛选条件后就能继续**,这不是请求写错了。
// 批量写入尤其不能在这种状态下执行 —— 页面说"将影响 800 张",实际只有 500 张
// 参与了集合运算,剩下 300 张既没被标记也没被跳过,事后谁都对不上账。
var ErrResultTruncated = errors.New("service: 筛选结果达到拉取上限,可能不完整,请收窄筛选条件")

// SettlementFilter 是结算单集合的筛选条件(预览与执行共用同一个解析器)。
//
// 语义与"传一批 id"最重要的区别:同一组条件在 preview 与 apply 里解析出的
// 集合必须一致 —— 所以两端都走 ResolveSettlementSet,不允许 apply 再收窄。
type SettlementFilter struct {
	// From / To 是**账期结束日**(period_end)闭开区间 [From, To),
	// 传空串表示不限。
	//
	// ⚠️ 2026-10-19 更正:这段注释原先写的是"与 GET cube/settlements 的
	// settled_at 过滤同一口径" —— 后半句没错,但前半句是错的:当时的实现
	// 真的把 from/to 打在 settled_at 上,而 settled_at 是**结算日**,
	// 另一个列(源列 settle_date,period_end 则是 end_date)。
	// ysx 源库 4379 张已审核结款单里 settled_at 只有 1 张有值,
	// 所以那次筛选恒返 0 张 —— 日期范围在页面上等于完全不起作用。
	// 现已改回 period_end,见 DaprCubeQuerier.ListAuditedSettlements 的注释。
	From string
	To   string
	// SupplierID 精确匹配供应商。
	SupplierID string
	// Query 在结算单号与供应商 id 上做包含匹配(大小写不敏感)。
	// cube 的 /query 协议只有精确算子,包含匹配只能在 Go 侧做。
	Query string
	// DocumentType 空串 = DP + CP(采购结款单);否则只取指定类型。
	DocumentType string
	// PayStatus:unpaid(默认,只看还没付的)/ paid / all。
	// 这是台账过滤,与其他条件正交。
	PayStatus string
	// Ids 是**显式勾选**的单号列表。非空时集合 = 筛选条件命中的集合 ∩ Ids。
	//
	// 为什么需要它:"勾选这几张"和"全选搜索结果"是两个不同的意图 ——
	// 前者要精确到张,后者要覆盖整个筛选结果。客户端只传条件时无法表达前者,
	// 于是界面上会出现一个"勾了但不起作用"的复选框,比没有更糟。
	//
	// IdSource 让客户端说清这批 id 是怎么来的(勾选 / 全选搜索结果),
	// 只进日志与审计备注,不影响集合计算。
	Ids []string
	// Limit 限制**返回给客户端展示**的行数;0 = 不限。
	// 它不影响 preview 的 Count / TotalGoodsYuan,也不影响 apply 的落库集合。
	Limit int
}

// normalize 收敛大小写与空白,并把非法取值变成显式错误。
func (f *SettlementFilter) normalize() error {
	f.From = strings.TrimSpace(f.From)
	f.To = strings.TrimSpace(f.To)
	f.SupplierID = strings.TrimSpace(f.SupplierID)
	f.Query = strings.TrimSpace(f.Query)
	f.DocumentType = strings.ToUpper(strings.TrimSpace(f.DocumentType))
	switch strings.ToLower(strings.TrimSpace(f.PayStatus)) {
	case "", "unpaid":
		f.PayStatus = model.PayStatusUnpaid
	case "paid":
		f.PayStatus = model.PayStatusPaid
	case "all":
		f.PayStatus = "all"
	default:
		return fmt.Errorf("%w: pay_status 只能是 unpaid / paid / all,收到 %q", ErrInvalidInput, f.PayStatus)
	}
	if f.DocumentType != "" && f.DocumentType != "DP" && f.DocumentType != "CP" {
		return fmt.Errorf("%w: document_type 只能是空 / DP / CP,收到 %q", ErrInvalidInput, f.DocumentType)
	}
	if f.From != "" && len(f.From) != 10 {
		return fmt.Errorf("%w: from 必须是 YYYY-MM-DD", ErrInvalidInput)
	}
	if f.To != "" && len(f.To) != 10 {
		return fmt.Errorf("%w: to 必须是 YYYY-MM-DD", ErrInvalidInput)
	}
	if f.Limit < 0 {
		return fmt.Errorf("%w: limit 不能为负", ErrInvalidInput)
	}
	// 去空白 + 去重,但**不**剔除空串之外的"查不到"的 id:
	// 客户端勾了 800 张而 cube 只回 760 张这件事,必须由 Truncated / matched
	// 如实报出去,而不是在这里悄悄少算几张。
	seen := make(map[string]bool, len(f.Ids))
	ids := make([]string, 0, len(f.Ids))
	for _, id := range f.Ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	f.Ids = ids
	return nil
}

// SettlementSet 是筛选条件解析出的结算单集合(尚未套用 PayStatus 台账过滤)。
type SettlementSet struct {
	// Rows 是从 cube 拉到的、已审核的采购结款单。
	Rows []SettlementRow
	// Truncated 为 true 表示 cube 回的行数触到了拉取上限,结果**可能不完整**。
	// cube 的 /query 没有 COUNT,判据只能是"取回来的行数 == 上限"。
	Truncated bool
	// FetchCap 是本次使用的拉取上限,随响应回给调用方以便定位。
	FetchCap int
	// Requested 是客户端显式勾选的张数(非勾选模式为 0)。
	Requested int
	// Unmatched = Requested - len(Rows):勾了但查不到的张数。
	Unmatched int
}

// SettlementSetPreview 是"本次会改动什么"的回显(不落库)。
//
// 前端必须先拿它弹确认框(本次将影响 N 张、合计 ¥X),再调执行端 ——
// 集合由服务端解析,调用方不 preview 就等于闭眼点确认。
type SettlementSetPreview struct {
	BranchID string `json:"branch_id"`
	// Count 是筛选条件解析出、且符合 PayStatus 的张数。
	Count int `json:"count"`
	// TotalGoodsYuan 是这 Count 张的货款净额合计(元,2 位小数字符串)。
	TotalGoodsYuan string `json:"total_goods_yuan"`
	// AlreadyPaid 是其中已被台账记为已付款的张数。
	AlreadyPaid int `json:"already_paid"`
	// ByApproval 是其中**由正常审批流程**付掉的张数。
	//
	// 执行时这些单会被跳过(初始化不得覆盖审批留下的状态),所以这个数
	// 必须提前告知,否则"标了 800 张只生效 760 张"会被当成丢数据。
	ByApproval int `json:"by_approval"`
	// Truncated / FetchCap 见 SettlementSet。
	Truncated bool `json:"truncated"`
	FetchCap  int  `json:"fetch_cap"`
	// Requested / Unmatched 见 SettlementSet。Unmatched > 0 表示"你勾的那些单里
	// 有几张在源库里查不到",不报出来就会变成静默少处理。
	Requested int `json:"requested"`
	Unmatched int `json:"unmatched"`
	// Sample 是前若干行样本,供确认框展示"长这样"。
	Sample []SettlementRow `json:"sample"`
}

// SettlementInitResult 是 ApplySettlementInit / RevertSettlementInit 的执行结果。
type SettlementInitResult struct {
	BranchID string `json:"branch_id"`
	// Matched 是筛选条件解析出的总张数(含被跳过的)。
	Matched int `json:"matched"`
	// Changed 是真正写入台账的张数。
	Changed int `json:"changed"`
	// AlreadyPaid 是命中集合里已被记为已付款的张数(执行时不会被改动)。
	AlreadyPaid int `json:"already_paid"`
	// ByApproval 是 AlreadyPaid 里由正常审批流程付掉的那部分。
	ByApproval int `json:"by_approval"`
	// SkippedNonInit 是撤销时跳过的张数:不是初始化补录的(审批流程或无记录),
	// 撤销不对它们下手。
	SkippedNonInit int    `json:"skipped_non_init"`
	TotalGoodsYuan string `json:"total_goods_yuan"`
	Truncated      bool   `json:"truncated"`
	FetchCap       int    `json:"fetch_cap"`
	// Requested / Unmatched 见 SettlementSet。
	Requested int `json:"requested"`
	Unmatched int `json:"unmatched"`
}

// PayStateOperator 是"谁来改的状态"的入参。
type PayStateOperator struct {
	UserID string
	Name   string
}

func (o PayStateOperator) valid() bool { return strings.TrimSpace(o.UserID) != "" }

// ResolveSettlementSet 按筛选条件解析结算单全集(不套台账过滤)。
//
// 这是预览与执行**唯一**的集合解析器 —— 两端必须走同一个函数,否则
// "预览说 800 张、执行落 760 张"这种差异只可能来自两边算法漂移。
func (s *Service) ResolveSettlementSet(ctx context.Context, branchID string, f SettlementFilter) (*SettlementSet, error) {
	if err := f.normalize(); err != nil {
		return nil, err
	}
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	// 拉满 cap 而不是拉 Limit:Limit 只管展示,集合本身要全。
	rows, err := s.cube.ListAuditedSettlements(ctx, branchID,
		f.From, f.To, f.SupplierID, cubeSettlementFetchCap)
	if err != nil {
		return nil, err
	}
	out := make([]SettlementRow, 0, len(rows))
	// pick 是筛选判据,**全程只读**;seen 单独用于去重计数。
	//
	// 早先图省事用了同一个 map + delete,结果第一行命中后 map 被清空,
	// 后续行的 `len(pick) > 0` 变成 false —— 筛选直接失效,后面所有单都进来了。
	// 这类 bug 只在"勾选数少于总数"时才显形,恰好是最常见的用法。
	pick := make(map[string]bool, len(f.Ids))
	for _, id := range f.Ids {
		pick[id] = true
	}
	seen := make(map[string]bool, len(f.Ids))
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			continue
		}
		if len(pick) > 0 {
			if !pick[id] {
				continue
			}
			// 源库可能同号多行(跨单据类型),去重后才算"命中了几张单"。
			seen[id] = true
		}
		if f.DocumentType != "" && strings.TrimSpace(r.DocumentType) != f.DocumentType {
			continue
		}
		if f.Query != "" && !settlementRowMatches(r, f.Query) {
			continue
		}
		out = append(out, r)
	}
	unmatched := 0
	if len(f.Ids) > 0 {
		unmatched = len(pick) - len(seen)
	}
	return &SettlementSet{
		Rows:      out,
		Truncated: len(rows) >= cubeSettlementFetchCap,
		FetchCap:  cubeSettlementFetchCap,
		// Requested 是客户端显式勾选的张数;Unmatched 是其中在 cube 里查不到的。
		// 不报出来的话,"勾了 800 张、只生效 760 张"就变成静默丢单。
		Requested: len(f.Ids),
		Unmatched: unmatched,
	}, nil
}

// settlementRowMatches 判断结算单是否命中关键字(单号或供应商 id,忽略大小写)。
func settlementRowMatches(r SettlementRow, query string) bool {
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(strings.TrimSpace(r.ID)), q) {
		return true
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.SupplierID)), q)
}

// payStatesOf 读候选 id 在台账里的现状(台账里没有的单不返回)。
func (s *Service) payStatesOf(ctx context.Context, branchID string, ids []string) (map[string]model.SettlementPayState, error) {
	out := make(map[string]model.SettlementPayState, len(ids))
	for start := 0; start < len(ids); start += settlementInClauseChunk {
		end := start + settlementInClauseChunk
		if end > len(ids) {
			end = len(ids)
		}
		var rows []model.SettlementPayState
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND settlement_id IN ?", branchID, ids[start:end]).
			Find(&rows).Error; err != nil {
			return nil, fmt.Errorf("read pay states: %w", err)
		}
		for _, r := range rows {
			out[r.SettlementID] = r
		}
	}
	return out, nil
}

// payStats 统计候选集合里的已付款张数,以及其中由正常审批付掉的张数。
func payStats(rows []SettlementRow, states map[string]model.SettlementPayState) (alreadyPaid, byApproval int) {
	for _, r := range rows {
		st, ok := states[strings.TrimSpace(r.ID)]
		if !ok || !st.IsPaid() {
			continue
		}
		alreadyPaid++
		if st.SetMethod == model.PayMethodApproval {
			byApproval++
		}
	}
	return alreadyPaid, byApproval
}

// sumGoods 求货款净额合计。
func sumGoods(rows []SettlementRow) decimal.Decimal {
	total := decimal.Zero
	for _, r := range rows {
		total = total.Add(r.TotalGoodsYuan)
	}
	return total
}

// sampleRows 只取前若干行做回显样本(不修改入参)。
func sampleRows(rows []SettlementRow) []SettlementRow {
	if len(rows) > payStateSampleRows {
		return rows[:payStateSampleRows]
	}
	return rows
}

// PreviewSettlementInit 解析筛选条件并回显影响面,不写任何数据。
func (s *Service) PreviewSettlementInit(ctx context.Context, branchID string, f SettlementFilter) (*SettlementSetPreview, error) {
	set, err := s.ResolveSettlementSet(ctx, branchID, f)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(set.Rows))
	for _, r := range set.Rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	states, err := s.payStatesOf(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	rows := set.Rows
	if f.PayStatus != "all" {
		rows = make([]SettlementRow, 0, len(set.Rows))
		for _, r := range set.Rows {
			st, ok := states[strings.TrimSpace(r.ID)]
			isPaid := ok && st.IsPaid()
			if (f.PayStatus == model.PayStatusPaid) != isPaid {
				continue
			}
			rows = append(rows, r)
		}
	}
	alreadyPaid, byApproval := payStats(rows, states)
	sample := rows
	if f.Limit > 0 && len(sample) > f.Limit {
		sample = sample[:f.Limit]
	}
	return &SettlementSetPreview{
		BranchID:       branchID,
		Count:          len(rows),
		TotalGoodsYuan: sumGoods(rows).StringFixed(2),
		AlreadyPaid:    alreadyPaid,
		ByApproval:     byApproval,
		Truncated:      set.Truncated,
		FetchCap:       set.FetchCap,
		Requested:      set.Requested,
		Unmatched:      set.Unmatched,
		Sample:         sample,
	}, nil
}

// ApplySettlementInit 把筛选条件命中的结算单批量记为"已付款",设置方式=初始化。
//
// 四条硬规则:
//
//  1. **集合不完整就拒绝执行**(Truncated → ErrResultTruncated)。
//     预览回显的张数与实际落库的张数必须一致,否则台账与对账单永久对不上。
//  2. **只作用于还没付的单**。已经是 paid 的(不管哪种方式)一律跳过并计数,
//     重复点执行是幂等的,不会把设置方式改写成 init。
//  3. **设置方式是常量 init,不是入参**。客户端没有、也不该有指定
//     set_method 的自由 —— 否则"与正常审批流程区分"就变成了客户端说了算。
//  4. **设置人是当前登录管理员**,从入参 operator 取,服务端不猜。
func (s *Service) ApplySettlementInit(ctx context.Context, branchID string, f SettlementFilter, operator PayStateOperator, note string) (*SettlementInitResult, error) {
	if !operator.valid() {
		return nil, fmt.Errorf("%w: 缺少设置人(operator user id)", ErrInvalidInput)
	}
	set, err := s.ResolveSettlementSet(ctx, branchID, f)
	if err != nil {
		return nil, err
	}
	if set.Truncated {
		return nil, ErrResultTruncated
	}
	ids := make([]string, 0, len(set.Rows))
	for _, r := range set.Rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	states, err := s.payStatesOf(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}

	now := s.now()
	note = strings.TrimSpace(note)
	payload := make([]model.SettlementPayState, 0, len(set.Rows))
	var targets []SettlementRow
	for _, r := range set.Rows {
		id := strings.TrimSpace(r.ID)
		if st, ok := states[id]; ok && st.IsPaid() {
			// 已是已付款(含初始化补录过的)→ 不重复写入,也不改写设置方式。
			continue
		}
		targets = append(targets, r)
		payload = append(payload, model.SettlementPayState{
			ID:              uuid.NewString(),
			BranchID:        branchID,
			SettlementID:    id,
			SupplierID:      strings.TrimSpace(r.SupplierID),
			DocumentType:    strings.TrimSpace(r.DocumentType),
			PaidStatus:      model.PayStatusPaid,
			SetMethod:       model.PayMethodInit,
			OperatorID:      operator.UserID,
			OperatorName:    operator.Name,
			GoodsAmountYuan: r.TotalGoodsYuan,
			Note:            truncate(note, 512),
			PaidAt:          &now,
		})
	}

	if len(payload) > 0 {
		if err := s.db.WithContext(ctx).Clauses(payStateUpsertClause(nil)).CreateInBatches(&payload, payStateUpsertBatch).Error; err != nil {
			return nil, fmt.Errorf("apply settlement init: %w", err)
		}
	}

	alreadyPaid, byApproval := payStats(set.Rows, states)
	s.logger.Info("settlement init applied",
		"branch_id", branchID, "matched", len(set.Rows), "changed", len(payload),
		"already_paid", alreadyPaid, "by_approval", byApproval,
		"operator", operator.UserID, "method", model.PayMethodInit, "note", note)

	return &SettlementInitResult{
		BranchID:       branchID,
		Matched:        len(set.Rows),
		Changed:        len(payload),
		AlreadyPaid:    alreadyPaid,
		ByApproval:     byApproval,
		TotalGoodsYuan: sumGoods(targets).StringFixed(2),
		Truncated:      set.Truncated,
		FetchCap:       set.FetchCap,
		Requested:      set.Requested,
		Unmatched:      set.Unmatched,
	}, nil
}

// payStateUpsertClause 是台账的条件 upsert(同门店同单号只留一行)。
//
// 不覆盖 audit_id:审批流程写进来的 approval_id 是"这张单是哪次审批付的",
// 初始化补录不该把它抹成空。
func payStateUpsertClause(extra map[string]any) clause.OnConflict {
	updates := map[string]any{
		"supplier_id":       gorm.Expr("excluded.supplier_id"),
		"document_type":     gorm.Expr("excluded.document_type"),
		"set_method":        gorm.Expr("excluded.set_method"),
		"operator_id":       gorm.Expr("excluded.operator_id"),
		"operator_name":     gorm.Expr("excluded.operator_name"),
		"goods_amount_yuan": gorm.Expr("excluded.goods_amount_yuan"),
		"note":              gorm.Expr("excluded.note"),
		"reverted_at":       nil,
		"reverted_by":       "",
		"updated_at":        gorm.Expr("excluded.updated_at"),
	}
	for k, v := range extra {
		updates[k] = v
	}
	return clause.OnConflict{
		Columns:   []clause.Column{{Name: "branch_id"}, {Name: "settlement_id"}},
		DoUpdates: clause.Assignments(updates),
	}
}

// RevertSettlementInit 撤销初始化:只把 set_method=init 且仍为 paid 的单退回 unpaid。
//
// 撤销**保留行与原设置人**,另记 reverted_at / reverted_by ——
// 「谁在什么时候把哪张单补录成已付款」必须永远查得到,撤销不该抹掉它。
// 审批流程付的单(set_method=approval)不在撤销范围内。
func (s *Service) RevertSettlementInit(ctx context.Context, branchID string, f SettlementFilter, operator PayStateOperator) (*SettlementInitResult, error) {
	if !operator.valid() {
		return nil, fmt.Errorf("%w: 缺少操作人(operator user id)", ErrInvalidInput)
	}
	set, err := s.ResolveSettlementSet(ctx, branchID, f)
	if err != nil {
		return nil, err
	}
	if set.Truncated {
		return nil, ErrResultTruncated
	}
	ids := make([]string, 0, len(set.Rows))
	for _, r := range set.Rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	states, err := s.payStatesOf(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	reverted := make([]string, 0, len(states))
	for _, id := range ids {
		st, ok := states[id]
		if !ok || !st.IsPaid() || st.SetMethod != model.PayMethodInit {
			continue
		}
		reverted = append(reverted, id)
	}
	now := s.now()
	if len(reverted) > 0 {
		err := s.db.WithContext(ctx).Model(&model.SettlementPayState{}).
			Where("branch_id = ? AND settlement_id IN ? AND paid_status = ? AND set_method = ?",
				branchID, reverted, model.PayStatusPaid, model.PayMethodInit).
			Updates(map[string]any{
				"paid_status": model.PayStatusUnpaid,
				"reverted_at": now,
				"reverted_by": operator.UserID,
				"updated_at":  now,
			}).Error
		if err != nil {
			return nil, fmt.Errorf("revert settlement init: %w", err)
		}
	}
	s.logger.Info("settlement init reverted",
		"branch_id", branchID, "matched", len(set.Rows), "reverted", len(reverted),
		"operator", operator.UserID)
	return &SettlementInitResult{
		BranchID:       branchID,
		Matched:        len(set.Rows),
		Changed:        len(reverted),
		SkippedNonInit: len(set.Rows) - len(reverted),
		Truncated:      set.Truncated,
		FetchCap:       set.FetchCap,
		Requested:      set.Requested,
		Unmatched:      set.Unmatched,
	}, nil
}

// ListPayStateRecords 查台账(初始化记录审计页)。
func (s *Service) ListPayStateRecords(ctx context.Context, branchID string, method, paidStatus, supplierID string, limit, offset int) ([]model.SettlementPayState, int, error) {
	q := s.db.WithContext(ctx).Model(&model.SettlementPayState{}).Where("branch_id = ?", branchID)
	if m := strings.TrimSpace(method); m != "" && m != "all" {
		q = q.Where("set_method = ?", m)
	}
	if p := strings.TrimSpace(paidStatus); p != "" && p != "all" {
		q = q.Where("paid_status = ?", p)
	}
	if sup := strings.TrimSpace(supplierID); sup != "" {
		q = q.Where("supplier_id = ?", sup)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count pay states: %w", err)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.SettlementPayState
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("list pay states: %w", err)
	}
	return rows, int(total), nil
}

// markApprovalSettlementsPaid 审批终审通过时,把该审批勾选的结算单记为已付款。
//
// 数据取自 approval_settlements 快照(流水线的 replaceApprovalSettlements 已经
// 用 cube 数据补全了供应商/单据类型/货款净额),不再回查 cube ——
// 这条路径跑在企微回调里,多一次 live 查询就多一次失败点。
//
// 设置方式=approval(区别于管理员的初始化补录),并带 approval_id 关联回
// 这次审批。best-effort:台账写失败只 log,不改变审批实例状态 ——
// 「审批已经通过」是外部既成事实,不该因为本地台账抖动而被回滚。
func (s *Service) markApprovalSettlementsPaid(ctx context.Context, ap *model.ApprovalInstance) {
	if ap == nil {
		return
	}
	var rows []model.ApprovalSettlement
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", ap.ID).Find(&rows).Error; err != nil {
		s.logger.Error("read approval_settlements for pay ledger failed(审批状态不受影响)",
			"approval_id", ap.ID, "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	now := s.now()
	// 先读现状:**已被记为已付款的行一律不碰**。
	//
	// 这条不能靠"从 DoUpdates 里少列一列"来实现 —— OnConflict 的 DoUpdates 是
	// 行级 SQL 表达式,没法"只在 paid_status 还是 unpaid 时才改 set_method"。
	// 实测踩过:把 paid_status 从 DoUpdates 里去掉之后,初始化补录的行照样被
	// 改成了 set_method=approval,于是"这张单是上线前就付过的"变成"这次流程付的",
	// 页面上却看不出任何异常。跳过必须发生在写入**之前**。
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(r.SettlementID); id != "" {
			ids = append(ids, id)
		}
	}
	states, err := s.payStatesOf(ctx, ap.BranchID, ids)
	if err != nil {
		s.logger.Error("读付款台账失败(审批状态不受影响)", "approval_id", ap.ID, "err", err)
		return
	}
	payload := make([]model.SettlementPayState, 0, len(rows))
	skipped := 0
	for _, r := range rows {
		id := strings.TrimSpace(r.SettlementID)
		if id == "" {
			continue
		}
		if st, ok := states[id]; ok && st.IsPaid() {
			// 已付款(初始化补录或上一轮审批)→ 保留原来源,不覆盖。
			skipped++
			continue
		}
		payload = append(payload, model.SettlementPayState{
			ID:              uuid.NewString(),
			BranchID:        ap.BranchID,
			SettlementID:    id,
			SupplierID:      strings.TrimSpace(r.SupplierID),
			DocumentType:    strings.TrimSpace(r.DocumentType),
			PaidStatus:      model.PayStatusPaid,
			SetMethod:       model.PayMethodApproval,
			OperatorID:      ap.ApplicantUserID,
			ApprovalID:      ap.ID,
			GoodsAmountYuan: r.GoodsAmountYuan,
			Note:            truncate("审批终审通过:"+ap.ID, 512),
			PaidAt:          &now,
		})
	}
	if len(payload) == 0 {
		s.logger.Info("mark approval settlements paid: 全部已在台账,不改写来源",
			"approval_id", ap.ID, "skipped_paid", skipped)
		return
	}
	// 走到这里的目标行一定不是 paid(可能存在 reverted 的空行),所以复用
	// payStateUpsertClause 是安全的:审批侧要把 approval_id 一并写上。
	upsert := payStateUpsertClause(map[string]any{
		"approval_id": gorm.Expr("excluded.approval_id"),
		"paid_status": gorm.Expr("excluded.paid_status"),
	})
	if err := s.db.WithContext(ctx).Clauses(upsert).CreateInBatches(&payload, payStateUpsertBatch).Error; err != nil {
		s.logger.Error("mark approval settlements paid failed(审批状态不受影响)",
			"approval_id", ap.ID, "err", err)
		return
	}
	s.logger.Info("mark approval settlements paid",
		"approval_id", ap.ID, "branch_id", ap.BranchID, "count", len(payload),
		"method", model.PayMethodApproval)
}

// ResolveOperatorName 尽力解析设置人显示名(handler 用)。
//
// 审计的锚点是 UserID,显示名只是给人看的,所以这里**永不返回错误**:
// userinfo 不可用 / 网络抖动 / 人已被删,都只留空并 warn。
func (s *Service) ResolveOperatorName(ctx context.Context, userID string) string {
	return s.resolveOperatorName(ctx, userID)
}

// resolveOperatorName 尽力解析设置人显示名。
//
// best-effort:解析不到就留空。审计的锚点是 UserID,显示名只是给人看的,
// 为它失败一次整笔批量写入不划算。
func (s *Service) resolveOperatorName(ctx context.Context, userID string) string {
	if s.userInfo == nil || strings.TrimSpace(userID) == "" {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u, err := s.userInfo.Get(c, userID)
	if err != nil || u == nil {
		s.logger.Warn("resolve operator name failed(留空,不影响写入)", "user_id", userID, "err", err)
		return ""
	}
	return u.DisplayName
}

// ListPaidSettlementIDsOf 读给定 id 中哪些在台账里记为已付款。
func (s *Service) ListPaidSettlementIDsOf(ctx context.Context, branchID string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool)
	if len(ids) == 0 {
		return out, nil
	}
	states, err := s.payStatesOf(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	for id, st := range states {
		if st.IsPaid() {
			out[id] = true
		}
	}
	return out, nil
}

// FilterOutPaidSettlements 从结算单列表里剔除台账中已付款的单。
//
// 供 GET /settlement-approvals/cube/settlements 使用:发起审批页只应看到
// 还没付的单(初始化页要 includePaid=true 才能看到补录结果)。
//
// 返回剔除后的行与被剔除的张数 —— 剔除张数要回给前端,否则店长看到列表突然
// 少了一堆单,会以为是系统出错。
func FilterOutPaidSettlements(rows []SettlementRow, paid map[string]bool) ([]SettlementRow, int) {
	if len(paid) == 0 {
		return rows, 0
	}
	out := make([]SettlementRow, 0, len(rows))
	hidden := 0
	for _, r := range rows {
		if paid[strings.TrimSpace(r.ID)] {
			hidden++
			continue
		}
		out = append(out, r)
	}
	return out, hidden
}
