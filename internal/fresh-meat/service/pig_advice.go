package service

// 分割建议(LLM)的**异步生成**。
//
// # 为什么改成异步(2026-10-26 门店反馈)
//
// 门店反馈:「点『提交录入并生成分割建议』报网络错误,刷新后猪录进去了,
// 但**没有分割建议**。」
//
// 生产现场(店 00 / 2026-10-26 19:32):
//
//	19:32:18  whole_pigs 落库(pig WP20261026ef79e859,110.2kg)
//	19:32:47  WARN RecordWholePig: predictFn failed, fallback to history_avg
//	          err="dapr converse: rpc error: code = Canceled desc = context canceled"
//	19:32:47  WARN RecordWholePig: update llm_advice_json failed  err="context canceled"
//	19:32:48  客户端弹「网络连接失败」
//
// ⇒ 完整链条:
//
//  1. 录猪接口**同步**等 LLM ⇒ 客户端 dio 的 receiveTimeout(30s)
//     与服务端 LLM 超时(FRESHMEAT_LLM_TIMEOUT,默认 30s)**撞车**
//     ⇒ 客户端先超时 → 主动断开;
//  2. 客户端断开 ⇒ **请求 ctx 被取消**;
//  3. ctx 取消 ⇒ LLM 调用被取消 ⇒ fallback(history_avg)也用同一 ctx ⇒ 也被取消;
//  4. ctx 取消 ⇒ **写回 llm_advice_json 也被取消**
//     ⇒ 猪在库里(事实成立),但建议**永久丢失**(data_source=stub、advice 为 NULL)
//     ⇒ 而且**没有任何补偿机制**(没有重试、没有补偿任务)。
//
// ⇒ 修法(三条同时做):
//
//  ① 录猪**立即返回**,不等 LLM(客户端不再脆弱,不会再超时);
//  ② 建议在**后台**用**独立 ctx**生成(客户端断网/超时都不影响它落库);
//  ③ 给一个**手动重生成**入口(RegeneratePigAdvice)——
//     兜住"服务重启时后台任务丢了""历史上已经丢了建议的猪"。
//
// ⚠️ 注意 ② 的 ctx 选型:后台任务**不能**用请求 ctx(会被客户端取消),
//  也不能用 context.Background() 丢掉 trace 等 value
//  ⇒ 用 context.WithoutCancel(ctx)(保留 value、去掉取消)+ 自己的超时。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// adviceTimeout 是后台生成分割建议的**整体**超时。
//
// ⚠️ 必须**大于** LLM 自身的超时(cmd/fresh-meat/main.go: FRESHMEAT_LLM_TIMEOUT,
// 默认 30s)—— 否则外层先到点,连回落 history_avg 的机会都没有。
const adviceTimeout = 45 * time.Second

// kickAdviceGeneration 触发后台生成分割建议,**立即返回**(不等)。
//
// ⚠️ 同一头猪同时只跑一个(用户可能连点「重新生成建议」)。
func (s *Service) kickAdviceGeneration(ctx context.Context, pigID, branchID string) {
	if _, loaded := s.adviceInFlight.LoadOrStore(pigID, struct{}{}); loaded {
		s.pubLogger.Info("kickAdviceGeneration: 已在生成中,跳过重复触发", "pig_id", pigID)
		return
	}
	// ⚠️ 通过 s.adviceRunner 执行(生产 = go f();单测 = 同步执行)。
	s.adviceRunner(func() {
		defer s.adviceInFlight.Delete(pigID)
		// ⚠️ **不能**用请求 ctx:客户端一断(超时/断网)ctx 就被取消
		//   ⇒ LLM 调用 + 写回都被取消 ⇒ 建议永久丢失(2026-10-26 的真因)。
		// ⇒ WithoutCancel:保留 ctx 的 value(trace 等),只去掉取消信号。
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), adviceTimeout)
		defer cancel()
		s.generateAdvice(bg, pigID, branchID)
	})
}
// generateAdvice 后台跑一次预测并把结果写回 whole_pig。
//
// 失败(LLM + history_avg 双失败)⇒ 只把状态置 failed,不改业务数据
// ⇒ 门店可以从「重新生成建议」重试。
func (s *Service) generateAdvice(ctx context.Context, pigID, branchID string) {
	var pig model.WholePig
	if err := s.db.WithContext(ctx).First(&pig, "id = ?", pigID).Error; err != nil {
		s.pubLogger.Warn("generateAdvice: 读猪失败", "pig_id", pigID, "err", err)
		return
	}
	// 查近 30 天 ±10% 重量段历史(LLM 的输入 + history_avg 的兜底依据)。
	history := s.findHistoryPigs(ctx, branchID, pig.GrossWeightKg, pig.ArrivedAt)

	result, predictErr := s.predictFn(ctx, &pig, history)
	if predictErr != nil {
		s.pubLogger.Warn("generateAdvice: predictFn failed, fallback to history_avg",
			"pig_id", pigID, "err", predictErr)
		result, predictErr = defaultPredictFn(ctx, &pig, history)
		if predictErr != nil {
			s.pubLogger.Error("generateAdvice: history_avg fallback also failed",
				"pig_id", pigID, "err", predictErr)
			s.markAdviceFailed(ctx, pigID)
			return
		}
	}

	ds := model.DataSource(result.Source)
	if ds != model.DataSourceLLM && ds != model.DataSourceHistoryAvg {
		ds = model.DataSourceHistoryAvg
	}
	// ⚠️ **LLM 顺带给的部位单价 ⇒ 写进缓存**(2026-10-31 owner 的退取链第②步)。
	//
	//	「今天没卖出过的部位」(搭赠等)⇒ 没有单价基线 ⇒ 建议成本单价取不到
	//	⇒ 退取链用「LLM 缓存单价」兜底 ⇒ 而缓存正是这里写的。
	//
	//	⚠️ 失败**不影响建议本身**(建议已经拿到了)⇒ 只记日志。
	if err := s.captureLLMCutPrices(ctx, branchID, result.Cuts); err != nil {
		s.pubLogger.Warn("generateAdvice: 写 LLM 单价缓存失败(不影响分割建议)",
			"pig_id", pigID, "err", err)
	}
	// ⚠️ 写回用**不带超时的** ctx:上面那个可能刚好到期,
	//   写回是短操作,不该被 LLM 的超时连累(2026-10-26 就是这么丢的)。
	writeCtx := context.WithoutCancel(ctx)
	if err := s.db.WithContext(writeCtx).Model(&model.WholePig{}).
		Where("id = ?", pigID).
		Updates(map[string]any{
			"llm_advice_json": datatypes.JSON(result.RawJSON),
			"data_source":     ds,
			"advice_status":   model.AdviceStatusReady,
			"updated_at":      s.now(),
		}).Error; err != nil {
		s.pubLogger.Warn("generateAdvice: 写回 llm_advice_json 失败", "pig_id", pigID, "err", err)
		s.markAdviceFailed(ctx, pigID)
		return
	}
	s.pubLogger.Info("generateAdvice: 分割建议已写回",
		"pig_id", pigID, "data_source", string(ds))
}

// markAdviceFailed 把状态置 failed(建议仍为空 ⇒ 前端给「重新生成建议」入口)。
func (s *Service) markAdviceFailed(ctx context.Context, pigID string) {
	writeCtx := context.WithoutCancel(ctx)
	if err := s.db.WithContext(writeCtx).Model(&model.WholePig{}).
		Where("id = ?", pigID).
		Updates(map[string]any{
			"advice_status": model.AdviceStatusFailed,
			"updated_at":    s.now(),
		}).Error; err != nil {
		s.pubLogger.Warn("markAdviceFailed: 更新状态失败", "pig_id", pigID, "err", err)
	}
}

// RegeneratePigAdvice 手动重跑一次分割建议(**异步**,立即返回)。
//
// 用途:
//
//   - 历史上建议丢了的猪(2026-10-26 那类"猪录了、建议没了");
//   - 服务重启导致后台任务丢失;
//   - 门店觉得这次的建议不合理,想再试一次。
//
// 权限:freshmeat:write。
//
// ⚠️ 建议会参与"LLM 兜底产出"(computeProducedBySku)⇒ 会改成本链
//   ⇒ **已审计结转的期间冻结**(与补录同口径)。
func (s *Service) RegeneratePigAdvice(ctx context.Context, branchID, pigID string) (*model.WholePig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	pigID = strings.TrimSpace(pigID)
	if pigID == "" {
		return nil, fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
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
	if err := s.assertNotAudited(ctx, branchID, pig.ArrivedAt, "重新生成分割建议"); err != nil {
		return nil, err
	}
	// ⚠️ 「只用基线」的猪**不生成 AI 建议**(2026-10-31 owner)
	//   ⇒ 这里必须**明确拒绝**,不能让门店点一下就白花一次 LLM 调用,
	//   更不能让 ai_advice_json 里有数、成本链却不用(看着像"建议已生成",
	//   实际没用上 ⇒ 门店会以为"AI 建议生效了")。
	if adviceModeOf(pig) == model.AdviceModeBaseline {
		return nil, fmt.Errorf("%w: 这头猪的分割建议方式是「只用基线」,不需要 AI 建议", ErrInvalidInput)
	}
	// 先置 generating ⇒ 前端立刻能显示"生成中"并开始轮询。
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("id = ?", pigID).
		Updates(map[string]any{
			"advice_status": model.AdviceStatusGenerating,
			"updated_at":    s.now(),
		}).Error; err != nil {
		return nil, fmt.Errorf("update advice_status: %w", err)
	}
	s.kickAdviceGeneration(ctx, pig.ID, branchID)
	pig.AdviceStatus = model.AdviceStatusGenerating
	return &pig, nil
}
