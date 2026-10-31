// Package service / items.go —— 付款审批的「收付款明细」业务口径（2026-10-25 用户需求）。
//
// # 业务形态
//
// 「付款审批」不再只有"付供应商货款"这一种。门店实际还会发生：
//
//	付款项：水电费 / 租金 / 物业费 / 垃圾处理费 / 软件服务费 / 其它
//	收款项：陈列费 / 堆头费 / 补差价 / 兑换 / 纸皮回收 / 其它
//
// 供应商结算单归为**付款类型「货款」**。收款项**冲减**付款金额 ⇒
//
//	应付金额 = 货款 + 付款项合计 − 收款项合计（且不为负）
//
// # 三条口径（本文件的全部规则）
//
//  1. 类型与备注**皆以文本存储**（model.ApprovalItem）——「其它」允许用户把
//     "其它"两字改成自定义文字，所以类型不能是枚举。
//  2. 货款**不落明细表**：事实源是 approval_settlements（cube 快照），
//     明细表只存人填的行，货款行在读取/审核时派生。
//  3. 金额一律走"分位整数"语义的 decimal，不做元级四舍五入后再比较。
//
// # 机器审核怎么用这些明细（service/allowance.go）
//
// 收款项不只是抵扣金额，还是**审核放宽额度**：
//
//	陈列费 / 堆头费 ⇒ 进货可以多些（数量偏离不再阻断）
//	补差价         ⇒ 允许毛利降低（额度 = 补差价总额）
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 明细方向。
const (
	// ItemDirectionPayment 付款项（钱出去）。
	ItemDirectionPayment = "payment"
	// ItemDirectionReceipt 收款项（钱进来）。
	ItemDirectionReceipt = "receipt"
)

// 类型文本（与 H5 下拉选项逐字一致）。
//
// ⚠️ 「货款」是**派生**类型：它由勾选的供应商结算单产生，不进明细表。
const (
	ItemTypeGoods = "货款"

	// 收款项类型。
	ReceiptTypeDisplay     = "陈列费"
	ReceiptTypePromotion   = "堆头费"
	ReceiptTypePriceDiff   = "补差价"
	ReceiptTypeExchange    = "兑换"
	ReceiptTypePaperRecycl = "纸皮回收"
	ReceiptTypeOther       = "其它"

	// 付款项类型。
	//
	// 2026-10-30 用户口径：门店实际还会付运费、肉类加工费、设备维护费、
	// 耗材、招待费、燃气费、福利 ⇒ 一并进下拉,避免店长全填「其它」。
	// 2026-10-31 追加「电信费」(宽带/电话/流量)。
	PaymentTypeUtility          = "水电费"
	PaymentTypeRent             = "租金"
	PaymentTypeProperty         = "物业费"
	PaymentTypeTrash            = "垃圾处理费"
	PaymentTypeSoftware         = "软件服务费"
	PaymentTypeFreight          = "运费"
	PaymentTypeMeatProcessing   = "肉类加工费"
	PaymentTypeEquipMaintenance = "设备维护费"
	PaymentTypeConsumable       = "耗材"
	PaymentTypeEntertainment    = "招待费"
	PaymentTypeGas              = "燃气费"
	PaymentTypeTelecom          = "电信费"
	PaymentTypeWelfare          = "福利"
	PaymentTypeOther            = "其它"
)

// ReceiptTypeOptions 是收款项下拉选项（前端同款，改这里要同步改前端）。
func ReceiptTypeOptions() []string {
	return []string{
		ReceiptTypeDisplay, ReceiptTypePromotion, ReceiptTypePriceDiff,
		ReceiptTypeExchange, ReceiptTypePaperRecycl, ReceiptTypeOther,
	}
}

// PaymentTypeOptions 是付款项下拉选项（前端同款，改这里要同步改前端）。
func PaymentTypeOptions() []string {
	return []string{
		PaymentTypeUtility, PaymentTypeRent, PaymentTypeProperty,
		PaymentTypeTrash, PaymentTypeSoftware,
		PaymentTypeFreight, PaymentTypeMeatProcessing, PaymentTypeEquipMaintenance,
		PaymentTypeConsumable, PaymentTypeEntertainment, PaymentTypeGas, PaymentTypeTelecom, PaymentTypeWelfare,
		PaymentTypeOther,
	}
}

// maxItemsPerDirection 单个方向（收/付）的手工明细行数上限。
//
// 为什么要上限：这些行会进 LLM 提示词、企微备注、附件 CSV。
// 行数失控 ⇒ 提示词爆掉、审批单上一屏噪音。
const maxItemsPerDirection = 20

// maxItemTypeLen / maxItemRemarkLen 明细文本的长度上限。
const (
	maxItemTypeLen   = 64
	maxItemRemarkLen = 256
)

// ItemInput 是提交请求里的一行收付款明细（人填的）。
type ItemInput struct {
	// Type 是类型文本（"陈列费"…；「其它」可被改成自定义文字）。
	Type string
	// Remark 是备注（所属日期等补充信息）。
	Remark string
	// AmountYuan 是金额（元）。
	AmountYuan decimal.Decimal
}

// ItemRow 是**参与审核与展示**的一行明细（货款行也在内）。
//
// # 为什么货款行也要变成 ItemRow
//
// 机器审核、企微备注、附件清单三处都要"列出全部收付款项"
// ⇒ 需要一个**统一形状**的行；货款行由结算单快照派生后套进同一个形状。
type ItemRow struct {
	// Direction 方向：payment / receipt。
	Direction string
	// Type 类型文本。
	Type string
	// Remark 备注。
	Remark string
	// AmountYuan 金额（元）。
	AmountYuan decimal.Decimal
	// SettlementID 仅货款行有值（指回具体结算单）。
	SettlementID string
}

// ItemTotals 是本次审批的金额汇总。
type ItemTotals struct {
	// GoodsTotalYuan 是勾选结算单的货款合计（cube 口径）。
	GoodsTotalYuan decimal.Decimal
	// PaymentTotalYuan 是手工付款项合计（水电费/租金/…）。
	PaymentTotalYuan decimal.Decimal
	// ReceiptTotalYuan 是手工收款项合计（陈列费/堆头费/…）。
	ReceiptTotalYuan decimal.Decimal
	// NetYuan 是净额 = 货款 + 付款项 − 收款项（**可以为负**）。
	NetYuan decimal.Decimal
	// PayableYuan 是**真正要付的钱** = max(0, 净额)。
	//
	// # 为什么截断到 0 而不是报错
	//
	// 用户口径（2026-10-25）：「付款审批可以只有'货款'，只有'纸皮回收'，
	// 也可以同时有'货款'和'陈列费'」—— **纯收款项也是合法的一单**
	// （门店卖废纸皮收到的钱，钱进得来、这次不用付出去）。
	// 而「保持付款金额是大于或等于0的」⇒ 应付金额不能为负 ⇒ 截断到 0，
	// 而不是把这类单子挡在门外。
	PayableYuan decimal.Decimal
}

// computeTotals 汇总三笔账并算出应付金额。
func computeTotals(goods, payment, receipt decimal.Decimal) ItemTotals {
	t := ItemTotals{
		GoodsTotalYuan:   goods,
		PaymentTotalYuan: payment,
		ReceiptTotalYuan: receipt,
		NetYuan:          goods.Add(payment).Sub(receipt),
	}
	if t.NetYuan.GreaterThan(decimal.Zero) {
		t.PayableYuan = t.NetYuan
	} else {
		t.PayableYuan = decimal.Zero
	}
	return t
}

// validateItems 校验并归一化一批明细行。
//
// 校验项：类型非空且不超长、备注不超长、金额 > 0、行数不超上限。
//
// ⚠️ 金额用 decimal 判 > 0：0 元或负数的明细行没有业务含义
// （"负数的付款项"等于手工造出一笔收款，会把应付金额算歪）。
func validateItems(direction string, items []ItemInput) ([]ItemInput, error) {
	if len(items) > maxItemsPerDirection {
		return nil, fmt.Errorf("%w: %s明细最多 %d 行", ErrInvalidInput, directionLabel(direction), maxItemsPerDirection)
	}
	out := make([]ItemInput, 0, len(items))
	for i, in := range items {
		t := strings.TrimSpace(in.Type)
		if t == "" {
			return nil, fmt.Errorf("%w: 第 %d 行%s明细缺类型", ErrInvalidInput, i+1, directionLabel(direction))
		}
		if len([]rune(t)) > maxItemTypeLen {
			return nil, fmt.Errorf("%w: 第 %d 行类型超过 %d 字", ErrInvalidInput, i+1, maxItemTypeLen)
		}
		r := strings.TrimSpace(in.Remark)
		if len([]rune(r)) > maxItemRemarkLen {
			return nil, fmt.Errorf("%w: 第 %d 行备注超过 %d 字", ErrInvalidInput, i+1, maxItemRemarkLen)
		}
		if !in.AmountYuan.GreaterThan(decimal.Zero) {
			return nil, fmt.Errorf("%w: 第 %d 行金额必须大于 0", ErrInvalidInput, i+1)
		}
		out = append(out, ItemInput{Type: t, Remark: r, AmountYuan: in.AmountYuan})
	}
	return out, nil
}

// directionLabel 方向的展示名（错误提示用）。
func directionLabel(direction string) string {
	switch direction {
	case ItemDirectionPayment:
		return "付款"
	case ItemDirectionReceipt:
		return "收款"
	default:
		return "收付"
	}
}

// ---- 落库 / 读库 ----

// storeItems 落库手工收付款明细。
//
// 机器审核是异步的(go s.runAudit)⇒ 审核时要读得到这些行,必须先落库。
//
// 同一事务内同时写 `approval_monthly_items`（月度总表，计划 §9）：
// 明细与投影必须原子落库，否则"明细写了、总表没写"会让月度统计静默漏行。
func (s *Service) storeItems(ctx context.Context, ap *model.ApprovalInstance, receipts, payments []ItemInput) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rows := make([]model.ApprovalItem, 0, len(receipts)+len(payments))
		appendDir := func(direction string, items []ItemInput) error {
			for _, in := range items {
				row := &model.ApprovalItem{
					ID:              uuid.NewString(),
					ApprovalID:      ap.ID,
					BranchID:        ap.BranchID,
					ApplicantUserID: ap.ApplicantUserID,
					Direction:       direction,
					ItemType:        in.Type,
					Remark:          in.Remark,
					AmountYuan:      in.AmountYuan,
				}
				if err := tx.Create(row).Error; err != nil {
					return fmt.Errorf("create approval_item %s: %w", in.Type, err)
				}
				rows = append(rows, *row)
			}
			return nil
		}
		if err := appendDir(ItemDirectionReceipt, receipts); err != nil {
			return err
		}
		if err := appendDir(ItemDirectionPayment, payments); err != nil {
			return err
		}
		return writeMonthlyItemsTx(tx, ap, rows, s.now())
	})
}

// loadApprovalItems 读出本次审批的手工收付款明细(按创建顺序)。
//
// 读不到不报错(返回空切片)—— 明细是**增量**信息:
// 读不到只意味着"按没有收付款项审核",不该让整条流水线失败。
func (s *Service) loadApprovalItems(ctx context.Context, approvalID string) []model.ApprovalItem {
	var items []model.ApprovalItem
	if err := s.db.WithContext(ctx).
		Where("approval_id = ?", approvalID).
		Order("created_at ASC").
		Find(&items).Error; err != nil {
		s.logger.Warn("load approval items failed", "approval_id", approvalID, "err", err)
		return nil
	}
	return items
}

// ---- 派生货款行 ----

// goodsItemRows 把结算单（cube 行）派生为「货款」明细行。
//
// 派生而不是落库：货款的金额事实源是 cube 结算单，
// 再抄一份进明细表就会出现两个口径。
func goodsItemRows(settlements []SettlementRow) []ItemRow {
	out := make([]ItemRow, 0, len(settlements))
	for _, s := range settlements {
		out = append(out, ItemRow{
			Direction:    ItemDirectionPayment,
			Type:         ItemTypeGoods,
			Remark:       "",
			AmountYuan:   s.TotalGoodsYuan,
			SettlementID: s.ID,
		})
	}
	return out
}

// goodsItemRowsFromSnapshot 把结算单**快照**（approval_settlements）派生为货款行。
//
// 详情接口/重新发起预填用的是快照（审核当时记下的金额），不是 cube 当前值。
func goodsItemRowsFromSnapshot(snapshots []model.ApprovalSettlement) []ItemRow {
	out := make([]ItemRow, 0, len(snapshots))
	for _, s := range snapshots {
		out = append(out, ItemRow{
			Direction:    ItemDirectionPayment,
			Type:         ItemTypeGoods,
			Remark:       "",
			AmountYuan:   s.GoodsAmountYuan,
			SettlementID: s.SettlementID,
		})
	}
	return out
}

// manualItemRows 把落库的明细行转成 ItemRow。
func manualItemRows(items []model.ApprovalItem) []ItemRow {
	out := make([]ItemRow, 0, len(items))
	for _, it := range items {
		out = append(out, ItemRow{
			Direction:  it.Direction,
			Type:       it.ItemType,
			Remark:     it.Remark,
			AmountYuan: it.AmountYuan,
		})
	}
	return out
}

// buildItemRows 组装本次审批的**全部**明细行（货款行在前，手工行在后）。
//
// 顺序刻意固定：附件清单与 LLM 提示词都用这个顺序 ⇒ 同一笔单在各处看到的
// 顺序一致（人对照时不会"怎么两处排列不一样"）。
func buildItemRows(settlements []SettlementRow, items []model.ApprovalItem) []ItemRow {
	rows := goodsItemRows(settlements)
	rows = append(rows, manualItemRows(items)...)
	return rows
}

// sumItemsByDirection 按方向汇总落库的明细行金额。
func sumItemsByDirection(items []model.ApprovalItem, direction string) decimal.Decimal {
	sum := decimal.Zero
	for _, it := range items {
		if it.Direction != direction {
			continue
		}
		sum = sum.Add(it.AmountYuan)
	}
	return sum
}

// receiptItemsOf 取收款项（放宽额度按它算）。
func receiptItemsOf(items []model.ApprovalItem) []model.ApprovalItem {
	out := make([]model.ApprovalItem, 0, len(items))
	for _, it := range items {
		if it.Direction == ItemDirectionReceipt {
			out = append(out, it)
		}
	}
	return out
}

// totalsOf 汇总本次审批的金额（货款来自 cube 结算单，其余来自明细行）。
func totalsOf(settlements []SettlementRow, items []model.ApprovalItem) ItemTotals {
	goods := decimal.Zero
	for _, s := range settlements {
		goods = goods.Add(s.TotalGoodsYuan)
	}
	return computeTotals(goods,
		sumItemsByDirection(items, ItemDirectionPayment),
		sumItemsByDirection(items, ItemDirectionReceipt))
}

// ---- 审核放宽额度 ----

// Allowance 是收款项给机器审核的**放宽额度**。
//
// # 口径（2026-10-25 用户口径）
//
//	有堆头费、陈列费 ⇒ 进货可以多些（数量偏离不再阻断）
//	有补差价         ⇒ 可以允许毛利降低（额度 = 补差价总额）
//
// 金额单位：元。
type Allowance struct {
	// DisplayYuan 是「陈列费」收款合计。
	DisplayYuan decimal.Decimal
	// PromotionYuan 是「堆头费」收款合计。
	PromotionYuan decimal.Decimal
	// PriceDiffYuan 是「补差价」收款合计（毛利放宽额度）。
	PriceDiffYuan decimal.Decimal
}

// HasDisplayOrPromotion 是否有"允许进货多些"的额度（陈列费 + 堆头费 > 0）。
func (a Allowance) HasDisplayOrPromotion() bool {
	return a.DisplayYuan.Add(a.PromotionYuan).GreaterThan(decimal.Zero)
}

// Total 收款项合计。
func (a Allowance) Total() decimal.Decimal {
	return a.DisplayYuan.Add(a.PromotionYuan).Add(a.PriceDiffYuan)
}

// buildAllowance 按**类型文本**把收款项归类成放宽额度。
//
// # 归类口径：精确匹配类型文本
//
// 类型是用户可改的文本（「其它」能被改成"临期处理费"）⇒ 只能按文本匹配。
// 匹配不上的一律不进额度（宁可少放宽，不误放宽）。
func buildAllowance(receipts []model.ApprovalItem) Allowance {
	var a Allowance
	for _, it := range receipts {
		switch strings.TrimSpace(it.ItemType) {
		case ReceiptTypeDisplay:
			a.DisplayYuan = a.DisplayYuan.Add(it.AmountYuan)
		case ReceiptTypePromotion:
			a.PromotionYuan = a.PromotionYuan.Add(it.AmountYuan)
		case ReceiptTypePriceDiff:
			a.PriceDiffYuan = a.PriceDiffYuan.Add(it.AmountYuan)
		}
	}
	return a
}

// ---- 文本摘要（提示词 / 备注 / 清单共用） ----

// itemsPromptLines 生成明细行的文字摘要（一行一行）。
//
// 备注为空时不写括号（"陈列费 ¥200.00"而不是"陈列费 ¥200.00()"）。
func itemsPromptLines(rows []ItemRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		line := fmt.Sprintf("%s %s ¥%s", directionLabel(r.Direction), r.Type, yuanText(r.AmountYuan))
		if r.Remark != "" {
			line += "(" + r.Remark + ")"
		}
		out = append(out, line)
	}
	return out
}

// itemsSummaryText 一行摘要（企微备注控件用）。
//
// ⚠️ 收款项是**减项**：写成 "货款 ¥100 + 收款 ¥20" 会让人以为要付 120 ⇒
// 这里显式带符号（+ 付款项 / − 收款项）。
func itemsSummaryText(t ItemTotals) string {
	s := fmt.Sprintf("货款 ¥%s", yuanText(t.GoodsTotalYuan))
	if t.PaymentTotalYuan.GreaterThan(decimal.Zero) {
		s += fmt.Sprintf(" + 付款项 ¥%s", yuanText(t.PaymentTotalYuan))
	}
	if t.ReceiptTotalYuan.GreaterThan(decimal.Zero) {
		s += fmt.Sprintf(" - 收款项 ¥%s", yuanText(t.ReceiptTotalYuan))
	}
	return s + fmt.Sprintf(" = 应付 ¥%s", yuanText(t.PayableYuan))
}
