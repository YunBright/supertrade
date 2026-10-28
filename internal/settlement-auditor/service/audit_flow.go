// Package service / audit_flow.go —— 机器审核「流程定制」的配置读写。
//
// # 三张配置（model/audit_flow.go）
//
//	audit_supplier_flows     供应商级流程（有结算单那条路）
//	audit_payment_item_flows 付款项级流程（无结算单那条路）
//	audit_regex_patterns     正则表达式维护（从备注里提取所属日期用）
//
// # 为什么配置存在 settlement-auditor 自己这儿
//
// 读配置的就是本服务的审核流水线（同进程同库 ⇒ 零跨服务调用），
// 而拆到别的服务会凭空多一次 dapr 往返 + 一份"配置服务挂了要不要降级"的问题。
//
// # 配置**不进 LLM 输入面**
//
// 本文件的所有内容都不会进 AuditInput ⇒ 正则、备注、配置文案都不会发给 LLM。
// 它们的唯一作用是**决定走哪条决策路径**。
package service

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 写入入参 ----

// SupplierFlowInput 是供应商流程配置的写入入参。
type SupplierFlowInput struct {
	SupplierID   string
	SupplierName string
	Mode         string
	LogicKind    string
	Enabled      bool
	Note         string
}

// ItemFlowInput 是付款项流程配置的写入入参。
type ItemFlowInput struct {
	ItemType  string
	Mode      string
	LogicKind string
	RegexID   string
	Enabled   bool
	Note      string
}

// RegexPatternInput 是正则表达式的写入入参。
type RegexPatternInput struct {
	Name       string
	Pattern    string
	GroupIndex int
	Enabled    bool
	Note       string
}

// RegexTestInput 是「拿一段样例试跑正则」的入参。
//
// ⚠️ RegexID 与 Pattern **二选一**：
// 同时给 ⇒ 以 RegexID 为准（Pattern 被忽略）——
// 因为"改一条已保存的正则然后立刻试跑"是最常见的用法。
type RegexTestInput struct {
	RegexID string
	Pattern string
	// GroupIndex 覆盖已保存正则的组号；nil = 用已保存的值。
	GroupIndex *int
	// Sample 是被测的备注文本（例："9月水电费"）。
	Sample string
}

// RegexTestResult 是试跑结果。
type RegexTestResult struct {
	Pattern string `json:"pattern"`
	Sample  string `json:"sample"`
	Matched bool   `json:"matched"`
	// Value 是取到的捕获组文本（未命中时为空串）。
	Value string `json:"value"`
	// Groups 是全部捕获组（0 = 整个匹配）。
	Groups []string `json:"groups"`
	// Month / Year 是从 Value 里解析出的**所属年/月**（0 = 没解析出来）。
	Month int `json:"month"`
	Year  int `json:"year"`
	// Error 是可读的失败原因（正则非法 / 组号越界 / 没匹配到…）。
	Error string `json:"error"`
}

// SupplierDict 取该门店的供应商字典（id → 名称）。
//
// ⚠️ 字典来自 **cube 的 supplier 字典**（duck model，不碰思迅源库 ⇒ 很便宜）：
//
//	结算单上的 supplier_id 就是 cube 口径 ⇒ 配置里的 supplier_id 必须用这里的 id，
//	手工打 id 极易打错（打成 "19" 与 "019" 是两个供应商）。
//
// ⚠️ 没有 cube 源的门店（例：门店 10）查不到 —— 那是**正常的**：
//
//	它走不到供应商分支（有结算单才有供应商）。
func (s *Service) SupplierDict(ctx context.Context, branchID string) (map[string]string, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	return s.cube.ListSupplierNames(ctx, branchID)
}

// ---- 供应商流程配置 ----

// ListSupplierFlows 列出该门店的供应商流程配置。
func (s *Service) ListSupplierFlows(ctx context.Context, branchID string) ([]model.AuditSupplierFlow, error) {
	var rows []model.AuditSupplierFlow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("supplier_id asc").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list supplier flows: %w", err)
	}
	return rows, nil
}

// CreateSupplierFlow 新建供应商流程配置。
//
// ⚠️ 同一 (branch_id, supplier_id) 只能有一条 ⇒ 重复建返 ErrAlreadyExists。
func (s *Service) CreateSupplierFlow(ctx context.Context, branchID, operator string, in SupplierFlowInput) (*model.AuditSupplierFlow, error) {
	row, err := s.buildSupplierFlow(branchID, operator, in)
	if err != nil {
		return nil, err
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.AuditSupplierFlow{}).
		Where("branch_id = ? AND supplier_id = ?", branchID, row.SupplierID).
		Count(&n).Error; err != nil {
		return nil, fmt.Errorf("count supplier flows: %w", err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: 该供应商在本门店已配置过流程", ErrAlreadyExists)
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("create supplier flow: %w", err)
	}
	return row, nil
}

// UpdateSupplierFlow 整单覆盖某条供应商流程配置。
func (s *Service) UpdateSupplierFlow(ctx context.Context, branchID, id, operator string, in SupplierFlowInput) (*model.AuditSupplierFlow, error) {
	row, err := s.buildSupplierFlow(branchID, operator, in)
	if err != nil {
		return nil, err
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.AuditSupplierFlow{}).
		Where("branch_id = ? AND supplier_id = ? AND id <> ?", branchID, row.SupplierID, id).
		Count(&n).Error; err != nil {
		return nil, fmt.Errorf("count supplier flows: %w", err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: 该供应商在本门店已配置过流程", ErrAlreadyExists)
	}
	var updated model.AuditSupplierFlow
	if err := s.db.WithContext(ctx).Model(&model.AuditSupplierFlow{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(map[string]any{
			"supplier_id": row.SupplierID, "supplier_name": row.SupplierName,
			"mode": row.Mode, "logic_kind": row.LogicKind,
			"enabled": row.Enabled, "note": row.Note, "updated_by": operator,
		}).Error; err != nil {
		return nil, fmt.Errorf("update supplier flow: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		First(&updated).Error; err != nil {
		if errorsIsRecordNotFound(err) {
			return nil, ErrAuditFlowNotFound
		}
		return nil, fmt.Errorf("reload supplier flow: %w", err)
	}
	return &updated, nil
}

// DeleteSupplierFlow 删掉一条供应商流程配置（⇒ 该供应商回到机器人审核流程）。
func (s *Service) DeleteSupplierFlow(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		Delete(&model.AuditSupplierFlow{})
	if res.Error != nil {
		return fmt.Errorf("delete supplier flow: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrAuditFlowNotFound
	}
	return nil
}

// buildSupplierFlow 校验并组装供应商流程配置行。
func (s *Service) buildSupplierFlow(branchID, operator string, in SupplierFlowInput) (*model.AuditSupplierFlow, error) {
	supplierID := strings.TrimSpace(in.SupplierID)
	if supplierID == "" {
		return nil, fmt.Errorf("%w: supplier_id 必填", ErrInvalidInput)
	}
	mode, logicKind, err := normalizeFlowMode(in.Mode, in.LogicKind, []string{model.LogicDailySingle})
	if err != nil {
		return nil, err
	}
	return &model.AuditSupplierFlow{
		ID:           uuid.NewString(),
		BranchID:     branchID,
		SupplierID:   supplierID,
		SupplierName: strings.TrimSpace(in.SupplierName),
		Mode:         mode,
		LogicKind:    logicKind,
		Enabled:      in.Enabled,
		Note:         strings.TrimSpace(in.Note),
		CreatedBy:    operator,
		UpdatedBy:    operator,
	}, nil
}

// ---- 付款项流程配置 ----

// ListItemFlows 列出该门店的付款项流程配置。
func (s *Service) ListItemFlows(ctx context.Context, branchID string) ([]model.AuditPaymentItemFlow, error) {
	var rows []model.AuditPaymentItemFlow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("item_type asc").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list item flows: %w", err)
	}
	return rows, nil
}

// CreateItemFlow 新建付款项流程配置。
func (s *Service) CreateItemFlow(ctx context.Context, branchID, operator string, in ItemFlowInput) (*model.AuditPaymentItemFlow, error) {
	row, err := s.buildItemFlow(ctx, branchID, operator, in)
	if err != nil {
		return nil, err
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.AuditPaymentItemFlow{}).
		Where("branch_id = ? AND item_type = ?", branchID, row.ItemType).
		Count(&n).Error; err != nil {
		return nil, fmt.Errorf("count item flows: %w", err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: 该付款项目在本门店已配置过流程", ErrAlreadyExists)
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("create item flow: %w", err)
	}
	return row, nil
}

// UpdateItemFlow 整单覆盖某条付款项流程配置。
func (s *Service) UpdateItemFlow(ctx context.Context, branchID, id, operator string, in ItemFlowInput) (*model.AuditPaymentItemFlow, error) {
	row, err := s.buildItemFlow(ctx, branchID, operator, in)
	if err != nil {
		return nil, err
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.AuditPaymentItemFlow{}).
		Where("branch_id = ? AND item_type = ? AND id <> ?", branchID, row.ItemType, id).
		Count(&n).Error; err != nil {
		return nil, fmt.Errorf("count item flows: %w", err)
	}
	if n > 0 {
		return nil, fmt.Errorf("%w: 该付款项目在本门店已配置过流程", ErrAlreadyExists)
	}
	if err := s.db.WithContext(ctx).Model(&model.AuditPaymentItemFlow{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(map[string]any{
			"item_type": row.ItemType, "mode": row.Mode, "logic_kind": row.LogicKind,
			"regex_id": row.RegexID, "enabled": row.Enabled, "note": row.Note,
			"updated_by": operator,
		}).Error; err != nil {
		return nil, fmt.Errorf("update item flow: %w", err)
	}
	var updated model.AuditPaymentItemFlow
	if err := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		First(&updated).Error; err != nil {
		if errorsIsRecordNotFound(err) {
			return nil, ErrAuditFlowNotFound
		}
		return nil, fmt.Errorf("reload item flow: %w", err)
	}
	return &updated, nil
}

// DeleteItemFlow 删掉一条付款项流程配置。
func (s *Service) DeleteItemFlow(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		Delete(&model.AuditPaymentItemFlow{})
	if res.Error != nil {
		return fmt.Errorf("delete item flow: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrAuditFlowNotFound
	}
	return nil
}

// buildItemFlow 校验并组装付款项流程配置行。
func (s *Service) buildItemFlow(ctx context.Context, branchID, operator string, in ItemFlowInput) (*model.AuditPaymentItemFlow, error) {
	itemType := strings.TrimSpace(in.ItemType)
	if itemType == "" {
		return nil, fmt.Errorf("%w: item_type 必填", ErrInvalidInput)
	}
	mode, logicKind, err := normalizeFlowMode(in.Mode, in.LogicKind, []string{model.LogicMonthlySingle})
	if err != nil {
		return nil, err
	}
	regexID := strings.TrimSpace(in.RegexID)
	if regexID != "" {
		// 正则必须存在且属于本门店 —— 跨门店引用会让"提取日期"用错规则。
		var re model.AuditRegexPattern
		if err := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", regexID, branchID).
			First(&re).Error; err != nil {
			if errorsIsRecordNotFound(err) {
				return nil, fmt.Errorf("%w: 正则不存在(id=%s)", ErrInvalidInput, regexID)
			}
			return nil, fmt.Errorf("load regex pattern: %w", err)
		}
	}
	return &model.AuditPaymentItemFlow{
		ID:        uuid.NewString(),
		BranchID:  branchID,
		ItemType:  itemType,
		Mode:      mode,
		LogicKind: logicKind,
		RegexID:   regexID,
		Enabled:   in.Enabled,
		Note:      strings.TrimSpace(in.Note),
		CreatedBy: operator,
		UpdatedBy: operator,
	}, nil
}

// ---- 正则表达式维护 ----

// ListRegexPatterns 列出该门店的正则。
func (s *Service) ListRegexPatterns(ctx context.Context, branchID string) ([]model.AuditRegexPattern, error) {
	var rows []model.AuditRegexPattern
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("name asc").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list regex patterns: %w", err)
	}
	return rows, nil
}

// CreateRegexPattern 新建一条正则。
func (s *Service) CreateRegexPattern(ctx context.Context, branchID, operator string, in RegexPatternInput) (*model.AuditRegexPattern, error) {
	row, err := buildRegexPattern(branchID, operator, in)
	if err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("create regex pattern: %w", err)
	}
	return row, nil
}

// UpdateRegexPattern 整单覆盖一条正则。
func (s *Service) UpdateRegexPattern(ctx context.Context, branchID, id, operator string, in RegexPatternInput) (*model.AuditRegexPattern, error) {
	row, err := buildRegexPattern(branchID, operator, in)
	if err != nil {
		return nil, err
	}
	res := s.db.WithContext(ctx).Model(&model.AuditRegexPattern{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(map[string]any{
			"name": row.Name, "pattern": row.Pattern, "group_index": row.GroupIndex,
			"enabled": row.Enabled, "note": row.Note, "updated_by": operator,
		})
	if res.Error != nil {
		return nil, fmt.Errorf("update regex pattern: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, ErrAuditFlowNotFound
	}
	var updated model.AuditRegexPattern
	if err := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		First(&updated).Error; err != nil {
		return nil, fmt.Errorf("reload regex pattern: %w", err)
	}
	return &updated, nil
}

// DeleteRegexPattern 删掉一条正则。
//
// ⚠️ 被付款项流程配置引用时**允许删除**：引用方下次提取不到日期 ⇒
//
//	「每月有且仅有一单」判为不满足（宁可不通过，也不要按错误的月份放款）。
//	但仍会在日志里留一条 warn（见 service/audit_flow_decide.go）。
func (s *Service) DeleteRegexPattern(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", id, branchID).
		Delete(&model.AuditRegexPattern{})
	if res.Error != nil {
		return fmt.Errorf("delete regex pattern: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrAuditFlowNotFound
	}
	return nil
}

// buildRegexPattern 校验并组装正则行。
func buildRegexPattern(branchID, operator string, in RegexPatternInput) (*model.AuditRegexPattern, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name 必填", ErrInvalidInput)
	}
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		return nil, fmt.Errorf("%w: pattern 必填", ErrInvalidInput)
	}
	// ⚠️ **存之前就编译一次**：正则非法不该等到"某天真的用它判一笔付款"才暴露。
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, fmt.Errorf("%w: 正则非法: %v", ErrInvalidInput, err)
	}
	if in.GroupIndex < 0 {
		return nil, fmt.Errorf("%w: group_index 不能为负", ErrInvalidInput)
	}
	return &model.AuditRegexPattern{
		ID:         uuid.NewString(),
		BranchID:   branchID,
		Name:       name,
		Pattern:    pattern,
		GroupIndex: in.GroupIndex,
		Enabled:    in.Enabled,
		Note:       strings.TrimSpace(in.Note),
		CreatedBy:  operator,
		UpdatedBy:  operator,
	}, nil
}

// TestRegex 拿一段样例试跑正则（管理后台的"正则测试"功能）。
//
// ⚠️ **不写库**：试跑是纯函数式的，只把结果回给页面。
func (s *Service) TestRegex(ctx context.Context, branchID string, in RegexTestInput) (RegexTestResult, error) {
	pattern := strings.TrimSpace(in.Pattern)
	groupIndex := 1
	if in.RegexID != "" {
		var re model.AuditRegexPattern
		if err := s.db.WithContext(ctx).Where("id = ? AND branch_id = ?", in.RegexID, branchID).
			First(&re).Error; err != nil {
			if errorsIsRecordNotFound(err) {
				return RegexTestResult{}, fmt.Errorf("%w: 正则不存在(id=%s)", ErrInvalidInput, in.RegexID)
			}
			return RegexTestResult{}, fmt.Errorf("load regex pattern: %w", err)
		}
		pattern = re.Pattern
		groupIndex = re.GroupIndex
	}
	if in.GroupIndex != nil {
		groupIndex = *in.GroupIndex
	}
	res := RegexTestResult{Pattern: pattern, Sample: in.Sample, Groups: []string{}}
	if pattern == "" {
		res.Error = "没有可试跑的正则"
		return res, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		res.Error = "正则非法: " + err.Error()
		return res, nil
	}
	m := re.FindStringSubmatch(in.Sample)
	if m == nil {
		res.Error = "没有匹配到"
		return res, nil
	}
	res.Matched = true
	res.Groups = m
	if groupIndex >= len(m) {
		// ⚠️ 组号越界**明确报错**而不是静默返回空串 ——
		// "正则写了但组号填错"是最常见的配置错误，必须当场看得见。
		res.Error = fmt.Sprintf("捕获组 %d 越界（这条正则只有 %d 组）", groupIndex, len(m)-1)
		return res, nil
	}
	res.Value = m[groupIndex]
	year, month, ok := parsePeriodText(res.Value, time.Now().Year())
	if ok {
		res.Year = year
		res.Month = month
	}
	return res, nil
}

// parsePeriodText 从提取出的文本里解析出**所属年/月**。
//
// 支持的写法（用户口径：例 "9月水电费" 里的 "9月"）：
//
//	"9"        → 9 月（年取 defaultYear）
//	"9月"      → 9 月
//	"2026年9月" → 2026 年 9 月
//	"2026-09"  → 2026 年 9 月
//
// ⚠️ 年份**只认 4 位数**：2 位数（"26年9月"）不猜 ——
//
//	猜错年份会让"每月一单"落到错误的月份窗口上，而那个错误是静默的。
func parsePeriodText(text string, defaultYear int) (int, int, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return 0, 0, false
	}
	year, month := 0, 0
	// 年：4 位数 + 年/月/-
	if m := regexp.MustCompile(`(\d{4})\s*[年/\-]`).FindStringSubmatch(t); m != nil {
		if y, err := strconv.Atoi(m[1]); err == nil && y >= 1970 && y <= 2999 {
			year = y
		}
	}
	// 月：1~2 位数 + 月
	if m := regexp.MustCompile(`(\d{1,2})\s*月`).FindStringSubmatch(t); m != nil {
		if mo, err := strconv.Atoi(m[1]); err == nil && mo >= 1 && mo <= 12 {
			month = mo
		}
	}
	if month == 0 {
		// 没有"月"字 ⇒ 试 "2026-09" 这种连写，以及纯数字。
		if m := regexp.MustCompile(`\d{4}\D(\d{1,2})(?:\D|$)`).FindStringSubmatch(t); m != nil {
			if mo, err := strconv.Atoi(m[1]); err == nil && mo >= 1 && mo <= 12 {
				month = mo
			}
		}
	}
	if month == 0 {
		// 纯数字（"9"）⇒ 当月份。
		if re := regexp.MustCompile(`^\s*(\d{1,2})\s*$`); re.MatchString(t) {
			if m := re.FindStringSubmatch(t); m != nil {
				if mo, err := strconv.Atoi(m[1]); err == nil && mo >= 1 && mo <= 12 {
					month = mo
				}
			}
		}
	}
	if month == 0 {
		return 0, 0, false
	}
	if year == 0 {
		year = defaultYear
	}
	return year, month, true
}

// ---- 共用 ----

// normalizeFlowMode 校验 mode / logic_kind 的组合。
//
// allowedLogics 是**该分支允许的逻辑条件**白名单：
//
//	供应商分支 → daily_single
//	付款项分支 → monthly_single
//
// ⚠️ 白名单是**按分支**给的，不是全局的 ——
//
//	把 monthly_single 配到供应商上（或反之）会让"逻辑判断"跑错条件的语义，
//	而它不会报错，只会在某天悄悄按错误的条件放款。
func normalizeFlowMode(mode, logicKind string, allowedLogics []string) (string, string, error) {
	m := strings.TrimSpace(mode)
	switch m {
	case model.FlowModeAutoPass:
		// 直接通过不需要逻辑条件 ⇒ logic_kind 清空（避免留一条误导性的旧值）。
		return model.FlowModeAutoPass, "", nil
	case model.FlowModeLogic:
		lg := strings.TrimSpace(logicKind)
		if lg == "" {
			return "", "", fmt.Errorf("%w: mode=logic 时必须指定 logic_kind", ErrInvalidInput)
		}
		ok := false
		for _, a := range allowedLogics {
			if lg == a {
				ok = true
				break
			}
		}
		if !ok {
			return "", "", fmt.Errorf("%w: 该分支不支持逻辑条件 %q(可选: %s)",
				ErrInvalidInput, lg, strings.Join(allowedLogics, ", "))
		}
		return model.FlowModeLogic, lg, nil
	case "":
		return "", "", fmt.Errorf("%w: mode 必填(auto_pass 或 logic)", ErrInvalidInput)
	default:
		return "", "", fmt.Errorf("%w: mode 只能是 auto_pass 或 logic(实际 %q)", ErrInvalidInput, m)
	}
}

// errorsIsRecordNotFound 判 gorm.ErrRecordNotFound（包一层，单测里好替换）。
func errorsIsRecordNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound
}

// sortStrings 按字典序排（列配置时按名字排，页面稳定）。
func sortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
