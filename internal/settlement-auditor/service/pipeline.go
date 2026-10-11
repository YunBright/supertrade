// Package service / pipeline.go —— 审核流水线与状态机(FR-2 核心)。
//
// 流程:
//
//	SubmitApproval(received) → go runAudit:
//	  fetch_settlements → expand_lines → check_sheets → check_amounts
//	    → check_quantity → llm_verdict → finish
//	pass → publish apply_event 命令(wechat-bot 代提交企微)→ approved(等 command_result)
//	reject → publish notify_text 通知申请人 → rejected
//	建议盘点 → stocktake 建 plan 单 + 绑定表 + taskcard 待办
//	panic/error → failed(RetryApproval 人工重跑 / RescanStuck job 自动重跑)
//
// 每步写 audit_logs 并 publish settlement.audit.log(Flutter 实时监控);
// 信号落 audit_signals;结论落 llm_verdicts(一审批一条,重跑先删旧)。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/metadata"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 流水线步骤名(audit_logs.step / settlement.audit.log payload)。
const (
	StepFetchSettlements = "fetch_settlements"
	StepExpandLines      = "expand_lines"
	StepCheckSheets      = "check_sheets"
	StepCheckAmounts     = "check_amounts"
	StepCheckQuantity    = "check_quantity"
	StepLLMVerdict       = "llm_verdict"
	StepStocktake        = "stocktake"
	StepFinish           = "finish"
)

// SubmitInput 提交审批入参(handler 已从 claims 取 ApplicantUserID)。
type SubmitInput struct {
	ApplicantUserID  string
	ApplicantWecomID string // 企微 userid(前端 wecom OAuth 登录后传入)
	// ApplicantBearer 是请求侧 Authorization 头原值;异步流水线以申请人身份
	// 调 cube(cube sidecar bearer 中间件强制 JWT)。落库至机器终态清空。
	ApplicantBearer  string
	SettlementIDs    []string
	ManualAmountYuan decimal.Decimal
	Remark           string
	// ApproverUserIDs 申请人在 H5 用企微原生选人器选定的审批人(至少 1 人)。
	ApproverUserIDs []string
	// CcUserIDs 抄送人(可空)。
	CcUserIDs []string
}

// maxSettlementsPerApproval 单次审批勾选上限(防 LLM prompt 爆炸)。
const maxSettlementsPerApproval = 50

// maxApproversPerApproval 审批人/抄送人数量上限(企微 process 节点容量保护)。
const maxApproversPerApproval = 100

// rescanStuckAfter 卡死判定:received/auditing 超过该时长未更新 → 重跑。
const rescanStuckAfter = 10 * time.Minute

// newApprovalID 生成 32 位无横线 hex(对齐 ApprovalInstance.ID size:32)。
func newApprovalID() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// SubmitApproval 创建审批实例(status=received)并后台启动审核流水线。
//
// 校验:branch/申请人/结算单列表/手工金额;结算单 id 去重。
// 返回 202 语义:实例已落库,审核异步进行,前端轮询/订阅日志。
func (s *Service) SubmitApproval(ctx context.Context, branchID string, in SubmitInput) (*model.ApprovalInstance, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if in.ApplicantUserID == "" {
		return nil, fmt.Errorf("%w: applicant_user_id 必填", ErrInvalidInput)
	}
	if in.ApplicantWecomID == "" {
		return nil, fmt.Errorf("%w: applicant_wecom_id 必填(企微 userid,审批单提交人)", ErrInvalidInput)
	}
	if in.ApplicantBearer == "" {
		return nil, fmt.Errorf("%w: 缺少 Authorization 头(机器审核需以申请人身份调 cube)", ErrInvalidInput)
	}
	if !in.ManualAmountYuan.GreaterThan(decimal.Zero) {
		return nil, fmt.Errorf("%w: manual_amount_yuan 必须 > 0", ErrInvalidInput)
	}
	ids := dedupeStrings(in.SettlementIDs)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: settlement_ids 至少 1 张", ErrInvalidInput)
	}
	if len(ids) > maxSettlementsPerApproval {
		return nil, fmt.Errorf("%w: 单次审批最多 %d 张结算单", ErrInvalidInput, maxSettlementsPerApproval)
	}
	approvers := dedupeStrings(in.ApproverUserIDs)
	if len(approvers) == 0 {
		return nil, fmt.Errorf("%w: approver_userids 至少选择 1 名审批人", ErrInvalidInput)
	}
	if len(approvers) > maxApproversPerApproval {
		return nil, fmt.Errorf("%w: 审批人最多 %d 人", ErrInvalidInput, maxApproversPerApproval)
	}
	ccUsers := dedupeStrings(in.CcUserIDs)
	if len(ccUsers) > maxApproversPerApproval {
		return nil, fmt.Errorf("%w: 抄送人最多 %d 人", ErrInvalidInput, maxApproversPerApproval)
	}
	approversJSON, _ := json.Marshal(approvers)
	ccJSON, _ := json.Marshal(ccUsers)

	ap := &model.ApprovalInstance{
		ID:               newApprovalID(),
		CorpID:           s.cfg.CorpID,
		BranchID:         branchID,
		ApplicantUserID:  in.ApplicantUserID,
		ApplicantWecomID: in.ApplicantWecomID,
		ApplicantBearer:  in.ApplicantBearer,
		ManualAmountYuan: in.ManualAmountYuan,
		Remark:           in.Remark,
		ApproverUserIDs:  approversJSON,
		CcUserIDs:        ccJSON,
		Status:           model.StatusReceived,
	}
	if err := s.db.WithContext(ctx).Create(ap).Error; err != nil {
		return nil, fmt.Errorf("create approval: %w", err)
	}
	// 落初始勾选快照(runAudit 的 stepFetchSettlements 从这里读 id 列表,
	// 随后 replaceApprovalSettlements 用 cube 数据补全金额并置 Checked)。
	for _, sid := range ids {
		row := &model.ApprovalSettlement{
			ID:           uuid.NewString(),
			ApprovalID:   ap.ID,
			SettlementID: sid,
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			return nil, fmt.Errorf("create approval_settlement %s: %w", sid, err)
		}
	}

	// 后台异步审核(detach ctx:请求返回后流水线继续)。
	go s.runAudit(context.Background(), ap.ID)

	// 记住本次审批流人员(下次发起默认带出,对齐官方审批体验);失败不阻断提交。
	if err := s.upsertApproverPreference(ctx, branchID, in.ApplicantUserID, approvers, ccUsers); err != nil {
		s.logger.Warn("upsert approver preference failed",
			"approval_id", ap.ID, "user_id", in.ApplicantUserID, "err", err)
	}

	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: ap.Status, At: s.now(),
	})
	return ap, nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// ---- 流水线 ----

// runAudit 执行机器审核(任何失败/panic → 实例 failed,可 retry/rescan)。
func (s *Service) runAudit(ctx context.Context, approvalID string) {
	ap, err := s.loadApproval(ctx, approvalID)
	if err != nil {
		s.logger.Error("runAudit: load approval", "approval_id", approvalID, "err", err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.markFailed(ctx, ap, fmt.Sprintf("panic: %v", r))
		}
	}()

	if s.cube == nil {
		s.markFailed(ctx, ap, ErrCubeUnavailable.Error())
		return
	}
	// 注入申请人 JWT(cube 实例 sidecar 的 middleware.http.bearer 强制 JWT,
	// 空 bearer 必被 Unauthenticated 拒 → failed;JWT_TTL=1h,重跑由 Retry 刷新)。
	ctx = cubeclient.WithBearer(ctx, ap.ApplicantBearer)
	if err := s.transition(ctx, ap, model.StatusAuditing, "", nil); err != nil {
		return
	}

	var signals []signalDraft

	// step 1: 拉结算单 + 存在性/审核状态核对
	settlements, err := s.stepFetchSettlements(ctx, ap, &signals)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}
	// 落 approval_settlements 快照(重跑前先清)
	s.replaceApprovalSettlements(ctx, ap, settlements)

	// step 2: 逐张拆 PI/RO 行
	linesBySettlement, err := s.stepExpandLines(ctx, ap, settlements, &signals)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// step 3: 采购单存在/审核/供应商核对
	sheets, err := s.stepCheckSheets(ctx, ap, settlements, linesBySettlement, &signals)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// step 4: 金额勾稽
	s.stepCheckAmounts(ctx, ap, settlements, linesBySettlement, &signals)

	// step 5: 数量信号(采购量 vs 销售)
	if err := s.stepCheckQuantity(ctx, ap, linesBySettlement, &signals); err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// 信号统一落库
	if err := s.saveSignals(ctx, ap.ID, signals); err != nil {
		s.markFailed(ctx, ap, "save signals: "+err.Error())
		return
	}

	// step 6: LLM 综合研判(失败降级规则)
	verdict, err := s.stepLLMVerdict(ctx, ap, settlements, linesBySettlement, sheets, signals)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// step 7(可选): 建议盘点 → 建 plan 单 + 绑定 + taskcard
	s.stepStocktake(ctx, ap, verdict)

	// finish: pass → apply_event;reject → notify_text
	s.stepFinish(ctx, ap, verdict, signals)
}

// loadApproval 按 id 读实例(不分 branch:内部流水线用)。
func (s *Service) loadApproval(ctx context.Context, id string) (*model.ApprovalInstance, error) {
	var ap model.ApprovalInstance
	if err := s.db.WithContext(ctx).First(&ap, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrApprovalNotFound
		}
		return nil, err
	}
	return &ap, nil
}

// transition 更新状态并广播 status 事件。extra 可附带额外列更新(可 nil)。
func (s *Service) transition(ctx context.Context, ap *model.ApprovalInstance, status, errMsg string, extra map[string]any) error {
	updates := map[string]any{"status": status}
	if errMsg != "" {
		updates["error"] = errMsg
	}
	for k, v := range extra {
		updates[k] = v
	}
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", ap.ID).Updates(updates).Error; err != nil {
		s.logger.Error("transition failed", "approval_id", ap.ID, "status", status, "err", err)
		return err
	}
	ap.Status = status
	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: status, SpNo: ap.SpNo, SpStatus: ap.SpStatus, At: s.now(),
	})
	return nil
}

// markFailed 置 failed(仅 log,不再广播 error 细节给 Flutter 外的通道)。
// 同时清空 applicant_bearer(凭证不落库过终态;Retry 由请求侧重新提供)。
func (s *Service) markFailed(ctx context.Context, ap *model.ApprovalInstance, msg string) {
	s.logger.Error("audit failed", "approval_id", ap.ID, "err", msg)
	_ = s.transition(ctx, ap, model.StatusFailed, msg, map[string]any{"applicant_bearer": ""})
	s.logStep(ctx, ap, StepFinish, "", "审核失败: "+msg, "", 0)
}

// logStep 写 audit_logs + publish settlement.audit.log(失败仅 warn,不阻断)。
func (s *Service) logStep(ctx context.Context, ap *model.ApprovalInstance, step, inSummary, outSummary, llmTrace string, durMs int64) {
	row := &model.AuditLog{
		ID:            uuid.NewString(),
		ApprovalID:    ap.ID,
		Step:          step,
		InputSummary:  truncate(inSummary, 2048),
		OutputSummary: truncate(outSummary, 4096),
		LLMTrace:      truncate(llmTrace, 8192),
		DurationMs:    durMs,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		s.logger.Warn("insert audit_log failed", "approval_id", ap.ID, "step", step, "err", err)
	}
	s.publishLogEvent(ctx, &AuditLogEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Step: step, OutputSummary: row.OutputSummary, At: s.now(),
	})
}

// truncate 按字节上限截断,但**不能切断多字节 UTF-8 字符**——
// 2026-10-10 生产实踩:LLM 中文 reason 在 200 字节处被切成半个三字节字符
// (0xe5 0x85),Postgres 拒收(ERROR 22021 invalid byte sequence for encoding
// "UTF8"),llm_verdict/finish 两条审计日志整行丢失。
// 退回到最后一个完整 rune 边界。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	// 逐字节回退,直到剩余是合法 UTF-8(即去掉末尾不完整 rune)。
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// stepFetchSettlements 拉勾选结算单 + check 1。
func (s *Service) stepFetchSettlements(ctx context.Context, ap *model.ApprovalInstance, signals *[]signalDraft) ([]SettlementRow, error) {
	start := time.Now()
	ids := s.settlementIDsOf(ctx, ap.ID)
	rows, err := s.cube.ListSettlements(ctx, ap.BranchID, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch settlements: %w", err)
	}
	*signals = append(*signals, checkSettlements(ids, rows)...)
	s.logStep(ctx, ap, StepFetchSettlements,
		fmt.Sprintf("勾选 %d 张: %s", len(ids), strings.Join(ids, ",")),
		fmt.Sprintf("cube 命中 %d 张", len(rows)), "", time.Since(start).Milliseconds())
	return rows, nil
}

// settlementIDsOf 读审批勾选的结算单 id(快照表;首次跑时快照表为空 → 从提交参数走不到这里,
// 所以 SubmitApproval 必须在 runAudit 前落快照 —— 见 replaceApprovalSettlements 首次调用)。
func (s *Service) settlementIDsOf(ctx context.Context, approvalID string) []string {
	var ids []string
	s.db.WithContext(ctx).Model(&model.ApprovalSettlement{}).
		Where("approval_id = ?", approvalID).Pluck("settlement_id", &ids)
	return ids
}

// replaceApprovalSettlements 重写审批 ↔ 结算单快照(带 cube 金额/供应商)。
//
// 首跑时快照表为空:stepFetchSettlements 先按提交参数查 cube —— 但提交参数没存!
// 因此首跑由 SubmitApproval 在落库时先写一行"未核对"快照(settlement_ids),
// runAudit 跑到这里再用 cube 数据补全金额并置 Checked。
func (s *Service) replaceApprovalSettlements(ctx context.Context, ap *model.ApprovalInstance, rows []SettlementRow) {
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", ap.ID).Delete(&model.ApprovalSettlement{}).Error; err != nil {
		s.logger.Warn("clear approval_settlements failed", "approval_id", ap.ID, "err", err)
		return
	}
	checked := make(map[string]bool, len(rows))
	for _, r := range rows {
		checked[r.ID] = true
	}
	for _, r := range rows {
		row := &model.ApprovalSettlement{
			ID:              uuid.NewString(),
			ApprovalID:      ap.ID,
			SettlementID:    r.ID,
			SupplierID:      r.SupplierID,
			DocumentType:    r.DocumentType,
			GoodsAmountYuan: r.TotalGoodsYuan,
			Checked:         checked[r.ID] && r.ApproveStatus == "1",
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			s.logger.Warn("insert approval_settlement failed", "approval_id", ap.ID, "settlement_id", r.ID, "err", err)
		}
	}
}

// stepExpandLines 逐张拆 PI/RO 行 + 无采购行 info 信号。
func (s *Service) stepExpandLines(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, signals *[]signalDraft) (map[string][]SettlementLineRow, error) {
	start := time.Now()
	out := make(map[string][]SettlementLineRow, len(settlements))
	total := 0
	for _, st := range settlements {
		lns, err := s.cube.ListPurchaseLines(ctx, ap.BranchID, st.ID)
		if err != nil {
			return nil, fmt.Errorf("expand lines %s: %w", st.ID, err)
		}
		out[st.ID] = lns
		total += len(lns)
		if len(lns) == 0 {
			*signals = append(*signals, newSignal(model.SignalInfo, SigNoPurchaseLines, st.ID, "",
				fmt.Sprintf("结算单 %s 无采购进货/退货行(非采购结算单,仅提示)", st.ID),
				map[string]any{"settlement_id": st.ID}))
		}
	}
	s.logStep(ctx, ap, StepExpandLines,
		fmt.Sprintf("%d 张结算单", len(settlements)),
		fmt.Sprintf("拆出 %d 行 PI/RO", total), "", time.Since(start).Milliseconds())
	return out, nil
}

// stepCheckSheets 拉采购单头 + check 2。
func (s *Service) stepCheckSheets(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, signals *[]signalDraft) ([]PurchaseSheetRow, error) {
	start := time.Now()
	var allLines []SettlementLineRow
	voucherSet := make(map[string]struct{})
	for _, lns := range linesBySettlement {
		for _, ln := range lns {
			allLines = append(allLines, ln)
			voucherSet[ln.VoucherID] = struct{}{}
		}
	}
	vouchers := make([]string, 0, len(voucherSet))
	for v := range voucherSet {
		vouchers = append(vouchers, v)
	}
	sheets, err := s.cube.ListPurchaseSheets(ctx, ap.BranchID, vouchers)
	if err != nil {
		return nil, fmt.Errorf("fetch purchase sheets: %w", err)
	}
	settlementSupplier := make(map[string]string, len(settlements))
	for _, st := range settlements {
		settlementSupplier[st.ID] = st.SupplierID
	}
	*signals = append(*signals, checkSheets(allLines, sheets, settlementSupplier)...)
	s.logStep(ctx, ap, StepCheckSheets,
		fmt.Sprintf("关联采购单 %d 张", len(vouchers)),
		fmt.Sprintf("cube 命中 %d 张", len(sheets)), "", time.Since(start).Milliseconds())
	return sheets, nil
}

// stepCheckAmounts check 3(纯函数,无 cube 调用)。
func (s *Service) stepCheckAmounts(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, signals *[]signalDraft) {
	start := time.Now()
	before := len(*signals)
	tol := decimal.NewFromFloat(s.cfg.AmountToleranceYuan)
	*signals = append(*signals, checkAmounts(settlements, linesBySettlement, ap.ManualAmountYuan, tol)...)
	s.logStep(ctx, ap, StepCheckAmounts,
		fmt.Sprintf("手工金额 %s,容差 %s", ap.ManualAmountYuan.StringFixed(2), tol.String()),
		fmt.Sprintf("新增 %d 条金额信号", len(*signals)-before), "", time.Since(start).Milliseconds())
}

// stepCheckQuantity 聚合 PI 收货量 + 拉窗口销售 → check 4。
func (s *Service) stepCheckQuantity(ctx context.Context, ap *model.ApprovalInstance, linesBySettlement map[string][]SettlementLineRow, signals *[]signalDraft) error {
	start := time.Now()
	// 仅 PI(进货)单做收货量聚合;RO 退货本期不建信号(见 checks.go 注释)。
	piSheetSet := make(map[string]struct{})
	for _, lns := range linesBySettlement {
		for _, ln := range lns {
			if ln.SourceDocType == "PI" {
				piSheetSet[ln.VoucherID] = struct{}{}
			}
		}
	}
	piSheets := make([]string, 0, len(piSheetSet))
	for v := range piSheetSet {
		piSheets = append(piSheets, v)
	}
	if len(piSheets) == 0 {
		s.logStep(ctx, ap, StepCheckQuantity, "无 PI 进货单", "跳过数量信号", "", time.Since(start).Milliseconds())
		return nil
	}
	lines, err := s.cube.ListSheetLines(ctx, ap.BranchID, piSheets)
	if err != nil {
		return fmt.Errorf("fetch sheet lines: %w", err)
	}
	received := make(map[string]decimal.Decimal)
	productSet := make(map[string]struct{})
	for _, ln := range lines {
		if ln.ProductID == "" {
			continue
		}
		received[ln.ProductID] = received[ln.ProductID].Add(ln.ReceivedQuantity)
		productSet[ln.ProductID] = struct{}{}
	}
	products := make([]string, 0, len(productSet))
	for p := range productSet {
		products = append(products, p)
	}
	from, to := s.saleWindowDates()
	sales, err := s.cube.SumSalesByProduct(ctx, ap.BranchID, products, from, to)
	if err != nil {
		return fmt.Errorf("fetch sales: %w", err)
	}
	salesByProduct := make(map[string]SaleRow, len(sales))
	for _, r := range sales {
		salesByProduct[r.ProductID] = r
	}
	before := len(*signals)
	*signals = append(*signals, checkQuantity(received, salesByProduct, s.cfg.QtyDeviationFactor)...)
	s.logStep(ctx, ap, StepCheckQuantity,
		fmt.Sprintf("PI 单 %d 张,商品 %d 种,销售窗口 %s~%s", len(piSheets), len(products), from, to),
		fmt.Sprintf("新增 %d 条数量信号", len(*signals)-before), "", time.Since(start).Milliseconds())
	return nil
}

// saveSignals 信号落 audit_signals。
func (s *Service) saveSignals(ctx context.Context, approvalID string, drafts []signalDraft) error {
	for _, d := range drafts {
		row := &model.AuditSignal{
			ID:           uuid.NewString(),
			ApprovalID:   approvalID,
			SettlementID: d.SettlementID,
			ProductID:    d.ProductID,
			SignalType:   d.SignalType,
			Level:        d.Level,
			Message:      truncate(d.Message, 1024),
			Evidence:     marshalEvidence(d.Evidence),
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			return fmt.Errorf("insert signal %s: %w", d.SignalType, err)
		}
	}
	return nil
}

// stepLLMVerdict 调 verdictFn;失败降级 defaultVerdictFn;落 llm_verdicts。
func (s *Service) stepLLMVerdict(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, sheets []PurchaseSheetRow, signals []signalDraft) (*VerdictResult, error) {
	start := time.Now()
	in := &AuditInput{
		Approval:    ap,
		Settlements: settlements,
		Lines:       linesBySettlement,
		Sheets:      sheets,
		Signals:     signals,
	}
	verdict, err := s.verdictFn(ctx, in)
	trace := ""
	if err != nil {
		trace = "LLM 调用失败,降级规则: " + err.Error()
		s.logger.Warn("llm verdict failed, fallback to rule", "approval_id", ap.ID, "err", err)
		verdict, err = defaultVerdictFn(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("rule fallback verdict: %w", err)
		}
	}
	// 一审批一条:重跑先删旧。
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", ap.ID).Delete(&model.LLMVerdict{}).Error; err != nil {
		return nil, fmt.Errorf("clear old verdict: %w", err)
	}
	var stJSON []byte
	if verdict.SuggestStocktake != nil {
		stJSON, _ = json.Marshal(verdict.SuggestStocktake)
	}
	row := &model.LLMVerdict{
		ID:               uuid.NewString(),
		ApprovalID:       ap.ID,
		Conclusion:       verdict.Conclusion,
		Reason:           truncate(verdict.Reason, 2048),
		Confidence:       verdict.Confidence,
		SuggestStocktake: stJSON,
		RawJSON:          verdict.RawJSON,
		Source:           verdict.Source,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return nil, fmt.Errorf("insert verdict: %w", err)
	}
	s.logStep(ctx, ap, StepLLMVerdict,
		fmt.Sprintf("信号 %d 条", len(signals)),
		fmt.Sprintf("结论=%s 来源=%s 置信度=%s 原因=%s",
			verdict.Conclusion, verdict.Source, verdict.Confidence.StringFixed(3), truncate(verdict.Reason, 200)),
		trace, time.Since(start).Milliseconds())
	return verdict, nil
}

// stepStocktake 按 LLM 建议建针对性盘点计划 + 绑定 + taskcard 待办。
// 失败仅 log + 写 step 日志,不影响审批主结论。
//
// 幂等:同一审批已有 approval_stocktake_links 时直接跳过(重跑/RescanStuck
// 不会重复建单、不会重复推卡片)。
func (s *Service) stepStocktake(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult) {
	start := time.Now()
	st := verdict.SuggestStocktake
	if st == nil || len(st.IDs) == 0 {
		return
	}
	if s.stocktake == nil {
		s.logStep(ctx, ap, StepStocktake, "", "建议盘点但 stocktake client 未配置,跳过", "", time.Since(start).Milliseconds())
		return
	}
	var linkCount int64
	s.db.WithContext(ctx).Model(&model.ApprovalStocktakeLink{}).
		Where("approval_id = ?", ap.ID).Count(&linkCount)
	if linkCount > 0 {
		s.logStep(ctx, ap, StepStocktake, "",
			fmt.Sprintf("已存在 %d 条盘点计划绑定,跳过重跑建单(幂等)", linkCount),
			"", time.Since(start).Milliseconds())
		return
	}
	settlementIDs := s.settlementIDsOf(ctx, ap.ID)
	remark := fmt.Sprintf("结算付款审批 %s 机器审核建议盘点(scope=%s;结算单 %s)",
		ap.ID, st.Scope, strings.Join(settlementIDs, ","))
	in := StocktakeCreatePlanInput{
		BranchID:   ap.BranchID,
		OperatorID: ap.ApplicantUserID,
		Remark:     truncate(remark, 500),
	}
	// 仅 product scope 能直接落成计划商品;supplier/category 由盘点人自行圈品。
	if st.Scope == "product" {
		in.ProductIDs = st.IDs
	}
	// 透传申请人 JWT:stocktake 侧中间件把 Authorization 头转 cubeclient.WithBearer,
	// AddPlanItems 从 cube 现拉商品名/条码时 cube sidecar 的 bearer 中间件才不拒。
	if ap.ApplicantBearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", ap.ApplicantBearer)
	}
	header, err := s.stocktake.CreatePlanHeader(ctx, in)
	if err != nil {
		s.logger.Warn("create stocktake plan failed", "approval_id", ap.ID, "err", err)
		s.logStep(ctx, ap, StepStocktake, remark, "建盘点计划失败: "+err.Error(), "", time.Since(start).Milliseconds())
		return
	}
	link := &model.ApprovalStocktakeLink{
		ID:                uuid.NewString(),
		ApprovalID:        ap.ID,
		StocktakeHeaderID: header.ID,
	}
	if err := s.db.WithContext(ctx).Create(link).Error; err != nil {
		s.logger.Warn("insert stocktake link failed", "approval_id", ap.ID, "err", err)
	}
	// taskcard 待办通知盘点人。链接深链到盘点页并自动选中本计划单:
	// inventory.html?header_id=<id>(wx-h5 切片页;旧的 /00/#/stocktake/ hash
	// 路由已随切片改名 00_1000003 失效)。
	if len(s.cfg.StocktakeNotifyUsers) > 0 {
		planURL := strings.TrimSuffix(s.cfg.WxH5BaseURL, "/") + "/inventory.html?header_id=" + header.ID
		desc := fmt.Sprintf("结算单 %s 审核发现库存异常,已生成针对性盘点计划,请及时盘点。计划单 %s",
			strings.Join(settlementIDs, ","), header.ID)
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
		fmt.Sprintf("盘点计划单 %s 已创建并通知 %d 人", header.ID, len(s.cfg.StocktakeNotifyUsers)),
		"", time.Since(start).Milliseconds())
}

// stepFinish 按结论收尾:pass → apply_event;reject → notify_text。
func (s *Service) stepFinish(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, signals []signalDraft) {
	start := time.Now()
	if verdict.Conclusion == ConclusionReject {
		reason := truncate(verdict.Reason, 1024)
		payload := NotifyTextPayload{
			ToUser:  []string{ap.ApplicantWecomID},
			Content: fmt.Sprintf("【结算付款审批】机器审核未通过\n单号:%s\n原因:%s\n请核对后重新提交。", ap.ID, reason),
		}
		if _, err := s.publishCommand(ctx, CmdNotifyText, payload); err != nil {
			s.markFailed(ctx, ap, "publish notify_text: "+err.Error())
			return
		}
		if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
			Where("id = ?", ap.ID).Updates(map[string]any{
			"status": model.StatusRejected, "reject_reason": reason, "applicant_bearer": "",
		}).Error; err != nil {
			s.logger.Error("update rejected failed", "approval_id", ap.ID, "err", err)
			return
		}
		ap.Status = model.StatusRejected
		s.publishStatusEvent(ctx, &AuditStatusEvent{
			ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
			Status: ap.Status, At: s.now(),
		})
		s.logStep(ctx, ap, StepFinish, "", "已拒绝并通知申请人: "+truncate(reason, 200), "", time.Since(start).Milliseconds())
		return
	}

	// pass → 代提交企微审批
	applyData, summaryList := s.buildApplyData(ctx, ap, verdict, signals)
	process, err := buildProcess(ap)
	if err != nil {
		s.markFailed(ctx, ap, "build process: "+err.Error())
		return
	}
	payload := ApplyEventPayload{
		CreatorUserID:       ap.ApplicantWecomID,
		TemplateID:          s.cfg.TemplateID,
		UseTemplateApprover: 0, // 始终用申请人在 H5 选定的审批流(模板内无需配审批人)
		Process:             process,
		NotifyType:          1,
		ApplyData:           applyData,
		SummaryList:         summaryList,
	}
	cmdID, err := s.publishCommand(ctx, CmdApplyEvent, payload)
	if err != nil {
		s.markFailed(ctx, ap, "publish apply_event: "+err.Error())
		return
	}
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", ap.ID).Updates(map[string]any{
		"status": model.StatusApproved, "last_command_id": cmdID, "applicant_bearer": "",
	}).Error; err != nil {
		s.logger.Error("update approved failed", "approval_id", ap.ID, "err", err)
		return
	}
	ap.Status = model.StatusApproved
	ap.LastCommandID = cmdID
	s.publishStatusEvent(ctx, &AuditStatusEvent{
		ApprovalID: ap.ID, BranchID: ap.BranchID, UserID: ap.ApplicantUserID,
		Status: ap.Status, At: s.now(),
	})
	s.logStep(ctx, ap, StepFinish, "",
		fmt.Sprintf("机器审核通过,已提交企微审批(command_id=%s),等待人工终审", cmdID),
		"", time.Since(start).Milliseconds())
}

// buildProcess 把审批单快照上的审批人/抄送人映射为企微 applyevent 的
// process.node_list:审批人节点(type=1,或签 apv_rel=2)+ 抄送人节点(type=2)。
// 审批人提交时已校验非空,这里若读不出视为数据损坏。
func buildProcess(ap *model.ApprovalInstance) (*ProcessConfig, error) {
	var approvers, cc []string
	if len(ap.ApproverUserIDs) > 0 {
		if err := json.Unmarshal(ap.ApproverUserIDs, &approvers); err != nil {
			return nil, fmt.Errorf("解析审批人列表: %w", err)
		}
	}
	if len(ap.CcUserIDs) > 0 {
		_ = json.Unmarshal(ap.CcUserIDs, &cc)
	}
	if len(approvers) == 0 {
		return nil, fmt.Errorf("审批单未记录审批人(数据异常)")
	}
	nodes := []ProcessNode{{Type: 1, ApvRel: 2, UserID: approvers}}
	if len(cc) > 0 {
		nodes = append(nodes, ProcessNode{Type: 2, UserID: cc})
	}
	return &ProcessConfig{NodeList: nodes}, nil
}

// buildApplyData 组企微 apply_event 的 apply_data / summary_list。
//
// apply_data 用模板的三个控件(cfg.ApplyControlIDs,类型 cfg.ApplyControlTypes):
//
//	[0] 结算单号(逗号分隔)
//	[1] 付款金额(元)
//	[2] 备注(附机器审核结论摘要)
//
// 另可选追加「AI审核结论」多行文本控件(cfg.AIConclusionControlID 非空时):
// 写入 LLM 研判结论精简文案(aiConclusionText);模板未配/结论为空则跳过。
func (s *Service) buildApplyData(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, signals []signalDraft) (applyData, summaryList json.RawMessage) {
	ids := s.settlementIDsOf(ctx, ap.ID)
	type textValue struct {
		Text string `json:"text"`
	}
	type content struct {
		Control string    `json:"control"`
		ID      string    `json:"id"`
		Value   textValue `json:"value"`
	}
	warnCount := 0
	for _, d := range signals {
		if d.Level == model.SignalWarn {
			warnCount++
		}
	}
	remark := ap.Remark
	if warnCount > 0 {
		remark = fmt.Sprintf("%s(机器一审通过,%d 条警告请关注)", ap.Remark, warnCount)
	}
	texts := []string{strings.Join(ids, ","), ap.ManualAmountYuan.StringFixed(2), remark}
	contents := make([]content, 0, 4)
	for i, id := range s.cfg.ApplyControlIDs {
		ctl := s.cfg.ApplyControlTypes[i]
		if ctl == "" {
			ctl = "Text"
		}
		contents = append(contents, content{Control: ctl, ID: id, Value: textValue{Text: texts[i]}})
	}
	// 「AI审核结论」控件:模板后台为多行文本(Textarea),applyevent 文本类
	// 控件取值同 {"text":"..."};未配置或结论为空时跳过,不阻塞提交。
	if s.cfg.AIConclusionControlID != "" {
		if txt := aiConclusionText(verdict, signals); txt != "" {
			contents = append(contents, content{Control: "Textarea", ID: s.cfg.AIConclusionControlID, Value: textValue{Text: txt}})
		}
	}
	applyData, _ = json.Marshal(map[string]any{"contents": contents})
	summaryList, _ = json.Marshal(map[string]any{
		"summary_info": []map[string]string{
			{"text": fmt.Sprintf("结算单 %d 张 / 付款 %s 元", len(ids), ap.ManualAmountYuan.StringFixed(2)), "lang": "zh_CN"},
		},
	})
	return applyData, summaryList
}

// aiConclusionText 生成「AI审核结论」控件文案(多行文本,审批人可读)。
//
//	通过 → "机器审核通过:<LLM reason 精简>"(≤200 字,rune 安全截断)
//	拒绝 → 理论上不会走到 applyevent(reject 分支在 stepFinish 提前返回);
//	       防御性兜底:取首条 block 信号(≤40 字),无 block 则空(跳过控件)。
//	verdict 缺失 → 空(跳过控件,不阻塞提交)。
func aiConclusionText(verdict *VerdictResult, signals []signalDraft) string {
	if verdict == nil {
		return ""
	}
	if verdict.Conclusion == ConclusionPass {
		return truncateRunes("机器审核通过:"+verdict.Reason, 200)
	}
	for _, d := range signals {
		if d.Level == model.SignalBlock {
			return truncateRunes(d.Message, 40)
		}
	}
	return ""
}

// truncateRunes 按 rune 数截断(中文安全)。
// 与 truncate(字节上限+rune 边界回退)不同,这里上限语义是"字数"——
// 审批单控件里中文按字计,按字节截断会得到明显偏短且不可控的文案。
func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max])
}

// ---- 重跑 ----

// RetryApproval 人工重跑:仅 failed 实例允许;清旧 signals/logs/verdict 后重跑。
// bearer 是重跑请求侧的 Authorization 头 —— failed 时旧申请人 JWT 已清空,
// 必须从这里重新落库供流水线调 cube(见 runAudit 注入)。
func (s *Service) RetryApproval(ctx context.Context, id, bearer string) error {
	ap, err := s.loadApproval(ctx, id)
	if err != nil {
		return err
	}
	if ap.Status != model.StatusFailed {
		return fmt.Errorf("%w: 仅 failed 状态可重跑(当前 %s)", ErrInvalidStatus, ap.Status)
	}
	if bearer == "" {
		return fmt.Errorf("%w: 缺少 Authorization 头(重跑需要凭证调 cube)", ErrInvalidInput)
	}
	for _, m := range []any{&model.AuditSignal{}, &model.AuditLog{}, &model.LLMVerdict{}} {
		if err := s.db.WithContext(ctx).Where("approval_id = ?", id).Delete(m).Error; err != nil {
			return fmt.Errorf("clear old audit data: %w", err)
		}
	}
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("id = ?", id).Updates(map[string]any{
		"status": model.StatusReceived, "error": "", "reject_reason": "",
		"applicant_bearer": bearer,
	}).Error; err != nil {
		return err
	}
	go s.runAudit(context.Background(), id)
	return nil
}

// RescanStuck 扫卡死实例(received/auditing 超 10min 未更新)重跑;返回触发数。
// 由 dapr job(settlement-auditor-rescan,@every 5m)触发。
func (s *Service) RescanStuck(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-rescanStuckAfter)
	var ids []string
	if err := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("status IN ? AND updated_at < ?", []string{model.StatusReceived, model.StatusAuditing}, cutoff).
		Limit(50).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	for _, id := range ids {
		s.logger.Warn("rescan stuck approval", "approval_id", id)
		go s.runAudit(context.Background(), id)
	}
	return len(ids), nil
}

// ---- 查询 ----

// ApprovalDetail 审批详情(实例 + 快照 + 信号 + 结论 + 盘点绑定)。
type ApprovalDetail struct {
	Approval    *model.ApprovalInstance       `json:"approval"`
	Settlements []model.ApprovalSettlement    `json:"settlements"`
	Signals     []model.AuditSignal           `json:"signals"`
	Verdict     *model.LLMVerdict             `json:"verdict,omitempty"`
	Links       []model.ApprovalStocktakeLink `json:"stocktake_links"`
}

// ListApprovals 按 branch + 条件分页(created_at 降序)。
func (s *Service) ListApprovals(ctx context.Context, branchID, status, from, to string, limit, offset int) ([]model.ApprovalInstance, int64, error) {
	if branchID == "" {
		return nil, 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Model(&model.ApprovalInstance{}).Where("branch_id = ?", branchID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if from != "" {
		q = q.Where("created_at >= ?", from)
	}
	if to != "" {
		q = q.Where("created_at < ?", to)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var items []model.ApprovalInstance
	if err := q.Order("created_at DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// GetApprovalDetail 读详情(branch 隔离由 handler 校验后传入)。
func (s *Service) GetApprovalDetail(ctx context.Context, branchID, id string) (*ApprovalDetail, error) {
	var ap model.ApprovalInstance
	q := s.db.WithContext(ctx).First(&ap, "id = ?", id)
	if q.Error != nil {
		if q.Error == gorm.ErrRecordNotFound {
			return nil, ErrApprovalNotFound
		}
		return nil, q.Error
	}
	if branchID != "" && ap.BranchID != branchID {
		return nil, ErrApprovalNotFound // 跨 branch 视为不存在(不泄露)
	}
	d := &ApprovalDetail{Approval: &ap}
	s.db.WithContext(ctx).Where("approval_id = ?", id).Find(&d.Settlements)
	s.db.WithContext(ctx).Where("approval_id = ?", id).Order("created_at ASC").Find(&d.Signals)
	var verdict model.LLMVerdict
	if err := s.db.WithContext(ctx).First(&verdict, "approval_id = ?", id).Error; err == nil {
		d.Verdict = &verdict
	}
	s.db.WithContext(ctx).Where("approval_id = ?", id).Find(&d.Links)
	return d, nil
}

// GetLogs 读推理日志(created_at 升序)。
func (s *Service) GetLogs(ctx context.Context, branchID, approvalID string) ([]model.AuditLog, error) {
	if _, err := s.GetApprovalDetail(ctx, branchID, approvalID); err != nil {
		return nil, err
	}
	var logs []model.AuditLog
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", approvalID).Order("created_at ASC").Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}

// ExportRow 对账导出行(审批 + 勾选结算单快照)。
type ExportRow struct {
	Approval    *model.ApprovalInstance
	Settlements []model.ApprovalSettlement
}

// ExportApprovals 拉时间段内审批用于纸质打印对账(created_at 升序)。
func (s *Service) ExportApprovals(ctx context.Context, branchID, from, to string) ([]ExportRow, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Where("branch_id = ?", branchID)
	if from != "" {
		q = q.Where("created_at >= ?", from)
	}
	if to != "" {
		q = q.Where("created_at < ?", to)
	}
	var aps []model.ApprovalInstance
	if err := q.Order("created_at ASC").Limit(1000).Find(&aps).Error; err != nil {
		return nil, err
	}
	rows := make([]ExportRow, 0, len(aps))
	for i := range aps {
		var settlements []model.ApprovalSettlement
		s.db.WithContext(ctx).Where("approval_id = ?", aps[i].ID).Find(&settlements)
		rows = append(rows, ExportRow{Approval: &aps[i], Settlements: settlements})
	}
	return rows, nil
}
