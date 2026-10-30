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
	"sort"
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
	// StepFlowDecision 是**流程定制**的结论步骤（2026-10-28 用户口径）。
	//
	// ⚠️ 与 StepLLMVerdict 分开记：审批详情页的时间线上必须能看出
	//    "这一单没问机器人，是配置放行的"，否则店长会误以为机器人审过了。
	StepFlowDecision    = "flow_decision"
	// StepAdminApprove 是**管理员人工放行**的审计步骤（2026-10-28 用户口径）。
	StepAdminApprove    = "admin_approve"
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
	// FlowSelections 是按**节点类型**分桶的人员选择(键:approver/handler/cc)。
	//
	// 节点集合由 Config.ApprovalFlow 决定(与企微模板的流程设置对应)——
	// 模板加一个节点,协议不用改。
	FlowSelections FlowSelections
	// ApproverUserIDs / HandlerUserIDs / CcUserIDs 是**按固定字段**给的兼容入参。
	//
	// ⚠️ 只给这些字段 ⇒ 拿不到"办理人"节点(旧客户端/旧契约测试不知道这回事)。
	// 新客户端一律用 FlowSelections。
	ApproverUserIDs []string
	HandlerUserIDs  []string
	CcUserIDs       []string
	// Attachments 是**发起人自己上传**的附件(拍照/相册选的图片、文件)。
	//
	// 随审批一路活到企微审批单的「附件」控件上(审批人直接看得到发票照片)。
	// ⚠️ 上限:最多 3 个,单个解码后 ≤2MB(见 maxAttachmentsPerApproval)。
	Attachments []AttachmentInput
	// ReceiptItems / PaymentItems 是**手工收付款明细行**(2026-10-25 用户需求)。
	//
	// 供应商结算单不在这里 —— 它是「付款类型-货款」,由勾选结果派生
	// (事实源是 cube 快照,见 service/items.go)。
	//
	// 收款项冲减付款金额 ⇒ 应付金额 = 货款 + 付款项 − 收款项(不为负)。
	ReceiptItems []ItemInput
	PaymentItems []ItemInput
	// BankAccountID 是发起人选中的**供应商收款账号 id**(可选)。
	//
	// 用户口径(2026-10-28):H5「申请付款审批」页选供应商收款账号,
	// 选中后按行写进企微模板的「付款信息」控件(开户行/账号/户名)。
	//
	// ⚠️ **前端只传 id,不传账号文本** —— 账号信息由**服务端**按 id 回查
	// bank-account 服务取得(见 service/payee_account.go)。
	// 否则客户端可以伪造收款账号,让审批人看到假的付款目标。
	//
	// 空 ⇒ 本次审批不带「付款信息」(选了结算单但没选账号是允许的)。
	BankAccountID string
	// StocktakeHeaderIDs 是「重新发起」时**关联的盘点计划单**(2026-10-30 用户口径)。
	//
	// 被机器拒绝的那一单曾自动建计划盘点单;盘点审核完成后从「审批记录」
	// 重新发起,把那张盘点单关联进来 ⇒ 机器审核看到"库存已盘点核对",
	// 数量类信号不再阻断,从而通过机器人审核。
	//
	// ⚠️ 前端只传 header id;是否存在/属于本门店/是否已审核由服务端核对。
	StocktakeHeaderIDs []string
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
	// ⚠️ 手工金额允许为 **0**:纯收款项(只有"纸皮回收"这类)应付金额就是 0
	//   (用户口径 2026-10-25:「付款审批可以只有'纸皮回收'」)。
	//   真的填错了(应付 > 0 却填 0)由金额勾稽(check_amounts)拦,不在这层拦。
	if in.ManualAmountYuan.IsNegative() {
		return nil, fmt.Errorf("%w: manual_amount_yuan 不能为负", ErrInvalidInput)
	}
	ids := dedupeStrings(in.SettlementIDs)
	// ⚠️ 结算单可以**一张都不勾** —— 前提是填了收/付款明细。
	//
	// 用户口径（2026-10-25）：「付款审批可以只有'货款'，只有'纸皮回收'」——
	// 纯收款项（只有纸皮回收这类）没有货款，也就没有结算单。
	// 没有明细也没有结算单 ⇒ 是一张空单，必须拒。
	if len(ids) == 0 && len(in.ReceiptItems) == 0 && len(in.PaymentItems) == 0 {
		return nil, fmt.Errorf("%w: settlement_ids 至少 1 张,或填收/付款明细", ErrInvalidInput)
	}
	if len(ids) > maxSettlementsPerApproval {
		return nil, fmt.Errorf("%w: 单次审批最多 %d 张结算单", ErrInvalidInput, maxSettlementsPerApproval)
	}
	// 结算单「待审批中」不可重复提交（2026-10-29 用户口径）。
	//
	// 为什么不能只靠列表过滤：列表过滤是**读时**的 —— 页面开着的时候
	// 另一台设备（或另一个人）可能已经把同一张单提交走了；
	// 也可能有人直接调接口提交。⇒ 提交时必须**再校验一次**。
	//
	// 校验通过后那张单被拒 ⇒ 它自动恢复可选（判定见 pending_approval.go）。
	pending, err := s.ListPendingSettlements(ctx, branchID, ids)
	if err != nil {
		return nil, fmt.Errorf("check pending settlements: %w", err)
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrSettlementPending, describePendingSettlements(pending))
	}
	// 附件:个数上限 + 逐个解码校验(base64 坏/超大 ⇒ 当场拒,别留到流水线里炸)。
	if len(in.Attachments) > maxAttachmentsPerApproval {
		return nil, fmt.Errorf("%w: 附件最多 %d 个", ErrInvalidInput, maxAttachmentsPerApproval)
	}
	attachments := make([]AttachmentInput, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		va, err := validateAttachment(a)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, va)
	}
	// 收付款明细:类型/备注/金额/行数校验(见 service/items.go)。
	receiptItems, err := validateItems(ItemDirectionReceipt, in.ReceiptItems)
	if err != nil {
		return nil, err
	}
	paymentItems, err := validateItems(ItemDirectionPayment, in.PaymentItems)
	if err != nil {
		return nil, err
	}
	// 审批流人员:按**节点类型**分桶(键 approver/handler/cc)。
	//
	// 节点集合与校验规则都来自 Config.ApprovalFlow —— 那份声明对应企微
	// 管理后台里「付款审批」模板的流程设置(节点类型/顺序/选人方式)。
	// ⚠️ 旧客户端只发 approver_userids/cc_userids ⇒ 合并进来,仍能走通
	// (只是"办理人"节点拿不到人 ⇒ 按 required 判定给出可读报错)。
	selections := in.FlowSelections.Merge(in.ApproverUserIDs, in.HandlerUserIDs, in.CcUserIDs)
	flow, err := s.resolveFlowSelections(selections)
	if err != nil {
		return nil, err
	}
	approversJSON, _ := json.Marshal(flow[FlowNodeApprover])
	handlersJSON, _ := json.Marshal(flow[FlowNodeHandler])
	ccJSON, _ := json.Marshal(flow[FlowNodeCC])

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
		HandlerUserIDs:   handlersJSON,
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
	// 落发起人上传的附件(机器审核是异步的 ⇒ 字节必须活到 pass 那一刻)。
	if err := s.storeAttachments(ctx, ap, attachments); err != nil {
		return nil, err
	}
	// 落手工收付款明细(机器审核异步 ⇒ 审核时要读得到它们)。
	if err := s.storeItems(ctx, ap, receiptItems, paymentItems); err != nil {
		return nil, err
	}
	// 落收款账号快照(2026-10-28 用户口径:选中收款账号后按行写进企微「付款信息」控件)。
	//
	// ⚠️ **同步取号**(提交那一刻就查 bank-account),而不是等流水线里再取:
	//   流水线是异步的(可能几分钟后才走到 applyevent),那时账号可能已被改/删
	//   ⇒ 审批单上会显示一个"当时不存在的账号",或者干脆取不到。
	//
	// ⚠️ 取不到**不阻断提交**:没选账号是合法的;选了但取不到是异常,
	//   记 warn 让审批人自己去银行核对 —— 比"整个提交失败"更可用。
	if err := s.resolveAndStorePayeeAccount(ctx, ap, in.BankAccountID); err != nil {
		logPayeeFetchError(s.logger, ap.ID, err)
	}
	// 关联盘点计划单(重新发起路径,2026-10-30):落 approval_stocktake_links。
	//
	// ⚠️ 校验不过**不阻断提交**(只是失去放宽依据):盘点单被删/换了门店
	// 都不该让这一单提交失败。
	if err := s.storeStocktakeLinks(ctx, ap, in.StocktakeHeaderIDs); err != nil {
		s.logger.Warn("store stocktake links failed", "approval_id", ap.ID, "err", err)
	}

	// 后台异步审核(detach ctx:请求返回后流水线继续)。
	go s.runAudit(context.Background(), ap.ID)

	// 记住本次审批流人员(下次发起默认带出,对齐官方审批体验);失败不阻断提交。
	//
	// ⚠️ 三个节点都要记:模板里审批人/办理人/抄送人都是「申请人自选」,
	// 下次发起时把上次选的**都**带出来,店长只改要改的那个。
	if len(flow) > 0 {
		if err := s.upsertApproverPreference(ctx, branchID, in.ApplicantUserID, flow); err != nil {
			s.logger.Warn("upsert approver preference failed",
				"approval_id", ap.ID, "user_id", in.ApplicantUserID, "err", err)
		}
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

	// 手工收付款明细(读库)—— 审核要用它算应付金额与放宽额度。
	items := s.loadApprovalItems(ctx, ap.ID)
	allowance := buildAllowance(receiptItemsOf(items))

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

	// step 4: 金额勾稽(比的是**应付金额** = 货款 + 付款项 − 收款项)
	amountCheck := s.stepCheckAmounts(ctx, ap, settlements, linesBySettlement, &signals, items)

	// step 5: 数量信号(采购量 vs 销售) + 毛利额度信号(补差价)
	margins, err := s.stepCheckQuantity(ctx, ap, linesBySettlement, sheets, &signals, allowance)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// step 5.5: 收款项放宽(有陈列费/堆头费 ⇒ 进货可以多些)
	//
	// 放在数量信号之后、落库之前 ⇒ 落库的信号级别就是放宽后的级别,
	// LLM 与规则降级路径看到的也是同一份。
	signals = applyReceiptAllowance(signals, allowance)

	// step 5.6: 盘点核对放宽(关联盘点单已审核通过 ⇒ 数量类信号不再阻断)
	//
	// 用户口径(2026-10-30):「盘点审核后重新提交付款审批可以通过机器人审核」。
	// 盘点单是**库存事实的核对结果** ⇒ 数量类信号(自入库无销售/数量偏离)
	// 的疑问已被盘点回答,不应再拦这一单。
	verified := s.verifiedStocktakes(ctx, ap)
	signals = applyStocktakeVerification(signals, verified)

	// 信号统一落库
	if err := s.saveSignals(ctx, ap.ID, signals); err != nil {
		s.markFailed(ctx, ap, "save signals: "+err.Error())
		return
	}

	// step 6: 出结论 —— 命中流程定制配置 ⇒ 用流程结论；没命中 ⇒ 现有机器人(LLM)研判
	extras := AuditExtras{Margins: *marginsOrZero(margins), Stocktakes: verified}
	verdict, err := s.decideVerdict(ctx, ap, settlements, linesBySettlement, sheets, signals, &amountCheck, items, allowance, extras)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
		return
	}

	// step 7(可选): 建议盘点 → 建 plan 单 + 绑定 + taskcard
	s.stepStocktake(ctx, ap, verdict)

	// finish: pass → apply_event;reject → notify_text
	s.stepFinish(ctx, ap, verdict, signals, settlements, items)
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
	// 补供应商名称（2026-10-29 用户口径：LLM 结论里的供应商要显示「<名称>-<编号>」）。
	//
	// ⚠️ cube 的 JOIN 只支持 live↔live，settlement 是 live、supplier 是 duck
	// ⇒ 只能在 Go 侧 join（与 fillSupplierNames 同一套路）。
	// 字典取不到时名称留空 ⇒ 提示词里只给编号（同 fillSupplierNames 的降级取舍）。
	s.fillSupplierNames(ctx, ap.BranchID, rows)
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

// stepCheckAmounts check 3(纯函数,无 cube 调用),并把金额勾稽结论带出来。
//
// 返回 AmountCheck 是给 LLM 用的:金额结论必须由引擎算好后原样传递,
// 不能让 LLM 拿着 user prompt 里的**显示值**自己相加 ——
// 2026-10-22 生产实踩,它把"差 0.0054 元"算成了"差 0.01 元"并当成 block 理由拒单。
func (s *Service) stepCheckAmounts(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, signals *[]signalDraft, items []model.ApprovalItem) AmountCheck {
	start := time.Now()
	before := len(*signals)
	tol := decimal.NewFromFloat(s.cfg.AmountToleranceYuan)
	payTol := decimal.NewFromFloat(s.cfg.PaymentToleranceYuan)
	// 手工付款项/收款项合计(货款来自 cube 结算单,不在这里)。
	paymentTotal := sumItemsByDirection(items, ItemDirectionPayment)
	receiptTotal := sumItemsByDirection(items, ItemDirectionReceipt)
	sigs, summary := checkAmounts(settlements, linesBySettlement, ap.ManualAmountYuan, tol, payTol, paymentTotal, receiptTotal)
	*signals = append(*signals, sigs...)
	verdict := "不一致"
	if summary.Consistent {
		verdict = "一致(差额在付款取整容差内,属正常取整)"
	}
	s.logStep(ctx, ap, StepCheckAmounts,
		fmt.Sprintf("手工金额 %s,付款项 %s,收款项 %s,数值容差 %s,付款取整容差 %s",
			yuanText(ap.ManualAmountYuan), yuanText(summary.PaymentTotalYuan), yuanText(summary.ReceiptTotalYuan),
			yuanText(tol), yuanText(payTol)),
		fmt.Sprintf("新增 %d 条金额信号;货款合计 %s,应付 %s,差额 %s,结论:%s",
			len(*signals)-before, yuanText(summary.SettledTotalYuan), yuanText(summary.PayableYuan),
			yuanText(summary.DiffYuan), verdict),
		"", time.Since(start).Milliseconds())
	return summary
}

// stepCheckQuantity 聚合 PI 收货量 + 按"最后一次采购入库日"起算的销量 → check 4。
//
// # 销量窗口(2026-10-22 用户口径)
//
// **从最后一次采购的采购入库单日期到当前日期**,按商品分别取。
// 详见 buildSaleWindowGroups 的注释。
//
// # 商品名
//
// 信号里写「<商品名称>-<商品货号>」(ProductLabel)⇒ 这里要先把名字查出来。
// 名字查不到只 warn 不失败:回退成纯货号,不影响主结论。
func (s *Service) stepCheckQuantity(ctx context.Context, ap *model.ApprovalInstance, linesBySettlement map[string][]SettlementLineRow, sheets []PurchaseSheetRow, signals *[]signalDraft, allowance Allowance) (*MarginSummary, error) {
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
		return nil, nil
	}
	lines, err := s.cube.ListSheetLines(ctx, ap.BranchID, piSheets)
	if err != nil {
		return nil, fmt.Errorf("fetch sheet lines: %w", err)
	}
	received := make(map[string]decimal.Decimal)
	productSheets := make(map[string][]string) // 商品 → 它出现过的采购单
	productSet := make(map[string]struct{})
	for _, ln := range lines {
		if ln.ProductID == "" {
			continue
		}
		received[ln.ProductID] = received[ln.ProductID].Add(ln.ReceivedQuantity)
		productSet[ln.ProductID] = struct{}{}
		if !containsString(productSheets[ln.ProductID], ln.SheetID) {
			productSheets[ln.ProductID] = append(productSheets[ln.ProductID], ln.SheetID)
		}
	}
	products := make([]string, 0, len(productSet))
	for p := range productSet {
		products = append(products, p)
	}
	sort.Strings(products)

	// 每张 PI 单的制单日期(同一单多行时取最晚的一条,防御脏数据)。
	sheetDates := make(map[string]string)
	for _, sh := range sheets {
		if sh.DocumentType != "PI" || sh.CreatedAt == "" {
			continue
		}
		if cur, ok := sheetDates[sh.ID]; !ok || sh.CreatedAt > cur {
			sheetDates[sh.ID] = sh.CreatedAt
		}
	}
	// 窗口终点 = 明天(开区间)⇒ **今天当天的销售也算在内**。
	// 源表 t_rm_daysum 当天就有数据(实测卖到当天),用今天做上界会把当天的销量漏掉。
	now := s.now()
	today := now.Format("2006-01-02")
	toDate := now.AddDate(0, 0, 1).Format("2006-01-02")
	// 取不到采购日期时的回退窗口(老口径:今天往前 SaleWindowDays 天)。
	fallbackFrom, _ := s.saleWindowDates()

	groups := buildSaleWindowGroups(sheetDates, productSheets, today, fallbackFrom, 0)
	salesByProduct := make(map[string]SaleRow, len(products))
	for _, g := range groups {
		sales, err := s.cube.SumSalesByProduct(ctx, ap.BranchID, g.Products, g.From, toDate)
		if err != nil {
			return nil, fmt.Errorf("fetch sales: %w", err)
		}
		for _, r := range sales {
			salesByProduct[r.ProductID] = r
		}
	}

	// 商品名(信号里要写「<名称>-<货号>」);查不到只回退成纯货号。
	names, err := s.cube.ListProductNames(ctx, ap.BranchID, products)
	if err != nil {
		s.logger.Warn("settlement-auditor: 取商品名失败,信号里只写货号",
			"approval_id", ap.ID, "err", err)
		names = nil
	}

	before := len(*signals)
	*signals = append(*signals, checkQuantity(received, salesByProduct, s.cfg.QtyDeviationFactor, QuantityContext{
		Names:      names,
		WindowFrom: windowFromByProduct(groups),
	})...)
	// 毛利额度信号(见 service/allowance.go)。
	//
	// 成本用**库存加权平均成本价**估算(cube `stock.avg_cost` = 思迅「库存查询」
	// 里的平均成本价)—— 这与财务核对的数是**同一个数**。
	//
	// ⚠️ 2026-10-29 换过口径(生产实踩):旧口径是"本次进货均价",
	//    拿最新一批的进价当成本 ⇒ 生鲜类商品必偏(蜜梨-01860 算成 -280.57 元,
	//    而思迅库存里的平均成本价 2.82 明明是正毛利)。
	//    ⇒ 优先 avg_cost,拿不到才回退本次进货均价(见 mergeCostBasis)。
	//
	// ⚠️ 逐 SKU 一起算(perSKU):整体毛利为正时,亏钱的 SKU 会被利润 SKU 盖住,
	//    只看整体就发现不了"这批货卖一单亏一单"(2026-10-29 用户口径)。
	cost := s.costBasisFor(ctx, ap.BranchID, products, lines)
	gm := estimateGrossMargin(cost, salesByProduct)
	perSKU := estimateProductMargins(cost, salesByProduct)
	gmSignals := checkGrossMarginWithAllowance(gm, allowance, perSKU, names)
	*signals = append(*signals, gmSignals...)

	// ---- 实现毛利（2026-10-29 用户口径）----
	//
	// 窗口毛利问的是"这批货整体上赚不赚钱"（成本用库存加权平均成本价）；
	// 实现毛利问的是"**这批新进的货**按它自己的进价卖出去赚不赚钱" ——
	// 后者才是"要不要付这笔货款"的依据。
	//
	// 口径：用本批次进货均价与【最早采购日 ~ 发起付款审批当天】的销售均价，
	// 量取 min(本批次进货总量, 窗口销售总量)。
	realizedSales, realizedFrom := s.realizedWindowSales(
		ctx, ap.BranchID, sheetDates, productSheets, today, toDate, fallbackFrom)
	realized := estimateRealizedMargins(
		batchPurchaseByProduct(lines), realizedSales, realizedFrom)
	realizedNeg := negativeRealizedMargins(realized)
	if len(realizedNeg) > 0 {
		*signals = append(*signals, checkRealizedMargin(realized, names)...)
	}

	avgCnt, purchaseCnt := costBasisCounts(cost)
	negGross := negativeProductMargins(perSKU)
	summary := &MarginSummary{GrossNegCount: len(negGross), RealizedNegCount: len(realizedNeg)}
	for _, pm := range negGross {
		summary.GrossNegYuan = summary.GrossNegYuan.Add(pm.AmountYuan)
	}
	for _, rm := range realizedNeg {
		summary.RealizedNegYuan = summary.RealizedNegYuan.Add(rm.AmountYuan)
	}
	s.logStep(ctx, ap, StepCheckQuantity,
		fmt.Sprintf("PI 单 %d 张,商品 %d 种,窗口 %s~%s(按各商品最后一次采购入库日起算,取不到则回退 %s)",
			len(piSheets), len(products), groups[0].From, toDate, fallbackFrom),
		fmt.Sprintf("新增 %d 条数量信号;窗口毛利估算 %s 元(成本口径:库存加权平均成本价 %d 个商品,回退本次进货均价 %d 个),补差价额度 %s 元,毛利为负的商品 %d 个;实现毛利为负的商品 %d 个",
			len(*signals)-before, yuanText(gm.AmountYuan), avgCnt, purchaseCnt,
			yuanText(allowance.PriceDiffYuan),
			len(negGross),
			len(realizedNeg)),
		"", 0)
	return summary, nil
}

// realizedWindowSales 拉「实现毛利」口径要用的窗口销售汇总。
//
// # 窗口口径（2026-10-29 用户口径）
//
// 起点 = 该 SKU 在**本单采购单**里的**最早**制单日期；
// 终点 = 发起付款审批当天（开区间到明天 ⇒ 当天的销售也算在内，与窗口毛利一致）。
//
// ⚠️ 与窗口毛利的窗口**不是同一个**：窗口毛利取**最晚**采购日起算
//    （"这批货进来到今天卖了多少"），实现毛利取**最早**采购日起算
//    （"这批货从进店那天起一共卖了多少钱/多少量"）。
//
// # 性能
//
// 一次 sale_day 查询 ≈ 1.2s（源表 120 万行）⇒ 不能每个商品查一次。
// ⇒ 与窗口毛利同一手法：**按起点日期归组**，一组一次查询，
// 组数上限 [maxSaleWindowGroups]。
func (s *Service) realizedWindowSales(
	ctx context.Context,
	branchID string,
	sheetDates map[string]string,
	productSheets map[string][]string,
	today, toDate, fallbackFrom string,
) (map[string]SaleRow, map[string]string) {
	groups := buildSaleWindowGroupsByDate(
		sheetDates, productSheets, today, fallbackFrom, maxSaleWindowGroups, windowStartEarliest)
	sales := make(map[string]SaleRow)
	fromByProduct := make(map[string]string)
	for _, g := range groups {
		rows, err := s.cube.SumSalesByProduct(ctx, branchID, g.Products, g.From, toDate)
		if err != nil {
			// 拉不到 ⇒ 这一组不参与实现毛利口径（宁可少算一个商品，
			// 也不要拿半个数据编出一个"亏损"结论）。
			s.logger.Warn("settlement-auditor: 取实现毛利窗口销量失败,该组跳过",
				"branch_id", branchID, "from", g.From, "products", len(g.Products), "err", err)
			continue
		}
		for _, r := range rows {
			sales[r.ProductID] = r
		}
		for _, pid := range g.Products {
			fromByProduct[pid] = g.From
		}
	}
	return sales, fromByProduct
}

// unitCostByProduct 按货号算本次进货均价(元/件)。
//
// 口径:Σ(该商品各行进货金额) ÷ Σ(该商品各行收货量)。
//
// ⚠️ **它不再是毛利估算的主口径**(2026-10-29):主口径是库存加权平均成本价
//    (stock.avg_cost),这里只作为**回退**(拿不到库存成本时用)。
//    理由见 service/allowance.go 的 GrossMargin 注释(生产实踩)。
//
// ⚠️ 只用于**毛利估算**(相对判断),不是财务成本口径 —— 引擎没有历史成本。
func unitCostByProduct(lines []SheetLineRow) map[string]decimal.Decimal {
	amount := make(map[string]decimal.Decimal)
	qty := make(map[string]decimal.Decimal)
	for _, ln := range lines {
		if ln.ProductID == "" {
			continue
		}
		amount[ln.ProductID] = amount[ln.ProductID].Add(ln.TotalAmountYuan)
		qty[ln.ProductID] = qty[ln.ProductID].Add(ln.ReceivedQuantity)
	}
	out := make(map[string]decimal.Decimal, len(amount))
	for pid, a := range amount {
		q := qty[pid]
		if !q.GreaterThan(decimal.Zero) {
			continue
		}
		out[pid] = a.Div(q)
	}
	return out
}

// mergeCostBasis 合并两种成本口径,得到每个货号最终用的成本。
//
// # 优先级:库存加权平均成本价 > 本批次进货均价
//
// 库存加权平均成本价 = 思迅「库存查询」里的**平均成本价**,是财务核对的同一个数;
// 本批次进货均价 = 这一笔进货的价格,只在该商品**没有库存成本记录**时回退。
//
// ⚠️ "没有库存成本记录"有两种:① cube 里查不到这个商品(没维护/没同步);
//    ② avg_cost = 0(库存为 0 或成本没维护)—— 两种都要回退,否则该 SKU
//    会因为"没成本"而**整个不进毛利口径**(静默少算一个商品)。
func mergeCostBasis(batch map[string]decimal.Decimal, avgCost map[string]decimal.Decimal) map[string]CostBasis {
	out := make(map[string]CostBasis, len(batch)+len(avgCost))
	for pid, c := range avgCost {
		if !c.GreaterThan(decimal.Zero) {
			continue
		}
		out[pid] = CostBasis{UnitCostYuan: c, Source: CostSourceAvgCost}
	}
	for pid, c := range batch {
		if !c.GreaterThan(decimal.Zero) {
			continue
		}
		if _, ok := out[pid]; ok {
			continue // 已有库存成本口径 ⇒ 本批次进货价不再覆盖
		}
		out[pid] = CostBasis{UnitCostYuan: c, Source: CostSourcePurchase}
	}
	return out
}

// costBasisFor 取本次审批涉及商品的**成本口径**。
//
// 优先 cube `stock.avg_cost`(思迅库存加权平均成本价);
// 拉不到(查不到该商品 / cube 报错)时回退本批次进货均价。
//
// ⚠️ 拉库存成本失败**只告警不阻断**:毛利信号本来就是"估算 + 永不 block",
//    因一个旁路查询失败就让整单审核失败是本末倒置。
func (s *Service) costBasisFor(ctx context.Context, branchID string, products []string, lines []SheetLineRow) map[string]CostBasis {
	batch := unitCostByProduct(lines)
	avgCost := make(map[string]decimal.Decimal)
	if s.cube != nil && len(products) > 0 {
		stocks, err := s.cube.GetStocks(ctx, branchID, products)
		if err != nil {
			s.logger.Warn("settlement-auditor: 取库存成本失败,毛利估算回退本次进货均价",
				"branch_id", branchID, "err", err)
		} else {
			for _, st := range stocks {
				id := strings.TrimSpace(st.ProductID)
				if id == "" {
					continue
				}
				avgCost[id] = st.AvgCost
			}
		}
	}
	return mergeCostBasis(batch, avgCost)
}

// costBasisCounts 统计两种口径各覆盖了多少个商品(日志/信号里要能说清按哪个口径算的)。
func costBasisCounts(cost map[string]CostBasis) (avgCost int, purchase int) {
	for _, c := range cost {
		switch c.Source {
		case CostSourceAvgCost:
			avgCost++
		default:
			purchase++
		}
	}
	return avgCost, purchase
}

// containsString 判断字符串切片里是否已有该值(小规模,线性扫足够)。
func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
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
func (s *Service) stepLLMVerdict(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow, linesBySettlement map[string][]SettlementLineRow, sheets []PurchaseSheetRow, signals []signalDraft, amount *AmountCheck, items []model.ApprovalItem, allowance Allowance, extras ...AuditExtras) (*VerdictResult, error) {
	start := time.Now()
	var ex AuditExtras
	if len(extras) > 0 {
		ex = extras[0]
	}
	in := &AuditInput{
		Approval:    ap,
		Settlements: settlements,
		Lines:       linesBySettlement,
		Sheets:      sheets,
		Signals:     signals,
		Amount:      amount,
		Items:       buildItemRows(settlements, items),
		Totals:      totalsOf(settlements, items),
		Allowance:   allowance,
		Margins:     ex.Margins,
		Stocktakes:  ex.Stocktakes,
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
	// 通过结论必须带**毛利合计**(2026-10-30 用户口径:不能缺少总和)。
	// LLM 漏了就由引擎补一句 —— 总不能因为模型没写,审批人就看不到总账。
	if verdict.Conclusion == ConclusionPass {
		verdict.Reason = ensureMarginTotals(verdict.Reason, ex.Margins)
	}
	if err := s.saveVerdict(ctx, ap, verdict, trace, StepLLMVerdict, len(signals),
		time.Since(start).Milliseconds()); err != nil {
		return nil, err
	}
	return verdict, nil
}

// saveVerdict 落 llm_verdicts（一审批一条，重跑先删旧）+ 记一条 step 日志。
//
// ⚠️ **流程定制与 LLM 研判共用这张表**：靠 source 区分（llm / rule_fallback /
//    flow_config）。这样"这一单当时是怎么得出结论的"在审批详情页只有一个出处，
//    不会出现"详情页显示 LLM 结论、但实际是配置放行"这种对不上的情况。
func (s *Service) saveVerdict(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, trace, step string, signalCount int, durMs int64) error {
	// 一审批一条:重跑先删旧。
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", ap.ID).Delete(&model.LLMVerdict{}).Error; err != nil {
		return fmt.Errorf("clear old verdict: %w", err)
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
		return fmt.Errorf("insert verdict: %w", err)
	}
	s.logStep(ctx, ap, step,
		fmt.Sprintf("信号 %d 条", signalCount),
		fmt.Sprintf("结论=%s 来源=%s 置信度=%s 原因=%s",
			verdict.Conclusion, verdict.Source, verdict.Confidence.StringFixed(3), truncate(verdict.Reason, 200)),
		trace, durMs)
	return nil
}

// maxStocktakeLinksPerApproval 单次审批可关联的盘点单上限(防滥用/脏数据)。
const maxStocktakeLinksPerApproval = 5

// storeStocktakeLinks 落「本审批 ↔ 盘点计划单」绑定(重新发起路径)。
//
// 只存**核对通过**的 id:盘点单不存在 / 不属于本门店 ⇒ 跳过(丢了放宽依据,
// 但不会让审批带上别人的盘点单)。
func (s *Service) storeStocktakeLinks(ctx context.Context, ap *model.ApprovalInstance, headerIDs []string) error {
	ids := dedupeStrings(headerIDs)
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxStocktakeLinksPerApproval {
		ids = ids[:maxStocktakeLinksPerApproval]
	}
	for _, hid := range ids {
		if s.stocktake != nil {
			h, err := s.stocktake.GetHeader(ctx, hid)
			if err != nil {
				s.logger.Warn("verify stocktake header failed", "approval_id", ap.ID,
					"header_id", hid, "err", err)
				continue
			}
			if ap.BranchID != "" && h.BranchID != "" && h.BranchID != ap.BranchID {
				s.logger.Warn("stocktake header branch mismatch", "approval_id", ap.ID,
					"header_id", hid, "header_branch", h.BranchID, "approval_branch", ap.BranchID)
				continue
			}
		}
		link := &model.ApprovalStocktakeLink{
			ID:                uuid.NewString(),
			ApprovalID:        ap.ID,
			StocktakeHeaderID: hid,
		}
		if err := s.db.WithContext(ctx).Create(link).Error; err != nil {
			return fmt.Errorf("create stocktake link %s: %w", hid, err)
		}
	}
	return nil
}

// marginsOrZero 把可空的毛利汇总转成零值(避免解引用 nil)。
func marginsOrZero(m *MarginSummary) *MarginSummary {
	if m == nil {
		return &MarginSummary{}
	}
	return m
}

// verifiedStocktakes 读本审批关联的盘点单,只保留**已审核通过**的。
//
// ⚠️ 跨服务调用失败只 warn 不阻断:盘点信息是**放宽依据**,
// 取不到就退回"没有盘点"的严格口径,不该让整条审核失败。
func (s *Service) verifiedStocktakes(ctx context.Context, ap *model.ApprovalInstance) []StocktakeVerification {
	var links []model.ApprovalStocktakeLink
	if err := s.db.WithContext(ctx).Where("approval_id = ?", ap.ID).Find(&links).Error; err != nil {
		s.logger.Warn("load stocktake links failed", "approval_id", ap.ID, "err", err)
		return nil
	}
	if len(links) == 0 || s.stocktake == nil {
		return nil
	}
	out := make([]StocktakeVerification, 0, len(links))
	for _, l := range links {
		h, err := s.stocktake.GetHeader(ctx, l.StocktakeHeaderID)
		if err != nil {
			s.logger.Warn("get stocktake header failed", "approval_id", ap.ID,
				"header_id", l.StocktakeHeaderID, "err", err)
			continue
		}
		if h.BranchID != "" && ap.BranchID != "" && h.BranchID != ap.BranchID {
			continue
		}
		if h.Status != stocktakeStatusApproved {
			continue
		}
		out = append(out, StocktakeVerification{HeaderID: h.ID, Status: h.Status})
	}
	return out
}

// stocktakeStatusApproved 是盘点单「已审核」状态字面量
// (与 internal/stocktake/model.StatusApproved 同值;本包不跨引 stocktake 模型)。
const stocktakeStatusApproved = "approved"

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
	// 透传申请人 JWT **和门店** 给 stocktake:
	//   - Authorization:AddPlanItems 从 cube 现拉商品名/条码时 cube 侧要认这个凭证;
	//   - X-Branch-ID:cube-router 的 /v1/load 挂着 rbac.RequireScopeWithBranch("cube:read"),
	//     门店从 header 取 —— 不带就是 400 branch_required。
	//
	// ⚠️ 2026-10-22 生产实踩:只传 Authorization、不传门店 ⇒ stocktake 侧
	// 建单 500(Internal Server Error)⇒ 审批日志里"建盘点计划失败",
	// 而拒绝理由里还写着"建议针对性盘点" —— 盘点单根本没建出来。
	if ap.ApplicantBearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", ap.ApplicantBearer)
	}
	if ap.BranchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", ap.BranchID)
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
			return
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
}

// notifyTargetText 描述本次通知发给了谁(写进 step 日志,便于排查"没收到通知")。
func notifyTargetText(group *model.StocktakeNotifyGroup, users []string) string {
	switch {
	case group != nil:
		name := group.ChatName
		if name == "" {
			name = group.ChatID
		}
		return "群「" + name + "」(" + group.ChatID + ")"
	case len(users) > 0:
		return fmt.Sprintf("%d 位成员", len(users))
	default:
		return "未配置(既没有群也没有成员)"
	}
}

// stepFinish 按结论收尾:pass → apply_event;reject → notify_text。
//
// settlements 是本次勾选的结算单(cube 行,带供应商名与金额),
// items 是手工收付款明细 ⇒ 二者一起生成「附件」控件的**结算单明细清单**。
func (s *Service) stepFinish(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, signals []signalDraft, settlements []SettlementRow, items []model.ApprovalItem) {
	start := time.Now()
	if verdict.Conclusion == ConclusionReject {
		reason := truncate(verdict.Reason, 1024)
		// 机器审核未通过的通知发给**发起人**(用户口径 2026-10-22 终稿:
		// 仍是通知发起人,但措辞必须中性 —— 结论只陈述事实,不做动机推断)。
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
		s.logStep(ctx, ap, StepFinish, "", "已拒绝并通知发起人: "+truncate(reason, 200), "", time.Since(start).Milliseconds())
		return
	}

	// pass → 代提交企微审批
	//
	// ⚠️ 收款账号快照在**这里**读出来(而不是从 AuditInput 拿):
	// 它必须待在 LLM 输入面之外(见 service/payee_account.go 的结构隔离说明)。
	cmdID, err := s.publishApplyEvent(ctx, ap, verdict, signals, settlements, items)
	if err != nil {
		s.markFailed(ctx, ap, err.Error())
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

// publishApplyEvent 组装并下发企微 apply_event 命令。
//
// ⚠️ **机器通过**与**管理员人工放行**共用这一段 ——
//    两者的企微单长得一样（同样的控件、同样的流程人），
//    差别只在「AI审核结论」控件的**首行**（放行时是"已转人工审核并通过"）。
//    ⇒ 拆成两段就会漂移：改了一处忘了另一处 ⇒ 两类单的企微单长得不一样。
func (s *Service) publishApplyEvent(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, signals []signalDraft, settlements []SettlementRow, items []model.ApprovalItem) (string, error) {
	// ⚠️ 收款账号快照在**这里**读出来(而不是从 AuditInput 拿):
	// 它必须待在 LLM 输入面之外(见 service/payee_account.go 的结构隔离说明)。
	payee, err := s.loadPayeeAccount(ctx, ap.ID)
	if err != nil {
		return "", fmt.Errorf("load payee account: %w", err)
	}
	applyData, summaryList := s.buildApplyData(ctx, ap, verdict, signals, settlements, items, payee)
	// 按**模板的流程配置**组装 process.node_list(审批人 / 办理人 / 抄送人)。
	//
	// ⚠️ 必须是 mode=0(自带 process):这个模板的三个节点全是「申请人自选」,
	// 成员由发起人在 H5 选。mode=1 时企微会忽略 app 给的成员 ⇒ 节点空 ⇒ 没人办。
	selections, err := s.snapshotFlowSelections(ap)
	if err != nil {
		return "", fmt.Errorf("build process: %w", err)
	}
	process, err := s.buildProcess(selections)
	if err != nil {
		return "", fmt.Errorf("build process: %w", err)
	}
	payload := ApplyEventPayload{
		CreatorUserID:       ap.ApplicantWecomID,
		TemplateID:          s.cfg.TemplateID,
		UseTemplateApprover: 0,
		Process:             process,
		NotifyType:          1,
		ApplyData:           applyData,
		SummaryList:         summaryList,
		// 「附件」:自动生成的结算单明细清单 + 发起人自己上传的文件。
		// (素材上传与控件组装在 wechat-bot 侧 —— 它持有 OA token。)
		AttachmentControlID:   s.cfg.AttachmentControlID,
		AttachmentControlType: s.cfg.AttachmentControlType,
		Attachments:           s.loadApprovalAttachments(ctx, ap, settlements, items),
	}
	cmdID, err := s.publishCommand(ctx, CmdApplyEvent, payload)
	if err != nil {
		return "", fmt.Errorf("publish apply_event: %w", err)
	}
	return cmdID, nil
}

// snapshotFlowSelections 从审批单快照里读回各节点成员。
//
// 快照是**提交时**选的人(随单存档);机器审核是异步的,到 applyevent 那一步
// 早就不是同一个请求上下文了 ⇒ 只能从快照读。
func (s *Service) snapshotFlowSelections(ap *model.ApprovalInstance) (FlowSelections, error) {
	out := FlowSelections{}
	for _, nd := range []struct {
		nodeType string
		raw      []byte
	}{
		{FlowNodeApprover, ap.ApproverUserIDs},
		{FlowNodeHandler, ap.HandlerUserIDs},
		{FlowNodeCC, ap.CcUserIDs},
	} {
		if len(nd.raw) == 0 {
			continue
		}
		var ids []string
		if err := json.Unmarshal(nd.raw, &ids); err != nil {
			return nil, fmt.Errorf("解析 %s 列表: %w", nd.nodeType, err)
		}
		out[nd.nodeType] = ids
	}
	return out, nil
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
//
// 另可选追加「付款信息」多行文本控件(cfg.PaymentInfoControlID 非空时):
// 写入 payee 的开户行/账号/户名(分行)。payee 为 nil(没选收款账号)则跳过。
//
// ⚠️ payee 是**独立表**的快照,**不进 AuditInput** ⇒ 账号信息不会发给 LLM。
func (s *Service) buildApplyData(ctx context.Context, ap *model.ApprovalInstance, verdict *VerdictResult, signals []signalDraft, settlements []SettlementRow, items []model.ApprovalItem, payee *model.ApprovalPayeeAccount) (applyData, summaryList json.RawMessage) {
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
	// 备注控件里带上**收付款摘要**(2026-10-25 用户需求:审批人要在企微单上
	// 看得见"这单除了货款还有什么、冲减了多少")。
	//
	// ⚠️ 摘要只用引擎口径(cube 货款 + 明细行)⇒ 与附件清单同源,不会自相矛盾。
	if len(items) > 0 {
		parts := []string{}
		if strings.TrimSpace(ap.Remark) != "" {
			parts = append(parts, strings.TrimSpace(ap.Remark))
		}
		parts = append(parts, "收付:"+itemsSummaryText(totalsOf(settlements, items)))
		remark = strings.Join(parts, ";")
	}
	if warnCount > 0 {
		remark = fmt.Sprintf("%s(机器一审通过,%d 条警告请关注)", remark, warnCount)
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
	// 「付款信息」控件(2026-10-28 用户口径):多行文本,按行写开户行/账号/户名。
	//
	// ⚠️ 账号信息**只到这里为止** —— 它不进 AuditInput、不进 LLM prompt
	// (结构隔离见 service/payee_account.go 的包注释)。
	//
	// ⚠️ 控件 id 没配 ⇒ 跳过(不阻塞提交):控件 id 随企微后台那张模板走,
	// 猜错 id 只会让企微报"控件不存在" ⇒ 宁可这次不带,也不能让整单提交失败。
	if s.cfg.PaymentInfoControlID != "" {
		if txt := payeeAccountLines(payee); txt != "" {
			ctl := s.cfg.PaymentInfoControlType
			if ctl == "" {
				ctl = "Textarea"
			}
			contents = append(contents, content{Control: ctl, ID: s.cfg.PaymentInfoControlID, Value: textValue{Text: txt}})
		}
	}
	applyData, _ = json.Marshal(map[string]any{"contents": contents})
	// 摘要行:没有收付款明细时保持原样(纯货款审批的摘要不变),
	// 有明细时把三笔账都写上 ⇒ 企微列表页一眼看清冲减关系。
	summaryText := fmt.Sprintf("结算单 %d 张 / 付款 %s 元", len(ids), ap.ManualAmountYuan.StringFixed(2))
	if len(items) > 0 {
		t := totalsOf(settlements, items)
		// 2026-10-30 用户口径:值为 0 的项**不写出来**
		// (企微卡片原文「付款 5403.52 元(货款 6284.947 + 付款项 0 - 收款项 881.43)」
		//  里的「+ 付款项 0」是噪音)。
		parts := make([]string, 0, 3)
		if t.GoodsTotalYuan.GreaterThan(decimal.Zero) {
			parts = append(parts, "货款 "+yuanText(t.GoodsTotalYuan))
		}
		if t.PaymentTotalYuan.GreaterThan(decimal.Zero) {
			parts = append(parts, "+ 付款项 "+yuanText(t.PaymentTotalYuan))
		}
		if t.ReceiptTotalYuan.GreaterThan(decimal.Zero) {
			parts = append(parts, "- 收款项 "+yuanText(t.ReceiptTotalYuan))
		}
		if len(parts) > 0 {
			summaryText = fmt.Sprintf("结算单 %d 张 / 付款 %s 元(%s)",
				len(ids), ap.ManualAmountYuan.StringFixed(2), strings.Join(parts, " "))
		}
	}
	summaryList, _ = json.Marshal(map[string]any{
		"summary_info": []map[string]string{
			{"text": summaryText, "lang": "zh_CN"},
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
//
//	**管理员人工放行**(2026-10-28 用户口径):
//	控件**第一行**是「已转人工审核并通过」，其后跟机器原来的结论。
//	⇒ 审批人看到的是"人放行的 + 机器当初为什么拒"，而不是一句含糊的"机器审核通过"。
func aiConclusionText(verdict *VerdictResult, signals []signalDraft) string {
	if verdict == nil {
		return ""
	}
	var base string
	switch {
	case verdict.Source == model.VerdictSourceAdminOverride:
		// ⚠️ **不能**写"机器审核通过" —— 机器人审的是"拒绝"，
		//    是人放行的。写成"机器审核通过"会让审批人以为机器审过了。
		//
		// 放行说明（note）在 reason 的最前面 ⇒ 按句子边界截断也不会把它切掉。
		base = truncateAtSentence(strings.TrimSpace(verdict.Reason), 500)
	case verdict.Conclusion == ConclusionPass:
		// 企微「多行文本(Textarea)」控件上限 10000 字符(官方帮助 docid=14876),
		// 这里按**可读性**限到 300 字,且按句子边界截断 ——
		// 2026-10-30 用户口径:「避免语义割裂」(旧实现按 200 字硬切会断在句子中间)。
		base = truncateAtSentence("机器审核通过:"+verdict.Reason, 300)
	default:
		for _, d := range signals {
			if d.Level == model.SignalBlock {
				base = truncateRunes(d.Message, 40)
				break
			}
		}
	}
	prefix := strings.TrimSpace(verdict.AIConclusionPrefix)
	if prefix == "" {
		return base
	}
	if base == "" {
		return prefix
	}
	return prefix + "\n" + base
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
	// Items 是手工收付款明细(2026-10-25):重新发起时用它预填两张明细表。
	Items   []model.ApprovalItem          `json:"items"`
	Signals []model.AuditSignal           `json:"signals"`
	Verdict *model.LLMVerdict             `json:"verdict,omitempty"`
	Links   []model.ApprovalStocktakeLink `json:"stocktake_links"`
	// Stocktakes 是关联盘点单的**当前状态**(2026-10-30 重新发起用)。
	//
	// H5 用它判断"被拒时建的计划盘点单是否已审核通过" ⇒ 决定是否带
	// stocktake_header_ids 重新提交。⚠️ 跨服务取不到就留空(不影响详情主体)。
	Stocktakes []StocktakeHeaderDTO `json:"stocktake_headers"`
	// Payee 是本次付款的**收款账号快照**(2026-10-28)。
	//
	// ⚠️ 它**不在** `Approval`(ApprovalInstance)里 ⇒ 不在 LLM 输入面内
	// (见 service/payee_account.go 的结构隔离说明)。
	// 没选收款账号时为 nil。
	Payee *model.ApprovalPayeeAccount `json:"payee_account,omitempty"`
}

// ListApprovals 按 branch + 条件分页(created_at 降序)。
//
// 返回的是 [ApprovalListItem]（审批实例 + 列表卡片摘要：供应商名 / 付款项名）。
// ⚠️ 摘要是**附带**的：读不到只少显示名字,列表照常(见 fillListSummaries)。
func (s *Service) ListApprovals(ctx context.Context, branchID, status, from, to string, limit, offset int) ([]ApprovalListItem, int64, error) {
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
	// 列表卡片摘要（供应商名 / 付款项名）—— 2026-10-29 用户口径。
	out := make([]ApprovalListItem, 0, len(items))
	for _, it := range items {
		out = append(out, ApprovalListItem{ApprovalInstance: it})
	}
	s.fillListSummaries(ctx, branchID, out)
	return out, total, nil
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
	// 补供应商名(读时 join;cube 拿不到就留空,详情页其余内容照常)。
	//
	// ⚠️ 放在**读出快照之后**:快照行是 PG 里的历史行,名字不在行上,
	// 得用字典现查。字典查询失败只告警不报错 —— 详情页的主体是审批快照,
	// 为了一个显示字段让整页 503 不划算。
	s.fillApprovalSettlementNames(ctx, branchID, d.Settlements)
	// 手工收付款明细(重新发起时用来预填两张明细表)。
	s.db.WithContext(ctx).Where("approval_id = ?", id).Order("created_at ASC").Find(&d.Items)
	s.db.WithContext(ctx).Where("approval_id = ?", id).Order("created_at ASC").Find(&d.Signals)
	var verdict model.LLMVerdict
	if err := s.db.WithContext(ctx).First(&verdict, "approval_id = ?", id).Error; err == nil {
		d.Verdict = &verdict
	}
	s.db.WithContext(ctx).Where("approval_id = ?", id).Find(&d.Links)
	// 关联盘点单的当前状态(重新发起的 H5 用来判断"盘点是否已审核")。
	// 跨服务失败只跳过 —— 详情主体是审批快照,不该因盘点服务抖动而 500。
	if s.stocktake != nil {
		for _, l := range d.Links {
			h, err := s.stocktake.GetHeader(ctx, l.StocktakeHeaderID)
			if err != nil {
				s.logger.Warn("detail: get stocktake header failed",
					"approval_id", id, "header_id", l.StocktakeHeaderID, "err", err)
				continue
			}
			d.Stocktakes = append(d.Stocktakes, *h)
		}
	}
	// 收款账号快照(2026-10-28):独立表,**不混进 d.Approval** ——
	// d.Approval 会进 AuditInput ⇒ 账号信息混进去就等于发给 LLM 了。
	if payee, err := s.loadPayeeAccount(ctx, id); err == nil {
		d.Payee = payee
	}
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
