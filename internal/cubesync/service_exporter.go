package cubesync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// ServiceEndpoint 描述"某个 scope 的归档导出端点在哪"。
type ServiceEndpoint struct {
	// AppID 是拥有该 scope 的服务的 dapr app-id（如 supertrade-catalog）。
	AppID string
	// Path 是**相对 app 根**的路径（如 "archive/export"）。
	//
	// ⚠️ 不能写 "/api/v1/catalog/archive/export"：
	// dapr service invocation 打到 app 的**根**路径上，
	// nginx 剥掉的那段前缀在 app 侧不存在 ⇒ 那样写必然 404。
	Path string
}

// ArchiveOwners 是"哪个服务拥有哪些 scope"。
type ArchiveOwners struct {
	CatalogAppID   string
	PricingAppID   string
	InventoryAppID string
	MemberAppID    string
}

// ArchiveExportEndpoints 由「scope → 拥有它的服务」生成 scope → 导出端点的映射。
//
// ⚠️ **必须与导入方用同一份 scope 归属**（DefaultCatalogScopes 等）：
// 导出与导入不是同一个服务 ⇒ 映射写错只会表现为
// "导出了 catalog 的数据、写进了 pricing 的库"，而且**零报错**。
//
// path 是**相对 app 根**的路径（如 "archive/export"）。
func ArchiveExportEndpoints(owners ArchiveOwners, path string) map[string]ServiceEndpoint {
	out := make(map[string]ServiceEndpoint, 16)
	add := func(appID string, scopes []string) {
		appID = strings.TrimSpace(appID)
		if appID == "" {
			return
		}
		for _, s := range scopes {
			out[s] = ServiceEndpoint{AppID: appID, Path: path}
		}
	}
	add(owners.CatalogAppID, DefaultCatalogScopes())
	add(owners.PricingAppID, DefaultPricingScopes())
	add(owners.InventoryAppID, DefaultInventoryScopes())
	add(owners.MemberAppID, DefaultMemberScopes())
	return out
}

// ServiceExporter 从**各服务自己的归档导出端口**取数据（而不是直接问 cube-router）。
//
// # 为什么不是直接问 cube-router
//
// 服务的导出端口自己按「有源走 cube、无源走本地」分流：
//
//	源门店配了数据源 ⇒ 导出的是那个数据源的**实时**数据；
//	源门店没配数据源   ⇒ 导出的是源门店**本地表**的数据。
//
// ⇒ 搬运方**不需要知道 cube 的存在**：它只认"服务 + scope"。
//
// ⚠️ 这也是把"搬运"从 cube-sync 挪到 cube-async 的原因：
// cube-sync 直接问 cube-router ⇒ 它只能搬 cube 里的数据，
// 而服务层的端口能搬"任何服务的数据"（含本域数据）。
type ServiceExporter struct {
	sidecarBase string
	branchID    string
	endpoints   map[string]ServiceEndpoint
	http        *http.Client
}

// NewServiceExporter 构造 ServiceExporter。
//
// endpoints 是 scope → 端点的映射；scope 不在映射里 ⇒ 导出直接报错
// （而不是悄悄导 0 行 ⇒ 那会被误读成"这个门店没有数据"）。
func NewServiceExporter(sidecarBase, branchID string, endpoints map[string]ServiceEndpoint) *ServiceExporter {
	return &ServiceExporter{
		sidecarBase: strings.TrimRight(strings.TrimSpace(sidecarBase), "/"),
		branchID:    strings.TrimSpace(branchID),
		endpoints:   endpoints,
		http:        &http.Client{Timeout: 180 * time.Second},
	}
}

var _ Exporter = (*ServiceExporter)(nil)

// Export 取某个 scope 的一页（导出端点无状态，分页是调用方的循环）。
func (e *ServiceExporter) Export(ctx context.Context, bearer, scope string, offset, limit int) (*archivedto.ExportPage, error) {
	if e == nil {
		return nil, fmt.Errorf("cubesync: 导出方未装配")
	}
	if e.sidecarBase == "" {
		return nil, fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法调用导出方")
	}
	ep, ok := e.endpoints[scope]
	if !ok || strings.TrimSpace(ep.AppID) == "" {
		return nil, fmt.Errorf("cubesync: scope %q 没有登记导出端点(服务映射缺一条)", scope)
	}
	path := strings.TrimPrefix(strings.TrimSpace(ep.Path), "/")
	if path == "" {
		return nil, fmt.Errorf("cubesync: scope %q 的导出端点路径为空", scope)
	}

	q := url.Values{}
	q.Set("scope", scope)
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	u := e.sidecarBase + "/v1.0/invoke/" + ep.AppID + "/method/" + path + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setAPIToken(req)
	setCallerHeaders(req, bearer, e.branchID)

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cubesync: 调用导出方失败(scope=%s app=%s offset=%d): %w",
			scope, ep.AppID, offset, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		return nil, downstreamError("导出方", resp)
	}
	var page archivedto.ExportPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("cubesync: 解析导出响应失败(scope=%s): %w", scope, err)
	}
	return &page, nil
}
