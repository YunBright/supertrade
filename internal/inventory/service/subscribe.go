// Package service / subscribe.go —— Dapr pub/sub 订阅回调实现。
//
// 监听领域:
//   - sale.paid → ApplySalePaid:写 stock_moves + 更新 stock_levels(幂等扣减)
//
// # 幂等保证(本文件是全仓"op_id 幂等"的第一处落地)
//
// EVENT-CATALOG §0.3:订阅方必须按 id 去重,重复事件不重复处理。
// 方案 v2 §6.6:sale.paid 的 payload 带 op_id,订阅方用
// `clause.OnConflict{DoNothing}` ⇒ 同一笔销售重复投递,库存只扣一次。
//
// # ⚠️ 顺序不能反:先插流水,再改快照
//
//	一笔事件 = 一个事务里:
//	  ① INSERT stock_moves(唯一索引 (branch_id, product_id, ref_type, ref_id))
//	     —— RowsAffected == 1 ⇒ 这是新事件;RowsAffected == 0 ⇒ 重复投递,跳过。
//	  ② 只有 ① 真的插进去了,才 UPDATE stock_levels。
//
// 先改快照再插流水 ⇒ 重复投递会把库存扣两次(流水只多一行,库存却少了两份)。
//
// # 数量方向
//
// ⚠️ **上游 pos 在退款事件里已经把 qty 与金额取负**
// (销售行 qty=2、退货行 qty=-2),所以本服务**统一处理**:
//
//	流水 qty      = -行 qty      (库存方向:负=出库,正=入库)
//	快照 qty      = 快照 qty + 流水 qty
//
// ⇒ 销售行 qty=2 ⇒ 库存 -2;退货行 qty=-2 ⇒ 库存 +2(加回)。
package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/inventory/model"
)

// 订单状态(pos 的 sale.paid 契约)。
const (
	// OrderStatusSale 是销售(扣库存)。
	OrderStatusSale = "S"
	// OrderStatusRefund 是退货(加回库存)。
	OrderStatusRefund = "R"
)

// 库存变动类型(与 model.StockMove.MoveType 同名空间)。
const (
	// MoveTypeSale 是销售出库。
	MoveTypeSale = "sale"
	// MoveTypeRefund 是退货入库。
	MoveTypeRefund = "refund"
)

// SalePaidLine 是 sale.paid 里 lines[] 的一个元素。
//
// ⚠️ 数量与金额在 wire 上是**字符串**(Dart 侧 jsonDecode 不会变成 double)。
//
//	decimal.Decimal 的 UnmarshalJSON 同时接受字符串与 JSON number,
//	⇒ 上游万一发了数字也不会把整笔单丢掉。
type SalePaidLine struct {
	// LineID 是 pos 的行 id(可空,只作可观测用)。
	LineID string `json:"line_id"`
	// ProductID 是商品 id(= catalog.products.id)。必填。
	ProductID string `json:"product_id"`
	// Qty 是本行数量(销售为正,退货为负)。
	Qty decimal.Decimal `json:"qty"`
	// UnitPriceYuan 是本行单价。
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	// AmountYuan 是本行金额。
	AmountYuan decimal.Decimal `json:"amount_yuan"`
}

// SalePaidPayload 是 sale.paid 事件的 data(EVENT-CATALOG §0.1)。
type SalePaidPayload struct {
	// OpID 是**业务幂等键**(pos 的 outbox.id)。
	//
	// ⚠️ 必填:它是 stock_moves.ref_id,也是"这笔单是不是已经处理过"的唯一判据。
	OpID string `json:"op_id"`
	// SaleID 是销售单号(SO20261021000001)。
	SaleID string `json:"sale_id"`
	// BranchID 是门店。必填。
	BranchID string `json:"branch_id"`
	// MemberID 是会员号(可空;库存不关心)。
	MemberID string `json:"member_id"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
	// AuthorizedBy 是越权授权人。
	//
	// ⚠️ 当前 pos 的 sale.paid 契约里**没有**这个字段(恒为空);
	//    预留给"越权改价 / 越权退货"的审计场景。
	AuthorizedBy string `json:"authorized_by"`
	// TotalAmountYuan 是本单合计(可空,库存不依赖它)。
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	// PaidAt 是收款时刻(RFC3339)。空 = 用收到事件的时刻。
	PaidAt string `json:"paid_at"`
	// OrderStatus 是 "S"(销售) / "R"(退货)。
	OrderStatus string `json:"order_status"`
	// Lines 是本单的商品行。
	Lines []SalePaidLine `json:"lines"`
}

// SalePaidResult 是一次 sale.paid 处理的结果(可观测用)。
type SalePaidResult struct {
	// Groups 是本单聚合后的 (商品) 组数。
	Groups int `json:"groups"`
	// Applied 是**真正写入**的流水行数(= 真正扣/加库存的商品数)。
	Applied int `json:"applied"`
	// Skipped 是被唯一索引挡下的行数(= 重复投递的部分)。
	Skipped int `json:"skipped"`
}

// ApplySalePaid 处理一笔 sale.paid 事件:写 stock_moves + 更新 stock_levels。
//
// ⚠️ **整个事件在一个事务里**:要么流水与快照都写成功,要么都不写。
//
//	中途失败(比如第 3 个商品写失败)⇒ 整单回滚 ⇒ 库存不会只扣一半。
//
// 返回 (result, nil) 表示已 ack;返回 error 表示调用方应当重试。
func (s *StockService) ApplySalePaid(ctx context.Context, p SalePaidPayload) (SalePaidResult, error) {
	branchID := strings.TrimSpace(p.BranchID)
	opID := strings.TrimSpace(p.OpID)

	// ---- 入参校验(失败时**不落任何行**)----
	if branchID == "" {
		return SalePaidResult{}, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	if opID == "" {
		return SalePaidResult{}, fmt.Errorf("%w: op_id 必填(幂等键)", model.ErrInvalidInput)
	}
	if len(p.Lines) == 0 {
		return SalePaidResult{}, fmt.Errorf("%w: lines 为空", model.ErrInvalidInput)
	}

	status := strings.ToUpper(strings.TrimSpace(p.OrderStatus))
	switch status {
	case "":
		// 缺省按销售处理(与 fresh-meat 的 sale.completed 同款兜底)。
		status = OrderStatusSale
	case OrderStatusSale, OrderStatusRefund:
	default:
		// 未知状态**不猜**:猜错方向 = 库存凭空多/少。
		return SalePaidResult{}, fmt.Errorf("%w: order_status=%q 非法(只能是 S / R)",
			model.ErrInvalidInput, p.OrderStatus)
	}

	occurredAt, err := parseOccurredAt(p.PaidAt)
	if err != nil {
		return SalePaidResult{}, fmt.Errorf("%w: paid_at 非法: %v", model.ErrInvalidInput, err)
	}

	groups, err := aggregateSaleLines(p.Lines)
	if err != nil {
		return SalePaidResult{}, err
	}
	if len(groups) == 0 {
		return SalePaidResult{}, fmt.Errorf("%w: lines 里没有有效商品行", model.ErrInvalidInput)
	}

	moveType := MoveTypeSale
	if status == OrderStatusRefund {
		moveType = MoveTypeRefund
	}

	now := s.now()
	if occurredAt != nil {
		now = *occurredAt
	}

	result := SalePaidResult{Groups: len(groups)}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, g := range groups {
			// 库存方向:负 = 出库,正 = 入库。
			moveQty := g.qty.Neg()
			moveAmount := g.amount.Neg()
			unitCost := decimal.Zero
			if !g.absQty.IsZero() {
				unitCost = g.absAmount.Div(g.absQty)
			}

			// ① 快照行不存在 ⇒ 先建一行 0 库存。
			//
			// DoNothing:已存在就保留原值(不覆盖 qty)。
			seed := model.StockLevel{
				BranchID:  branchID,
				ProductID: g.productID,
				Qty:       decimal.Zero,
				CreatedBy: actor,
				UpdatedBy: actor,
				CreatedAt: s.now(),
				UpdatedAt: s.now(),
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
				return fmt.Errorf("seed stock_levels(%s/%s): %w", branchID, g.productID, err)
			}

			// ② 读当前库存。
			//
			// ⚠️ 生产(Postgres)上用 FOR UPDATE 锁行:两个收银台同时卖同一商品时,
			//
			//	没有行锁会 read-modify-write 丢更新(qty_after 与实际 qty 对不上)。
			//	SQLite 不支持 FOR UPDATE ⇒ 按方言跳过(单测库,单写者)。
			//
			// Unscoped:软删的行也要能读到并被改 ——
			//	否则"流水写了、快照没改"= 账实不符,而且**没有任何报错**。
			var level model.StockLevel
			q := tx.Unscoped().Where("branch_id = ? AND product_id = ?", branchID, g.productID)
			if tx.Dialector.Name() == "postgres" {
				q = q.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			if err := q.First(&level).Error; err != nil {
				return fmt.Errorf("read stock_levels(%s/%s): %w", branchID, g.productID, err)
			}
			qtyAfter := level.Qty.Add(moveQty)

			// ③ **先插流水** —— 拿到"是不是新事件"的判定。
			move := model.StockMove{
				ID:           uuid.NewString(),
				BranchID:     branchID,
				ProductID:    g.productID,
				MoveType:     moveType,
				Qty:          moveQty,
				QtyAfter:     qtyAfter,
				UnitCostYuan: unitCost,
				AmountYuan:   moveAmount,
				RefType:      moveType,
				RefID:        opID,
				OccurredAt:   now,
				OperatorID:   strings.TrimSpace(p.OperatorID),
				AuthorizedBy: strings.TrimSpace(p.AuthorizedBy),
				CreatedAt:    s.now(),
				UpdatedAt:    s.now(),
			}
			inserted := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&move)
			if inserted.Error != nil {
				return fmt.Errorf("insert stock_moves(%s/%s): %w", branchID, g.productID, inserted.Error)
			}
			if inserted.RowsAffected == 0 {
				// 唯一索引 (branch_id, product_id, ref_type, ref_id) 撞了
				// ⇒ 这笔单的这个商品**已经处理过** ⇒ 绝不能再动快照。
				result.Skipped++
				continue
			}

			// ④ 只有 ③ 真的插进去了,才改快照。
			//
			// qty 用 `qty + ?` 原子表达式而不是写绝对值:
			// 万一有并发,写绝对值会把别人的变动覆盖掉。
			if err := tx.Unscoped().Model(&model.StockLevel{}).
				Where("branch_id = ? AND product_id = ?", branchID, g.productID).
				Updates(map[string]any{
					"qty":            gorm.Expr("qty + ?", moveQty),
					"server_version": gorm.Expr("server_version + 1"),
					"updated_by":     actor,
					"updated_at":     s.now(),
				}).Error; err != nil {
				return fmt.Errorf("update stock_levels(%s/%s): %w", branchID, g.productID, err)
			}
			result.Applied++
		}
		return nil
	})
	if err != nil {
		return SalePaidResult{}, err
	}
	return result, nil
}

// saleLineGroup 是"同一笔单里同一个商品"的聚合结果。
//
// ⚠️ **必须按商品聚合**:一笔单里同一个商品可能出现多次(POS 拆行),
//
//	而唯一索引是 (branch_id, product_id, ref_type, ref_id)
//	⇒ 不聚合的话第二行会被 DoNothing 静默丢掉,**数量就少了**。
type saleLineGroup struct {
	productID string
	// qty 是原始行 qty 之和(销售为正,退货为负)。
	qty decimal.Decimal
	// amount 是原始行 amount 之和。
	amount decimal.Decimal
	// absQty / absAmount 是绝对值之和,用来算加权单价。
	absQty    decimal.Decimal
	absAmount decimal.Decimal
}

// aggregateSaleLines 把 lines 按 product_id 聚合成组。
//
// ⚠️ 数量为 0 的行直接丢掉(不产生库存变动);缺 product_id 的行**报错**
// (不静默丢 —— 静默丢 = 库存少扣,而且没人知道)。
func aggregateSaleLines(lines []SalePaidLine) ([]saleLineGroup, error) {
	order := make([]string, 0, len(lines))
	byProduct := make(map[string]*saleLineGroup, len(lines))

	for _, line := range lines {
		productID := strings.TrimSpace(line.ProductID)
		if productID == "" {
			return nil, fmt.Errorf("%w: lines[].product_id 不能为空", model.ErrInvalidInput)
		}
		qty := line.Qty
		if qty.IsZero() {
			continue
		}
		amount := line.AmountYuan
		if amount.IsZero() && !line.UnitPriceYuan.IsZero() {
			// 事件没给金额 ⇒ 用 数量 × 单价 兜底,免得流水金额全丢。
			amount = qty.Mul(line.UnitPriceYuan)
		}
		g, ok := byProduct[productID]
		if !ok {
			g = &saleLineGroup{productID: productID}
			byProduct[productID] = g
			order = append(order, productID)
		}
		g.qty = g.qty.Add(qty)
		g.amount = g.amount.Add(amount)
		g.absQty = g.absQty.Add(qty.Abs())
		g.absAmount = g.absAmount.Add(amount.Abs())
	}

	out := make([]saleLineGroup, 0, len(order))
	for _, productID := range order {
		out = append(out, *byProduct[productID])
	}
	return out, nil
}

// parseOccurredAt 解析业务发生时刻;空串返 (nil, nil) —— 调用方用当前时刻兜底。
func parseOccurredAt(s string) (*time.Time, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
