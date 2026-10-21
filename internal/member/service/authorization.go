// Package service / authorization.go —— 越权授权链路(图4 / 图5)。
//
// # 时序(方案 §7.3)
//
//	POS 触发「会员冲减积分」
//	  → member 查当前操作人在**当前门店**的 scope(per-branch 权限矩阵)
//	      ├─ 有 member:point-deduct → 执行
//	      └─ 无 → 403 {code:"member_permission_required",
//	                    message:"当前收银员没有冲减积分权限，请输入授权",
//	                    required_scope:"member:point-deduct"}
//	  → POS 弹【授权对话框】(图4):收银员编码 + 收银员口令
//	  → 授权通过 ⇒ 带 authorization(工号 + 口令)重新提交 ⇒ member 自己验 ⇒ 放行并写审计
//	      否 ⇒ 403 {code:"member_permission_denied", message:"你没有冲减积分的权限！"}
//
// # ⚠️ **"只带 authorized_by"不再放行**(2026-10-21 安全修复)
//
//	旧实现里"只带 authorized_by(POS 说它已自行验过)"直接放行 ⇒
//	**任何调用方只要在 body 里填一个工号,就能在没有任何权限的情况下写数据**。
//	⇒ 现在:调用方自己没有该 scope 时,必须带 authorization(工号 + 口令)让 member 验;
//	  调用方自己就有该 scope 时(①),authorized_by 只作为审计信息透传。
//
// # 为什么"授权人有没有权限"要在 member 再查一遍
//
// POS 已经调过 master-data 的 `POST /employees/{code}/verify`(它用机器身份调 auth
// 验口令 + 查 per-branch scope)。但 member 是**最终写数据的那一方**,
// 它不能只凭客户端声称的 authorized_by 就放行 ——
// 否则任何客户端都能自称"我已被授权"。
//
// ⇒ member 用**自己的** per-branch 权限矩阵再确认一次授权人的能力。
//
// # 为什么"用户没绑定这家店 ⇒ 在该店没有任何 scope"
//
// 权限是 **per-branch 实时解析**的:userd 的 per-branch 矩阵里,
// 没绑定该门店的行**根本不存在** ⇒ 连全局角色权限也没有。
// ⇒ member 侧只要查不到该 user 在该 branch 的行,就当"没有权限"。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/YunBright/supertrade/internal/member/model"
)

// ScopeChecker 判定"某用户在某门店是否拥有某 scope"。
//
// ⚠️ 实现方必须走 **per-branch 权限矩阵**(userd /internal/users/{id}/permissions
//
//	?branch_id=…),不能用 JWT 里的 scopes(本体系恒为空数组)。
type ScopeChecker interface {
	// HasScope 返回 (是否有权限, 错误)。
	// 错误 ⇒ 调用方应 fail-closed(503),**不能**当成"有权限"。
	HasScope(ctx context.Context, userID, branchID, scope string) (bool, error)
}

// EmployeeAuth 是工号+口令校验的结果。
type EmployeeAuth struct {
	// EmployeeCode 是工号(= 审计里的 authorized_by)。
	EmployeeCode string `json:"employee_code"`
	// Name 是姓名。
	Name string `json:"name"`
	// Role 是角色。
	Role string `json:"role"`
	// Verified 是"口令是否通过"。
	Verified bool `json:"verified"`
	// Scopes 是该工号在**当前门店**的有效 scope 集合。
	Scopes []string `json:"scopes"`
}

// HasScope 判断该工号在当前门店是否拥有某 scope。
func (e *EmployeeAuth) HasScope(scope string) bool {
	if e == nil {
		return false
	}
	for _, s := range e.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// EmployeeVerifier 校验工号+口令,并返回该工号在当前门店的 scope 集合。
//
// ⚠️ 口令哈希(bcrypt/pbkdf2)在 auth,member 侧**不持有任何凭据**
//
//	⇒ 口令校验只能落在 auth / master-data。
type EmployeeVerifier interface {
	VerifyEmployee(ctx context.Context, branchID, employeeCode, password string) (*EmployeeAuth, error)
}

// ---- 授权相关的哨兵错误 ----

var (
	// ErrCredentialsInvalid:工号或口令不对(⇒ 403 member_permission_denied)。
	ErrCredentialsInvalid = errors.New("member: 工号或口令错误")
	// ErrVerifierUnavailable:授权链路本身不可用(机器身份没配 / master-data 调不通)
	// ⇒ 503(**不是** 403):这不是用户输错了,是服务端配置问题。
	ErrVerifierUnavailable = errors.New("member: 授权校验链路不可用")
	// ErrInvalidOldPassword:改密时原口令不对(403)。
	ErrInvalidOldPassword = errors.New("member: 原口令不正确")
)

// PermissionError 是越权授权链路的两种 403。
//
// ⚠️ 它**必须**与普通的 model.ErrXxx 分开:这两种 403 要带
//
//	`required_scope` 字段(POS 靠它决定弹不弹授权框),
//	而普通的 404/409 不带。
type PermissionError struct {
	// Code 是 member_permission_required / member_permission_denied。
	Code string `json:"code"`
	// Message 是给收银员看的中文文案(**带功能名**)。
	Message string `json:"message"`
	// RequiredScope 是缺少的 scope(仅 member_permission_required 有)。
	RequiredScope string `json:"required_scope,omitempty"`
}

// Error 实现 error。
func (e *PermissionError) Error() string { return e.Message }

// 权限错误码。
const (
	// CodePermissionRequired 是"当前操作人没有该权限,请输入授权"。
	CodePermissionRequired = "member_permission_required"
	// CodePermissionDenied 是"授权不通过"。
	CodePermissionDenied = "member_permission_denied"
)

// Authorization 是写请求里携带的越权授权信息(图4 的工号 + 口令)。
type Authorization struct {
	// EmployeeCode 是授权人的工号。
	EmployeeCode string `json:"employee_code"`
	// Password 是授权人的口令(明文,只在本次调用里用一次)。
	Password string `json:"password"`
}

// IsZero 判断是否没带授权信息。
func (a *Authorization) IsZero() bool {
	return a == nil || strings.TrimSpace(a.EmployeeCode) == "" || strings.TrimSpace(a.Password) == ""
}

// AuthorizeInput 是授权判定的入参。
type AuthorizeInput struct {
	// OperatorID 是当前操作人(auth user id)。
	OperatorID string
	// BranchID 是当前门店。
	BranchID string
	// Scope 是需要的 scope(如 member:point-deduct)。
	Scope string
	// Feature 是**功能名**(如 冲减积分)—— 出现在 403 文案里。
	Feature string
	// AuthorizedBy 是请求里带的授权人(POS 已自行调 master-data 验过时的工号/user id)。
	AuthorizedBy string
	// Authorization 是请求里带的"工号+口令"(member 自行验时的凭据)。
	Authorization *Authorization
}

// AuthorizeResult 是授权判定的结果。
type AuthorizeResult struct {
	// AuthorizedBy 是要写进审计列的授权人;空 = 操作人自己有权限。
	AuthorizedBy string
	// Scope 是本次校验的 scope。
	Scope string
}

// Authorize 判定当前操作人能否执行需要 scope 的写操作。
//
// ⚠️ **fail-closed**:scope 判定出错(权限服务不可用)⇒ 返 error(503),
//
//	**绝不**当成"有权限"。
//
// 返回 *PermissionError 时,handler 映 403:
//   - member_permission_required:操作人没权限、也没带授权 ⇒ POS 弹授权框(图4);
//   - member_permission_denied:带了授权但**不通过** ⇒ POS 弹图5 的 alert。
func (s *Service) Authorize(ctx context.Context, in AuthorizeInput) (*AuthorizeResult, error) {
	branchID := strings.TrimSpace(in.BranchID)
	scope := strings.TrimSpace(in.Scope)
	if branchID == "" || scope == "" {
		return nil, fmt.Errorf("%w: branch_id / scope 必填", model.ErrInvalidInput)
	}
	feature := strings.TrimSpace(in.Feature)
	if feature == "" {
		feature = scope
	}
	if s.checker == nil {
		// 没装配权限判定器 ⇒ 一律拒绝(fail-closed)。
		return nil, &PermissionError{
			Code:          CodePermissionRequired,
			Message:       fmt.Sprintf("当前收银员没有%s权限，请输入授权", feature),
			RequiredScope: scope,
		}
	}

	// ① 操作人自己有权限 ⇒ 直接放行。
	operatorID := strings.TrimSpace(in.OperatorID)
	if operatorID != "" && operatorID != actor {
		has, err := s.checker.HasScope(ctx, operatorID, branchID, scope)
		if err != nil {
			return nil, err
		}
		if has {
			return &AuthorizeResult{AuthorizedBy: strings.TrimSpace(in.AuthorizedBy), Scope: scope}, nil
		}
	}

	// ② 请求带了"工号+口令" ⇒ member 自己验(图4 的凭据链路)。
	if !in.Authorization.IsZero() {
		if s.verifier == nil {
			slog.Error("member: 越权授权校验器未装配(降级为 503)",
				"scope", scope, "branch_id", branchID)
			return nil, ErrVerifierUnavailable
		}
		auth, err := s.verifier.VerifyEmployee(ctx, branchID,
			strings.TrimSpace(in.Authorization.EmployeeCode), in.Authorization.Password)
		if err != nil {
			if errors.Is(err, ErrCredentialsInvalid) {
				return nil, &PermissionError{
					Code:    CodePermissionDenied,
					Message: fmt.Sprintf("你没有%s的权限！", feature),
				}
			}
			// ⚠️ **必须记日志** —— 不记的话,这条链路一旦出问题,
			//	现场只看到 503「授权校验暂不可用」,**没有任何线索**:
			//	是机器身份没配?dapr 调不通?token 的 aud 不对?master-data 拒了?
			//	⇒ 全部归成同一句"暂不可用",排查方向完全跑偏。
			slog.Error("member: 越权授权校验链路失败(不是口令错)",
				"scope", scope, "branch_id", branchID,
				"employee_code", strings.TrimSpace(in.Authorization.EmployeeCode),
				"err", err)
			return nil, err
		}
		if auth == nil || !auth.Verified || !auth.HasScope(scope) {
			// 授权人不通过 / 授权人自己也没有该权限 ⇒ 图5 的 alert 文案。
			return nil, &PermissionError{
				Code:    CodePermissionDenied,
				Message: fmt.Sprintf("你没有%s的权限！", feature),
			}
		}
		return &AuthorizeResult{AuthorizedBy: firstNonEmpty(auth.EmployeeCode, in.AuthorizedBy), Scope: scope}, nil
	}

	// ③ 只带了 authorized_by —— **不放行**(2026-10-21 安全修复)。
	//
	// ⚠️ 这里原来是无条件放行 ⇒ **提权漏洞**:
	//
	//	任何调用方只要在 body 里填 authorized_by,就能在**没有任何权限**的情况下
	//	完成写操作(开卡 / 赠分 / 充值 / 改状态 …)——
	//	而 POST /members 这些端点是**登录用户就能调到的**。
	//
	//	症状极安静:请求 200、审计列写着"经 1003 授权",
	//	而 1003 从头到尾没参与过 ⇒ 事后对账也看不出是谁干的。
	//
	// ⇒ 现在的规则:**调用方自己没有该 scope 时,必须走 authorization(工号 + 口令)**
	//	让 member 侧验(见 ②);只有调用方**自己就有**该 scope 时(①)
	//	才允许 authorized_by 直接透传(那时它只是审计信息)。
	//
	// ⇒ 这里返 member_permission_required(带 required_scope)而不是 denied:
	//	POS 收到 required 会弹授权框(图4)⇒ 调用方补上工号 + 口令即可继续;
	//	denied(图5 的 alert)会让收银员以为"这条路走不通"。
	if by := strings.TrimSpace(in.AuthorizedBy); by != "" {
		slog.Warn("member: 调用方只带 authorized_by 就想执行写操作,已拒绝(必须走工号+口令授权)",
			"scope", scope, "branch_id", branchID,
			"operator_id", operatorID, "authorized_by", by)
	}

	// ④ 什么都没有(或只带了 authorized_by)⇒ 403,让 POS 弹授权框(图4)。
	return nil, &PermissionError{
		Code:          CodePermissionRequired,
		Message:       fmt.Sprintf("当前收银员没有%s权限，请输入授权", feature),
		RequiredScope: scope,
	}
}
