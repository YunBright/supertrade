// Package model —— 分割基线的**单价**三件套(2026-10-31 owner:「分割基线加入单价」)。
//
// # 为什么要有这三张表
//
//	分割基线原来只管**重量**(mean/p10/p50/p90 kg)。
//	owner 追加要求：「分割基线加入单价 —— 样本以日为单位,从每日各部位的实际销售单价
//	和整体毛利率来推算部位进货单价」。
//
// ⇒ 于是有了**两段式**(与重量基线同构):
//
//	① **日样本**(pig_cut_price_samples)= 店 × 日 × 部位 的一条**观察**:
//	   当日该部位的零售均价 + 当日整体毛利率 ⇒ 倒推出**部位进货单价**。
//	② **基线**(pig_cut_price_baselines)= 店 × 部位 的**派生结果**:
//	   多个日样本聚合成"这个部位的进货单价是多少元/kg",
//	   同时记下**参考猪进价**(缩放基准,见 service 的 suggestedCutUnitPrice)。
//
// ③ **LLM 单价缓存**(cut_unit_price_caches)= 给"没有销售记录的部位"兜底用
//	(owner 的退取链第②步:LLM 缓存单价)。
//
// # 门店隔离
//
//	三张表的唯一键都含 branch_id ⇒ **每店一份** ⇒ 每店生成**自己的**单价基线。
//
// # ⚠️ 为什么单价基线**不分体重段**
//
//	重量基线必须分体重段(300kg 的猪和 100kg 的猪出肉率不同)。
//	但**单价**是"这个部位值多少钱/kg",主要由品种/行情/部位本身决定,
//	与猪的大小关系不大 ⇒ 单价基线按 (店, 部位) 一条。
//
//	⇒ 猪与猪之间的**价差**用 owner 要的"结合单猪采购价按比例缩放"吸收:
//	  基线记的是"参考猪进价 14.6 元/kg 时的部位单价",
//	  今天这头猪进 16 元/kg ⇒ 各部位单价 × (16 / 14.6)。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// ---- 表:pig_cut_price_samples(单价的**日样本**) ----

// PigCutPriceSample 某店某营业日某部位的**单价观察**。
//
// # 一条样本是怎么来的(owner 的口径)
//
//	① 该部位当天的**实际销售单价** = 当日营收 ÷ 当日销量(零售 + 统收);
//	② 当天的**整体毛利率**(全店口径,不含预期毛利/预售结转);
//	③ 由①②**倒推**该部位的**进货单价**:
//
//		进货单价 = 实际销售单价 ÷ (1 + 整体毛利率)
//
//	⚠️ 这条公式是 owner 2026-10-31 亲口定的(统计栏「日终剩余」那一格:
//	  「用零售均价/(1+今日毛利率)」)⇒ 本仓**统一**用它:
//	  单价样本倒推、建议成本单价、日终剩余估值三处自洽。
//
//	⚠️ 这条公式隐含一个假设:**当天所有部位的毛利率相同**
//	  (owner 明确指定的简化口径:用整体毛利率倒推各部位进货单价)。
//
// # ⚠️ 没有销售记录 ⇒ **没有样本**
//
//	被搭赠等"当天没卖出过的部位"**不产出样本**
//	(owner:「被搭赠等无记录的则保持无分割单价基线」)
//	⇒ 该部位在单价基线里**保持空** ⇒ 应用时走退取链(整店均价 → LLM 缓存 → LLM 重新给价)。
type PigCutPriceSample struct {
	// ID 是确定性代理键(surrogateID("PCP", branch_id, biz_date, cut_type))
	// ⇒ 同一 (店, 日, 部位) 只有一行 ⇒ 采集是**覆盖**语义。
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_price_sample,priority:1" json:"branch_id"`
	// BizDate 是这条样本所属的**营业日**(business day,bizTZ),格式 "YYYY-MM-DD"。
	//
	// ⚠️ **必须用 varchar,不能用 datatypes.Date**(2026-10-31 实测踩过的坑):
	//
	//	`datatypes.Date.Value()` 保留**原时区**⇒ 服务按 bizTZ(Asia/Shanghai)写的行
	//	落库成 "2026-10-06T00:00:00+08:00",而脚手架/别的时区写的落成
	//	"2026-10-06T00:00:00Z" ⇒ **同一个业务日的两种形态**
	//	⇒ ON CONFLICT(branch_id, biz_date, cut_type) **不触发**
	//	⇒ 转而撞主键 id(确定性代理键,两边算出来一模一样 ⇒ 必然撞)。
	//
	// ⇒ 用**纯日期字符串**:排序/比较/唯一键都跨驱动安全(PG 与 SQLite 一致)。
	BizDate string `gorm:"column:biz_date;type:varchar(10);not null;uniqueIndex:idx_pig_price_sample,priority:2" json:"biz_date"`
	CutType CutType         `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_price_sample,priority:3" json:"cut_type"`
	// SoldKg 是当日该部位的**销量**(零售 + 统收,kg)—— 零售价的分母。
	SoldKg decimal.Decimal `gorm:"column:sold_kg;type:decimal(12,3);not null;default:0" json:"sold_kg"`
	// RevenueYuan 是当日该部位的**营收**(元,含统收)—— 零售价的分子。
	RevenueYuan decimal.Decimal `gorm:"column:revenue_yuan;type:decimal(14,2);not null;default:0" json:"revenue_yuan"`
	// RetailPriceYuan 是**实际销售单价**(元/kg)= 营收 ÷ 销量。
	RetailPriceYuan decimal.Decimal `gorm:"column:retail_price_yuan;type:decimal(12,4);not null;default:0" json:"retail_price_yuan"`
	// MarginRate 是当日**整体毛利率**(0~1,不含预期毛利)。
	//
	// ⚠️ 只允许 (0, 0.95) 的值产出样本:毛利率 ≤ 0(亏本/成本算错)或 ≥ 0.95
	//   (成本几乎为 0 ⇒ 多半是"成本算不出来")时,倒推出的进货单价没有意义
	//   ⇒ **整天不产样本**,而不是产出一条垃圾样本把基线教歪。
	MarginRate decimal.Decimal `gorm:"column:margin_rate;type:decimal(10,6);not null;default:0" json:"margin_rate"`
	// CostPriceYuan 是**倒推出的部位进货单价**(元/kg)= 零售价 × (1 − 毛利率)。
	CostPriceYuan decimal.Decimal `gorm:"column:cost_price_yuan;type:decimal(12,4);not null;default:0" json:"cost_price_yuan"`
	// PigPriceYuan 是当日该店**猪的加权平均进价**(元/kg)= Σ猪成本 ÷ Σ猪毛重。
	//
	// ⚠️ 它是"这条样本是在什么猪价下产生的"的上下文
	//   ⇒ 应用基线时用它当**缩放基准**(owner:「基线是 14.6/kg 的,
	//     那 16/kg 的猪每个部位单价都要按比例上涨」)。
	PigPriceYuan decimal.Decimal `gorm:"column:pig_price_yuan;type:decimal(12,4);not null;default:0" json:"pig_price_yuan"`
	// ExcludedAt 是**墓碑时间**(与重量样本同口径:删除 = 打墓碑,不物理删)。
	//
	// ⚠️ 为什么也要墓碑:引擎每天重跑那天的成本链 ⇒ 会重建那天的单价样本
	//   ⇒ 物理删掉的样本会**复活** ⇒ "删除"等于没删。
	ExcludedAt *time.Time `gorm:"column:excluded_at" json:"excluded_at,omitempty"`
	// ExcludedReason 是删除原因留痕。
	ExcludedReason string    `gorm:"column:excluded_reason;type:varchar(32);not null;default:''" json:"excluded_reason,omitempty"`
	CreatedAt      time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (PigCutPriceSample) TableName() string { return "pig_cut_price_samples" }

// ---- 表:pig_cut_price_baselines(单价的**统计基线**) ----

// PigCutPriceBaseline 某店某部位的**进货单价基线**(派生结果)。
//
// # 口径
//
//	样本 = 该部位的所有**日样本**(pig_cut_price_samples,未墓碑)
//	聚合 = **按销量加权平均**(销量大的那天更有代表性)
//
//	UnitCostYuan    = 部位进货单价基线(元/kg)
//	RefPigPriceYuan = 这些样本的**加权平均猪进价**(元/kg)—— 缩放基准
//
// # 怎么用(owner 的原话)
//
//	「应用此基线(注意应用过程中需要结合单猪的采购价,如基线是 14.6/kg 的,
//	  那 16/kg 的猪每个部位单价都要按比例上涨)」
//
// ⇒ 建议成本单价 = UnitCostYuan × (这头猪的进价 ÷ RefPigPriceYuan)
type PigCutPriceBaseline struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_price_baseline,priority:1" json:"branch_id"`
	CutType  CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_price_baseline,priority:2" json:"cut_type"`
	// UnitCostYuan 是部位进货单价基线(元/kg)。
	UnitCostYuan decimal.Decimal `gorm:"column:unit_cost_yuan;type:decimal(12,4);not null;default:0" json:"unit_cost_yuan"`
	// RefPigPriceYuan 是**参考猪进价**(元/kg)—— 单价缩放的基准。
	RefPigPriceYuan decimal.Decimal `gorm:"column:ref_pig_price_yuan;type:decimal(12,4);not null;default:0" json:"ref_pig_price_yuan"`
	// SampleCount 是参与聚合的**日样本数**(n ≥ 1 才可用,见 service 的 MinForecastSamples)。
	SampleCount int `gorm:"column:sample_count;not null;default:0" json:"sample_count"`
	// SourceBranchID 是这条基线实际采用的样本来源门店(跨店借用时 = 被借方)。
	SourceBranchID string `gorm:"column:source_branch_id;type:varchar(64);not null;default:''" json:"source_branch_id"`
	// Borrowed = true ⇒ 这一行是重算时**借来**的(借用一关/一改,它必须能被刷新、也能被清掉)。
	//
	// ⚠️ 单独一列,不靠 source_branch_id 推断(与重量基线同口径的教训:
	//   "曾手工触发"这类标志位会被继承 ⇒ 生命周期语义会串)。
	Borrowed   bool            `gorm:"column:borrowed;not null;default:false" json:"borrowed"`
	ComputedAt time.Time       `gorm:"column:computed_at;not null" json:"computed_at"`
}

// TableName 显式表名。
func (PigCutPriceBaseline) TableName() string { return "pig_cut_price_baselines" }

// ---- 表:cut_unit_price_caches(LLM 单价缓存) ----

// CutUnitPriceCache LLM 给过的**部位单价**缓存(owner 退取链第②步)。
//
// # 它是什么
//
//	LLM 在生成分割建议时,顺带给出每个部位的**进货单价**(unit_price_yuan)
//	⇒ 这份单价被缓存下来 ⇒
//	  ① 报表/建议里"这个部位没有销售记录 ⇒ 没有单价基线"时,**直接用缓存**;
//	  ② 缓存没有 ⇒ 才去**重新问 LLM**(第③步)。
//
// # 为什么单独一张表,而不是塞进 whole_pigs.llm_advice_json
//
//	llm_advice_json 是"**某头猪**的分割建议"(随猪走);
//	而这里要的是"**某店某部位**的当前单价"(随部位走、跨猪复用)
//	⇒ 两者生命周期不同 ⇒ 单独存。
type CutUnitPriceCache struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_cut_unit_price,priority:1" json:"branch_id"`
	CutType  CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_cut_unit_price,priority:2" json:"cut_type"`
	// UnitPriceYuan 是 LLM 给的部位进货单价(元/kg)。
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:decimal(12,4);not null;default:0" json:"unit_price_yuan"`
	// Source 是这个价的来源(llm = LLM 给的;未来可加 manual = 管理员手填)。
	Source     string    `gorm:"column:source;type:varchar(16);not null;default:'llm'" json:"source"`
	ComputedAt time.Time `gorm:"column:computed_at;not null" json:"computed_at"`
}

// TableName 显式表名。
func (CutUnitPriceCache) TableName() string { return "cut_unit_price_caches" }
