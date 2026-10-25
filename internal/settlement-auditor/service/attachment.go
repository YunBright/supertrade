// service/attachment.go —— 「附件」控件的内容生成(结算单明细清单)。
//
// 背景(2026-10-25 门店反馈 + 企微模板实测):
// 企微「结算付款」模板里有一个 **File 型控件「附件」**(id=item-1494251220825,
// require=0 选填)。审批人在审批单上只能看到「结算单号(逗号分隔)+付款金额+备注」,
// 要判断"这十几万到底付给了哪些供应商、每张多少"得自己去 sixun 查 ⇒
// 把本次付款的**结算单明细**做成清单附在审批单上。
//
// # 为什么这里只生成"内容"、不直接写控件
//
// 企微的 File 控件要的是 media_id,而 media_id **只能用持有 OA token 的一侧**
// (wechat-bot,企业微信 OA 的唯一出口)上传素材才能拿到。
// 而"附件里放什么"是业务知识 ⇒ **业务侧生成字节、wechat-bot 上传并组装控件**。
// (wire: service.ApprovalAttachment ↔ wapproval.ApprovalAttachment)
//
// # 为什么是 CSV(带 UTF-8 BOM)
//
// 审批人是拿 Excel 打开的:不带 BOM 的 UTF-8 CSV 在 Excel 里按 GBK 猜 ⇒ 乱码。
// ⇒ 必须写 BOM(EF BB BF)。
package service

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// 明细清单的表头(审批人第一眼就要看懂)。
var settlementDetailHeader = []string{"结算单号", "供应商", "单据类型", "账期", "货款金额(元)"}

// buildSettlementDetailCSV 生成结算单明细清单(CSV,UTF-8 BOM)。
//
// rows 来自 cube(带供应商名与**当前**货款金额)⇒ 清单里的金额与审批单上的
// 付款金额是同一份数据,不会自相矛盾。
func buildSettlementDetailCSV(rows []SettlementRow) []byte {
	var buf bytes.Buffer
	// Excel 认 BOM 才不会把 UTF-8 当 GBK 猜。
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	_ = w.Write(settlementDetailHeader)
	total := decimal.Zero
	for _, r := range rows {
		_ = w.Write([]string{
			r.ID,
			detailSupplierLabel(r.SupplierID, r.SupplierName),
			r.DocumentType,
			detailPeriodLabel(r.PeriodStart, r.PeriodEnd),
			r.TotalGoodsYuan.StringFixed(2),
		})
		total = total.Add(r.TotalGoodsYuan)
	}
	_ = w.Write([]string{"合计", fmt.Sprintf("%d 张", len(rows)), "", "", total.StringFixed(2)})
	w.Flush()
	return buf.Bytes()
}

// detailSupplierLabel 供应商展示:"编号-名称"(与 H5 发起页口径一致)。
//
// 编号不能丢:同名供应商(不同门店常见的粮油批发同名)分不清是哪一家;
// 名称不能丢:店长核对的对象是**供应商**,只有编号他得回 sixun 查名字。
func detailSupplierLabel(id, name string) string {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	switch {
	case id == "" && name == "":
		return "-"
	case name == "":
		return id
	case id == "":
		return name
	default:
		return id + "-" + name
	}
}

// detailPeriodLabel 账期展示:"起 ~ 止"(起止都缺时给 "-")。
//
// cube 的日期串有两种形态(ISO / MSSQL 美式),统一走 normalizeCubeDate
// 规整成 2006-01-02 —— 解析不了的保留原串(总比空强)。
func detailPeriodLabel(start, end string) string {
	s := normalizeCubeDate(start)
	e := normalizeCubeDate(end)
	switch {
	case s == "" && e == "":
		return "-"
	case s == "":
		return e
	case e == "":
		return s
	default:
		return s + " ~ " + e
	}
}

// buildSettlementDetailAttachment 生成「附件」控件的待上传内容(明细清单)。
//
// 返回 nil 表示"这次不带附件"(没有配控件 id 或没有结算单)。
func (s *Service) buildSettlementDetailAttachment(rows []SettlementRow) *ApprovalAttachment {
	controlID := strings.TrimSpace(s.cfg.AttachmentControlID)
	if controlID == "" || len(rows) == 0 {
		return nil
	}
	ctlType := strings.TrimSpace(s.cfg.AttachmentControlType)
	if ctlType == "" {
		ctlType = "File"
	}
	csvBytes := buildSettlementDetailCSV(rows)
	return &ApprovalAttachment{
		ControlID:    controlID,
		ControlType:  ctlType,
		FileName:     detailFileName(rows),
		ContentBase64: base64.StdEncoding.EncodeToString(csvBytes),
	}
}

// detailFileName 附件名:"结算单明细-<首单号>.csv"。
//
// 用首单号而不是日期:审批单列表里一眼能对上是哪一批。
func detailFileName(rows []SettlementRow) string {
	first := ""
	if len(rows) > 0 {
		first = strings.TrimSpace(rows[0].ID)
	}
	if first == "" {
		return "结算单明细.csv"
	}
	return "结算单明细-" + first + ".csv"
}
