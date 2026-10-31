// Package service / rules.go —— 规则配置（FR-16）。
//
// 规则存 `recon_rules`：`branch_id=”` 是全局默认值，门店行覆盖全局行。
//
// # 为什么"未配置"要给默认值，而"配错了"要报错
//
//	未配置  ⇒ 正常状态，用代码里的默认口径（出纳不配也能跑）
//	配错了  ⇒ 读配置时直接报错（fail-closed），不静默回落默认值
//
// 静默回落最坏：出纳以为改过的口径生效了，而系统仍按旧口径算。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// 规则键（与计划 §6 的默认值表一致）。
const (
	RulePaymentMethodMap          = "payment_method_map"
	RuleChannelFeeRate            = "channel_fee_rate"
	RuleArrivalToleranceDays      = "arrival_tolerance_days"
	RuleCashVarianceToleranceYuan = "cash_variance_tolerance_yuan"
	RuleCashSourceBranchNos       = "cash_source_branch_nos"
)

// KnownRuleKeys 是允许写入的规则键集合。
var KnownRuleKeys = []string{
	RulePaymentMethodMap,
	RuleChannelFeeRate,
	RuleArrivalToleranceDays,
	RuleCashVarianceToleranceYuan,
	RuleCashSourceBranchNos,
}

// standardMethods 是合法的标准支付方式（映射值只能落在这里）。
var standardMethods = map[string]bool{
	model.MethodCash:        true,
	model.MethodWechat:      true,
	model.MethodBankCard:    true,
	model.MethodStoredValue: true,
	model.MethodCredit:      true,
	model.MethodChange:      true,
	model.MethodUnmapped:    true,
}

// Rules 是解析后的规则集（默认值 + 全局行 + 门店行合并）。
type Rules struct {
	// MethodMap 是 `payment_method_map` 的门店覆盖项（默认项见 defaultMethodMap）。
	MethodMap map[string]string
	// ArrivalToleranceDays 是 T+N 到账窗口（1~7，引擎再归一）。
	ArrivalToleranceDays int
	// CashVarianceTolerance 是长短款容差（元）。
	CashVarianceTolerance decimal.Decimal
	// ChannelFeeRate 是渠道费率（仅展示/预估用，实际以银行流水手续费为准）。
	ChannelFeeRate map[string]decimal.Decimal
	// CashSourceBranchNos 非空时按源分店号过滤收银流水。
	CashSourceBranchNos []string
}

// DefaultRules 用代码默认口径构造规则集。
func (s *Service) DefaultRules() Rules {
	return Rules{
		MethodMap:             map[string]string{},
		ArrivalToleranceDays:  s.cfg.DefaultToleranceDays,
		CashVarianceTolerance: s.cfg.DefaultCashTolerance,
		ChannelFeeRate:        map[string]decimal.Decimal{},
		CashSourceBranchNos:   nil,
	}
}

// LoadRules 合并全局行与门店行；未配置的键用默认值。
//
// ⚠️ 已配置但解析失败 ⇒ **报错**（不静默用默认值）。
func (s *Service) LoadRules(ctx context.Context, branchID string) (Rules, error) {
	rs := s.DefaultRules()
	if branchID == "" {
		return rs, nil
	}
	var rows []model.ReconRule
	err := s.db.WithContext(ctx).
		Where("branch_id IN ?", []string{"", branchID}).
		Find(&rows).Error
	if err != nil {
		return Rules{}, err
	}
	// 全局先行、门店覆盖 ⇒ 按 branch_id 是否为空排序后再 apply。
	for pass := 0; pass < 2; pass++ {
		want := branchID
		if pass == 0 {
			want = ""
		}
		for _, r := range rows {
			if strings.TrimSpace(r.BranchID) != want {
				continue
			}
			if err := applyRule(&rs, r.RuleKey, r.ValueJSON); err != nil {
				return Rules{}, fmt.Errorf("cash-recon: 规则 %s 配置非法: %w", r.RuleKey, err)
			}
		}
	}
	return rs, nil
}

// applyRule 把一条规则值并入 Rules。
func applyRule(rs *Rules, key, raw string) error {
	switch key {
	case RulePaymentMethodMap:
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		if rs.MethodMap == nil {
			rs.MethodMap = map[string]string{}
		}
		for k, v := range m {
			k = strings.ToUpper(strings.TrimSpace(k))
			v = strings.TrimSpace(v)
			if !standardMethods[v] {
				return fmt.Errorf("不支持的 method %q", v)
			}
			rs.MethodMap[k] = v
		}
	case RuleArrivalToleranceDays:
		var n int
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			return err
		}
		if n < 0 || n > 7 {
			return fmt.Errorf("到账窗口必须在 0~7 天, got %d", n)
		}
		rs.ArrivalToleranceDays = n
	case RuleCashVarianceToleranceYuan:
		d, err := decimal.NewFromString(strings.Trim(strings.TrimSpace(raw), `"`))
		if err != nil {
			return err
		}
		if d.IsNegative() {
			return errors.New("长短款容差不能为负")
		}
		rs.CashVarianceTolerance = d
	case RuleChannelFeeRate:
		var m map[string]json.Number
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return err
		}
		if rs.ChannelFeeRate == nil {
			rs.ChannelFeeRate = map[string]decimal.Decimal{}
		}
		for k, v := range m {
			d, err := decimal.NewFromString(v.String())
			if err != nil {
				return err
			}
			if d.IsNegative() || d.GreaterThan(decimal.NewFromInt(1)) {
				return fmt.Errorf("渠道费率 %s 必须在 0~1, got %s", k, d)
			}
			rs.ChannelFeeRate[strings.TrimSpace(k)] = d
		}
	case RuleCashSourceBranchNos:
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return err
		}
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
		rs.CashSourceBranchNos = out
	default:
		return fmt.Errorf("未知规则键 %q", key)
	}
	return nil
}

// normalizeRuleValue 校验并规范化一条规则值（写入前调用）。
func normalizeRuleValue(key, raw string) (string, error) {
	rs := Rules{}
	if err := applyRule(&rs, key, raw); err != nil {
		return "", err
	}
	switch key {
	case RulePaymentMethodMap:
		b, err := json.Marshal(rs.MethodMap)
		return string(b), err
	case RuleChannelFeeRate:
		b, err := json.Marshal(rs.ChannelFeeRate)
		return string(b), err
	case RuleCashSourceBranchNos:
		b, err := json.Marshal(rs.CashSourceBranchNos)
		return string(b), err
	default:
		// 数值型原样（去空白）。
		return strings.TrimSpace(raw), nil
	}
}

// SaveRule 写入/覆盖门店规则（upsert）。
func (s *Service) SaveRule(ctx context.Context, branchID, actor, key, valueJSON string) error {
	if branchID == "" || key == "" {
		return model.ErrInvalidInput
	}
	if !knownRuleKey(key) {
		return fmt.Errorf("%w: 未知规则键 %q", model.ErrInvalidInput, key)
	}
	normalized, err := normalizeRuleValue(key, valueJSON)
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrInvalidInput, err)
	}
	now := s.now()
	row := model.ReconRule{
		ID:        newID(),
		BranchID:  branchID,
		RuleKey:   key,
		ValueJSON: normalized,
		UpdatedBy: actor,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err = s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}, {Name: "rule_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"value_json": normalized,
			"updated_by": actor,
			"updated_at": now,
		}),
	}).Create(&row).Error
	if err != nil {
		return err
	}
	return s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   branchID,
		ObjectType: "rule",
		ObjectID:   key,
		Action:     "update",
		AfterJSON:  normalized,
		Actor:      actor,
		CreatedAt:  now,
	})
}

// EffectiveRules 返回「键 → 生效值」的 JSON 友好视图（GET /rules 用）。
func (s *Service) EffectiveRules(ctx context.Context, branchID string) (map[string]any, error) {
	rs, err := s.LoadRules(ctx, branchID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		RulePaymentMethodMap:          methodMap(rs),
		RuleArrivalToleranceDays:      rs.ArrivalToleranceDays,
		RuleCashVarianceToleranceYuan: rs.CashVarianceTolerance.String(),
		RuleChannelFeeRate:            rs.ChannelFeeRate,
		RuleCashSourceBranchNos:       append([]string{}, rs.CashSourceBranchNos...),
	}, nil
}

// knownRuleKey 判断规则键是否允许写入。
func knownRuleKey(key string) bool {
	for _, k := range KnownRuleKeys {
		if k == key {
			return true
		}
	}
	return false
}

// writeAudit 写一条审计日志（FR-11，append-only）。
//
// 失败**不阻断**主流程：审计写不进去是日志设施问题，
// 不该让"出纳保存不了期初"这种业务动作连带失败。
func (s *Service) writeAudit(ctx context.Context, entry model.ReconAuditLog) error {
	if entry.ID == "" {
		entry.ID = newID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = s.now()
	}
	err := s.db.WithContext(ctx).Create(&entry).Error
	if err != nil {
		s.cfg.Logger.Warn("cash-recon: 审计日志写入失败", "object_type", entry.ObjectType,
			"object_id", entry.ObjectID, "action", entry.Action, "err", err)
	}
	return nil
}

// newID 生成业务主键（uuid v4）。
//
// 放在 service 里是为了让单测能替换（见 NewWithConfig 的 Clock 同理）。
var newID = func() string {
	return uuidV4()
}
