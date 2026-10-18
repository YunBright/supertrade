// Package salesdaily 持久化「门店 × 营业日」的完整销售事实。
//
// # 谁写、谁读（2026-10-17 定稿）
//
//	**sales-agg 写，sales-insight 读。** 本包是两边**唯一**共用的地方。
//
// 这么分是因为"谁拥有销售数字"必须唯一：
//
//   - 每日销售统计卡片（走 sales-agg）与 AI 日报（走 sales-insight）
//     是**同一份销售事实的两种呈现**。改造前两边各自现查 cube，
//     于是 2026-10-16 出现了「品类口径 2024.6 元 vs 供应商口径
//     13003.8 元」—— 同一个营业日、同一套源库，两个数字差 6 倍，
//     **零报错**。
//   - 两边各写各的 PG 行、或各查各的 cube，都只是把同一个坑换个地方重挖。
//
// → 采集只做一次、只由 sales-agg 做，两个消费方读**同一行**。
//
// # 为什么要有这一层
//
// 2026-10-17 之前，日报/周报/月报与每日统计卡片都是**推送那一刻现查 cube**。
// 三个后果：
//
//  1. 同一个营业日的数字在不同时间点会变（POS 还在补写、思迅日结未完成），
//     而用户会把两次看到的数当成"系统算错了"；
//  2. 月报只能整月重查 cube，无法"聚合每天已定稿的日数据"；
//  3. 推送与取数耦死 —— 推送延迟、取数失败都会变成"群里没消息"。
//
// → 改成：22:45 采集一次落 PG（次日 01:00 再校正一次），
//
//	之后所有消费方一律**读 PG**，不再打 cube。
//
// # 四张表而不是一张 jsonb
//
// 用户要求"存完整事实包（总额 + 品类 + 供应商 + 商品）"，目的是月报能聚合。
// jsonb 单表也能存全，但月报的 `GROUP BY category` 只能在 Go 里归并，
// 且"重跑一次采集"要整行覆盖、无法只更新某个维度。
// 规范化之后：
//
//   - 月报聚合是一条 SQL（`GROUP BY name WHERE oper_date BETWEEN ...`）；
//   - 唯一键天然去重，重复采集走 upsert 而不是叠加；
//   - 某维度当天为空（还没有商品维度）不影响其它维度。
//
// # oper_date 是 char(10) 字符串，不是 date
//
// 思迅源库的 `oper_date` 本身就是 char(10)（2026-10-17 实测），
// 而事实包里的 `From`/`To` 也一直是 `YYYY-MM-DD` 字符串。
// 全程用字符串意味着**没有时区换算、没有 date↔time 的类型往返** ——
// 这类转换正是"日报少了一天/多了一天"最常见的来源。
//
// # 独立于 internal/salesinsight 而不是放在里面
//
// 早先这层放在 sales-insight 内部，于是"谁采集"跟着推理服务走了 ——
// 而 sales-insight 的职责是"组装事实包 → 调 LLM 出结论"，写入不属于它。
// 现在提到顶层共享包：两个服务 import 的是**同一份模型定义**，
// 写侧改字段而读侧没跟上会在编译期就炸，而不是上线后读出零值。
package salesdaily

// DateLayout 是营业日的字符串格式。
//
// ⚠️ 与思迅源库 `oper_date` 的 char(10)、以及事实包的 From/To 完全一致。
//
//	全程用字符串意味着没有时区换算、没有 date↔time 往返 ——
//	那类转换正是"日报少一天/多一天"最常见的来源。
const DateLayout = "2006-01-02"

// Snapshot 是某门店某个营业日的完整销售事实（写入与读出的单位）。
type Snapshot struct {
	BranchID string
	// OperDate 形如 "2026-10-16"。
	OperDate string
	// Totals 是当天的大盘汇总（**无分组、无限量**）。
	Totals TotalsRow
	// Categories 是**全量**品类，不做 topN 截断 —— 月报要靠它聚合。
	Categories []CategoryRow
	// Suppliers 是**全量**供应商。
	Suppliers []SupplierRow
	// Products 是**全量**商品。
	Products []ProductRow

	// ---- 以下三个是**只读字段**：Save 会忽略它们，Get 会填上 ----
	//
	// ⚠️ 它们放在 Snapshot 里而不是让调用方再查一次 Meta：
	//
	//	消费方（推送卡片、AI 报告）要展示"这个数是什么时候采的、
	//	有没有被次日校正过"，为此多一次往返不值得；
	//	而这两个字段**正是**这条数据链上最容易被误读的 ——
	//	"被校正过的数字"与"从未被校正的数字"在数值上完全一样。
	Source      CollectSource
	CollectedAt string
}

// TotalsRow 是当天的大盘汇总。
//
// ⚠️ 这四项（CostedRow/TotalRow）必须一起存：没有它们就无法判断
// "毛利率 100%"是真实的还是"成本全为 0"的假象
// （hbposv10 实测当日多组成本覆盖率为 0）。
type TotalsRow struct {
	NetAmount       float64
	NetQty          float64
	RefCost         float64
	RefMargin       float64
	RefMarginPct    float64
	CostCoverage    float64
	MarginAvailable bool
	TotalRows       int64
	CostedRows      int64
}

// CategoryRow 是一个品类的当日表现（Name 即主键的一部分）。
type CategoryRow struct {
	Name      string
	Amount    float64
	Qty       float64
	Cost      float64
	CostedRow int64
	TotalRow  int64
}

// SupplierRow 是一家供应商的当日表现。
type SupplierRow struct {
	Code      string
	Name      string
	Amount    float64
	Qty       float64
	ItemCount int
	TopItems  []TopItemRow
}

// TopItemRow 是供应商维度下的一个商品。
type TopItemRow struct {
	ItemNo string
	Name   string
	Amount float64
	Qty    float64
}

// ProductRow 是一个商品的当日表现。
type ProductRow struct {
	ItemNo   string
	Name     string
	Category string
	Amount   float64
	Qty      float64
}

// CollectSource 记录这一次采集是"首次"还是"校正"。
type CollectSource string

const (
	// CollectSourceInitial 是 22:45 的首次采集。
	CollectSourceInitial CollectSource = "initial"
	// CollectSourceCorrected 是次日 01:00 的校正采集。
	//
	// ⚠️ 必须能区分这两个来源：思迅日结可能晚于 22:45，
	// 于是首次采到的可能是残缺数据。校正采集覆盖同一行，
	// 但"这个数字被校正过"这件事本身要留下痕迹 ——
	// 否则一个被校正的数字与一个从未被校正的数字长得一模一样。
	CollectSourceCorrected CollectSource = "corrected"
)

// ---------------------------------------------------------------------------
// GORM 模型
// ---------------------------------------------------------------------------

// DailyTotals 表 sales_daily_totals —— 门店 × 营业日，一行。
//
// ⚠️ 主键是 (branch_id, oper_date) 而**不是**自增 id：
//
//	采集是幂等的，同一天可能被采两次（首次 + 校正），
//	主键定死才能 upsert 而不是追加出一行互相打架的数字。
type DailyTotals struct {
	BranchID string `gorm:"column:branch_id;type:varchar(64);primaryKey"`
	OperDate string `gorm:"column:oper_date;type:char(10);primaryKey"`

	NetAmount       float64 `gorm:"column:net_amount"`
	NetQty          float64 `gorm:"column:net_qty"`
	RefCost         float64 `gorm:"column:ref_cost"`
	RefMargin       float64 `gorm:"column:ref_margin"`
	RefMarginPct    float64 `gorm:"column:ref_margin_pct"`
	CostCoverage    float64 `gorm:"column:cost_coverage"`
	MarginAvailable bool    `gorm:"column:margin_available"`
	TotalRows       int64   `gorm:"column:total_rows"`
	CostedRows      int64   `gorm:"column:costed_rows"`

	Source      string `gorm:"column:source;type:varchar(16)"`
	CollectedAt string `gorm:"column:collected_at;type:char(25)"`
}

// TableName 指定表名。
func (DailyTotals) TableName() string { return "sales_daily_totals" }

// DailyCategory 表 sales_daily_categories。
type DailyCategory struct {
	BranchID string `gorm:"column:branch_id;type:varchar(64);primaryKey"`
	OperDate string `gorm:"column:oper_date;type:char(10);primaryKey"`
	Name     string `gorm:"column:name;type:varchar(128);primaryKey"`

	Amount    float64 `gorm:"column:amount"`
	Qty       float64 `gorm:"column:qty"`
	Cost      float64 `gorm:"column:cost"`
	CostedRow int64   `gorm:"column:costed_row"`
	TotalRow  int64   `gorm:"column:total_row"`
}

// TableName 指定表名。
func (DailyCategory) TableName() string { return "sales_daily_categories" }

// DailySupplier 表 sales_daily_suppliers。
type DailySupplier struct {
	BranchID string `gorm:"column:branch_id;type:varchar(64);primaryKey"`
	OperDate string `gorm:"column:oper_date;type:char(10);primaryKey"`
	Code     string `gorm:"column:code;type:varchar(64);primaryKey"`

	Name      string  `gorm:"column:name;type:varchar(128)"`
	Amount    float64 `gorm:"column:amount"`
	Qty       float64 `gorm:"column:qty"`
	ItemCount int     `gorm:"column:item_count"`
	// TopItems 是该供应商贡献最高的若干商品（JSON 数组）。
	//
	// ⚠️ 用 JSON 文本而不是再拆一张表：这张子表只随父行整体替换，
	// 从不单独按商品查询；拆表会让"重跑一次采集"要删两次。
	// 读-改-写是整行的，参照 compare.Template 的 stores/rows 同一取舍。
	TopItems string `gorm:"column:top_items;type:text"`
}

// TableName 指定表名。
func (DailySupplier) TableName() string { return "sales_daily_suppliers" }

// DailyProduct 表 sales_daily_products。
type DailyProduct struct {
	BranchID string `gorm:"column:branch_id;type:varchar(64);primaryKey"`
	OperDate string `gorm:"column:oper_date;type:char(10);primaryKey"`
	ItemNo   string `gorm:"column:item_no;type:varchar(64);primaryKey"`

	Name     string  `gorm:"column:name;type:varchar(128)"`
	Category string  `gorm:"column:category;type:varchar(128)"`
	Amount   float64 `gorm:"column:amount"`
	Qty      float64 `gorm:"column:qty"`
}

// TableName 指定表名。
func (DailyProduct) TableName() string { return "sales_daily_products" }

// Models 是 AutoMigrate 要用的全部模型（按依赖顺序）。
func Models() []any {
	return []any{&DailyTotals{}, &DailyCategory{}, &DailySupplier{}, &DailyProduct{}}
}
