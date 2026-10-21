// Package service / masterdata.go —— 调 master-data 的越权授权校验(图4)。
//
// # 为什么是 master-data 而不是 auth
//
// 授权对话框(图4)收的是**工号 + 口令**。工号 → auth user_id 的映射在
// master-data 的 `employees` 表里(主键就是 auth user_id)。
// 而 auth 只认 user_id / 手机号,⛔ 不认工号。
//
// ⇒ member 必须问 master-data:"这个工号 + 口令对不对?他在**当前门店**有哪些 scope?"
//
// # 调的是哪个端点
//
//	master-data POST /employees/{code}/verify
//
// ⚠️ 它是 master-data 里**唯一不挂 scope 守门**的端点(但仍在鉴权链内):
//
//	调用方是**已登录的收银员**,不是管理员 ⇒ 不能用 employee:manage 守门。
//
// # 机器身份
//
// ⚠️ 必须带 **机器 token**:master-data 的 `RequireAudience("master-data")`
//
//	要求 token 的 aud 含 "master-data"。
//	auth 的机器 token(aud = jwt.service_audience ∪ {internal-s2s})里**含**
//	"master-data"(auth/internal/config/config.go 的默认 service_audience 列表)。
//	⇒ member 用自己的服务账号换一枚机器 token 即可。
//
// ⚠️ **绝不能用 `dapr.NewClient()`**:go-sdk 的 api-token 拦截器会**整块替换**
//
//	outgoing metadata ⇒ 转发的 Authorization 头被抹掉 ⇒ 恒 401。
//	⇒ 用 `github.com/YunBright/authkit/daprclient`(它只**补** api-token)。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/YunBright/authkit/daprclient"
)

// 机器身份与目标 app 的配置。
const (
	// EnvServiceClientID 是服务账号 client_id。
	EnvServiceClientID = "AUTH_SERVICE_CLIENT_ID"
	// EnvServiceClientSecret 是服务账号 client_secret。
	EnvServiceClientSecret = "AUTH_SERVICE_CLIENT_SECRET"
	// EnvMasterDataAppID 是 master-data 的 **dapr app-id**(不是 cmdbootstrap AppID)。
	EnvMasterDataAppID = "MASTER_DATA_APP_ID"
	// EnvAuthAppID 是 auth(userd)的 dapr app-id。
	EnvAuthAppID = "AUTH_APP_ID"

	// DefaultMasterDataAppID 是 master-data 的默认 dapr app-id。
	DefaultMasterDataAppID = "supertrade-master-data"
	// DefaultAuthAppID 是 auth 的默认 dapr app-id。
	DefaultAuthAppID = "userd"
)

// MasterDataVerifier 是 EmployeeVerifier 的 master-data 实现。
type MasterDataVerifier struct {
	cli dapr.Client
	// masterDataAppID 是目标 dapr app-id。
	masterDataAppID string
	// authAppID 是换机器 token 的目标(userd 承载 /auth/token)。
	authAppID string
	// mu 保护下面的机器 token 缓存。
	mu sync.Mutex
	// token 是缓存的机器 token(不含 "Bearer " 前缀时另加)。
	token string
	// expAt 是 token 的本地过期时刻。
	expAt time.Time
	// now 是可注入时钟(测试用)。
	now func() time.Time
}

// NewMasterDataVerifier 构造 MasterDataVerifier。
func NewMasterDataVerifier() (*MasterDataVerifier, error) {
	cli, err := daprclient.New()
	if err != nil {
		return nil, fmt.Errorf("member: 创建 dapr 客户端失败: %w", err)
	}
	return &MasterDataVerifier{
		cli:             cli,
		masterDataAppID: envOr(EnvMasterDataAppID, DefaultMasterDataAppID),
		authAppID:       envOr(EnvAuthAppID, DefaultAuthAppID),
		now:             func() time.Time { return time.Now() },
	}, nil
}

// VerifyEmployee 调 master-data 校验工号+口令,返回该工号在当前门店的 scope。
func (v *MasterDataVerifier) VerifyEmployee(ctx context.Context, branchID, employeeCode, password string) (*EmployeeAuth, error) {
	if strings.TrimSpace(branchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 不能为空", ErrVerifierUnavailable)
	}
	if strings.TrimSpace(employeeCode) == "" || password == "" {
		return nil, ErrCredentialsInvalid
	}
	token, err := v.machineToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		return nil, fmt.Errorf("member: 序列化授权请求失败: %w", err)
	}
	// ⚠️ 机器 token 放 outgoing metadata(不是 dapr 的 metadata 字段)。
	//   master-data 从 X-Branch-ID header 取门店 ⇒ 必须把它一起带过去。
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", token)
	ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)

	resp, err := v.cli.InvokeMethodWithContent(ctx, v.masterDataAppID,
		"employees/"+employeeCode+"/verify", "POST",
		&dapr.DataContent{Data: body, ContentType: "application/json"})
	if err != nil {
		return nil, classifyVerifyError(err)
	}
	var out EmployeeAuth
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("member: 解析 master-data 授权响应失败: %w", err)
	}
	if !out.Verified {
		return nil, ErrCredentialsInvalid
	}
	if out.EmployeeCode == "" {
		out.EmployeeCode = employeeCode
	}
	return &out, nil
}

// machineToken 取一枚机器 token(带缓存)。
//
// ⚠️ 没配服务账号 ⇒ ErrVerifierUnavailable(**不是**"口令错"):
//
//	那是服务端配置问题,报成 403 会让现场以为用户输错了口令。
func (v *MasterDataVerifier) machineToken(ctx context.Context) (string, error) {
	clientID := strings.TrimSpace(os.Getenv(EnvServiceClientID))
	secret := strings.TrimSpace(os.Getenv(EnvServiceClientSecret))
	if clientID == "" || secret == "" {
		return "", fmt.Errorf("%w: 未配置 %s / %s", ErrVerifierUnavailable, EnvServiceClientID, EnvServiceClientSecret)
	}
	v.mu.Lock()
	cached, expAt := v.token, v.expAt
	v.mu.Unlock()
	if cached != "" && v.now().Before(expAt) {
		return cached, nil
	}
	body, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"client_secret": secret,
	})
	if err != nil {
		return "", fmt.Errorf("%w: 序列化服务账号凭据失败: %v", ErrVerifierUnavailable, err)
	}
	resp, err := v.cli.InvokeMethodWithContent(ctx, v.authAppID, "auth/token", "POST",
		&dapr.DataContent{Data: body, ContentType: "application/json"})
	if err != nil {
		return "", fmt.Errorf("%w: 换机器 token 失败: %v", ErrVerifierUnavailable, err)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("%w: 解析机器 token 响应失败: %v", ErrVerifierUnavailable, err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("%w: 服务账号换到的机器 token 为空", ErrVerifierUnavailable)
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	refreshAfter := ttl - 5*time.Minute
	if refreshAfter <= 0 {
		refreshAfter = ttl / 2
	}
	token := "Bearer " + out.AccessToken
	v.mu.Lock()
	v.token, v.expAt = token, v.now().Add(refreshAfter)
	v.mu.Unlock()
	return token, nil
}

// classifyVerifyError 把 dapr 折叠后的 gRPC 错误映射成哨兵错误。
//
// ⚠️ dapr 会把目标 app 的 HTTP 状态码折成 gRPC 码:
//
//	400 → InvalidArgument、401 → Unauthenticated、403 → PermissionDenied、404 → NotFound。
//	⇒ 判据只能用 **gRPC 码**,不能看文案。
func classifyVerifyError(err error) error {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unauthenticated, codes.PermissionDenied, codes.NotFound:
			// 口令错 / 没绑定该店 / 工号不存在 ⇒ 对外统一是"授权不通过"。
			return ErrCredentialsInvalid
		case codes.InvalidArgument:
			return fmt.Errorf("%w: master-data 拒了请求参数: %v", ErrVerifierUnavailable, err)
		}
	}
	return fmt.Errorf("%w: 调用 master-data 失败: %v", ErrVerifierUnavailable, err)
}

// envOr 取环境变量,空则用默认值。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// 编译期断言:MasterDataVerifier 实现了 EmployeeVerifier。
var _ EmployeeVerifier = (*MasterDataVerifier)(nil)
