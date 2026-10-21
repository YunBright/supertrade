// Package model —— inventory 域的本地表(库存快照 + 库存流水)。
//
// # 域边界
//
// inventory 拥有两张表:
//
//	stock_levels  当前库存快照(每个 (branch_id, product_id) 一行)
//	stock_moves   库存流水(append-only,一次库存变动一行)
//
// # 为什么快照与流水都要
//
//	快照回答"现在还有多少货" —— POS 收银时只看这个;
//	流水回答"为什么变成这样" —— 盘点差异、损耗追溯、成本核算。
//
// 只留流水不落快照 ⇒ 每次查库存都要 SUM 全部流水,收银台扛不住;
// 只留快照不留流水 ⇒ 库存对不上时无从追溯,只能靠人工回忆。
//
// # 幂等(全仓首例的 op_id 幂等落地处)
//
// stock_moves 上有**唯一索引 (branch_id, product_id, ref_type, ref_id)**:
//
//	同一笔业务单据的同一个商品,只可能落一行流水。
//
// ⇒ 重复投递 sale.paid 时,INSERT 撞唯一索引被 `clause.OnConflict{DoNothing}` 跳过,
//
//	业务层据此判定"这不是新事件",**不再改快照** ⇒ 库存只扣一次。
//
// ⚠️ 索引里必须带 product_id:一笔单里同一个商品可能出现多次(拆行),
//
//	而**同一笔单同一个商品只应有一条流水** —— 数量由 service 层先按商品聚合。
//
// # 金额 / 数量
//
// ⚠️ **一律 decimal(Shopspring),DB 列 numeric(18,4),绝不用 float**。
//
// decimal.Decimal 的 MarshalJSON 默认输出**带引号的字符串**(`"22.2"`)⇒
// JSON 里金额与数量天然是字符串,Dart 侧不会变成 double。
//
// # 软删
//
// 用 `gorm.DeletedAt`(不是 *time.Time)⇒ 删掉的行查不到。
package model

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// StockLevel 是**当前库存快照**。
//
// PK = (branch_id, product_id):同一商品在不同门店各有一份库存。
//
// ⚠️ **只放必要的列**:商品名 / 条码 / 单位归 catalog(商品档案),
//
//	这里存一份就是第二个真相源。要带名字展示,由调用方按 product_id join catalog。
type StockLevel struct {
	// BranchID 是门店。
	BranchID string `gorm:"primaryKey;column:branch_id;type:varchar(64)" json:"branch_id"`
	// ProductID 是商品 id(= catalog.products.id)。
	ProductID string `gorm:"primaryKey;column:product_id;type:varchar(64)" json:"product_id"`
	// Qty 是当前库存数量。
	//
	// ⚠️ 可为负(卖超了 / 盘点亏损)—— **不做夹逼**:
	//
	//	夹到 0 会把"超卖"这件事从数据里抹掉,现场再也查不出"为什么少了 3 件"。
	//	负库存是一个**要被看见的信号**,不是一个要被藏起来的错误。
	Qty decimal.Decimal `gorm:"column:qty;type:numeric(18,4);not null;default:0" json:"qty"`
	// AvgCostYuan 是移动加权平均成本(元/件)。
	//
	// ⚠️ **当前恒为 0**:成本档案(采购入库价)尚未接入
	// (purchase.completed → inventory 的链路还没实装)。
	//	⇒ 本字段是**预留位**,不是"成本已知"。
	AvgCostYuan decimal.Decimal `gorm:"column:avg_cost_yuan;type:numeric(18,4);not null;default:0" json:"avg_cost_yuan"`
	// ServerVersion 是**服务端版本号**(单调递增)。
	//
	// ⚠️ 拉取水位必须用它,**不能用客户端时间戳** ——
	//
	//	门店机器的时钟会漂,时间戳水位会漏数据 / 重数据。
	//	每次快照变更 +1,客户端记住上次拿到的版本号即可增量拉。
	ServerVersion int64 `gorm:"column:server_version;type:bigint;not null;default:0" json:"server_version"`
	// CreatedBy / UpdatedBy 是审计列(写入者)。
	CreatedBy string `gorm:"column:created_by;type:varchar(64);not null" json:"created_by"`
	UpdatedBy string `gorm:"column:updated_by;type:varchar(64)" json:"updated_by"`
	// CreatedAt / UpdatedAt 是审计时间戳。
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
	// DeletedAt 是软删标记。
	//
	// ⚠️ `json:"-"`:软删时间不该出现在对外 JSON 里(它是内部实现细节)。
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`
}

// TableName 显式指定表名。
func (StockLevel) TableName() string { return "stock_levels" }

// StockMove 是**库存流水**(append-only,一次库存变动一行)。
//
// ⚠️ **append-only**:流水行只增不改(软删除外)。
//
//	库存快照是可变的当前态,流水是不可变的历史 —— 两者的写入纪律不同。
type StockMove struct {
	// ID 是流水 id(uuid / op_id 派生)。
	ID string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	// BranchID 是门店。
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index:idx_stock_move_branch_occurred,priority:1;uniqueIndex:uq_stock_move_ref,priority:1" json:"branch_id"`
	// ProductID 是商品 id。
	ProductID string `gorm:"column:product_id;type:varchar(64);not null;index:idx_stock_move_product;uniqueIndex:uq_stock_move_ref,priority:2" json:"product_id"`
	// MoveType 是变动类型:sale / refund / stocktake / adjust / in / out。
	MoveType string `gorm:"column:move_type;type:varchar(32);not null" json:"move_type"`
	// Qty 是本次库存变动量(**库存方向**)。
	//
	// ⚠️ **负 = 出库(库存减少),正 = 入库(库存增加)**。
	//
	//	对 sale.paid 而言:销售行 qty=2 ⇒ 本列 = -2;
	//	退货行(上游已把 qty 取负)qty=-2 ⇒ 本列 = +2。
	//	⇒ **上游给的是"卖了多少",本列是"库存怎么动"**,两者差一个负号。
	Qty decimal.Decimal `gorm:"column:qty;type:numeric(18,4);not null" json:"qty"`
	// QtyAfter 是本次变动**之后**的库存数量(快照值)。
	//
	// ⚠️ 它让流水**自带上下文**:只看流水也能还原出每一步的库存,
	//	不必回放整条链。
	QtyAfter decimal.Decimal `gorm:"column:qty_after;type:numeric(18,4);not null" json:"qty_after"`
	// UnitCostYuan 是本次变动的**单位价值**(元/件,非负)。
	//
	// ⚠️ 当前取事件里的**售价**(unit_price_yuan)作为单位价值 ——
	//
	//	尚无成本档案来源(采购入库价未接入),而留 0 会让这条流水在
	//	成本核算里彻底不可用。接入 purchase.completed 后应改用
	//	stock_levels.avg_cost_yuan(移动加权成本)。
	UnitCostYuan decimal.Decimal `gorm:"column:unit_cost_yuan;type:numeric(18,4);not null;default:0" json:"unit_cost_yuan"`
	// AmountYuan 是本次变动的**金额**(元,库存方向)。
	//
	// ⚠️ 负 = 出库(价值流出),正 = 入库(价值流回)—— 与 Qty 同号约定,
	//
	//	免得"数量是负的、金额是正的"这种自相矛盾的行出现在账上。
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:numeric(18,4);not null;default:0" json:"amount_yuan"`
	// RefType 是来源单据类型:sale / refund / stocktake / adjust / purchase。
	RefType string `gorm:"column:ref_type;type:varchar(32);not null;uniqueIndex:uq_stock_move_ref,priority:3" json:"ref_type"`
	// RefID 是来源单据 id(**幂等键**)。
	//
	// ⚠️ sale.paid 事件里 = **op_id**(pos 的业务幂等键)。
	//
	//	重发时订阅方必须看到**同一个**去重键 ⇒ 这就是 outbox.id = op_id 的原因。
	RefID string `gorm:"column:ref_id;type:varchar(64);not null;uniqueIndex:uq_stock_move_ref,priority:4" json:"ref_id"`
	// OccurredAt 是业务发生时刻(不是入库时刻)。
	//
	// ⚠️ 用**业务时刻**而不是"收到事件的时刻":
	//
	//	补传的历史单据按收到时间排序会把历史流水挤到最后。
	OccurredAt time.Time `gorm:"column:occurred_at;not null;index:idx_stock_move_branch_occurred,priority:2" json:"occurred_at"`
	// OperatorID 是操作人(收银员 / 仓管)。
	OperatorID string `gorm:"column:operator_id;type:varchar(64)" json:"operator_id"`
	// AuthorizedBy 是越权授权人(改价 / 退货需上级授权时填)。
	AuthorizedBy string `gorm:"column:authorized_by;type:varchar(64)" json:"authorized_by"`
	// CreatedAt / UpdatedAt 是审计时间戳(流水实际只写一次)。
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
	// DeletedAt 是软删标记(`json:"-"` 同上)。
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"-"`
}

// TableName 显式指定表名。
func (StockMove) TableName() string { return "stock_moves" }

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrStockLevelNotFound:按 (branch_id, product_id) 未命中。
	ErrStockLevelNotFound = errors.New("inventory: 库存记录不存在")
	// ErrInvalidInput:入参非法(缺 branch_id / op_id / lines 为空 / 商品 id 为空)。
	ErrInvalidInput = errors.New("inventory: 入参非法")
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(503)。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店 10),
	// 问不到是**链路故障** ⇒ 后者必须让用户看到"暂不可用",
	// 而不是让它悄悄回落成本地(那会让用户看到一份空列表,
	// 而真相是"这个门店的库存现在查不了")。
	ErrCubeSourceUnavailable = errors.New("inventory: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用(503)。
	//
	// ⚠️ **不是"没配"**:配了但被关停 ⇒ 这个门店现在查不了,
	// 回落成本地 = 给用户一份"看起来正常"的空列表。
	ErrCubeSourceDisabled = errors.New("inventory: 该门店的数据源已停用")
	// ErrExternalSourceReadOnly:该门店的库存来自外部系统,本域只读(409)。
	//
	// ⚠️ 写端点(导入)专用:往本域 PG 写一份"没人看的库存",
	// 而前台还显示"写成功了" ⇒ 比报错更糟。
	ErrExternalSourceReadOnly = errors.New("inventory: 该门店的库存来自外部系统,本域只读")
	// ErrUnsupportedExportScope:导出的 scope 不归本服务(400)。
	//
	// ⚠️ 与 cube-router 的同名错误码一致(unsupported_export_scope):
	// 编排方(cubesync)按 code 分支,两边不一样会让它"看不出是 scope 写错了"。
	ErrUnsupportedExportScope = errors.New("inventory: 不支持的导出 scope")
)
