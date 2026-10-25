package cubeclient_test

// 2026-10-23:供应商检索的**编码路径**回归锁。
//
// # 它守的是什么 bug
//
// `SearchSuppliers` 原来靠"query 是否以 `SUP-` 开头"来决定走 id 精确还是 name 模糊:
//
//	if strings.HasPrefix(query, "SUP-") { id equals } else { name contains }
//
// 而**思迅的供应商编码是数字**(`1001` / `1002` / `0` / `0001` …),
// **不带 `SUP-` 前缀** ⇒ 用真实编码查时落到 name contains
// ⇒ 在供应商**名字**里搜 "1001" 搜不到 ⇒ 返回空。
//
// ⇒ 后果(实测 2026-10-23,打生产):
// 门店用真实供应商编码进猪 ⇒ fresh-meat 校验失败 ⇒
// 「service: 输入校验失败: supplier_id 1001 在 cube 中不存在」
// ⇒ **整猪根本进不来**,而 cube 与 fresh-meat 都是 200 正常、零报错。
//
// # 现在的行为
//
// 先按 id 精确查;没命中再按 name 模糊查(人输的是供应商名字)。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
)

// supplierFilterOf 取出这次查询的 filters(第一个 filter 的 member/operator/values)。
func supplierFilterOf(t *testing.T, content *dapr.DataContent) (member string, operator string, value string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(content.Data, &body); err != nil {
		t.Fatalf("unmarshal query body: %v", err)
	}
	filters, _ := body["filters"].([]any)
	if len(filters) == 0 {
		return "", "", ""
	}
	f, _ := filters[0].(map[string]any)
	values, _ := f["values"].([]any)
	v := ""
	if len(values) > 0 {
		v, _ = values[0].(string)
	}
	m, _ := f["member"].(string)
	op, _ := f["operator"].(string)
	return m, op, v
}

// TestSearchSuppliers_NumericID_MatchesByID 锁"数字供应商编码能按 id 查到"。
func TestSearchSuppliers_NumericID_MatchesByID(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		member, operator, value := supplierFilterOf(t, content)
		if member != "supplier.id" || operator != "equals" {
			t.Errorf("第一次查询应是 id equals, got member=%q operator=%q", member, operator)
		}
		if value != "1001" {
			t.Errorf("过滤值应为 1001, got %q", value)
		}
		return 200, []map[string]any{
			{"supplier.id": "1001", "supplier.name": "岑溪恒兴扬商贸有限公司", "supplier.type": "1"},
		}, nil
	})

	out, err := c.SearchSuppliers(context.Background(), "1001", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 || out[0].ID != "1001" {
		t.Fatalf("应按 id 命中 1 条, got %+v", out)
	}
}

// TestSearchSuppliers_FallsBackToName 锁"id 查不到时按名字模糊查"。
func TestSearchSuppliers_FallsBackToName(t *testing.T) {
	calls := 0
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		calls++
		member, operator, _ := supplierFilterOf(t, content)
		if calls == 1 {
			if member != "supplier.id" || operator != "equals" {
				t.Errorf("第一次查询应是 id equals, got member=%q operator=%q", member, operator)
			}
			return 200, []map[string]any{}, nil // id 没命中
		}
		if member != "supplier.name" || operator != "contains" {
			t.Errorf("第二次查询应是 name contains, got member=%q operator=%q", member, operator)
		}
		return 200, []map[string]any{
			{"supplier.id": "1002", "supplier.name": "岑溪市鸿群商贸有限公司", "supplier.type": "1"},
		}, nil
	})

	out, err := c.SearchSuppliers(context.Background(), "鸿群", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 || out[0].ID != "1002" {
		t.Fatalf("应按名字命中 1 条, got %+v", out)
	}
	if calls != 2 {
		t.Errorf("应查两次(id 一次 + name 一次), got %d", calls)
	}
}

// TestSearchSuppliers_EmptyQuery_NoFilter 锁"空 query 不加过滤器"。
func TestSearchSuppliers_EmptyQuery_NoFilter(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		var body map[string]any
		_ = json.Unmarshal(content.Data, &body)
		if filters, _ := body["filters"].([]any); len(filters) != 0 {
			t.Errorf("空 query 不应有 filters, got %v", filters)
		}
		return 200, []map[string]any{
			{"supplier.id": "1001", "supplier.name": "A"},
			{"supplier.id": "1002", "supplier.name": "B"},
		}, nil
	})
	out, err := c.SearchSuppliers(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应返 2 条, got %d", len(out))
	}
}

// TestSearchSuppliers_IDPrefixStillWorks 锁"旧的 SUP- 前缀用法没被改坏"。
func TestSearchSuppliers_IDPrefixStillWorks(t *testing.T) {
	c := newFakeClient(func(_ context.Context, _, _, _ string, content *dapr.DataContent) (int, []map[string]any, error) {
		member, operator, value := supplierFilterOf(t, content)
		if member != "supplier.id" || operator != "equals" || value != "SUP-001" {
			t.Errorf("应为 id equals SUP-001, got member=%q operator=%q value=%q", member, operator, value)
		}
		return 200, []map[string]any{
			{"supplier.id": "SUP-001", "supplier.name": "可口可乐华南"},
		}, nil
	})
	out, err := c.SearchSuppliers(context.Background(), "SUP-001", 0)
	if err != nil {
		t.Fatalf("SearchSuppliers: %v", err)
	}
	if len(out) != 1 || !strings.EqualFold(out[0].ID, "SUP-001") {
		t.Fatalf("应命中 SUP-001, got %+v", out)
	}
}
