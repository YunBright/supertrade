package service

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"strings"
	"testing"
)

// 明细清单的形状回归锁。
//
// # 为什么锁这些
//
// 这份 CSV 是**审批人唯一的明细来源**(企微审批单上只有"结算单号+金额+备注")：
//   - 少一列 ⇒ 审批人看不到"付给哪家、多少钱"；
//   - 少 BOM ⇒ Excel 按 GBK 猜 ⇒ 整张表乱码(而且乱得很安静,没人会怪代码)；
//   - 合计算错 ⇒ 审批人按合计复核会以为金额对不上 ⇒ 单被反复打回。
func TestBuildSettlementDetailCSV(t *testing.T) {
	rows := []SettlementRow{
		{ID: "CP001", SupplierID: "142", SupplierName: "北方鲜鸡蛋店", DocumentType: "CP",
			PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", TotalGoodsYuan: dec("2000")},
		{ID: "CP002", SupplierID: "425", DocumentType: "CP",
			PeriodStart: "09  1 2026 12:00AM", PeriodEnd: "09 30 2026 12:00AM", TotalGoodsYuan: dec("1252.5")},
	}
	out := buildSettlementDetailCSV(rows)

	if !bytes.HasPrefix(out, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("缺 UTF-8 BOM:Excel 会按 GBK 猜,整张表乱码")
	}
	recs, err := csv.NewReader(bytes.NewReader(out[3:])).ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("行数 = %d, want 4(表头+2 单+合计)", len(recs))
	}
	if got := recs[0]; strings.Join(got, "|") != "结算单号|供应商|单据类型|账期|货款金额(元)" {
		t.Fatalf("表头 = %v", got)
	}
	// 供应商:编号-名称(与 H5 口径一致)
	if got := recs[1][1]; got != "142-北方鲜鸡蛋店" {
		t.Fatalf("供应商列 = %q", got)
	}
	// 没有名称时只给编号,不补一个悬空的 "-"
	if got := recs[2][1]; got != "425" {
		t.Fatalf("无名称的供应商列 = %q, want 425", got)
	}
	// 账期:ISO 与 MSSQL 两种来源都要规整成 2006-01-02
	if got := recs[1][3]; got != "2026-09-01 ~ 2026-09-30" {
		t.Fatalf("ISO 账期 = %q", got)
	}
	if got := recs[2][3]; got != "2026-09-01 ~ 2026-09-30" {
		t.Fatalf("MSSQL 账期 = %q(未规整)", got)
	}
	// 金额两位小数
	if got := recs[1][4]; got != "2000.00" {
		t.Fatalf("金额列 = %q", got)
	}
	// 合计行
	if got := recs[3][4]; got != "3252.50" {
		t.Fatalf("合计 = %q, want 3252.50", got)
	}
	if got := recs[3][1]; got != "2 张" {
		t.Fatalf("合计张数 = %q", got)
	}
}

func TestBuildSettlementDetailCSV_Empty(t *testing.T) {
	out := buildSettlementDetailCSV(nil)
	recs, err := csv.NewReader(bytes.NewReader(out[3:])).ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("行数 = %d, want 2(表头+合计)", len(recs))
	}
}

func TestDetailSupplierLabel(t *testing.T) {
	cases := []struct{ id, name, want string }{
		{"142", "北方鲜鸡蛋店", "142-北方鲜鸡蛋店"},
		{"425", "", "425"},
		{"", "自购蔬菜", "自购蔬菜"},
		{"", "", "-"},
		{" 142 ", " 北方鲜鸡蛋店 ", "142-北方鲜鸡蛋店"},
	}
	for _, c := range cases {
		if got := detailSupplierLabel(c.id, c.name); got != c.want {
			t.Errorf("detailSupplierLabel(%q,%q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}
}

func TestBuildSettlementDetailAttachment(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	svc.cfg.AttachmentControlID = "item-1494251220825"

	rows := []SettlementRow{{ID: "CP001", SupplierID: "142", TotalGoodsYuan: dec("10")}}
	att := svc.buildSettlementDetailAttachment(rows)
	if att == nil {
		t.Fatal("配了控件 id 却没生成附件")
	}
	if att.ControlID != "item-1494251220825" {
		t.Fatalf("control id = %q", att.ControlID)
	}
	if att.ControlType != "File" {
		t.Fatalf("control type = %q, want File", att.ControlType)
	}
	if !strings.HasSuffix(att.FileName, ".csv") || !strings.Contains(att.FileName, "CP001") {
		t.Fatalf("文件名 = %q(应含首单号)", att.FileName)
	}
	raw, err := base64.StdEncoding.DecodeString(att.ContentBase64)
	if err != nil {
		t.Fatalf("内容不是合法 base64: %v", err)
	}
	if !bytes.Contains(raw, []byte("CP001")) {
		t.Fatalf("清单内容不含单号: %q", raw)
	}
}

func TestBuildSettlementDetailAttachment_Nil(t *testing.T) {
	svc, _, _, _, _ := newTestService(t)
	rows := []SettlementRow{{ID: "CP001", TotalGoodsYuan: dec("10")}}

	// 没配控件 id ⇒ 不带附件(退回旧行为,不猜 id)
	svc.cfg.AttachmentControlID = ""
	if att := svc.buildSettlementDetailAttachment(rows); att != nil {
		t.Fatalf("没配控件 id 却生成了附件: %+v", att)
	}
	// 没有结算单 ⇒ 也不带附件
	svc.cfg.AttachmentControlID = "item-1"
	if att := svc.buildSettlementDetailAttachment(nil); att != nil {
		t.Fatalf("没有结算单却生成了附件: %+v", att)
	}
}
