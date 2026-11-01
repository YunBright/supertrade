// Package service / stocktake_plan.go —— 计划盘点单的建单链路（自动 + 手动）。
//
// 自动路径：流水线 stepStocktake（仅**阻断级**信号才建单）。
// 手动路径：审批详情页「生成计划盘点单」（仅 warn 级盘点类信号时由用户触发）。
//
// ⚠️ 两条路径共用 createStocktakePlan 这一个实现 —— 拆成两份的话，
// “建单参数 / 绑定 / 群通知”迟早一边改一边忘，出现两种形态的计划单。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// isStocktakeSignalType 判断信号是否属**盘点类**（数量类）。
//
// 只有这两类信号的 warn 才会在审批详情页出现「生成计划盘点单」按钮；
// 其他 warn（联营扣点率、备货过量、毛利异常）不涉及实物库存，不该引导盘点。
func isStocktakeSignalType(signalType string) bool {
	switch signalType {
	case SigQtyDeviation, SigPurchaseWithoutSales:
		return true
	}
	return false
}

// storedStocktakeSuggestion 读该审批已落库的盘点建议（没有/解析失败返回 nil）。
func (s *Service) storedStocktakeSuggestion(ctx context.Context, approvalID string) *StocktakeSuggestion {
	var v model.LLMVerdict
	if err := s.db.WithContext(ctx).First(&v, "approval_id = ?", approvalID).Error; err != nil {
		return nil
	}
	if len(v.SuggestStocktake) == 0 {
		return nil
	}
	var st StocktakeSuggestion
	if err := json.Unmarshal(v.SuggestStocktake, &st); err != nil {
		return nil
	}
	return &st
}

// createStocktakePlan 建 plan 单 → 落审批绑定 → 推盘点通知。
//
// operatorID 为空时退回申请人；bearer 为空时退回审批单上保存的申请人 JWT
// （⚠️ 终态会把 applicant_bearer 清空 ⇒ 手动生成必须由调用方传当前请求的凭证）。
// triggerText 说明谁触发的（写进计划单备注与 step 日志）。
func (s *Service) createStocktakePlan(ctx context.Context, ap *model.ApprovalInstance, st *StocktakeSuggestion, signals []signalDraft, operatorID, bearer, triggerText string) (*StocktakeHeaderDTO, error) {
	start := time.Now()
	productIDs, derived := stocktakePlanProductIDs(st, signals)
	// 计划单商品是**必填语义**:没有商品就是一张空计划单,
	// 盘点人打开只看到"无计划项",等于这次建单完全没意义(2026-10-31 生产事故:
	// ST20261031501addf7 建出来 0 个计划项)。⇒ 宁可跳过并留痕,也不建空单。
	if len(productIDs) == 0 {
		err := fmt.Errorf("%w: 没有可落地的计划商品", ErrInvalidInput)
		s.logStep(ctx, ap, StepStocktake, "",
			fmt.Sprintf("%s(scope=%s)但没有可落地的计划商品,跳过建单", triggerText, st.Scope),
			"", time.Since(start).Milliseconds())
		return nil, err
	}
	settlementIDs := s.settlementIDsOf(ctx, ap.ID)
	remark := fmt.Sprintf("结算付款审批 %s %s(scope=%s;结算单 %s;计划商品 %d 个%s)",
		ap.ID, triggerText, st.Scope, strings.Join(settlementIDs, ","), len(productIDs), derived)
	if operatorID == "" {
		operatorID = ap.ApplicantUserID
	}
	if bearer == "" {
		bearer = ap.ApplicantBearer
	}
	in := StocktakeCreatePlanInput{
		BranchID:   ap.BranchID,
		OperatorID: operatorID,
		Remark:     truncate(remark, 500),
		ProductIDs: productIDs,
	}
	// 透传 JWT **和门店** 给 stocktake:
	//   - Authorization:AddPlanItems 从 cube 现拉商品名/条码时 cube 侧要认这个凭证;
	//   - X-Branch-ID:cube-router 的 /v1/load 挂着 rbac.RequireScopeWithBranch("cube:read"),
	//     门店从 header 取 —— 不带就是 400 branch_required。
	//
	// ⚠️ 2026-10-22 生产实踩:只传 Authorization、不传门店 ⇒ stocktake 侧
	// 建单 500(Internal Server Error)⇒ 审批日志里"建盘点计划失败",
	// 而拒绝理由里还写着"建议针对性盘点" —— 盘点单根本没建出来。
	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if ap.BranchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", ap.BranchID)
	}
	header, err := s.stocktake.CreatePlanHeader(ctx, in)
	if err != nil {
		s.logStep(ctx, ap, StepStocktake, remark, "建盘点计划失败: "+err.Error(), "", time.Since(start).Milliseconds())
		return nil, err
	}
	link := &model.ApprovalStocktakeLink{
		ID:                uuid.NewString(),
		ApprovalID:        ap.ID,
		StocktakeHeaderID: header.ID,
	}
	if err := s.db.WithContext(ctx).Create(link).Error; err != nil {
		s.logger.Warn("insert stocktake link failed", "approval_id", ap.ID, "err", err)
	}
	// 盘点计划通知:优先发到**门店指定的群**(Flutter 管理后台「仓库-设置」里配),
	// 没配群才退回固定的人(env SETTLEMENT_AUDITOR_STOCKTAKE_NOTIFY_USERS)。
	//
	// 群消息用 markdown 而不是 taskcard:企微的 taskcard(任务卡片)只支持成员消息,
	// 群聊(appchat)只吃 text/markdown ⇒ 卡片形态在群里发不出去。
	planURL := strings.TrimSuffix(s.cfg.WxH5BaseURL, "/") + "/inventory.html?header_id=" + header.ID
	desc := fmt.Sprintf("结算单 %s 审核发现库存异常,已生成针对性盘点计划,请及时盘点。计划单 %s",
		strings.Join(settlementIDs, ","), header.ID)
	group := s.notifyGroupOf(ctx, ap.BranchID)
	switch {
	case group != nil:
		payload := TaskCardPayload{
			ToChat:      group.ChatID,
			AppID:       group.AppID,
			Title:       "结算异常盘点计划",
			Description: truncate(desc, 200),
			URL:         planURL,
			TaskID:      "stocktake-plan-" + header.ID,
			// 让 wechat-bot 把链接包成企微 OAuth2 授权链接 ⇒
			// 群卡片点「去盘点」进自建应用「云商AI」的盘点单页,而不是系统浏览器。
			OpenInApp: true,
		}
		if _, err := s.publishCommand(ctx, CmdTaskCard, payload); err != nil {
			s.logger.Warn("publish stocktake_taskcard(群) failed", "approval_id", ap.ID, "err", err)
			s.logStep(ctx, ap, StepStocktake, remark, "盘点计划已创建,但推送到群失败: "+err.Error(), "", time.Since(start).Milliseconds())
			return header, nil
		}
	case len(s.cfg.StocktakeNotifyUsers) > 0:
		payload := TaskCardPayload{
			ToUser:      s.cfg.StocktakeNotifyUsers,
			Title:       "结算异常盘点计划",
			Description: truncate(desc, 200),
			URL:         planURL,
			TaskID:      "stocktake-plan-" + header.ID,
		}
		if _, err := s.publishCommand(ctx, CmdTaskCard, payload); err != nil {
			s.logger.Warn("publish stocktake_taskcard failed", "approval_id", ap.ID, "err", err)
		}
	}
	s.logStep(ctx, ap, StepStocktake, remark,
		fmt.Sprintf("盘点计划单 %s 已创建(通知:%s)", header.ID, notifyTargetText(group, s.cfg.StocktakeNotifyUsers)),
		"", time.Since(start).Milliseconds())
	return header, nil
}

// GenerateStocktakePlan 是审批详情页「生成计划盘点单」的服务端实现。
//
// 仅当本审批存在**盘点类 warn 信号**时才允许（自动建单只认 block，warn 由人判断）。
// 幂等：已存在审批↔盘点绑定 ⇒ 直接返回那张单，不重复建。
//
// 返回 (headerID, created, err)。
func (s *Service) GenerateStocktakePlan(ctx context.Context, branchID, approvalID, operatorID, bearer string) (string, bool, error) {
	if s.stocktake == nil {
		return "", false, fmt.Errorf("盘点服务未配置")
	}
	ap, err := s.loadApproval(ctx, approvalID)
	if err != nil {
		return "", false, err
	}
	if branchID != "" && ap.BranchID != branchID {
		return "", false, ErrApprovalNotFound // 跨门店视为不存在(不泄露)
	}
	var links []model.ApprovalStocktakeLink
	if err := s.db.WithContext(ctx).Where("approval_id = ?", ap.ID).
		Order("created_at ASC").Find(&links).Error; err != nil {
		return "", false, fmt.Errorf("load stocktake links: %w", err)
	}
	if len(links) > 0 {
		return links[0].StocktakeHeaderID, false, nil
	}
	var rows []model.AuditSignal
	if err := s.db.WithContext(ctx).
		Where("approval_id = ? AND level = ?", ap.ID, model.SignalWarn).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return "", false, fmt.Errorf("load warn signals: %w", err)
	}
	signals := make([]signalDraft, 0, len(rows))
	warnProductIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		if !isStocktakeSignalType(r.SignalType) {
			continue
		}
		signals = append(signals, signalDraft{
			SettlementID: r.SettlementID,
			ProductID:    r.ProductID,
			SignalType:   r.SignalType,
			Level:        r.Level,
			Message:      r.Message,
		})
		if r.ProductID != "" {
			warnProductIDs = append(warnProductIDs, r.ProductID)
		}
	}
	if len(signals) == 0 {
		return "", false, fmt.Errorf("%w: 本审批没有盘点类 warn 信号,无需生成计划盘点单", ErrInvalidInput)
	}
	// 优先沿用当时的 LLM 建议范围；没有建议才按 warn 信号点名商品派生。
	st := s.storedStocktakeSuggestion(ctx, ap.ID)
	if st == nil || len(st.IDs) == 0 {
		st = &StocktakeSuggestion{Scope: "product", IDs: dedupeStrings(warnProductIDs)}
	}
	header, err := s.createStocktakePlan(ctx, ap, st, signals, operatorID, bearer, "用户手动生成计划盘点单")
	if err != nil {
		return "", false, err
	}
	return header.ID, true, nil
}
