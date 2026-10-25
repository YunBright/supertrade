// service/attachment.go —— 审批附件：发起人上传的文件 + 自动生成的结算单明细清单。
//
// 背景(2026-10-25 门店反馈 + 企微模板实测):
// 企微「结算付款」模板里有一个 **File 型控件「附件」**(id=item-1494251220825,
// require=0 选填)。后端原先不写它 ⇒ 审批单上那个控件一直是空的。
// 而发起人自己也需要一个"附件"入口 —— 像企微官方审批应用那样，**拍照/相册选图**
// 传上去(发票、收据照片)，审批人在企微单上直接看得到。
//
// # 两条线
//
//   1. 发起人自己上传的图片/文件 —— 提交时随 body 过来 → **落库**(机器审核是异步的)
//      → 机器审核通过时读出来 → 交给 wechat-bot 上传企微素材。
//   2. 自动生成的「结算单明细清单」(CSV) —— 机器审核通过那一刻现算(用 cube 当前金额)
//      → 交给 wechat-bot 上传企微素材。
//
// # 为什么上传动作在 wechat-bot 侧
//
// 企微的 File 型控件要的是 media_id,而 media_id **只能用持 OA token 的一侧**
// (wechat-bot,企业微信 OA 的唯一出口)上传素材才能拿到 ——
// media_id 绑定上传它的应用,别的应用用不了。
// 而"附件里放什么"是业务知识 ⇒ **业务侧给内容,企微出口负责上传并组装控件**。
// (wire: service.ApprovalAttachment ↔ wapproval.ApprovalAttachment)
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 附件上限（防"发一张 20MB 图把审批单撑爆"）。
//
// ⚠️ base64 会让体积涨约 1/3 ⇒ 单个上限按**解码后**的字节数算。
const (
	// maxAttachmentsPerApproval 单次审批附件个数上限。
	maxAttachmentsPerApproval = 3
	// maxAttachmentBytes 单个附件解码后的字节上限(2MB)。
	maxAttachmentBytes = 2 << 20
)

// AttachmentInput 是提交请求里"发起人上传的一个附件"。
type AttachmentInput struct {
	FileName      string
	ContentType   string
	ContentBase64 string
	Size          int
}

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

// sanitizeAttachmentName 去掉路径分隔符并限长(文件名会显示在审批单上)。
//
// ⚠️ 不能直接用前端给的名字:可能带 "../../etc/passwd" 或超长中文。
func sanitizeAttachmentName(name string) string {
	name = strings.TrimSpace(name)
	// 只取最后一段(跨 / 与 \ 两种分隔符)。
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = filepath.Base(name)
	if name == "" || name == "." || name == ".." {
		return "附件"
	}
	// 按 rune 限长(中文按字符算)。
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	return name
}

// validateAttachment 校验并解码一个上传附件。
func validateAttachment(in AttachmentInput) (AttachmentInput, error) {
	in.FileName = sanitizeAttachmentName(in.FileName)
	if in.FileName == "" {
		return in, fmt.Errorf("%w: 附件文件名不能为空", ErrInvalidInput)
	}
	raw, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		return in, fmt.Errorf("%w: 附件 %q 不是合法 base64", ErrInvalidInput, in.FileName)
	}
	if len(raw) == 0 {
		return in, fmt.Errorf("%w: 附件 %q 是空文件", ErrInvalidInput, in.FileName)
	}
	if len(raw) > maxAttachmentBytes {
		return in, fmt.Errorf("%w: 附件 %q 超过 %d KB 上限", ErrInvalidInput, in.FileName, maxAttachmentBytes>>10)
	}
	in.Size = len(raw)
	return in, nil
}

// storeAttachments 落库发起人上传的附件(机器审核是异步的 ⇒ 字节必须活到 pass 那一刻)。
func (s *Service) storeAttachments(ctx context.Context, ap *model.ApprovalInstance, ins []AttachmentInput) error {
	if len(ins) == 0 {
		return nil
	}
	for _, in := range ins {
		raw, err := base64.StdEncoding.DecodeString(in.ContentBase64)
		if err != nil {
			return fmt.Errorf("%w: 附件 %q 不是合法 base64", ErrInvalidInput, in.FileName)
		}
		row := &model.ApprovalAttachment{
			ID:              uuid.NewString(),
			ApprovalID:      ap.ID,
			BranchID:        ap.BranchID,
			ApplicantUserID: ap.ApplicantUserID,
			FileName:        in.FileName,
			ContentType:     in.ContentType,
			Size:            len(raw),
			Data:            raw,
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			return fmt.Errorf("create approval_attachment %s: %w", in.FileName, err)
		}
	}
	return nil
}

// loadApprovalAttachments 读出这次审批要附的全部文件(明细清单 + 发起人上传的)。
//
// 顺序：**明细清单在前**，发起人上传的在后(审批人先看单据明细，再看凭证)。
func (s *Service) loadApprovalAttachments(ctx context.Context, ap *model.ApprovalInstance, settlements []SettlementRow) []ApprovalAttachment {
	out := make([]ApprovalAttachment, 0, maxAttachmentsPerApproval+1)
	// ① 自动生成的结算单明细清单(用 cube 当前金额 ⇒ 与审批单上的付款金额同源)。
	if strings.TrimSpace(s.cfg.AttachmentControlID) != "" && len(settlements) > 0 {
		out = append(out, ApprovalAttachment{
			FileName:      detailFileName(settlements),
			ContentBase64: base64.StdEncoding.EncodeToString(buildSettlementDetailCSV(settlements)),
		})
	}
	// ② 发起人自己上传的(落库的那批)。
	var rows []model.ApprovalAttachment
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", ap.ID).
		Order("created_at asc").
		Find(&rows).Error; err != nil {
		s.logger.Warn("load approval attachments failed", "approval_id", ap.ID, "err", err)
		return out
	}
	for _, r := range rows {
		if len(r.Data) == 0 {
			continue
		}
		out = append(out, ApprovalAttachment{
			FileName:      r.FileName,
			ContentBase64: base64.StdEncoding.EncodeToString(r.Data),
		})
	}
	return out
}
