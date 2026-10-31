// Package service / forecast_price_preview.go —— 「按基线预览 / 重算」通道(2026-10-31 owner)。
//
// # 为什么需要这条通道
//
// owner：「我打算用 29 日的 2 个分割样本、1 个 7.3 元/斤(14.6 元/kg)的 29 日猪单价样本
//
//	倒推出的『分割基线 + 部位进货单价基线』应用到 30 日去,
//	需要你在录入猪的时候提供使用基线重算建议分割和建议成本单价的功能
//	(目前只能靠删除和重录实现)。」
//
// ⇒ 三个动作：
//
//	① **预览**：还没录猪时，按基线算一遍「建议分割 + 建议成本单价」给门店看（不写库）；
//	② **重算**：已录的猪切成「只用基线」口径 + 重算当天成本链（不用删了重录）；
//	③ 报表上「成本单价」的建议行 ⇒ 就是这条链算出来的（见 cut_margin_dayend.go）。
//
// # 口径(与报表**完全一致**，不是另算一套)
//
//	建议分割   = 分割重量基线的 p50（该猪体重段）× 缩到不超过猪重
//	建议成本单价 = 部位进货单价基线 × (这头猪进价 ÷ 基线参考猪进价)
//	              退取链:基线 → 门店整店加权均价 → LLM 缓存单价 → 都没有
//	封顶       = 各部位合计价值 > 猪的总进货金额 ⇒ 按比例只往下缩
//
// ⚠️ 单价基线只用**该营业日之前**的日样本(见 loadPriceBaselinesAsOf)
//   ⇒ 结构上不可能自证(算 30 日的建议时看不到 30 日的样本)。
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// previewPigID 预览用的**假想猪** id。
//
// ⚠️ 它只在本文件内部活着(不落库)⇒ 不会和真猪撞 id。
const previewPigID = "__preview__"

// CutPricePreview 一头猪按**基线**算出来的建议分割 + 建议成本单价。
//
// ⚠️ 它的数值与毛利看板「成本单价」列的建议行**同源**
//   (都走 loadSuggestedSplitPerPig + suggestedCutUnitPrices)⇒
//   门店在录猪页看到的数,和在看板看到的数**不会打架**。
type CutPricePreview struct {
	// PigID 空 = 只是预览(还没录猪);有值 = 已录的那头猪。
	PigID string `json:"pig_id,omitempty"`
	// Date 是这个预览对应的营业日(bizTZ)。
	Date string `json:"date"`
	// AdviceMode 是重算后这头猪的建议生成方式(baseline / auto);预览时为空。
	AdviceMode string `json:"advice_mode,omitempty"`
	// RematerializedDays 是重算了几天成本链(预览时为 0)。
	RematerializedDays int `json:"rematerialized_days,omitempty"`
	// WeightKg 是这头猪的毛重(kg)。
	WeightKg decimal.Decimal `json:"weight_kg"`
	// PigPriceYuan 是这头猪的进价(元/kg)—— 单价基线按它缩放。
	PigPriceYuan decimal.Decimal `json:"pig_price_yuan"`
	// PigTotalCostYuan 是这头猪的总进货金额(元)= 毛重 × 进价。
	PigTotalCostYuan decimal.Decimal `json:"pig_total_cost_yuan"`
	// TotalSplitKg 是各部位建议分割之和(kg)。
	TotalSplitKg decimal.Decimal `json:"total_split_kg"`
	// TotalValueYuan 是各部位「建议成本单价 × 建议分割 kg」之和(元)。
	TotalValueYuan decimal.Decimal `json:"total_value_yuan"`
	// Capped = true ⇒ 合计价值超过了猪的总进货金额,已按比例**只往下缩**。
	//
	// ⚠️ 必须让门店看见:不然他会以为"基线给的价比猪还贵,系统算错了"。
	Capped bool `json:"capped"`
	// Rows 是逐部位的建议(按建议分割 kg 降序)。
	Rows []CutPricePreviewRow `json:"rows"`
}

// CutPricePreviewRow 一个部位的基线口径建议。
type CutPricePreviewRow struct {
	CutType model.CutType `json:"cut_type"`
	// SuggestedSplitKg 是该部位的建议分割(kg)。
	SuggestedSplitKg decimal.Decimal `json:"suggested_split_kg"`
	// SuggestedCostUnitPriceYuan 是该部位的建议成本单价(元/kg)。
	//
	// ⚠️ nil = **取不到价**(基线/整店均价/LLM 缓存都没有)⇒ 前端显示「—」(不是 0)。
	SuggestedCostUnitPriceYuan *decimal.Decimal `json:"suggested_cost_unit_price_yuan,omitempty"`
	// CostPriceSource 是这个价的来源:baseline | store_avg | llm_cache | none。
	CostPriceSource string `json:"cost_price_source"`
}

// PreviewCutPrice 按**基线**预览某头猪的建议分割 + 建议成本单价(**不写库**)。
//
// day 是这个预览对应的营业日(bizTZ)—— 单价基线只用该日**之前**的样本。
//
// ⚠️ 预览一律用**基线口径**(advice_mode=baseline)⇒ 不取 LLM 兜底
//   ⇒ 门店看到的就是"纯基线会给什么",不会被 AI 的建议搅混。
func (s *Service) PreviewCutPrice(ctx context.Context, branchID string, day time.Time,
	weightKg, pigPriceYuan decimal.Decimal) (*CutPricePreview, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if !weightKg.IsPositive() {
		return nil, fmt.Errorf("%w: weight_kg 必须大于 0", ErrInvalidInput)
	}
	pig := model.WholePig{
		ID:                    previewPigID,
		BranchID:              branchID,
		GrossWeightKg:         weightKg,
		PurchaseUnitPriceYuan: pigPriceYuan,
		PurchaseCostYuan:      weightKg.Mul(pigPriceYuan),
		AdviceMode:            model.AdviceModeBaseline,
	}
	return s.cutPricePreviewOf(ctx, branchID, pig, day)
}

// cutPricePreviewOf 算一头猪的基线口径预览(建议分割 + 建议成本单价)。
//
// ⚠️ 与报表同源:loadSuggestedSplitPerPig(建议分割) + suggestedCutUnitPrices(建议成本单价)。
func (s *Service) cutPricePreviewOf(ctx context.Context, branchID string, pig model.WholePig,
	day time.Time) (*CutPricePreview, error) {
	pigs := []model.WholePig{pig}
	splitPerPig, err := s.loadSuggestedSplitPerPig(ctx, branchID, pigs)
	if err != nil {
		return nil, err
	}
	unit, sources, capped, err := s.suggestedCutUnitPrices(ctx, branchID, pigs, splitPerPig, day)
	if err != nil {
		return nil, err
	}
	split := splitPerPig[pig.ID]
	rows := make([]CutPricePreviewRow, 0, len(split))
	var totalKg, totalValue decimal.Decimal
	for cut, kg := range split {
		row := CutPricePreviewRow{
			CutType:          model.CutType(cut),
			SuggestedSplitKg: kg,
			CostPriceSource:  PriceSourceNone,
		}
		if v, ok := unit[cut]; ok && v.IsPositive() {
			price := v
			row.SuggestedCostUnitPriceYuan = &price
			row.CostPriceSource = sources[cut]
			totalValue = totalValue.Add(v.Mul(kg))
		}
		totalKg = totalKg.Add(kg)
		rows = append(rows, row)
	}
	// 排序:按建议分割 kg 降序 ⇒ 门店最关心"哪个部位出多少肉";同重按部位名 ⇒ 稳定。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SuggestedSplitKg.Equal(rows[j].SuggestedSplitKg) {
			return rows[i].CutType < rows[j].CutType
		}
		return rows[i].SuggestedSplitKg.GreaterThan(rows[j].SuggestedSplitKg)
	})
	return &CutPricePreview{
		PigID:            pig.ID,
		Date:             day.In(s.bizTZOrUTC()).Format("2006-01-02"),
		WeightKg:         pig.GrossWeightKg,
		PigPriceYuan:     pigUnitPriceYuan(pig),
		PigTotalCostYuan: pig.PurchaseCostYuan,
		TotalSplitKg:     totalKg,
		TotalValueYuan:   totalValue,
		Capped:           capped,
		Rows:             rows,
	}, nil
}

// ApplyBaselineToPig 把**已录的猪**切成「只用基线」口径 + 重算当天成本链。
//
// # 干什么
//
//	① 这头猪的 advice_mode 改成 baseline(清掉 AI 建议,免得两套口径打架);
//	② 重算它所属营业日 → 今天 的成本链(派生数据,幂等);
//	③ 返回按基线算出来的建议分割 + 建议成本单价(门店当场看到效果)。
//
// # ⚠️ 为什么允许"改已录的猪"(2026-10-31 owner)
//
//	owner 原来只能靠「删除 + 重录」换口径 ⇒ 太重。
//	⇒ 这里给一个**明确的、可审计的**动作,省掉删了重录。
//
// # ⚠️ 已审计结转的期间**冻结**(与补录/重新生成建议同口径)
//
//	改建议 ⇒ 改成本链 ⇒ 已结转的期间不能被静默改写。
func (s *Service) ApplyBaselineToPig(ctx context.Context, branchID, pigID, mode string) (*CutPricePreview, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	pigID = strings.TrimSpace(pigID)
	if pigID == "" {
		return nil, fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
	}
	switch mode {
	case "", model.AdviceModeBaseline:
		mode = model.AdviceModeBaseline
	case model.AdviceModeAuto:
	default:
		return nil, fmt.Errorf("%w: mode 只能是 baseline 或 auto", ErrInvalidInput)
	}
	var pig model.WholePig
	if err := s.db.WithContext(ctx).First(&pig, "id = ?", pigID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWholePigNotFound
		}
		return nil, err
	}
	if pig.BranchID != branchID {
		return nil, ErrBranchMismatch
	}
	if err := s.assertNotAudited(ctx, branchID, pig.ArrivedAt, "按基线重算分割建议"); err != nil {
		return nil, err
	}
	// ---- ① 改口径 ----
	//
	// ⚠️ **切回「自动」时必须先校验该体重段有可用基线**:
	//   auto = 基线优先 + LLM 兜底 ⇒ 永远有建议 ⇒ 不拦。
	//   但 baseline = **基线没覆盖的部位没有建议** ⇒ 若这个体重段压根没基线,
	//   这头猪就**一条建议都没有** ⇒ 当天产出凭空少一整头猪
	//   ⇒ 到处是「缺口 / 短卖 / 幻影搭赠」⇒ 门店会以为系统算错了。
	//
	//   ⇒ 与录猪时的 hasUsableBaselineBand **同一个口径**(见 cut_split_algo.go)。
	if mode == model.AdviceModeBaseline {
		usable, err := s.hasUsableBaselineBand(ctx, branchID, pig.GrossWeightKg)
		if err != nil {
			return nil, err
		}
		if !usable {
			return nil, fmt.Errorf("%w: %s 体重段还没有可用的分割基线,切成「只用基线」后这头猪会一条建议都没有"+
				"(当天会凭空少一整头猪的产出);请先补该体重段的基线,或改用「自动」",
				ErrInvalidInput, WeightBandOf(pig.GrossWeightKg))
		}
	}
	updates := map[string]any{
		"advice_mode": mode,
		"updated_at":  s.now(),
	}
	if mode == model.AdviceModeBaseline {
		// ⚠️ **清掉 AI 建议**:切成基线后 AI 建议不再参与计算
		//   (loadSuggestedSplitPerPig / computeProducedBySku 都按模式取数)
		//   ⇒ 留着它只会让门店以为"AI 建议生效了"(而实际没生效)。
		//
		//   ⚠️ 这一步同时让「盘点预期剩余」的兜底(它读 llm_advice_json)不再含 LLM 那份
		//   ⇒ 与成本链口径一致(都是基线接管)⇒ 不会出现「链走基线、盘点还按 LLM 算」的对不上。
		updates["llm_advice_json"] = nil
		// 基线口径**没有** AI 建议 ⇒ 状态落 none(前端据此显示"没有 AI 建议")。
		updates["advice_status"] = model.AdviceStatusNone
		updates["data_source"] = string(model.DataSourceStub)
	}
	// ⚠️ 切回「自动」时**不清** llm_advice_json:
	//   新建议是**异步**生成的 ⇒ 清掉会让这段时间这头猪一条建议都没有
	//   ⇒ 反而制造缺口。⇒ 保留旧建议(它本来就是这头猪的建议),生成成功后自然覆盖。
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("id = ? AND branch_id = ?", pigID, branchID).
		Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("改猪的建议生成方式: %w", err)
	}
	// 改回「自动」⇒ 重新触发一次 AI 建议生成(异步,立即返回)。
	if mode == model.AdviceModeAuto {
		s.kickAdviceGeneration(ctx, pigID, branchID)
	}
	// ---- ② 重算成本链(这头猪所在营业日 → 今天)----
	dayKey := s.pigBizDateKey(pig)
	days, err := s.MaterializeCostChain(ctx, branchID, dayKey)
	if err != nil {
		return nil, fmt.Errorf("重算成本链: %w", err)
	}
	// ---- ③ 返回基线口径的预览(门店当场看到效果)----
	pig.AdviceMode = mode
	preview, err := s.cutPricePreviewOf(ctx, branchID, pig, s.bizDateOf(pig))
	if err != nil {
		return nil, err
	}
	preview.AdviceMode = mode
	preview.RematerializedDays = days
	return preview, nil
}

// pigBizDateKey 取这头猪所属营业日的日期键(bizTZ,"YYYY-MM-DD")。
//
// ⚠️ 营业日**可能**跨自然日(跨零点营业)⇒ 不能直接用 arrived_at 的日历日。
func (s *Service) pigBizDateKey(pig model.WholePig) string {
	return s.bizDateOf(pig).In(s.bizTZOrUTC()).Format("2006-01-02")
}

// bizDateOf 取这头猪所属营业日的**当天零点**(bizTZ)。
func (s *Service) bizDateOf(pig model.WholePig) time.Time {
	start, _ := s.businessDayBounds(pig.ArrivedAt)
	return start
}
