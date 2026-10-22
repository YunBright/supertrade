// 本文件锁「增量水位(since)在 catalog 导出链路上**真的传到了上游**」。
//
// # 为什么要有这条
//
// 2026-10-23 生产事故:cubesync 把 since 交给了 catalog 的 /archive/export,
// 而 catalog 的导出端点**根本不读 since**(也不转发给 cube-router)
// ⇒ cube 查询没有 gt 过滤 ⇒ 每次同步都是全量(27569 行)
// ⇒ 而编排方**照样推进水位** ⇒ 增量形同虚设
// ⇒ 报告写着"成功",**全程零报错**。
//
// ⇒ 这条测试断言"since 出现在上游调用里",而不是断言"实现细节"。
package handler

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestArchiveExport_ForwardsSinceToUpstream 锁"since 原样透传给上游导出端点"。
func TestArchiveExport_ForwardsSinceToUpstream(t *testing.T) {
	exp := &fakeExporter{rows: map[string][]map[string]any{
		"products": {{"id": "P-1", "name": "测试商品"}},
	}}
	r := newSourceTestRouterWith(t, &fakeSource{src: configuredSource()}, exp)

	// since 里有空格 ⇒ 用 '+' 编码(Go 的 c.Query 会把 '+' 解成空格)。
	w := do(t, r, "/archive/export?scope=products&since=2026-10-23+01%3A43%3A56", "00")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	require.Len(t, exp.sinces, 1, "上游应被调用一次")
	assert.Equal(t, "2026-10-23 01:43:56", exp.sinces[0],
		"since 必须原样透传给上游:漏传 ⇒ 上游导全量而水位照样推进(零报错)")
}

// TestArchiveExport_NoSince_UpstreamGetsEmpty 锁"没给 since ⇒ 上游收到空串(= 全量)"。
func TestArchiveExport_NoSince_UpstreamGetsEmpty(t *testing.T) {
	exp := &fakeExporter{rows: map[string][]map[string]any{
		"products": {{"id": "P-1"}},
	}}
	r := newSourceTestRouterWith(t, &fakeSource{src: configuredSource()}, exp)

	w := do(t, r, "/archive/export?scope=products", "00")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	require.Len(t, exp.sinces, 1)
	assert.Empty(t, exp.sinces[0], "没给 since ⇒ 上游按全量处理")
}

// TestArchiveExport_BadSince_400 锁"非法 since ⇒ 400(不静默回落成全量)"。
//
// ⚠️ 静默回落的后果:调用方以为导了增量,其实导了全量 ——
// 而**没有一行报错**,增量报告也会写成"全量成功"。
func TestArchiveExport_BadSince_400(t *testing.T) {
	exp := &fakeExporter{rows: map[string][]map[string]any{
		"products": {{"id": "P-1"}},
	}}
	r := newSourceTestRouterWith(t, &fakeSource{src: configuredSource()}, exp)

	w := do(t, r, "/archive/export?scope=products&since=not-a-time", "00")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, "bad_since", errorCode(t, w))
	assert.Empty(t, exp.sinces, "校验没过就不该调上游")
}
