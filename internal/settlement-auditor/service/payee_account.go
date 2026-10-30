// Package service / payee_account.go —— 收款账号快照的写入与读取。
//
// # 这条链路在**架构上**保证「账号信息不进 LLM」
//
// 用户口径（2026-10-28）：「settlement-auditor 服务的付款审批模块，
// **不向机器人审核（LLM）发送账号信息**。」
//
// 实现方式不是"记得别塞进 prompt"，而是**结构隔离**：
//
//	账号快照 → model.ApprovalPayeeAccount（**独立表**）
//	LLM 输入面 → AuditInput.Approval = *model.ApprovalInstance（**没有账号字段**）
//
// ⇒ 无论 buildAuditUserPrompt 以后怎么改，账号信息都不在它的可达范围内。
// （若把账号字段挂在 ApprovalInstance 上，`in.Approval` 整体就在 LLM 面里，
//   哪天加一行 `fmt.Fprintf(&sb, "%v", ap)` 就泄漏，且没有任何测试会红。）
//
// # 谁提供账号信息
//
// 发起人在 H5「申请付款审批」页选了收款账号 ⇒ 前端传 **bank_account_id**
// ⇒ 本服务用 dapr service invocation 调 bank-account 服务按 id 取回
// 开户行/账号/户名 ⇒ 落 approval_payee_accounts。
//
// ⚠️ **前端只传 id，不传账号文本**：否则客户端可以伪造收款账号，
// 让审批人看到"付给某某行 6222****"——而实际付到别处。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// PayeeAccountDTO 是 bank-account 服务返回的收款账号。
//
// ⚠️ 字段名必须与 bank-account 服务的 model.BankAccount 的 json tag 逐字对齐
// （两个 module 不能互相 import，字段名错位 ⇒ **静默**拿到空账号）。
type PayeeAccountDTO struct {
	ID           string `json:"id"`
	BranchID     string `json:"branch_id"`
	SupplierID   string `json:"supplier_id"`
	SupplierName string `json:"supplier_name"`
	// BankName 是开户行。
	BankName string `json:"bank_name"`
	// AccountNo 是银行账号。
	AccountNo string `json:"account_no"`
	// AccountName 是户名。
	AccountName string `json:"account_name"`
}

// BankAccountClient 是按 id 取收款账号的抽象（单测注入 fake）。
type BankAccountClient interface {
	// GetAccount 按 id 取一个收款账号（限门店）。
	//
	// ⚠️ bearer 是**调用者的 Authorization 头原值**（含 "Bearer " 前缀），
	// 必须原样透传给 bank-account —— 不带就是 401（2026-10-28 生产实踩）。
	GetAccount(ctx context.Context, branchID, bearer, accountID string) (*PayeeAccountDTO, error)
}

// DaprBankAccountClient 经 dapr service invocation 调 bank-account 服务的
// **公开读端点** GET /supplier-bank-accounts/:id。
//
// # 为什么走公开端点而不是内部端点
//
// ① 公开端点有 per-branch scope 守门（settlement:view）⇒ 调用者身份被校验；
//    内部端点（免鉴权）只适合"同信任域内的纯内部编排"。
// ② 内部端点需要 nginx 加 404 屏蔽块 + 契约测试表里加行 ⇒ 多一处能写错的地方。
//
// ⚠️ **JWT 必须透传**：被调方在应用层验签 + 校验 aud 含 "bank-account"
// ⇒ 不带 Authorization 就是 401。而 JWT 里**没有** "bank-account" 这个 aud
// 的话，申请人 token 会 403 invalid_audience —— 那是**部署配置缺口**，
// 不是代码问题（见 deployer 的 JWT_AUDIENCE 说明）。
//
// ⚠️ 用 authkit/daprclient.New() 而不是 dapr.NewClient()：
//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata，
//   把这里要透传的 authorization 抹掉（2026-10-20 全网加 DAPR_API_TOKEN
//   之后暴露的）。详见 authkit/daprclient 的包注释。
type DaprBankAccountClient struct {
	client dapr.Client
	appID  string
}

// NewDaprBankAccountClient 构造 bank-account 调用客户端。
func NewDaprBankAccountClient(daprCli dapr.Client, appID string) *DaprBankAccountClient {
	if appID == "" {
		appID = "bank-account"
	}
	return &DaprBankAccountClient{client: daprCli, appID: appID}
}

// GetAccount 按 id 取收款账号。
func (c *DaprBankAccountClient) GetAccount(ctx context.Context, branchID, bearer, accountID string) (*PayeeAccountDTO, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("bank-account client 未装配")
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("%w: bank_account_id 必填", ErrInvalidInput)
	}
	method := "supplier-bank-accounts/" + accountID
	// ⚠️⚠️ **JWT 必须透传**(2026-10-28 生产实踩,第二次踩同一个坑):
	//   bank-account 在**应用层**验签 + 校验 aud 含 "bank-account" + 按门店判 scope,
	//   所以它要求请求带 Authorization 头。
	//
	//   **漏掉它的症状极具误导性**(第一版就是这样):
	//     - dapr 调用**成功到达**了被调方(被调方访问日志里有这条请求),
	//     - 门店头也到了(X-Branch-ID 照传,被调方日志 branch 有值),
	//     - 但 claims 没注入 ⇒ 被调方返 401 ⇒ dapr 把它包成
	//       `rpc error: code = Unauthenticated desc = Unauthorized`,
	//     - 而取号失败只 warn 不阻断提交 ⇒ 审批单照常走完,
	//       只有企微的「付款信息」控件永远是空的。
	//   ⇒ 判据必须看**被调方的访问日志**(sub="" + branch 有值 = 缺 Authorization),
	//     不能只看调用方的报错文案。
	//
	//   dapr sidecar 把 outgoing gRPC metadata 的 key 转成 outgoing HTTP 头给目标 app
	//   (与 internal/cubeclient/dapr_client.go 的同名写法一致)。
	if strings.TrimSpace(bearer) != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	// ⚠️ 门店必须一起透传：被调方的 scope 守门按 X-Branch-ID 头判门店，
	// 不带就是 400 branch_required。
	ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	// ⚠️⚠️ **GET 也必须传非 nil 的 content**（空结构体即可）——
	//   go-sdk 的 InvokeMethodWithContent 对 nil content 直接返
	//   "content required"，**在发出请求之前**就失败。
	//   对照 authkit/userinfo/userinfo.go 的同款调用(那里传 &dapr.DataContent{})。
	//
	//   这个坑很隐蔽：症状是"取号失败"这条 warn 日志，**提交照常成功**、
	//   审批单照常走完 —— 只有企微的「付款信息」控件是空的。
	//   ⇒ 必须有单测钉住"content 非 nil"(见 payee_account_dapr_test.go)。
	resp, err := c.client.InvokeMethodWithContent(ctx, c.appID, method, "GET", &dapr.DataContent{})
	if err != nil {
		return nil, fmt.Errorf("invoke bank-account get account: %w", err)
	}
	var out PayeeAccountDTO
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("decode bank account: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("bank-account 返回空账号(id=%s)", accountID)
	}
	// ⚠️ 归属校验：被调方按 branch 过滤了，但**再确认一次**。
	// 客户端拿别的门店的 account_id 来提交 ⇒ 这里必须挡住
	// （否则 A 店的付款单上会出现 B 店的收款账号）。
	if branchID != "" && out.BranchID != "" && out.BranchID != branchID {
		return nil, fmt.Errorf("%w: 收款账号不属于该门店", ErrInvalidInput)
	}
	return &out, nil
}

// storePayeeAccount 落收款账号快照。
//
// ⚠️ 账号信息**只写这张表**，不写 approval_instances（见包注释的结构隔离理由）。
func (s *Service) storePayeeAccount(ctx context.Context, ap *model.ApprovalInstance, acct *PayeeAccountDTO) error {
	if acct == nil {
		return nil
	}
	row := &model.ApprovalPayeeAccount{
		ID:              uuid.NewString(),
		ApprovalID:      ap.ID,
		BranchID:        ap.BranchID,
		ApplicantUserID: ap.ApplicantUserID,
		BankAccountID:   acct.ID,
		SupplierID:      acct.SupplierID,
		SupplierName:    acct.SupplierName,
		BankName:        acct.BankName,
		AccountNo:       acct.AccountNo,
		AccountName:     acct.AccountName,
		CreatedAt:       s.now(),
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		return fmt.Errorf("insert payee account: %w", err)
	}
	return nil
}

// resolveAndStorePayeeAccount 按 id 回查收款账号并落快照。
//
// ⚠️ **用申请人的身份调 bank-account**（透传 Authorization）：
// 被调方的 scope 守门按调用者身份判 ⇒ 不透传就是 401/403。
//
// ⚠️ accountID 为空 ⇒ 直接返回(nil) —— 没选账号是合法情形，不是错误。
func (s *Service) resolveAndStorePayeeAccount(ctx context.Context, ap *model.ApprovalInstance, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	if s.bankAccount == nil {
		return fmt.Errorf("bank-account client 未装配")
	}
	// ⚠️ 透传申请人 JWT：bank-account 端点要过 aud + scope 两道关。
	//
	// ⚠️ **必须显式传给 GetAccount**(2026-10-28 生产实踩):
	//   第一版把它塞进 ctx(cubeclient.WithBearer),而 DaprBankAccountClient
	//   从没去 ctx 里读 ⇒ 透传**根本没发生** ⇒ 被调方 401。
	//   ⇒ 凭证走**参数**,不走 ctx 隐式约定:参数是调用点上看得见的东西。
	acct, err := s.bankAccount.GetAccount(ctx, ap.BranchID, ap.ApplicantBearer, accountID)
	if err != nil {
		return err
	}
	return s.storePayeeAccount(ctx, ap, acct)
}

// loadPayeeAccount 读本次审批的收款账号快照（没有则返 nil）。
func (s *Service) loadPayeeAccount(ctx context.Context, approvalID string) (*model.ApprovalPayeeAccount, error) {
	var row model.ApprovalPayeeAccount
	err := s.db.WithContext(ctx).
		Where("approval_id = ?", approvalID).
		Order("created_at asc").First(&row).Error
	if err != nil {
		// 没有快照是**正常情况**（发起人没选收款账号）⇒ 不是错误。
		if strings.Contains(err.Error(), "record not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("load payee account: %w", err)
	}
	return &row, nil
}

// payeeAccountLines 生成「付款信息」控件的文本。
//
// 用户口径（2026-10-28）：「当选择供应商收款账号时，**分行**向该控件填写
// 开户行、账号、户名。」
//
// ⇒ 三行，一行一项，顺序固定（开户行 / 账号 / 户名）。
//
// ⚠️ 格式与「AI审核结论」控件**分开**：那是给审批人看的结论，
// 这是给审批人看的**付款目标**（审批人要照着它打款）⇒ 必须一眼能读。
//
// ⚠️ 账号**分段显示**（2026-10-29 用户口径）：审批人要在企微单上**照着打款**，
// 一长串 19 位数字没法核对位数 ⇒ 与支付宝/微信绑卡、银行 App 的银行卡号
// 显示口径一致：从左往右每 4 位一组，最后一组可以短。
//
// 分段只影响**显示**：落库的账号原文（approval_payee_accounts.account_no）
// 不变，付款时用的仍是原文。
func payeeAccountLines(row *model.ApprovalPayeeAccount) string {
	if row == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if v := strings.TrimSpace(row.BankName); v != "" {
		parts = append(parts, "开户行："+v)
	}
	if v := strings.TrimSpace(row.AccountNo); v != "" {
		parts = append(parts, "账号："+formatBankAccountNo(v))
	}
	if v := strings.TrimSpace(row.AccountName); v != "" {
		parts = append(parts, "户名："+v)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// bankAccountGroupSize 是账号分段的每组位数。
const bankAccountGroupSize = 4

// formatBankAccountNo 把银行账号按 4 位一组分段（**显示用**）。
//
// # 规则（与 wx-h5 的 formatAccountNo 一致，2026-10-29）
//
//   - 先去掉已有的空白与连字符（个别历史数据自带分隔），
//     再从左往右每 4 位一组，空格分隔；
//   - 长度不是 4 的倍数时，**最后那组不足 4 位就照原样**
//     （"1234567" ⇒ "1234 567"、"12345" ⇒ "1234 5"）—— 余数不补零、不丢位；
//   - ⚠️ **余数在末组**，不是在前 —— 这与支付宝/微信绑卡、银行 App 里
//     银行卡号的显示一致（从左往右每 4 位一组，最后一组可以短）。
//     审批人是**从左往右对着念**着核对的，余数在前会让人多看一遍才发现位数不对；
//   - 空账号 ⇒ 空串（不选收款账号时不会留下孤零零的分隔符）。
//
// ⚠️ 只做**显示层**：落库原文不变，提交给企微的也只是给人看的文本。
func formatBankAccountNo(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '-', ' ':
			return -1
		}
		return r
	}, s)
	if cleaned == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range []rune(cleaned) {
		if i > 0 && i%bankAccountGroupSize == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// logPayeeFetchError 记一条"取账号失败"的日志（不带账号内容）。
//
// ⚠️ 日志里**不写**开户行/账号/户名 —— 日志会进归档与第三方日志系统，
// 账号是敏感财务信息。slog 的结构化字段同理。
func logPayeeFetchError(logger *slog.Logger, approvalID string, err error) {
	logger.Warn("settlement-auditor: 取收款账号失败,本次审批不带付款信息",
		"approval_id", approvalID, "err", err)
}

// ensureTimeIsZero 是给零值 time 用的占位（避免 time.Now() 在模型里散落）。
var _ = time.Time{}
