// Package handler —— 导出端点的契约守卫。
//
// # 为什么要有这个文件
//
// 导出端点把 cube 成员名映射成**契约列名**(archivedto.ExportColumns),
// 编排方(cubesync)按同一份列名把数据读进行结构体。
// 两边各写一份字面量时,改了一边**不会编译报错** ——
//
//	症状:导出了 27565 条,导入报告显示"成功 27565 条",
//	但 POS 上商品名 / 条码**全是空的**,而且没有任何一行报错。
//
// ⇒ 本文件把"导出端点吐的列"与"契约列"锁在一起。
package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	dapr "github.com/dapr/go-sdk/client"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TestExportScopes_MatchContractColumns 锁"导出列 == 契约列"。
func TestExportScopes_MatchContractColumns(t *testing.T) {
	// 契约里有的 scope,导出端点必须都实现。
	for scope := range archivedto.ExportColumns {
		if _, ok := exportScopes[scope]; !ok {
			t.Errorf("契约里有 scope %q,导出端点没有实现", scope)
		}
	}
	// 导出端点有的 scope,契约里也要有(否则编排方会往一个没定义的 scope 发请求)。
	for scope := range exportScopes {
		if _, ok := archivedto.ExportColumns[scope]; !ok {
			t.Errorf("导出端点有 scope %q,契约里没定义", scope)
		}
	}

	for scope, sc := range exportScopes {
		want := append([]string(nil), archivedto.ExportColumns[scope]...)
		sort.Strings(want)

		got := make([]string, 0, len(sc.Fields))
		for _, v := range sc.Fields {
			got = append(got, v)
		}
		sort.Strings(got)

		if len(got) != len(want) {
			t.Errorf("scope %q 的导出列数不对\n  契约: %v\n  实际: %v", scope, want, got)
			continue
		}
		for i := range want {
			if want[i] != got[i] {
				t.Errorf("scope %q 的导出列与契约不一致\n  契约: %v\n  实际: %v", scope, want, got)
				break
			}
		}
	}
}

// TestExportScopes_OrderCoversAllDimensions 锁"排序覆盖全部维度"。
//
// ⚠️ 分页的稳定性取决于排序键是否**全序**。
// 只按主键排时,同一个 id 的多行之间顺序未定义 ⇒ 翻页可能重复或漏行,
// 而症状是"导出了 219 个供应商,导入后少了 3 个",且**没有任何一行报错**。
//
// ⇒ ORDER BY 必须覆盖全部维度(维度元组本身就是 GROUP BY 的键 ⇒ 必是全序)。
func TestExportScopes_OrderCoversAllDimensions(t *testing.T) {
	for scope, sc := range exportScopes {
		order := exportOrderFor(sc.Dimensions)
		if len(order) != len(sc.Dimensions) {
			t.Errorf("scope %q 的排序项数(%d)应等于维度数(%d)",
				scope, len(order), len(sc.Dimensions))
		}
		seen := map[string]bool{}
		for _, o := range order {
			if o.Order != "asc" {
				t.Errorf("scope %q 的排序方向应为 asc,实际 %q", scope, o.Order)
			}
			if seen[o.ID] {
				t.Errorf("scope %q 的排序项 %q 重复", scope, o.ID)
			}
			seen[o.ID] = true
			// 排序成员必须同时在 SELECT 里(cube 的硬要求)。
			if _, ok := sc.Fields[o.ID]; !ok {
				t.Errorf("scope %q 的排序成员 %q 不在本次 SELECT 里", scope, o.ID)
			}
		}
	}
}

// TestExportValue_KeepsAmountAsString 锁"金额导出成字符串"。
//
// 金额走 float64 会丢精度(9.90 → 9.899999999999999),
// 而下游拿到的若是字符串就没有这个问题。
func TestExportValue_KeepsAmountAsString(t *testing.T) {
	got := exportValue([]byte(`"9.900000"`), "price_yuan")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("金额应是字符串,实际 %T", got)
	}
	if s != "9.900000" {
		t.Errorf("金额应保留原始十进制文本,实际 %q", s)
	}
	if !strings.Contains(s, ".") {
		t.Errorf("金额文本不该被截断,实际 %q", s)
	}
}

// TestExportValue_NullIsNotEmptyString 锁"null 与空串可区分"。
//
// "没维护价"与"价是 0"必须能区分,否则 POS 会把没维护价的商品显示成 ¥0.00。
func TestExportValue_NullIsNotEmptyString(t *testing.T) {
	got := exportValue([]byte(`null`), "price_yuan")
	if got != nil {
		t.Errorf("null 应导出成 nil,实际 %v", got)
	}
}

// stockCapturingClient 捕获发往 cube 的**请求体**。
//
// 嵌入 dapr.Client(nil impl):没覆写的方法被调用会 panic,提示测试漏了。
type stockCapturingClient struct {
	dapr.Client
	body []byte
}

func (f *stockCapturingClient) InvokeMethodWithContent(
	_ context.Context, _, _, _ string, content *dapr.DataContent,
) ([]byte, error) {
	f.body = content.Data
	return []byte(`{"data":[{"stock.product_id":"P1","stock.total_quantity":12.500000,` +
		`"stock.avg_cost":9.900000}],"total":1}`), nil
}

// TestExportScopeStock_RequestShape 锁"scope=stock 的请求体形状"。
//
// ⚠️ 三条硬要求,每条都对应一个真实的坑:
//
//  1. dimensions / measures 齐全 —— 少一个字段,导出的行就少一列,
//     而导入端只报"成功 N 条"。
//  2. **没有任何时间过滤** —— stock 的 model 是 `storage: live`
//     (透传源表当前值),需求是"只同步同步时刻的库存";
//     误加 inDateRange ⇒ 恒不匹配 ⇒ 导不出行且**不报错**。
//  3. **不按 stock.branch_id 过滤** —— 那是思迅的 branch_no("0001"),
//     与本系统的 branch_id 无关;cube 的 GetStock 注释里明确写了不得按它过滤。
//     一个 cube 实例 = 一家门店,天然只含本店数据。
//
// ⚠️ 排序必须全序:唯一的维度 stock.product_id 就是排序键,
//
//	且它同时在 SELECT 里(cube 的硬要求,否则 GROUP BY 报 not a grouped column)。
//
// TestExportValue_DecodesBase64Numeric —— MSSQL live 源的 decimal 是 **base64**。
//
// # 症状（2026-10-21 生产实测）
//
// 库存快照导出来的每一行都是：
//
//	{"avg_cost_yuan":"MTAuMDAwMDAw","product_id":"00000297","qty":"MC4wMDAw"}
//
// 而 base64 解码后是 "10.000000" / "0.0000" ⇒ **库存全是乱码**。
//
// 根因：**MSSQL live 源**（思迅直查：stock / settlement / sale_day）的 decimal 列，
// `go-mssqldb` 以 `[]byte` 返回，`encoding/json` 把 `[]byte` 序列化成 **base64**；
// 而 **DuckDB 源**（product）的同类值是**明文 number**。
//
// ⇒ 消费方**两种都必须收**（与 `internal/cubeclient.ParseDecimal` 同款语义）。
//
// ⚠️ 反证：只按"明文 number"解析 ⇒ base64 那一支永远走不到 ⇒ 本测试变红。
func TestExportValue_DecodesBase64Numeric(t *testing.T) {
	cases := []struct {
		raw  string
		key  string
		want string
	}{
		// base64("0.0000") —— 生产实测的库存数量
		{`"MC4wMDAw"`, "qty", "0"},
		// base64("10.000000") —— 生产实测的库存均价
		{`"MTAuMDAwMDAw"`, "avg_cost_yuan", "10"},
		// base64("46492.0200") —— cubeclient 里的同款样本
		{`"NDY0OTIuMDIwMA=="`, "price_yuan", "46492.02"},
		// 明文 number（DuckDB 源）—— 保留原始字面量
		{`12.50`, "price_yuan", "12.50"},
		{`5.36`, "qty", "5.36"},
		// 明文数字串 —— 同样保留原文本
		{`"12.50"`, "price_yuan", "12.50"},
	}
	for _, c := range cases {
		got := exportValue(json.RawMessage(c.raw), c.key)
		s, ok := got.(string)
		if !ok {
			t.Errorf("exportValue(%s, %s) = %#v，应为字符串", c.raw, c.key, got)
			continue
		}
		if s != c.want {
			t.Errorf("exportValue(%s, %s) = %q, want %q", c.raw, c.key, s, c.want)
		}
	}
}

// TestExportValue_NonNumericFieldsAreNotDecoded —— 非数值字段**不许** base64 解码。
//
// ⚠️ 条码/名称/助记码是思迅的 char 列。若把它们也走 base64 分支，
//
//	就会把乱码写进库里 —— 而报告照样说"成功 N 条"。
func TestExportValue_NonNumericFieldsAreNotDecoded(t *testing.T) {
	// base64("ABC") = "QUJD" —— 一个看起来像 base64、但解出来不是数字的串。
	got := exportValue(json.RawMessage(`"QUJD"`), "barcode")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("barcode 字段应返回字符串,得到 %#v", got)
	}
	if s != "QUJD" {
		t.Errorf("非数值字段不该被解码，得到 %q", s)
	}
}

func TestExportScopeStock_RequestShape(t *testing.T) {
	fake := &stockCapturingClient{}
	h := &Handler{daprClient: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	sc, ok := exportScopes["stock"]
	if !ok {
		t.Fatalf("导出端点没有实现 stock scope")
	}

	rows, total, err := h.exportFromCube(context.Background(), "cube-00", "00", sc, "2026-10-21", "", 100, 0)
	if err != nil {
		t.Fatalf("导出 stock 失败: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("应导出 1 行,实际 total=%d rows=%d", total, len(rows))
	}

	var q exportQuery
	if err := json.Unmarshal(fake.body, &q); err != nil {
		t.Fatalf("发往 cube 的请求体不是 JSON: %v (body=%s)", err, fake.body)
	}

	if !containsStr(q.Dimensions, "stock.product_id") {
		t.Errorf("dimensions 应含 stock.product_id,实际 %v", q.Dimensions)
	}
	if !containsStr(q.Measures, "stock.total_quantity") {
		t.Errorf("measures 应含 stock.total_quantity,实际 %v", q.Measures)
	}
	if !containsStr(q.Measures, "stock.avg_cost") {
		t.Errorf("measures 应含 stock.avg_cost,实际 %v", q.Measures)
	}

	// 过滤条件必须为空:既没有时间过滤,也没有门店过滤。
	if len(q.Filters) != 0 {
		t.Errorf("stock 不该带任何过滤条件,实际 %+v", q.Filters)
	}
	body := string(fake.body)
	for _, banned := range []string{
		"inDateRange", "promotion.begin_date", "promotion.end_date",
		"stock.branch_id", "branch_id",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("stock 的请求体里不该出现 %q: %s", banned, body)
		}
	}

	// 全序排序:按 stock.product_id 排,且它必须在 SELECT 里。
	if len(q.Order) != 1 {
		t.Fatalf("stock 应按 1 个键排序,实际 %+v", q.Order)
	}
	if q.Order[0].ID != "stock.product_id" || q.Order[0].Order != "asc" {
		t.Errorf("stock 排序应为 stock.product_id asc,实际 %+v", q.Order[0])
	}
	if _, ok := sc.Fields[q.Order[0].ID]; !ok {
		t.Errorf("排序成员 %q 必须同时在 SELECT 里", q.Order[0].ID)
	}

	// 字段映射:cube 成员名 → 契约列名。
	row := rows[0]
	if row["product_id"] != "P1" {
		t.Errorf("product_id 应为 P1,实际 %v", row["product_id"])
	}
	// ⚠️ 数量与金额都是**字符串**(导出契约:金额一律字符串,别走 float64)。
	for _, key := range []string{"qty", "avg_cost_yuan"} {
		s, ok := row[key].(string)
		if !ok {
			t.Fatalf("%s 应导出成字符串,实际 %T(%v)", key, row[key], row[key])
		}
		if s == "" {
			t.Errorf("%s 不该为空", key)
		}
	}
	if row["avg_cost_yuan"] != "9.900000" {
		t.Errorf("avg_cost_yuan 应保留原始十进制文本,实际 %v", row["avg_cost_yuan"])
	}
	if row["qty"] != "12.500000" {
		t.Errorf("qty 应保留原始十进制文本,实际 %v", row["qty"])
	}
}

// containsStr 判断切片里有没有某个字符串。
func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
