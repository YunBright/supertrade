// 本文件锁住**会员档案**走 cube 时实际发出的查询与解析。
//
// # 为什么要有这个文件
//
// 商品那条链已经有 catalog_query_test.go 锁"发出去的 JSON 字面量";
// 会员这条链是新加的(门店配了数据源时读外部 ERP 的会员档案),
// 一旦 dimension / filter / order 写错,症状是"列表空了"或"排序没反应",
// **零报错** ⇒ 必须在客户端层钉住。
//
// # 几条不能被"优化"掉的规则
//
//  1. order 的 id 必须**同时在 SELECT 里**(cube 直接 400)。
//  2. 排序末尾必须追加 id 兜底(排序列会并列 ⇒ 没有兜底键就不是全序
//     ⇒ 翻页会重复/漏行)。
//  3. 计数必须带 total:true,否则响应里的 total 恒为 0。
//  4. 非法排序字段 ⇒ **本地拒绝**(不是发给 cube 再吃 400)。
package cubeclient

import (
	"context"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

// TestListMembers_QueryShape 锁"会员列表查询的 body 形状"。
func TestListMembers_QueryShape(t *testing.T) {
	inv := &recordingInvoker{}
	inv.responds = [][]map[string]any{{{}}}
	inv.totals = []int{4128}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, total, err := c.ListMembers(context.Background(), MemberQuery{
		Field: "name", Q: "郭", Limit: 20, Offset: 40,
		Sort: "points_accumulated", Order: "desc",
	})
	require.NoError(t, err)
	assert.Equal(t, 4128, total, "total 必须来自 cube 的 total:true")

	require.Len(t, inv.bodies, 1)
	body := inv.bodies[0]
	assert.Equal(t, float64(20), body["limit"])
	assert.Equal(t, float64(40), body["offset"])
	assert.Equal(t, true, body["total"], "不给 total:true ⇒ cube 的 total 恒 0")

	// ⚠️ **必须带 measure**:cube 拒收没有 measure 的查询
	// (QUERY_INVALID: at least one measure required)⇒ 少了它整个列表接口 503。
	measures, ok := body["measures"].([]any)
	require.True(t, ok, "measures 必须是数组")
	assert.Contains(t, measures, "member.count")

	// 过滤:按姓名 contains。
	filters := bodyFilters(t, body)
	require.Len(t, filters, 1)
	assert.Equal(t, "member.name", filters[0]["member"])
	assert.Equal(t, "contains", filters[0]["operator"])
	assert.Equal(t, []any{"郭"}, filters[0]["values"])

	// 排序:排序列 + id 兜底。
	orders, ok := body["order"].([]any)
	require.True(t, ok, "order 必须是数组")
	require.Len(t, orders, 2, "排序末尾必须追加 id 兜底(全序)")
	first, _ := orders[0].(map[string]any)
	assert.Equal(t, "member.points_accumulated", first["id"])
	assert.Equal(t, "desc", first["order"])
	second, _ := orders[1].(map[string]any)
	assert.Equal(t, "member.id", second["id"], "末尾必须是 id 兜底")

	// 排序列必须在 SELECT 里(cube 直接 400)。
	dims := dimsOf(t, body)
	assert.True(t, hasDim(dims, "member.points_accumulated"), "排序列必须在 SELECT 里")
	assert.True(t, hasDim(dims, "member.id"), "id 必须在 SELECT 里")
	assert.True(t, hasDim(dims, "member.balance_yuan"), "余额列必须在 SELECT 里")
	assert.True(t, hasDim(dims, "member.remaining_points"), "剩余积分列必须在 SELECT 里")
}

// TestListMembers_RejectsUnknownSort 锁"非法排序字段本地拒绝"。
func TestListMembers_RejectsUnknownSort(t *testing.T) {
	inv := &recordingInvoker{}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, _, err := c.ListMembers(context.Background(), MemberQuery{Sort: "drop_table"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCubeBadRequest)
	assert.Empty(t, inv.bodies, "非法排序不该发出去")
}

// TestListMembers_RejectsUnknownQueryField 锁"非法查询字段本地拒绝"。
func TestListMembers_RejectsUnknownQueryField(t *testing.T) {
	inv := &recordingInvoker{}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, _, err := c.ListMembers(context.Background(), MemberQuery{Field: "branch_id", Q: "x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCubeBadRequest)
}

// TestGetMember_PicksExactNormalizedID 锁"子串命中多个时挑**归一化后精确**的那条"。
//
// ⚠️ 源库实测有卡号带前导空格(' 801598')与反引号('`800973')
// ⇒ 用 contains 查会带出别的卡 ⇒ 必须精确挑,不能只取第一条。
func TestGetMember_PicksExactNormalizedID(t *testing.T) {
	inv := &recordingInvoker{}
	inv.responds = [][]map[string]any{{
		{"member.id": " 8015980", "member.name": "同前缀的另一张卡"},
		{"member.id": " 801598", "member.name": "邹美玉"},
	}}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	row, err := c.GetMember(context.Background(), "801598")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "邹美玉", row.Name, "必须挑归一化后精确匹配的那条")
}

// TestResolveBranchSource 锁"数据源解析的响应解析"。
func TestResolveBranchSource(t *testing.T) {
	inv := &jsonInvoker{body: []byte(`{"branch_id":"00","configured":true,"enabled":true,"cube_source_name":"cube-sixun-ysx-fb"}`)}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	src, err := c.ResolveBranchSource(context.Background(), "00")
	require.NoError(t, err)
	assert.True(t, src.Configured)
	assert.True(t, src.Enabled)
	assert.Equal(t, "cube-sixun-ysx-fb", src.CubeSourceName)
}

// TestResolveBranchSource_InjectsBranchHeader 锁"解析数据源会把参数门店带成 X-Branch-ID"。
//
// ⚠️ 后台流水线(runAudit)用 context.Background() 启动,ctx 里没有门店;
// 若这里不注入,cube-router 的 /v1/branch-source 一律 400 branch_required,
// 有源门店的盘损/其它辅助读全部降级(2026-10-31 生产实踩)。
func TestResolveBranchSource_InjectsBranchHeader(t *testing.T) {
	inv := &metaCaptureInvoker{body: []byte(`{"branch_id":"00","configured":true,"enabled":true}`)}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, err := c.ResolveBranchSource(context.Background(), " 00 ")
	require.NoError(t, err)
	assert.Equal(t, "00", inv.branchHeader, "必须把参数门店(去空白)写进 x-branch-id")
}

// TestResolveBranchSource_KeepsExistingBranch 锁"ctx 已有门店时不覆盖"。
//
// ⚠️ HTTP 路径上 middleware 写入的是 header 原值(可能是 "01,02" 多店形式),
// 不能被这里收窄成单店(否则权限判定会看到与请求头不一致的门店)。
func TestResolveBranchSource_KeepsExistingBranch(t *testing.T) {
	inv := &metaCaptureInvoker{body: []byte(`{"branch_id":"00","configured":true,"enabled":true}`)}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, err := c.ResolveBranchSource(WithBranchID(context.Background(), "01,02"), "00")
	require.NoError(t, err)
	assert.Equal(t, "01,02", inv.branchHeader, "ctx 已有门店时不得被参数覆盖")
}

// metaCaptureInvoker 记录 outgoing metadata 里的 x-branch-id。
type metaCaptureInvoker struct {
	body         []byte
	branchHeader string
}

func (m *metaCaptureInvoker) InvokeMethodWithContent(ctx context.Context, _, _, _ string,
	_ *dapr.DataContent) ([]byte, error) {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if v := md.Get("x-branch-id"); len(v) > 0 {
			m.branchHeader = v[0]
		}
	}
	return m.body, nil
}

// jsonInvoker 固定返回一段 JSON(给非查询类调用用)。
type jsonInvoker struct {
	body []byte
}

func (j *jsonInvoker) InvokeMethodWithContent(_ context.Context, _, _, _ string,
	_ *dapr.DataContent) ([]byte, error) {
	return j.body, nil
}
