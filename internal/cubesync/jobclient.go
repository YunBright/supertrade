// Package cubesync / jobclient.go —— dapr scheduler 的 job 注册/删除。
//
// # ⚠️ 同名 POST 返回 **409**,不是 upsert
//
// ✅ MEASURED 2026-10-14(wechat-bot 生产):同名 POST 返 409,
// 第二次调用(含进程重启)结果与第一次完全一致。
// ⇒ 所以必须**先 DELETE 再 POST**,否则每次启动都会失败。
//
// # 为什么自己实现而不复用 internal/salesagg/collect
//
// 那一包是 salesagg 的内部实现(采集凭据 + 采集 job),
// 耦合进来会让 cube-sync 依赖一个不相干的域。
// 两处逻辑都很短(各 ~100 行),各自实现反而更清楚 ——
// 而"抄一份然后各改各的"是本仓最贵的一类技术债。
package cubesync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// JobClient 是 dapr scheduler 的注册/删除能力。
type JobClient interface {
	Register(ctx context.Context, name, schedule, address string, data map[string]any) error
	Delete(ctx context.Context, name string) error
}

// DaprJobClient 经本机 sidecar 的 HTTP 调 dapr scheduler。
type DaprJobClient struct {
	sidecarBase string
	http        *http.Client
}

// NewDaprJobClient 构造。
func NewDaprJobClient(sidecarBase string) *DaprJobClient {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	return &DaprJobClient{
		sidecarBase: base,
		http:        &http.Client{Timeout: 10 * time.Second},
	}
}

var _ JobClient = (*DaprJobClient)(nil)

// jobBody 是 POST /v1.0-alpha1/jobs/<name> 的请求体。
type jobBody struct {
	Schedule string         `json:"schedule"`
	Data     map[string]any `json:"data,omitempty"`
	Address  string         `json:"address,omitempty"`
}

// Register 实现 JobClient。
func (c *DaprJobClient) Register(ctx context.Context, name, schedule, address string, data map[string]any) error {
	if c == nil {
		return nil
	}
	if c.sidecarBase == "" {
		return fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法注册 job")
	}
	// ① 尽力删旧 job。错误**只记不返回**:它的唯一目的是给 POST 扫清 409,
	//    而 409 由 POST 自己如实报告。让清理步骤有能力否决整个流程,
	//    是 wechat-bot 第一次部署"DELETE 报 connection refused 就 return"的成因。
	_ = c.Delete(ctx, name)

	raw, err := json.Marshal(jobBody{Schedule: schedule, Data: data, Address: address})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.sidecarBase+"/v1.0-alpha1/jobs/"+name, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	setAPIToken(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cubesync: 注册 job %s 失败: %w", name, err)
	}
	defer drainBody(resp)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cubesync: 注册 job %s 返回 %d", name, resp.StatusCode)
	}
	return nil
}

// Delete 实现 JobClient。
func (c *DaprJobClient) Delete(ctx context.Context, name string) error {
	if c == nil {
		return nil
	}
	if c.sidecarBase == "" {
		return fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法删除 job")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.sidecarBase+"/v1.0-alpha1/jobs/"+name, nil)
	if err != nil {
		return err
	}
	setAPIToken(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cubesync: 删除 job %s 失败: %w", name, err)
	}
	defer drainBody(resp)
	// 404 = 本来就没有,是 no-op(重注册场景的正常路径)。
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("cubesync: 删除 job %s 返回 %d", name, resp.StatusCode)
	}
	return nil
}

// drainBody 读干并关闭响应体(限长,防大 body 撑内存)。
func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}
