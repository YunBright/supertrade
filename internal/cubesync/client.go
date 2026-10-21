// Package cubesync / client.go —— 与两个下游(导出方 / 导入方)通话。
//
// # 两个下游
//
//	导出方 = cube-router  GET  /v1/export   （dapr app-id: supertrade-cube-router）
//	导入方 = catalog      POST /archive/import（dapr app-id: supertrade-catalog）
//
// # 鉴权:两枚"机器 token + 门店头"就够
//
//	Authorization: Bearer <机器 token>   —— 被调方验签 + 取 per-branch 权限
//	X-Branch-ID:   <门店>                —— 导出方据此路由到该门店的 cube 实例;
//	                                       导入方据此决定写到哪个门店
//
// ⚠️ **导出与导入的门店不同**:导出来自源门店(00),写进目标门店(10)。
// 混用同一个 header 的症状是"导出了 00 的数据、写进了 00 的门店"——
// 而报告里两个门店号都写着 00,不看配置根本发现不了。
//
// # 为什么错误要带上被调方的 code/message
//
// 被调方统一用 `{code, message}` 信封。若只报"HTTP 500",
// 排查就得到一句毫无指向的话;带上 code(如 `product_not_found` /
// `cube_source_not_configured`)才是可执行的线索。
package cubesync

import (
	"bytes"
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

// 下游错误响应体上限。
//
// ⚠️ 只取前 8KB:错误消息可能很长(逐行错误明细),
// 全量带回来会把日志撑爆,也会把敏感内容带进日志。
const maxErrBody = 8 << 10

// Exporter 从导出方取一页档案。
type Exporter interface {
	// Export 取某个 scope 的一页。offset / limit 由调用方决定
	// (导出端点**无状态**,分页是调用方的循环)。
	Export(ctx context.Context, bearer, scope string, offset, limit int) (*archivedto.ExportPage, error)
}

// Importer 把一批档案写进导入方。
type Importer interface {
	// Import 提交一批行。目标门店由 importer 自己的配置决定。
	Import(ctx context.Context, bearer string, req archivedto.ImportRequest) (*archivedto.ImportResult, error)
}

// SidecarExporter 经 sidecar 的 HTTP API 调 cube-router 的导出端点。
type SidecarExporter struct {
	sidecarBase string
	appID       string
	branchID    string
	http        *http.Client
}

// NewSidecarExporter 构造。
func NewSidecarExporter(sidecarBase, appID, branchID string) *SidecarExporter {
	return &SidecarExporter{
		sidecarBase: strings.TrimRight(strings.TrimSpace(sidecarBase), "/"),
		appID:       strings.TrimSpace(appID),
		branchID:    strings.TrimSpace(branchID),
		http:        &http.Client{Timeout: 120 * time.Second},
	}
}

var _ Exporter = (*SidecarExporter)(nil)

// Export 实现 Exporter。
func (e *SidecarExporter) Export(ctx context.Context, bearer, scope string, offset, limit int) (*archivedto.ExportPage, error) {
	if e == nil {
		return nil, fmt.Errorf("cubesync: 导出方未装配")
	}
	if e.sidecarBase == "" {
		return nil, fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法调用导出方")
	}
	q := url.Values{}
	q.Set("scope", scope)
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	u := e.sidecarBase + "/v1.0/invoke/" + e.appID + "/method/v1/export?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	setAPIToken(req)
	setCallerHeaders(req, bearer, e.branchID)

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cubesync: 调用导出方失败(scope=%s offset=%d): %w", scope, offset, err)
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

// DefaultImportPath 是导入端点的**默认**路径。
//
// ⚠️ catalog / pricing / inventory 三个下游都是 POST /archive/import。
const DefaultImportPath = "archive/import"

// SidecarImporter 经 sidecar 的 HTTP API 调导入方的导入端点。
type SidecarImporter struct {
	sidecarBase string
	appID       string
	branchID    string
	// importPath 是导入端点路径(不含前导斜杠)。
	//
	// ⚠️ **每个下游可以有自己的路径**:member 的导入端点是 POST /import
	//   (前端路径 /api/v1/member/import),而 catalog/pricing/inventory
	//   都是 /archive/import ⇒ 写死一个路径的话,要么 member 端点被迫改名,
	//   要么这里就得能按下游切换。
	importPath string
	http       *http.Client
}

// NewSidecarImporter 构造(默认导入路径 /archive/import)。
func NewSidecarImporter(sidecarBase, appID, branchID string) *SidecarImporter {
	return NewSidecarImporterAt(sidecarBase, appID, branchID, DefaultImportPath)
}

// NewSidecarImporterAt 构造(指定导入端点路径)。
func NewSidecarImporterAt(sidecarBase, appID, branchID, importPath string) *SidecarImporter {
	p := strings.Trim(strings.TrimSpace(importPath), "/")
	if p == "" {
		p = DefaultImportPath
	}
	return &SidecarImporter{
		sidecarBase: strings.TrimRight(strings.TrimSpace(sidecarBase), "/"),
		appID:       strings.TrimSpace(appID),
		branchID:    strings.TrimSpace(branchID),
		importPath:  p,
		http:        &http.Client{Timeout: 180 * time.Second},
	}
}

var _ Importer = (*SidecarImporter)(nil)

// Import 实现 Importer。
func (i *SidecarImporter) Import(ctx context.Context, bearer string, req archivedto.ImportRequest) (*archivedto.ImportResult, error) {
	if i == nil {
		return nil, fmt.Errorf("cubesync: 导入方未装配")
	}
	if i.sidecarBase == "" {
		return nil, fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法调用导入方")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	u := i.sidecarBase + "/v1.0/invoke/" + i.appID + "/method/" + i.importPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	setAPIToken(httpReq)
	setCallerHeaders(httpReq, bearer, i.branchID)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := i.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cubesync: 调用导入方失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		return nil, downstreamError("导入方", resp)
	}
	var out archivedto.ImportResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("cubesync: 解析导入响应失败: %w", err)
	}
	return &out, nil
}

// setCallerHeaders 带上被调方必需的两个头。
func setCallerHeaders(req *http.Request, bearer, branchID string) {
	if req == nil {
		return
	}
	if b := strings.TrimSpace(bearer); b != "" {
		req.Header.Set("Authorization", "Bearer "+b)
	}
	if br := strings.TrimSpace(branchID); br != "" {
		req.Header.Set("X-Branch-ID", br)
	}
}

// DownstreamError 是被调方返回的错误(带它的 code / message)。
type DownstreamError struct {
	// Who 是"导出方" / "导入方"。
	Who string
	// Status 是 HTTP 状态码。
	Status int
	// Code 是被调方的错误码(如 cube_source_not_configured)。
	Code string
	// Message 是被调方的错误文案。
	Message string
}

// Error 实现 error。
func (e *DownstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("cubesync: %s返回 %d %s: %s", e.Who, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("cubesync: %s返回 %d: %s", e.Who, e.Status, e.Message)
}

// downstreamError 从错误响应里抽出被调方的 {code, message}。
//
// ⚠️ 被调方的 body **可能不是 JSON**(网关错误页、sidecar 自己的错误),
// 那时原样带上截断后的文本 —— 比只报一个状态码有用。
func downstreamError(who string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && (env.Code != "" || env.Message != "") {
		return &DownstreamError{Who: who, Status: resp.StatusCode, Code: env.Code, Message: env.Message}
	}
	return &DownstreamError{Who: who, Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
}
