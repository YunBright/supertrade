// 本文件锁住 GetProduct 实际发出的 cube 查询。
//
// 为什么需要它(2026-10-09 的教训):
//
//	「商品单位恒为空 / 显示『件』」这个症状,在 supertrade 侧的根因只是
//	GetProduct 的 Dimensions 少写了 "product.unit" —— 没有报错、没有 500、
//	healthz 全绿,只是安静地少了一个字段。
//
//	而 cubeclientfake 直接返回预置的 ProductDTO,**根本不走 GetProduct 的
//	查询构造**,所以 service 层测试(fake)永远测不到这个遗漏。
//	换句话说:fake 测的是"拿到 unit 之后会不会带出去",
//	而这里测的是"到底有没有去要 unit" —— 后者才是真正出事的那一环。
//
// 反面教材(本仓库踩过的):测试写成 expect(常量 == 它自己),
// 于是改了实现也永远绿。这里断言的是**发出去的 JSON 字面量**。
package cubeclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
)

// capturingInvoker 捕获 LoadCubeQuery 真正序列化出去的 body。
type capturingInvoker struct {
	lastBody map[string]any
	lastApp  string
	lastVerb string
}

func (c *capturingInvoker) InvokeMethodWithContent(_ context.Context, appID, methodName, verb string,
	content *dapr.DataContent) ([]byte, error) {
	c.lastApp, c.lastVerb = appID, verb
	if content != nil && len(content.Data) > 0 {
		if err := json.Unmarshal(content.Data, &c.lastBody); err != nil {
			return nil, err
		}
	}
	return []byte(`{"data":[]}`), nil
}

func dimsOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["dimensions"].([]any)
	if !ok {
		t.Fatalf("query body 必须含 dimensions 数组, got %#v", body["dimensions"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("dimension 必须是字符串, got %#v", v)
		}
		out = append(out, s)
	}
	return out
}

func hasDim(dims []string, want string) bool {
	for _, d := range dims {
		if d == want {
			return true
		}
	}
	return false
}

// GetProduct 必须请求 product.unit —— 这是单位能不能显示出来的唯一开关。
func TestGetProduct_RequestsUnitDimension(t *testing.T) {
	inv := &capturingInvoker{}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	// 返回空 → GetProduct 会返 ErrProductNotFound,但 body 已经被捕获了。
	_, _ = c.GetProduct(context.Background(), "6922303199721")

	dims := dimsOf(t, inv.lastBody)
	if !hasDim(dims, "product.unit") {
		t.Fatalf("GetProduct 的 dimensions 必须含 product.unit,实际 got %v\n"+
			"删掉它不会让任何测试变红,只会让 wx-h5 静默退回显示「件」", dims)
	}
	// 反向锁:不能是裸 unit(缺 model 前缀会被 TrimPrefix 成空串后报 dimension not found)
	if hasDim(dims, "unit") {
		t.Fatalf("dimension 必须写全限定名 product.unit,不能是裸 unit;实际 got %v", dims)
	}
}

// 售价同理:avg_price_yuan 是 wx-h5「商品当前售价」的唯一来源。
func TestGetProduct_RequestsPriceMeasure(t *testing.T) {
	inv := &capturingInvoker{}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, _ = c.GetProduct(context.Background(), "6922303199721")

	raw, ok := inv.lastBody["measures"].([]any)
	if !ok {
		t.Fatalf("query body 必须含 measures 数组")
	}
	joined := ""
	for _, v := range raw {
		s, _ := v.(string)
		joined += s + " "
	}
	if !strings.Contains(joined, "product.avg_price_yuan") {
		t.Fatalf("measures 必须含 product.avg_price_yuan,实际 got %v", raw)
	}
}

// GetStock 刻意**不加**任何 branch 过滤(思迅 branch_no 与本系统 branch_id
// 是两套无关编码,加了必然失配)。这条锁防止有人"顺手补全"再制造一次
// 「疑似跨店盘点」故障。
func TestGetStock_MustNotFilterOnCubeBranchID(t *testing.T) {
	inv := &capturingInvoker{}
	c := NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")

	_, _ = c.GetStock(WithBranchID(context.Background(), "00"), "00", "6957583900828")

	raw, ok := inv.lastBody["filters"].([]any)
	if !ok {
		return // 没有 filter,天然满足
	}
	for _, f := range raw {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		if m, _ := fm["member"].(string); strings.Contains(m, "branch") {
			t.Fatalf("stock 查询不得按 branch 过滤(member=%q):思迅 branch_no 与本系统 branch_id 是两套编码", m)
		}
	}
}
