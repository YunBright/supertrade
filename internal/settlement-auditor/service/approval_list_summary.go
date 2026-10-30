// Package service / approval_list_summary.go —— 付款审批**列表卡片**的摘要信息（2026-10-29 用户口径）。
//
// # 用户口径原文
//
//	「结算-付款审批，列表卡片…如有货款支出请追加显示 1~2 个供应商名称，
//	  如有付款项（非货款）请追加显示 1~2 个付款项目名称」
//
// # 为什么放在后端算
//
// 供应商名要 JOIN cube 的 `supplier` 字典（duck model），付款项要读 `approval_items`
// —— 两件事都在服务端。前端要拿到它们只有两条路：
//
//	a) N+1：列表 50 行 ⇒ 50 次详情请求（不可接受）；
//	b) 自己拼：前端没有字典、也没有明细表。
//
// ⇒ 后端一次算好随列表返回。
//
// # 截断在前端做
//
// 后端给**完整**（去重后）的名称列表（上限 maxListSummaryNames 防 payload 膨胀），
// 「显示 1~2 个、其余用 ...」是**显示层**决策 ⇒ 由前端截断。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// maxListSummaryNames 单个列表行最多带多少个名称（防 payload 膨胀）。
//
// 单次审批最多 50 张结算单（maxSettlementsPerApproval）⇒ 理论上限 50 个供应商名。
// 列表一页 50 行 ⇒ 2500 个名字，payload 过大 ⇒ 这里截到 20 个（够显示用）。
const maxListSummaryNames = 20

// ApprovalListItem 是列表行：审批实例 + 列表卡片要显示的摘要。
//
// ⚠️ 用**匿名嵌入**（不是具名字段）⇒ JSON 里审批实例的字段仍在顶层，
// 只是多了两个新键 ⇒ **老客户端不受影响**（wx-h5 / 老版本 Flutter 照旧解析）。
type ApprovalListItem struct {
	model.ApprovalInstance
	// SupplierNames 是本单涉及的**供应商名称**（去重、按首次出现排序）。
	//
	// ⚠️ 只给**名称**，不给编号：前端卡片上要显示的是"是哪几家"，
	// 编号在详情页的结算单行里已经有了。
	SupplierNames []string `json:"supplier_names"`
	// PaymentItemNames 是本单的**付款项**（非货款）类型名（去重、排序）。
	//
	// ⚠️ 只取 direction=payment 的**手工**明细行 —— 货款不是明细行
	// （货款由结算单派生，见 service/items.go 的 goodsItemRows）。
	PaymentItemNames []string `json:"payment_item_names"`
}

// fillListSummaries 给列表行补摘要（供应商名 + 付款项名）。
//
// ⚠️ **降级不报错**：字典挂了 / 明细读不到 ⇒ 只少显示名字，列表照常出。
// 理由同 fillSupplierNames：摘要��显示层，让它把整个列表打成 500 是本末倒置。
func (s *Service) fillListSummaries(ctx context.Context, branchID string, items []ApprovalListItem) {
	if len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}

	// 供应商：approval_settlements 里的 supplier_id → cube 字典 → 名称。
	supplierIDs := map[string][]string{}
	var snaps []model.ApprovalSettlement
	if err := s.db.WithContext(ctx).Where("approval_id IN ?", ids).Find(&snaps).Error; err != nil {
		s.logger.Warn("load approval settlements for list summary failed", "err", err)
	} else {
		for _, sn := range snaps {
			sid := strings.TrimSpace(sn.SupplierID)
			if sid == "" {
				continue
			}
			supplierIDs[sn.ApprovalID] = append(supplierIDs[sn.ApprovalID], sid)
		}
	}
	names := map[string]string{}
	if s.cube != nil {
		dict, err := s.cube.ListSupplierNames(ctx, branchID)
		if err != nil {
			s.logger.Warn("list summary: 取供应商字典失败,名称留空", "err", err)
		} else {
			names = dict
		}
	}

	// 付款项：手工明细里 direction=payment 的类型文本。
	itemTypes := map[string][]string{}
	var rows []model.ApprovalItem
	if err := s.db.WithContext(ctx).
		Where("approval_id IN ? AND direction = ?", ids, ItemDirectionPayment).
		Find(&rows).Error; err != nil {
		s.logger.Warn("load approval items for list summary failed", "err", err)
	} else {
		for _, r := range rows {
			t := strings.TrimSpace(r.ItemType)
			if t == "" {
				continue
			}
			itemTypes[r.ApprovalID] = append(itemTypes[r.ApprovalID], t)
		}
	}

	for i := range items {
		sup := make([]string, 0, len(supplierIDs[items[i].ID]))
		for _, sid := range supplierIDs[items[i].ID] {
			name := strings.TrimSpace(names[sid])
			if name == "" {
				// 字典查不到时**退回编号** —— 空着会让卡片上少一家供应商，
				// 而编号至少还能让人去 sixun 对一下。
				name = sid
			}
			sup = append(sup, name)
		}
		items[i].SupplierNames = capStrings(dedupeStrings(sup), maxListSummaryNames)
		items[i].PaymentItemNames = capStrings(dedupeStrings(itemTypes[items[i].ID]), maxListSummaryNames)
	}
}

// capStrings 截断到最多 n 个（不足 n 个原样返回）。
func capStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// supplierLabelForPrompt 供应商展示串：「<名称>-<编号>」（2026-10-29 用户口径）。
//
// ⚠️ 名称在前、编号在后：店长认的是**名称**，编号用来核对到具体那家
// （同名供应商在门店很常见）。与 audit_flow 模块的 supplierDisplay 同口径。
//
// 名称查不到 ⇒ 只给编号，**不留悬空的「-」**。
func supplierLabelForPrompt(name, id string) string {
	name = strings.TrimSpace(name)
	id = strings.TrimSpace(id)
	switch {
	case name == "" && id == "":
		return "-"
	case name == "":
		return id
	case id == "":
		return name
	}
	return fmt.Sprintf("%s-%s", name, id)
}
