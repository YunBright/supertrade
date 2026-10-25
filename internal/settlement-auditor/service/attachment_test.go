package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
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
	out := buildSettlementDetailCSV(rows, nil)

	if !strings.HasPrefix(string(out), "\xEF\xBB\xBF") {
		t.Fatal("缺 UTF-8 BOM:Excel 会按 GBK 猜,整张表乱码")
	}
	recs, err := csv.NewReader(strings.NewReader(string(out[3:]))).ReadAll()
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
	out := buildSettlementDetailCSV(nil, nil)
	recs, err := csv.NewReader(strings.NewReader(string(out[3:]))).ReadAll()
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

// 附件文件名:前端给的名字可能带路径/超长/是空的 ⇒ 落库前必须收敛。
func TestSanitizeAttachmentName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"发票.jpg", "发票.jpg"},
		{"../../etc/passwd", "passwd"},
		{`C:\temp\photo.png`, "photo.png"},
		{"", "附件"},
		{"..", "附件"},
		{"  ", "附件"},
	}
	for _, c := range cases {
		if got := sanitizeAttachmentName(c.in); got != c.want {
			t.Errorf("sanitizeAttachmentName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := strings.Repeat("长", 200)
	if got := sanitizeAttachmentName(long); len([]rune(got)) != 120 {
		t.Errorf("超长名未截断: %d rune", len([]rune(got)))
	}
}

// 附件校验:base64 坏 / 空文件 / 超大 ⇒ 当场拒(别留到流水线里炸)。
func TestValidateAttachment(t *testing.T) {
	ok, err := validateAttachment(AttachmentInput{
		FileName:      "发票.jpg",
		ContentBase64: base64.StdEncoding.EncodeToString([]byte("JPEGBYTES")),
	})
	if err != nil {
		t.Fatalf("正常附件被拒: %v", err)
	}
	if ok.Size != len("JPEGBYTES") {
		t.Fatalf("size = %d, want %d", ok.Size, len("JPEGBYTES"))
	}

	if _, err := validateAttachment(AttachmentInput{FileName: "a.jpg", ContentBase64: "!!!not-base64!!!"}); err == nil {
		t.Error("非法 base64 必须被拒")
	}
	if _, err := validateAttachment(AttachmentInput{FileName: "a.jpg", ContentBase64: ""}); err == nil {
		t.Error("空内容必须被拒")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, maxAttachmentBytes+1))
	if _, err := validateAttachment(AttachmentInput{FileName: "a.bin", ContentBase64: big}); err == nil {
		t.Error("超限附件必须被拒")
	}
}

// 发起人上传的附件要活到"机器审核通过"那一刻 ⇒ 落库 + 能读回来。
func TestStoreAndLoadApprovalAttachments(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ap := &model.ApprovalInstance{
		ID: "ap-att", BranchID: "B1", ApplicantUserID: "u1",
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	ins := []AttachmentInput{
		{FileName: "发票.jpg", ContentBase64: base64.StdEncoding.EncodeToString([]byte("JPEGBYTES"))},
	}
	if err := svc.storeAttachments(context.Background(), ap, ins); err != nil {
		t.Fatalf("storeAttachments: %v", err)
	}

	// 读回来:第一份是自动生成的明细清单,后面是发起人上传的。
	svc.cfg.AttachmentControlID = "item-1494251220825"
	got := svc.loadApprovalAttachments(context.Background(), ap, []SettlementRow{
		{ID: "CP001", SupplierID: "142", TotalGoodsYuan: dec("10")},
	}, nil)
	if len(got) != 2 {
		t.Fatalf("附件数 = %d, want 2(明细清单 + 上传)", len(got))
	}
	if !strings.HasPrefix(got[0].FileName, "结算单明细-") {
		t.Fatalf("第 1 个应是明细清单: %q", got[0].FileName)
	}
	if got[1].FileName != "发票.jpg" {
		t.Fatalf("第 2 个应是上传的文件: %q", got[1].FileName)
	}
	raw, err := base64.StdEncoding.DecodeString(got[1].ContentBase64)
	if err != nil || string(raw) != "JPEGBYTES" {
		t.Fatalf("读回的内容不对: %q err=%v", raw, err)
	}
}

// 没配附件控件 id ⇒ 不带附件(不猜 id;猜错只会让企微报"控件不存在")。
func TestLoadApprovalAttachments_NoControlID(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-no", BranchID: "B1"}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	svc.cfg.AttachmentControlID = ""
	got := svc.loadApprovalAttachments(context.Background(), ap, []SettlementRow{{ID: "CP001", TotalGoodsYuan: dec("10")}}, nil)
	if len(got) != 0 {
		t.Fatalf("没配控件 id 却带了附件: %+v", got)
	}
}

// 跨仓 wire 契约(supertrade ↔ wechat-bot)。
//
// 两个 module 不能互相 import ⇒ 字段名对不上时**编译期看不出来**,
// 运行时表现为"附件静默丢失"(企微单上就是没有附件,没有任何报错)。
// ⇒ 这里把 JSON 形状钉死;wechat-bot 侧有对应的反序列化测试。
func TestApplyEventPayloadAttachmentWire(t *testing.T) {
	p := ApplyEventPayload{
		TemplateID:            "tpl",
		AttachmentControlID:   "item-1494251220825",
		AttachmentControlType: "File",
		Attachments: []ApprovalAttachment{
			{FileName: "发票.jpg", ContentBase64: "QUJD"},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["attachment_control_id"] != "item-1494251220825" {
		t.Fatalf("attachment_control_id 不对: %s", b)
	}
	if m["attachment_control_type"] != "File" {
		t.Fatalf("attachment_control_type 不对: %s", b)
	}
	atts, _ := m["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("attachments 数量 = %d: %s", len(atts), b)
	}
	a, _ := atts[0].(map[string]any)
	if a["file_name"] != "发票.jpg" || a["content_base64"] != "QUJD" {
		t.Fatalf("attachments 元素形状不对: %s", b)
	}
	// ⚠️ 附件内容**不能**出现在任何别的字段里(重复=体积翻倍)。
	if len(a) != 2 {
		t.Fatalf("attachments 元素字段数 = %d, want 2: %s", len(a), b)
	}
}
