// Package model 的 wire 形状回归锁。
//
// # 为什么这个测试存在
//
// 本模型是 bank-account 服务的**出参**(handler 直接 c.JSON(..., rows)),
// 所以 json tag 就是**前后端契约**,不是装饰。
//
// ⚠️ supertrade 里有两套 wire 风格,极易踩坑:
//
//   - cube 查询结果(如 settlement-auditor 的 SettlementRow)⇒ **没有** json tag,
//     wire 上就是 Go 导出名(ID / SupplierID / SupplierName);
//   - 本域自己的模型(如本表)⇒ **有** snake_case json tag。
//
// # 这个 bug 真实发生过(2026-10-28)
//
// 本模型最初只写了 GORM tag、没写 json tag ⇒ 响应是
// `{"ID":"...","SupplierID":"...","AccountNo":"..."}`,
// 而两个前端都按 snake_case 读(`a.id` / `a.account_no`)
// ⇒ 取到 undefined ⇒ 下拉列表被过滤成空
// ⇒ 页面显示"暂无可选收款账号",**HTTP 200 + 零报错**。
//
// 同一批代码里 settlement-auditor 的 PayeeAccountDTO 按 snake_case 反序列化
// ⇒ 账号 id 取不到 ⇒ 报"bank-account 返回空账号" ⇒
// 付款信息控件**永远是空的**,而审批提交**照常成功**。
//
// ⇒ 光靠"跑起来看着没报错"发现不了。这里把形状钉死。
package model

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBankAccountJSONWireShape 锁住 snake_case 出参形状。
func TestBankAccountJSONWireShape(t *testing.T) {
	row := BankAccount{
		ID:           "ba-1",
		BranchID:     "00",
		SupplierID:   "S-77",
		SupplierName: "南宁某贸易",
		BankName:     "中国银行南宁分行",
		AccountNo:    "6222000000000000",
		AccountName:  "南宁某贸易有限公司",
		IsDefault:    true,
		Remark:       "对公",
		CreatedBy:    "u-1",
		UpdatedBy:    "u-1",
		CreatedAt:    time.Unix(0, 0).UTC(),
		UpdatedAt:    time.Unix(0, 0).UTC(),
	}

	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// 前端(两个)依赖的字段必须逐字存在。
	want := []string{
		"id", "branch_id", "supplier_id", "supplier_name",
		"bank_name", "account_no", "account_name",
		"is_default", "remark", "created_at", "updated_at",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("wire 上缺字段 %q —— 前端按 snake_case 读,缺了就是「永远读不到」; got keys=%v", k, keysOf(m))
		}
	}

	// ⚠️ Go 导出名**不得**出现:出现即说明 json tag 掉了
	// (或者有人把 tag 写错成 `json:"ID"` 之类)。
	// 这两种情况的症状一样:前端取到 undefined,而 HTTP 200 零报错。
	for _, k := range []string{"ID", "BranchID", "SupplierID", "SupplierName",
		"BankName", "AccountNo", "AccountName", "IsDefault", "Remark",
		"CreatedBy", "UpdatedBy", "CreatedAt", "UpdatedAt"} {
		if _, ok := m[k]; ok {
			t.Errorf("wire 上出现了 Go 导出名 %q ⇒ json tag 掉了;"+
				"前端按 snake_case 读会静默取到 undefined", k)
		}
	}

	// 值也要对得上(不只是键存在)。
	if got := m["account_no"]; got != "6222000000000000" {
		t.Errorf("account_no 值应为 \"6222000000000000\",实际 %v", got)
	}
	if got := m["is_default"]; got != true {
		t.Errorf("is_default 值应为 true,实际 %v", got)
	}
}

// TestBankAccountJSONMatchesFrontEndContract 锁住**具体取值**的端到端形状。
//
// 这条比上一条更贴近真实使用:前端拿到这条 JSON,直接就能渲染下拉。
func TestBankAccountJSONMatchesFrontEndContract(t *testing.T) {
	row := BankAccount{
		ID:          "ba-9",
		SupplierID:  "S-1",
		BankName:    "工商银行",
		AccountNo:   "6222",
		AccountName: "某公司",
		IsDefault:   false,
	}
	raw, _ := json.Marshal(row)

	// 模拟前端(settlement-bank.js 的 normAccounts + isDefaultBankAccount)。
	var f struct {
		ID          string `json:"id"`
		SupplierID  string `json:"supplier_id"`
		BankName    string `json:"bank_name"`
		AccountNo   string `json:"account_no"`
		AccountName string `json:"account_name"`
		IsDefault   bool   `json:"is_default"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.ID == "" {
		t.Fatal("前端取不到 id ⇒ 账号列表会被过滤成空(症状:「暂无可选收款账号」)")
	}
	if f.SupplierID != "S-1" || f.BankName != "工商银行" ||
		f.AccountNo != "6222" || f.AccountName != "某公司" {
		t.Errorf("前端取到的字段不对: %+v", f)
	}
}

// keysOf 取 map 的键（错误信息里用）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
