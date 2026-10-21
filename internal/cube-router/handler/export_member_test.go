// Package handler —— 会员档案导出的单元测试。
//
// # 测什么(对着症状写)
//
//	① 会员 scope **不带时间过滤**(会员不是时效数据)
//	② 会员的积分 / 次数列走**数字通道**(base64 / 明文 number 都要能收)
//	③ 脏余额**原样带出**(不能被"顺手"变成 0)
package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
)

// TestExportScopeMembers_NoWindow 锁"会员 scope **没有**时效窗口"。
//
// ⚠️ 会员主档是"当前状态"而不是流水 ⇒ 导出的天然是源库此刻的会员。
//
//	误加 inDateRange ⇒ 恒不匹配 ⇒ **永远导不出行而且不报错**。
func TestExportScopeMembers_NoWindow(t *testing.T) {
	for _, scope := range []string{"members", "member_types"} {
		sc, ok := exportScopes[scope]
		if !ok {
			t.Fatalf("导出端点没有实现 %s scope", scope)
		}
		if sc.Window != nil {
			t.Errorf("scope %q 不该带时间窗口(会员不是时效数据)", scope)
		}
	}
}

// TestExportScopeMembers_RequestHasNoFilters 锁"请求里**没有**时间过滤"。
func TestExportScopeMembers_RequestHasNoFilters(t *testing.T) {
	fake := &memberCapturingClient{}
	h := &Handler{daprClient: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	sc, ok := exportScopes["members"]
	if !ok {
		t.Fatalf("导出端点没有实现 members scope")
	}
	_, _, err := h.exportFromCube(context.Background(), "cube-00", "00", sc, "2026-10-21", 100, 0)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	var body struct {
		Filters []map[string]any `json:"filters"`
	}
	if err := json.Unmarshal(fake.body, &body); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	if len(body.Filters) != 0 {
		t.Errorf("会员 scope 的请求不该带过滤条件(加了会导不出行),实际 %v", body.Filters)
	}
}

// TestExportValue_MemberNumericFields 锁"会员的积分 / 次数列走数字通道"。
//
// ⚠️ 两种源都要收:
//   - MSSQL live 源的 decimal 是 **base64**(go-mssqldb 返 []byte);
//   - DuckDB 源是明文 number。
//
// ⚠️ 而 JSON number 到 Dart 侧会变成 double ⇒ 导出契约里必须是**字符串**。
func TestExportValue_MemberNumericFields(t *testing.T) {
	cases := []struct {
		raw  string
		key  string
		want string
	}{
		// base64("10.000000")
		{`"MTAuMDAwMDAw"`, "points_accumulated", "10"},
		// base64("0.0000")
		{`"MC4wMDAw"`, "balance_yuan", "0"},
		// 明文 number(保留原始字面量)
		{`12.50`, "points_used", "12.50"},
		{`999998`, "limited_use_count", "999998"},
		{`3`, "consumed_count", "3"},
		// 明文数字串
		{`"12.50"`, "consumed_amount_yuan", "12.50"},
	}
	for _, c := range cases {
		got := exportValue(json.RawMessage(c.raw), c.key)
		s, ok := got.(string)
		if !ok {
			t.Errorf("exportValue(%s, %s) = %#v,应为字符串", c.raw, c.key, got)
			continue
		}
		if s != c.want {
			t.Errorf("exportValue(%s, %s) = %q, want %q", c.raw, c.key, s, c.want)
		}
	}
}

// TestExportValue_DirtyBalanceKeptAsIs 锁"脏余额**原样带出**"。
//
// ⚠️ 思迅的 residual_amt 是密文/脏值("@><:6642675" / "86220.." / ",")⇒
//
//	导出端"顺手"把它变成 0 = 把"读不懂"说成"余额 0"
//	⇒ 导入端就永远不会报"这个值我读不懂"。
func TestExportValue_DirtyBalanceKeptAsIs(t *testing.T) {
	for _, dirty := range []string{"@><:6642675", "86220..", ","} {
		got := exportValue(json.RawMessage(`"`+dirty+`"`), "balance_yuan")
		s, ok := got.(string)
		if !ok {
			t.Fatalf("balance_yuan 应返回字符串,得到 %#v", got)
		}
		if s != dirty {
			t.Errorf("脏余额 %q 应原样带出,实际 %q", dirty, s)
		}
	}
}

// TestExportValue_MemberNonNumericFieldsNotDecoded 锁"会员的文本列不许 base64 解码"。
func TestExportValue_MemberNonNumericFieldsNotDecoded(t *testing.T) {
	// base64("ABC") = "QUJD" —— 看着像 base64,但解出来不是数字。
	got := exportValue(json.RawMessage(`"QUJD"`), "name")
	s, ok := got.(string)
	if !ok {
		t.Fatalf("name 字段应返回字符串,得到 %#v", got)
	}
	if s != "QUJD" {
		t.Errorf("name 不该被 base64 解码,实际 %q", s)
	}
}

// memberCapturingClient 捕获发往 cube 的请求体,并回一整行会员数据。
type memberCapturingClient struct {
	dapr.Client
	body []byte
}

func (f *memberCapturingClient) InvokeMethodWithContent(
	_ context.Context, _, _, _ string, content *dapr.DataContent,
) ([]byte, error) {
	f.body = content.Data
	return []byte(`{"data":[{"member.id":"M1","member.card_face_no":"F1","member.card_type":"0",` +
		`"member.name":"张三","member.gender":"男","member.phone":"13800000000",` +
		`"member.mobile":"","member.birthday":"1990-01-01","member.id_card_no":"",` +
		`"member.status":"0","member.fee_amount_yuan":"0.0000","member.limited_use_count":"999998",` +
		`"member.points_accumulated":"5.0000","member.points_used":"0.0000",` +
		`"member.balance_yuan":"10.0000","member.consumed_amount_yuan":"0.0000",` +
		`"member.consumed_count":"0","member.favorite":"","member.remark":"",` +
		`"member.branch_no":"00","member.operator_id":"1003","member.operated_at":"2024-05-01 10:00:00",` +
		`"member.valid_from":"2024-05-01","member.valid_until":"2030-05-01",` +
		`"member.open_id":""}],"total":1}`), nil
}

// TestExportScopeMembers_RowShape 锁"导出的会员行字段名 == 契约列名"。
func TestExportScopeMembers_RowShape(t *testing.T) {
	fake := &memberCapturingClient{}
	h := &Handler{daprClient: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sc, ok := exportScopes["members"]
	if !ok {
		t.Fatalf("导出端点没有实现 members scope")
	}
	rows, _, err := h.exportFromCube(context.Background(), "cube-00", "00", sc, "2026-10-21", 100, 0)
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行,实际 %d", len(rows))
	}
	row := rows[0]
	if row["id"] != "M1" {
		t.Errorf("id 应为 M1,实际 %v", row["id"])
	}
	if row["card_type"] != "0" {
		t.Errorf("card_type 应原样带出 0,实际 %v", row["card_type"])
	}
	if row["status"] != "0" {
		t.Errorf("status 应原样带出 0(不在导出端归一化),实际 %v", row["status"])
	}
	// ⚠️ 金额保留**原始字面量**(契约:金额是原样字符串,不是数字)。
	if row["balance_yuan"] != "10.0000" {
		t.Errorf("balance_yuan 应保留原始字面量 10.0000,实际 %v", row["balance_yuan"])
	}
	// ⚠️ 999998 哨兵**原样带出**(归一化在导入端做)。
	if row["limited_use_count"] != "999998" {
		t.Errorf("limited_use_count 应原样带出 999998,实际 %v", row["limited_use_count"])
	}
	// 每一列都必须在。
	for _, col := range []string{"id", "card_face_no", "card_type", "name", "gender", "phone",
		"mobile", "birthday", "id_card_no", "status", "fee_amount_yuan", "limited_use_count",
		"points_accumulated", "points_used", "balance_yuan", "consumed_amount_yuan",
		"consumed_count", "favorite", "remark", "branch_no", "operator_id", "operated_at",
		"valid_from", "valid_until", "open_id"} {
		if _, ok := row[col]; !ok {
			t.Errorf("导出的行缺少列 %q", col)
		}
	}
}
