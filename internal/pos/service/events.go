// Package service —— pos 域的事件契约。
//
// # sale.paid
//
// `sale.paid` 是**库存扣减**与**积分累计**的触发点(见后端方案 §6.5 / §6.6):
//
//	pos --(outbox)--> sale.paid --> inventory(扣库存) / member(加积分)
//
// 下游(member / inventory)按本文件的 payload 字段名与类型消费,
// **字段名与类型逐字约定,不要改**:
//
//	op_id            string  幂等键(uuid)——订阅方按它去重
//	sale_id          string  销售单号
//	branch_id        string  门店
//	member_id        string  会员号;**空 ⇒ 不计积分**
//	operator_id      string  收银员
//	authorized_by    string  越权授权人(可空)
//	total_amount_yuan string 应收合计(元,2 位小数)
//	paid_at          string  成交时刻(RFC3339,固定 +08:00)
//	order_status     string  "S"=销售 / "R"=退货
//	lines[]          []struct {
//	    line_id         string
//	    product_id      string
//	    qty             string  数量
//	    unit_price_yuan string  成交单价(元,2 位小数)
//	    amount_yuan     string  行金额(元,2 位小数)
//	}
//
// # 退货也走 sale.paid
//
// ⚠️ **不用独立 topic** —— 下游只订阅一条 topic,少一条就少一处"订阅漏了导致
// 库存不扣"的坑。退货用 **order_status="R"** 区分,且:
//
//	qty 与金额**取负值**(退 2 件 ⇒ qty="-2",金额为负)
//
// ⇒ 下游实现:`order_status=="R"` 时按**反向**处理(库存 quantity -= qty ⇒ 负数即入库)。
//
// # 金额是字符串
//
// ⚠️ 金额/qty 一律**字符串**,不用 JSON number ——
//
//	JSON number 到 Dart 侧会变 double,0.1 + 0.2 = 0.30000000000000004。
//	decimal 的 String() 在 Go 侧也是定点字符串,不需要经过 float。
package service

// TopicSalePaid 是销售事件 topic(EVENT-CATALOG 的 topic 名)。
const TopicSalePaid = "sale.paid"

// 事件里的单据状态。
const (
	// OrderStatusSale = 销售。
	OrderStatusSale = "S"
	// OrderStatusRefund = 退货。
	OrderStatusRefund = "R"
)

// SalePaidEvent 是 sale.paid 的事件体。
//
// ⚠️ 字段名与 JSON tag 是**下游契约**,不可改。
type SalePaidEvent struct {
	// OpID 是幂等键(= outbox.id = 业务 op_id)。
	OpID string `json:"op_id"`
	// SaleID 是销售单号。
	SaleID string `json:"sale_id"`
	// BranchID 是门店。
	BranchID string `json:"branch_id"`
	// MemberID 是会员号;**空 ⇒ 不计积分**。
	MemberID string `json:"member_id"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
	// AuthorizedBy 是越权授权人(可空)。
	AuthorizedBy string `json:"authorized_by"`
	// TotalAmountYuan 是应收合计(字符串,2 位小数)。
	TotalAmountYuan string `json:"total_amount_yuan"`
	// PaidAt 是成交时刻(RFC3339,+08:00)。
	PaidAt string `json:"paid_at"`
	// OrderStatus 是 "S"(销售)或 "R"(退货)。
	OrderStatus string `json:"order_status"`
	// Lines 是明细。
	Lines []SalePaidLine `json:"lines"`
}

// SalePaidLine 是 sale.paid 里的明细行。
type SalePaidLine struct {
	// LineID 是明细 id(= <sale_id>-L<n>,全局唯一)。
	LineID string `json:"line_id"`
	// ProductID 是商品 id。
	ProductID string `json:"product_id"`
	// Qty 是数量(字符串;退货为负)。
	Qty string `json:"qty"`
	// UnitPriceYuan 是成交单价(字符串,2 位小数)。
	UnitPriceYuan string `json:"unit_price_yuan"`
	// AmountYuan 是行金额(字符串,2 位小数;退货为负)。
	AmountYuan string `json:"amount_yuan"`
}
