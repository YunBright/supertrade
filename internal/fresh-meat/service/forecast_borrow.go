// Package service / forecast_borrow.go —— **跨门店选用基线**(owner 2026-10-30 第 2 条)。
//
// # 要解决的问题
//
//	「每个门店样本要单独」⇒ 新店/小店的样本永远不够(≥5)⇒ 建议分割一直回退 LLM
//	⇒ 那是**语言先验**(通用出肉率),不是这家店的真实出肉率。
//
//	⇒ owner 的选择:**整店借用 + 可按分层覆盖** ——
//	  本店某个分层自己不够用时,**借**别店那一层的数字来用。
//
// # 三种粒度(weight_band / cut_type 都空 = 整店借用)
//
//	| weight_band | cut_type | 含义                   | 优先级 |
//	|-------------|----------|------------------------|--------|
//	| 都空        | 都空     | 整店借用               | 3(最低)|
//	| 有          | 都空     | 该体重段的**全部**部位 | 2      |
//	| 有          | 有       | **精确分层**           | 1(最高)|
//
//	⚠️ 优先级:**精确 > 按体重段 > 整店**。
//	同一优先级命中多个 ⇒ 取被借店**样本数最多**的那个(确定性;再并列 ⇒ 取门店号小的)。
//
// # 生效时机
//
//	`RecomputeForecastBaselines` 重算时解析:
//	本店该分层**自己不可用**(!Usable())且有**启用中**的借用配置
//	⇒ 采用被借店同一 (体重段, 部位)那一行的数字,并把 source_branch_id 记成被借方。
//
//	被借店那一行自己也不可用 ⇒ **不借用**(回退 LLM)。
//
// # ⚠️ 管理员手动生成的行不被借用覆盖
//
//	`manual_generated = true` 的行是管理员**显式指定**的数字
//	⇒ 重算时整层跳过(见 forecast.go::RecomputeForecastBaselines)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// 借用配置的粒度(数字越小 = 优先级越高)。
const (
	// borrowScopeExact 精确分层(体重段 + 部位)。
	borrowScopeExact = 1
	// borrowScopeBand 该体重段的全部部位。
	borrowScopeBand = 2
	// borrowScopeWhole 整店借用。
	borrowScopeWhole = 3
)

// ForecastBaselineBorrowInput 新增/更新借用配置的入参。
//
// ⚠️ `weight_band` 与 `cut_type` **都空** = 整店借用;
// 只给 `weight_band` = 该体重段全部部位;都给 = 精确分层。
type ForecastBaselineBorrowInput struct {
	// SourceBranchID 是**被借方**(样本来源门店)。
	SourceBranchID string `json:"source_branch_id"`
	// WeightBand 是借哪个体重段(空 = 整店/不限体重段)。
	WeightBand string `json:"weight_band"`
	// CutType 是借哪个部位(空 = 该体重段的全部部位)。
	CutType model.CutType `json:"cut_type"`
	// Enabled = false ⇒ 关掉就回到"只用本店样本"。
	Enabled bool `json:"enabled"`
}

// ForecastBaselineBorrowID 借用配置的**确定性**代理主键。
//
// ⚠️ 必须是确定性的(理由同 surrogate_id.go):
// purge 按"单列主键"删行 ⇒ 随机 id ⇒ purge 拿着不存在的 id 去删 ⇒ 行永远清不掉。
func ForecastBaselineBorrowID(branchID, weightBand, cutType string) string {
	return surrogateID("PFBB", branchID, weightBand, cutType)
}

// ListForecastBaselineBorrows 列本店的借用配置。
func (s *Service) ListForecastBaselineBorrows(ctx context.Context, branchID string) ([]model.PigForecastBaselineBorrow, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.PigForecastBaselineBorrow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("weight_band ASC, cut_type ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baseline_borrows: %w", err)
	}
	return rows, nil
}

// UpsertForecastBaselineBorrow 新增/更新一条借用配置。
//
// # 幂等
//
//	唯一键是 (branch_id, weight_band, cut_type)
//	⇒ 同一个"粒度"重复提交 = **覆盖**那一行(改被借方或开关),不会堆重复行。
func (s *Service) UpsertForecastBaselineBorrow(ctx context.Context, branchID string,
	in ForecastBaselineBorrowInput) (*model.PigForecastBaselineBorrow, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	src := strings.TrimSpace(in.SourceBranchID)
	if src == "" {
		return nil, fmt.Errorf("%w: source_branch_id 必填(被借方)", ErrInvalidInput)
	}
	if src == branchID {
		return nil, fmt.Errorf("%w: source_branch_id 不能是本店自己(借用是**跨店**的)", ErrInvalidInput)
	}
	band := strings.TrimSpace(in.WeightBand)
	cut := model.CutType(strings.TrimSpace(string(in.CutType)))
	// ⚠️ 只给部位、不给体重段 ⇒ 既不是整店也不是"体重段" ⇒ 语义未定义 ⇒ 直接拒,
	//   别让这种配置**永远不生效**(那是最难查的一类脏配置)。
	if band == "" && cut != "" {
		return nil, fmt.Errorf("%w: 只给 cut_type 不支持:要么两个都空(整店借用),要么给 weight_band(该体重段全部部位),要么两个都给(精确分层)",
			ErrInvalidInput)
	}
	if cut != "" && !cut.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在枚举内", ErrInvalidInput, cut)
	}

	now := s.now()
	row := model.PigForecastBaselineBorrow{
		ID:             ForecastBaselineBorrowID(branchID, band, string(cut)),
		BranchID:       branchID,
		WeightBand:     band,
		CutType:        cut,
		SourceBranchID: src,
		Enabled:        in.Enabled,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	// ⚠️ 先读后写(不靠 ON CONFLICT):主键是确定性算出来的
	//   ⇒ 同一粒度重复提交会**同时**撞主键与业务唯一键
	//   ⇒ PG 的 ON CONFLICT 只处理 arbiter 索引上的冲突 ⇒ 撞主键会直接报错。
	var cur model.PigForecastBaselineBorrow
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND weight_band = ? AND cut_type = ?", branchID, band, cut).
		First(&cur).Error
	switch {
	case err == nil:
		cur.SourceBranchID = src
		cur.Enabled = in.Enabled
		cur.UpdatedAt = now
		if err := s.db.WithContext(ctx).Model(&model.PigForecastBaselineBorrow{}).
			Where("id = ?", cur.ID).Updates(map[string]any{
			"source_branch_id": cur.SourceBranchID,
			"enabled":          cur.Enabled,
			"updated_at":       cur.UpdatedAt,
		}).Error; err != nil {
			return nil, fmt.Errorf("update pig_forecast_baseline_borrow: %w", err)
		}
		return &cur, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			return nil, fmt.Errorf("create pig_forecast_baseline_borrow: %w", err)
		}
		return &row, nil
	default:
		return nil, fmt.Errorf("query pig_forecast_baseline_borrow: %w", err)
	}
}

// DeleteForecastBaselineBorrow 删掉一条借用配置(按 id)。
//
// ⚠️ 只删**本店**的配置:别的店的配置不该被本店改。
func (s *Service) DeleteForecastBaselineBorrow(ctx context.Context, branchID, id string) error {
	if branchID == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id 必填", ErrInvalidInput)
	}
	var cur model.PigForecastBaselineBorrow
	err := s.db.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).First(&cur).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: 借用配置 %s 不存在", ErrInvalidInput, id)
	}
	if err != nil {
		return fmt.Errorf("query pig_forecast_baseline_borrow: %w", err)
	}
	if err := s.db.WithContext(ctx).Where("id = ?", cur.ID).Delete(&model.PigForecastBaselineBorrow{}).Error; err != nil {
		return fmt.Errorf("delete pig_forecast_baseline_borrow: %w", err)
	}
	return nil
}

// borrowScopeOf 一条借用配置的粒度(数字越小 = 优先级越高)。
func borrowScopeOf(cfg model.PigForecastBaselineBorrow) int {
	switch {
	case cfg.WeightBand != "" && cfg.CutType != "":
		return borrowScopeExact
	case cfg.WeightBand != "":
		return borrowScopeBand
	default:
		return borrowScopeWhole
	}
}

// borrowCovers 这条借用配置能不能命中某个分层。
func borrowCovers(cfg model.PigForecastBaselineBorrow, k forecastBucketKey) bool {
	if cfg.WeightBand != "" && cfg.WeightBand != k.band {
		return false
	}
	if cfg.CutType != "" && cfg.CutType != k.cut {
		return false
	}
	return true
}

// baselineRowUsable 一行基线**能不能用**(手动生成的行绕过 ≥5 门槛)。
func baselineRowUsable(row model.PigForecastBaseline) bool {
	return row.SampleCount >= MinForecastSamples || row.ManualGenerated
}

// loadBranchBaselineRows 读某店(被借方)的全部基线行,索引 = 分层键。
func (s *Service) loadBranchBaselineRows(ctx context.Context, branchID string) (
	map[forecastBucketKey]model.PigForecastBaseline, error) {
	out := map[forecastBucketKey]model.PigForecastBaseline{}
	var rows []model.PigForecastBaseline
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baselines(%s): %w", branchID, err)
	}
	for _, r := range rows {
		out[forecastBucketKey{band: r.WeightBand, cut: r.CutType}] = r
	}
	return out, nil
}

// resolveBorrowedBaselines 解析本店的借用配置 ⇒ 本店**自己不可用**的分层该用谁家的数字。
//
// 返回:分层键 → 被借店那一行的基线(已把 source_branch_id 改成被借方)。
func (s *Service) resolveBorrowedBaselines(ctx context.Context, branchID string) (
	map[forecastBucketKey]model.PigForecastBaseline, error) {
	out := map[forecastBucketKey]model.PigForecastBaseline{}
	cfgs, err := s.listEnabledBorrows(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return out, nil
	}
	// 被借方的基线行(每个被借店只读一次)。
	srcRows := map[string]map[forecastBucketKey]model.PigForecastBaseline{}
	for _, cfg := range cfgs {
		if _, ok := srcRows[cfg.SourceBranchID]; ok {
			continue
		}
		rows, err := s.loadBranchBaselineRows(ctx, cfg.SourceBranchID)
		if err != nil {
			return nil, err
		}
		srcRows[cfg.SourceBranchID] = rows
	}
	// 候选分层 = 命中任一配置的(被借店有的)分层。
	candidates := map[forecastBucketKey]bool{}
	for _, cfg := range cfgs {
		for k := range srcRows[cfg.SourceBranchID] {
			if borrowCovers(cfg, k) {
				candidates[k] = true
			}
		}
	}
	for k := range candidates {
		cfg, ok := pickBorrowWinner(cfgs, srcRows, k)
		if !ok {
			continue
		}
		row, ok := srcRows[cfg.SourceBranchID][k]
		if !ok {
			continue
		}
		// ⚠️ 被借店那一行自己也不可用 ⇒ **不借用**(回退 LLM)。
		if !baselineRowUsable(row) {
			continue
		}
		row.BranchID = branchID
		// 样本来源门店 = 被借方(不是本店)。
		row.SourceBranchID = cfg.SourceBranchID
		out[k] = row
	}
	return out, nil
}

// listEnabledBorrows 读本店**启用中**的借用配置。
func (s *Service) listEnabledBorrows(ctx context.Context, branchID string) ([]model.PigForecastBaselineBorrow, error) {
	var rows []model.PigForecastBaselineBorrow
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND enabled = ?", branchID, true).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query pig_forecast_baseline_borrows: %w", err)
	}
	return rows, nil
}

// pickBorrowWinner 选出某个分层该向谁借。
//
// # 判据(确定性 ⇒ 同一份配置每次结果一样)
//
//	① **优先级**:精确 > 按体重段 > 整店;
//	② 同优先级 ⇒ 被借店**样本数最多**的那个;
//	③ 再并列 ⇒ 门店号小的(纯确定性兜底)。
func pickBorrowWinner(cfgs []model.PigForecastBaselineBorrow,
	srcRows map[string]map[forecastBucketKey]model.PigForecastBaseline,
	k forecastBucketKey) (model.PigForecastBaselineBorrow, bool) {
	var best model.PigForecastBaselineBorrow
	bestScope := 0
	bestSamples := -1
	found := false
	for _, cfg := range cfgs {
		if !borrowCovers(cfg, k) {
			continue
		}
		scope := borrowScopeOf(cfg)
		samples := 0
		if row, ok := srcRows[cfg.SourceBranchID][k]; ok {
			samples = row.SampleCount
		}
		better := !found ||
			scope < bestScope ||
			(scope == bestScope && samples > bestSamples) ||
			(scope == bestScope && samples == bestSamples && cfg.SourceBranchID < best.SourceBranchID)
		if better {
			best, bestScope, bestSamples, found = cfg, scope, samples, true
		}
	}
	return best, found
}
