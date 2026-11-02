// Package model 定义 fresh-meat 服务用到的所有领域类型。
//
// 本包包含:
//   - 6 张 GORM 模型:whole_pigs / pig_cuts / pork_cuts_stocktakes / line_sales_by_pigs /
//     branch_cut_mappings / waste_logs
//   - 输入 DTO:CreateXxxInput / UpdateXxxInput(POST/PUT body 解)
//   - CutSnapshot:盘点行 JSON 元素(per-SKU:含 cube_product_id)
//   - EventData:发到 Dapr pub/sub 的 payload(EventData 与本仓其它服务**不**携带 tenant_id;
//     见 docs/EVENT-CATALOG.md §2.6/§2.8)。
//
// 命名:所有 GORM 表显式 lowercase + 下划线复数(实际 PG 默认 lowercase,本系统 schema 是 fresh_meat)。
//
// 设计要点:
//   - cut_type 枚举 34 类(2026-10-23 扩):整猪 1 + 分割肉 17 + 骨 5 + 下水 11。
//     命名对齐**行业标准猪部位名**(GB/T 生猪分割 / 零售分割惯例):
//     猪头/猪头皮/猪耳/猪舌(猪利)/猪颈肉/前腿肉(猪肩)/梅花肉(前夹心)/五花肉/
//     肋排(排骨)/里脊肉/瘦肉/后腿肉/猪蹄/猪尾(尾骨)/猪皮/板油(猪油)/
//     筒骨(腿骨)/扇骨(肩胛骨)/龙骨(脊骨)/沙骨(肋骨)/猪头骨/
//     猪肝/猪心/猪肺/猪腰(肾)/猪肚/猪小肚(蜂巢肚)/大肠(生肠)/大肠头/粉肠(小肠)/
//     猪横利(脾)/猪鞭。
//     ⚠️ 原有 12 值的 wire 名**一个都没改** ⇒ 历史数据全部仍然有效。
//   - local_pork(隔山肉/本地土猪)是**品种级**商品,不是解剖部位 ⇒ 单独一类,
//     且**不进整猪分割推演**(见 CarcassCutTypes):一头普通猪不会"分割出隔山肉"。
//   - whole_pig 加 3 字段:half_pig / offal_included / purchase_group_id(场景一/二/三区分)
//   - whole_pig.llm_advice_json 是 jsonb,字段实际写入时由 service 在 insert 后调
//     Dapr Conversation API(见 service/llm.go)同步写入;默认 predictFn 不接 LLM 时留空。
//   - line_sales_by_pig 用于 POS sale.completed 中 fresh_type=meat 行的本地聚合宽表,
//     含 unit_price_yuan 供毛利计算;order_status 区分 S/R(退货 R 行 qty/amount 直存负数)。
//   - waste_logs 表报损持久化(避免 dapr 失败丢数据)。
package model

import (
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// ---- 枚举 ----

// CutType 鲜肉部位。
//
// 与 docs/REQUIREMENTS.md §4.1 / docs/EVENT-CATALOG.md §2.3/§2.8 一致。
//
// ⚠️ **枚举是封闭集合**:`AllCutTypes` 会在 RecordPigCut / RecordPorkCutsStocktake
// 拒绝未知值。2026-10-23 依据真源库(佳乐 1201 / 富百家 010303 近 14 个完整营业日
// 的逐 SKU 销量)扩到 34 类;原有 12 值的 wire 名保持不变 ⇒ 历史数据兼容。
type CutType string

const (
	// ---- A. 整猪(胴体)----
	CutWholePig CutType = "whole_pig" // 白条猪/整猪

	// ---- B. 分割肉 ----
	CutHead          CutType = "head"           // 猪头(头肉)
	CutHeadSkin      CutType = "head_skin"      // 猪头皮
	CutEar           CutType = "ear"            // 猪耳
	CutTongue        CutType = "tongue"         // 猪舌(广西叫"猪利")
	CutNeck          CutType = "neck"           // 猪颈肉(血脖)
	CutLegFront      CutType = "leg_front"      // 前腿肉(猪肩)
	CutShoulderBlade CutType = "shoulder_blade" // 梅花肉(前夹心/夹心肉)
	CutBelly         CutType = "belly"          // 五花肉(肋腹/三层肉)
	CutRib           CutType = "rib"            // 肋排/排骨
	CutTenderloin    CutType = "tenderloin"     // 里脊肉(通脊/外脊)
	CutLean          CutType = "lean"           // 瘦肉
	CutLegRear       CutType = "leg_rear"       // 后腿肉(后臀)
	CutTrotter       CutType = "trotter"        // 猪蹄(前蹄=猪手/后蹄=猪肘)
	CutTail          CutType = "tail"           // 猪尾(尾骨)
	CutSkin          CutType = "skin"           // 猪皮
	CutFat           CutType = "fat"            // 板油/猪油(广西叫"水油")
	// CutLocalPork 隔山肉(本地土猪/黑猪)—— **品种级**商品,不是解剖部位。
	// 它不进整猪分割推演(见 CarcassCutTypes):普通猪不会"分割出隔山肉"。
	CutLocalPork CutType = "local_pork"
	// CutPromo 特价档(促销)—— **商品类别**,不是解剖部位。
	//
	// 2026-10-23 实测:特价档是"**价格槽位**",装的是当天滞销的那个部位
	// (成交价逐日变:03001 名义 5 元实际卖 9 元/斤;03007 名义 20 元
	// 10-13~10-20 卖 15 元、10-21 卖 20 元)⇒ **不能静态映射到某个部位**。
	//
	// 落在这个桶里的销售行:**金额与重量都进得来**(不再被丢弃),
	// 但**不摊到具体部位** ⇒ 毛利报告里单列"特价(未归属)"。
	CutPromo CutType = "promo"

	// ---- C. 骨 ----
	CutBoneShank   CutType = "bone_shank"   // 筒骨(腿骨/胫骨)
	CutBoneScapula CutType = "bone_scapula" // 扇骨(肩胛骨)
	CutBoneSpine   CutType = "bone_spine"   // 龙骨(脊骨)
	CutBoneRib     CutType = "bone_rib"     // 沙骨(肋骨)
	CutBoneHead    CutType = "bone_head"    // 猪头骨

	// ---- D. 下水 ----
	CutLiver            CutType = "liver"             // 猪肝
	CutHeart            CutType = "heart"             // 猪心
	CutLung             CutType = "lung"              // 猪肺
	CutKidney           CutType = "kidney"            // 猪腰(肾)
	CutStomach          CutType = "stomach"           // 猪肚(胃)
	CutStomachHoneycomb CutType = "stomach_honeycomb" // 猪小肚(蜂巢肚)
	CutIntestine        CutType = "intestine"         // 大肠(生肠/肥肠)
	CutIntestineHead    CutType = "intestine_head"    // 大肠头
	CutIntestineSmall   CutType = "intestine_small"   // 粉肠(小肠)
	CutSpleen           CutType = "spleen"            // 猪横利(脾,广西叫"猪横利")
	CutTendon           CutType = "tendon"            // 猪鞭
)

// AllCutTypes 全部 cut 枚举集合(给 RecordPigCut / stocktake 拒绝未知值)。
//
// 顺序 = 分组顺序(整猪 → 分割肉 → 骨 → 下水),前端下拉按同序渲染。
var AllCutTypes = []CutType{
	// A. 整猪
	CutWholePig,
	// B. 分割肉
	CutHead, CutHeadSkin, CutEar, CutTongue, CutNeck,
	CutLegFront, CutShoulderBlade, CutBelly, CutRib, CutTenderloin,
	CutLean, CutLegRear, CutTrotter, CutTail, CutSkin, CutFat, CutLocalPork,
	CutPromo,
	// C. 骨
	CutBoneShank, CutBoneScapula, CutBoneSpine, CutBoneRib, CutBoneHead,
	// D. 下水
	CutLiver, CutHeart, CutLung, CutKidney, CutStomach, CutStomachHoneycomb,
	CutIntestine, CutIntestineHead, CutIntestineSmall, CutSpleen, CutTendon,
}

// OffalCutTypes 下水(内脏)集合 —— 场景二"半头猪带全猪下水"用。
var OffalCutTypes = []CutType{
	CutLiver, CutHeart, CutLung, CutKidney, CutStomach, CutStomachHoneycomb,
	CutIntestine, CutIntestineHead, CutIntestineSmall, CutSpleen, CutTendon,
}

// CarcassCutTypes 整猪分割推演用的部位集合。
//
// = AllCutTypes **去掉 CutLocalPork 与 CutPromo**:前者是品种级商品、
// 后者是促销档位 —— 一头普通猪分割不出它们,让 LLM 去预测只会产生幻觉。
var CarcassCutTypes = func() []CutType {
	out := make([]CutType, 0, len(AllCutTypes))
	for _, c := range AllCutTypes {
		if c == CutLocalPork || c == CutPromo {
			continue
		}
		out = append(out, c)
	}
	return out
}()

// CutLabels cut_type → 中文部位名(行业标准命名)。
//
// 前端(FreshMeatCutType.label)有一份同名映射;两边必须逐字一致,
// 否则同一部位在 POS 明细和盘点单上显示成两个名字。
var CutLabels = map[CutType]string{
	CutWholePig:         "白条猪/整猪",
	CutHead:             "猪头(头肉)",
	CutHeadSkin:         "猪头皮",
	CutEar:              "猪耳",
	CutTongue:           "猪舌(猪利)",
	CutNeck:             "猪颈肉(血脖)",
	CutLegFront:         "前腿肉(猪肩)",
	CutShoulderBlade:    "梅花肉(前夹心)",
	CutBelly:            "五花肉",
	CutRib:              "肋排/排骨",
	CutTenderloin:       "里脊肉",
	CutLean:             "瘦肉",
	CutLegRear:          "后腿肉",
	CutTrotter:          "猪蹄(前蹄/后蹄)",
	CutTail:             "猪尾(尾骨)",
	CutSkin:             "猪皮",
	CutFat:              "板油/猪油",
	CutLocalPork:        "隔山肉(本地土猪)",
	CutPromo:            "特价档(促销)",
	CutBoneShank:        "筒骨(腿骨)",
	CutBoneScapula:      "扇骨(肩胛骨)",
	CutBoneSpine:        "龙骨(脊骨)",
	CutBoneRib:          "沙骨(肋骨)",
	CutBoneHead:         "猪头骨",
	CutLiver:            "猪肝",
	CutHeart:            "猪心",
	CutLung:             "猪肺",
	CutKidney:           "猪腰(肾)",
	CutStomach:          "猪肚(胃)",
	CutStomachHoneycomb: "猪小肚(蜂巢肚)",
	CutIntestine:        "大肠(生肠)",
	CutIntestineHead:    "大肠头",
	CutIntestineSmall:   "粉肠(小肠)",
	CutSpleen:           "猪横利(脾)",
	CutTendon:           "猪鞭",
}

// CutLabel 返回 cut 的中文部位名;未知值原样返回 wire(不吞错)。
func CutLabel(c CutType) string {
	if v, ok := CutLabels[c]; ok {
		return v
	}
	return string(c)
}

// IsOffal 判断 cut 是否属于下水(内脏)。
func IsOffal(c CutType) bool {
	for _, v := range OffalCutTypes {
		if v == c {
			return true
		}
	}
	return false
}

// Valid 判断 cut 是否在 AllCutTypes 集合内。
func (c CutType) Valid() bool {
	for _, v := range AllCutTypes {
		if v == c {
			return true
		}
	}
	return false
}

// DataSource 标识 whole_pig.llm_advice_json 的来源(BI 标注 / sales-agg 推算依据)。
type DataSource string

const (
	DataSourceLLM        DataSource = "llm"         // 由 Dapr Conversation API 同步返回
	DataSourceHistoryAvg DataSource = "history_avg" // LLM 失败/超时降级:近 30 天 ±10% 重量段均值
	DataSourceStub       DataSource = "stub"        // 骨架占位;LLM 真实接入后不再使用
)

// ---- 分割建议的生成状态 ----
//
// ⚠️ 2026-10-26:录猪后建议改成**异步**生成(见 service.RecordWholePig)
// ⇒ 前端必须能区分「还在算」「算好了」「算失败」。
//
// 光看 llm_advice_json 是否为空**分不出来**
// (空既可能是"在算",也可能是"算失败")⇒ 需要一个显式状态。
const (
	// AdviceStatusNone = 没生成过(历史数据 / 还没触发)。
	AdviceStatusNone = "none"
	// AdviceStatusGenerating = 生成中(前端显示"生成中"并轮询)。
	AdviceStatusGenerating = "generating"
	// AdviceStatusReady = 已就绪(llm_advice_json 有值)。
	AdviceStatusReady = "ready"
	// AdviceStatusFailed = 生成失败(前端给「重新生成建议」入口)。
	AdviceStatusFailed = "failed"
)

// ---- 分割建议的**生成方式**(owner 2026-10-31)----
//
// 门店反馈:「提交录入并生成分割建议时,要在提交**前**选用什么方式生成,
// 有基线时默认用基线,其次 LLM。」
//
// ⇒ 这里把「用什么方式」**落库到每头猪**上 ⇒ 成本链按**每头猪各自的模式**取数。
//
// ⚠️ 不能按天统一取：同一天可能一头用基线、一头用 AI。
const (
	// AdviceModeAuto = 自动:**基线优先**,基线没覆盖到的部位才用 LLM 建议。
	//
	// ⚠️ 为什么 auto 还要留 LLM 兜底(别把它做成"有基线就完全不调 AI"):
	//   店 00 实测(2026-10-31):基线 `100-149` 段只覆盖 **25** 个部位,
	//   而 LLM 建议覆盖 **32** 个部位 ⇒ 有 7 个部位**只有** LLM 有数
	//   ⇒ 一旦砍掉,那 7 个部位的产出直接归零 ⇒ 账面凭空缺货 ⇒ 假短卖。
	AdviceModeAuto = "auto"
	// AdviceModeBaseline = 只用基线:基线覆盖的部位用基线 p50,
	// **基线没覆盖的部位没有建议**。
	//
	// ⇒ 这个模式**不调 LLM**(省一次调用 + 省一次等待)⇒ 录猪时状态直接落 none。
	AdviceModeBaseline = "baseline"
	// AdviceModeLLM = 只用 AI:只用这头猪自己的 LLM 建议(**完全不查基线**)。
	AdviceModeLLM = "llm"
)

// ValidAdviceMode 校验建议生成方式(空 = auto ⇒ 兼容老前端)。
func ValidAdviceMode(m string) bool {
	switch m {
	case "", AdviceModeAuto, AdviceModeBaseline, AdviceModeLLM:
		return true
	}
	return false
}

// ---- 表1:whole_pig ----

// WholePig 整猪录入(早盘,一头一行)。
//
// 主键格式:`WP<yyyymmdd><8-hex random>`,由 service.RecordWholePig 生成。
// LLMAdviceJSON 同步调 Dapr Conversation API 后写入;predictFn 未注入或失败时留 NULL + DataSourceStub。
// BranchID 录入后不可改;ArrivedAt 索引 (branch_id, arrived_at) 便于按门店按日查。
type WholePig struct {
	ID                    string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	EarTag                string          `gorm:"column:ear_tag;type:varchar(64);not null;index" json:"ear_tag"`
	GrossWeightKg         decimal.Decimal `gorm:"column:gross_weight_kg;type:decimal(10,3);not null" json:"gross_weight_kg"`
	PurchaseUnitPriceYuan decimal.Decimal `gorm:"column:purchase_unit_price_yuan;type:decimal(10,2);not null" json:"purchase_unit_price_yuan"`
	PurchaseCostYuan      decimal.Decimal `gorm:"column:purchase_cost_yuan;type:decimal(12,2);not null" json:"purchase_cost_yuan"`
	SupplierID            string          `gorm:"column:supplier_id;type:varchar(64);not null" json:"supplier_id"`
	BranchID              string          `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_arrived,priority:1" json:"branch_id"`
	ArrivedAt             time.Time       `gorm:"column:arrived_at;not null;index:idx_branch_arrived,priority:2" json:"arrived_at"`
	RecordedBy            string          `gorm:"column:recorded_by;type:varchar(64);not null" json:"recorded_by"`
	LLMAdviceJSON         datatypes.JSON  `gorm:"column:llm_advice_json;type:jsonb" json:"llm_advice_json,omitempty"`
	DataSource            DataSource      `gorm:"column:data_source;type:varchar(16);not null;default:'stub'" json:"data_source"`
	// AdviceStatus 是**分割建议的生成状态**(见上面的 AdviceStatus* 常量)。
	//
	// ⚠️ 录猪后建议是**异步**生成的(2026-10-26)⇒ 前端靠它区分
	//「生成中 / 已就绪 / 失败」——光看 llm_advice_json 是否为空分不出来。
	AdviceStatus string `gorm:"column:advice_status;type:varchar(16);not null;default:'none'" json:"advice_status"`
	// AdviceMode 是**分割建议的生成方式**(见上面的 AdviceMode* 常量)。
	//
	// ⚠️ 2026-10-31 owner:录猪时要能选「用什么方式生成建议」
	//   ⇒ 落库到**每头猪**上(不是按天)⇒ 成本链逐猪取数。
	AdviceMode string `gorm:"column:advice_mode;type:varchar(16);not null;default:'auto'" json:"advice_mode"`
	// 场景一/二/三区分:
	HalfPig         bool      `gorm:"column:half_pig;not null;default:false" json:"half_pig"`                                                         // 半头猪
	OffalIncluded   bool      `gorm:"column:offal_included;not null;default:false" json:"offal_included"`                                             // 带全猪下水
	PurchaseGroupID string    `gorm:"column:purchase_group_id;type:varchar(64);index:idx_branch_group,priority:2" json:"purchase_group_id,omitempty"` // 场景三 2 头同 group
	CreatedAt       time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (WholePig) TableName() string { return "whole_pigs" }

// ---- 表2:pig_cuts ----

// PigCut 单品补录(部分追加条码;一头猪一个 cut 一行)。
//
// (pig_id, cut_type) 唯一:同一头猪的同一部位不重复补录(若同部位多次切分,合并为多行;
// 现阶段按物理唯一)。
//
// BranchID 冗余 whole_pig.branch_id,避免每次 join;输入由 service.RecordPigCut
// 从 pig 拷贝,不可由 caller 改(防跨店)。
type PigCut struct {
	ID    string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	PigID string `gorm:"column:pig_id;type:varchar(64);not null;index;uniqueIndex:idx_pig_cut,priority:1" json:"pig_id"`
	// ⚠️ 2026-10-23 修正:`idx_pig_cut` 原来**只含 cut_type 一列**
	// (PigID 上写的是普通 `index`,不是 `uniqueIndex`)⇒ 唯一索引变成 UNIQUE(cut_type)
	// ⇒ **整个系统每个部位只能补录一行**(第二头猪的同部位直接被拒)。
	// 意图是 UNIQUE(pig_id, cut_type)("同一头猪的同一部位不重复补录")。
	// ⇒ PigID 必须进同一个唯一索引(priority 1),cut_type 是 priority 2。
	CutType   CutType         `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_cut,priority:2" json:"cut_type"`
	BranchID  string          `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	Barcode   string          `gorm:"column:barcode;type:varchar(64);not null;index" json:"barcode"`
	WeightKg  decimal.Decimal `gorm:"column:weight_kg;type:decimal(10,3);not null" json:"weight_kg"`
	ExpiresAt time.Time       `gorm:"column:expires_at;not null" json:"expires_at"`
	CreatedAt time.Time       `gorm:"column:created_at;not null" json:"created_at"`

	// CubeProductID 是这条补录对应的 cube 商品(= 思迅 item_no)。
	//
	// ⚠️ **可空**:2026-10-23 之前录入的补录没有这个字段(只有条码)。
	// 它是 **SKU 级移动加权成本**的必需输入(入库必须落到 SKU 才能算单 SKU 成本);
	// 为空时按 cut_type 归集到该部位的**唯一/主** SKU(见 service 的兜底逻辑)。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);index" json:"cube_product_id,omitempty"`
}

// TableName 显式表名。
func (PigCut) TableName() string { return "pig_cuts" }

// ---- 表3:pork_cuts_stocktake ----

// PorkCutsStocktake 日终盘点(整店按 cut_type;**可选**,缺失不阻断销售)。
//
// 粒度:branch_id + **归属日**;不是一头猪一行,而是按 cut_type 一行。
// IsComplete 决定 BI 标注(✓ 已盘点 / ⚠ 未盘点 数据为推演)。
// Cuts JSONB 数组:CutSnapshot;LLMReviewJSON 由日终反推后写入。
//
// # 同一 (门店, 归属日) 只有一张盘点单(2026-10-24)
//
// 门店口径:「每天都有盘点单」⇒ 盘点单是**按日 upsert** 的,不是"每次提交新建一张"。
// ⇒ 主键是**确定性**的:surrogateID("SK", branch_id, 归属日)(见 service.StocktakeID)。
// ⇒ 重复提交同一天 = 覆盖同一张单,不会多出一张(多一张 ⇒ 报表重复计数)。
//
// # 补录往日(2026-10-24)
//
// TakenAt 可以落在**往日**(补录)⇒ 这张单描述的是**归属日**那天的事实。
// ⚠️ 余额/成本状态的**按营业日推导**尚未实现(见 service 注释与交付报告「未做」一节)。
type PorkCutsStocktake struct {
	ID       string    `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string    `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_taken,priority:1" json:"branch_id"`
	TakenAt  time.Time `gorm:"column:taken_at;not null;index:idx_branch_taken,priority:2" json:"taken_at"`
	// TakenDate 是这张盘点单的**归属日**(YYYY-MM-DD,营业日界时区)。
	//
	// ⚠️ **只读派生字段**(gorm:"-" ⇒ 不入库):由 TakenAt 按营业日界时区折算,
	// 方便前端直接显示/回传归属日,不用自己再做时区换算。
	TakenDate  string `gorm:"-" json:"taken_date,omitempty"`
	TakenBy    string `gorm:"column:taken_by;type:varchar(64);not null" json:"taken_by"`
	IsComplete bool   `gorm:"column:is_complete;not null" json:"is_complete"`
	// SoldOut 标记「当天一点货都没剩」(2026-10-24 门店口径:
	// 「每天都有盘点单,没有任何剩余也要创建一个空的,标记为『售罄』」)。
	//
	// 判定:cuts 为空数组,或所有行的 actual_remain_kg 都是 0。
	SoldOut bool `gorm:"column:sold_out;not null;default:false" json:"sold_out"`
	// RestDay 标记「**鲜肉休市**」(2026-10-24 门店口径:
	// 当天**既没有卖猪、也没有搬冷库商品上猪肉台** ⇒ 日终盘点标记为「鲜肉休市」)。
	//
	// ⚠️ 与 SoldOut 是**两个不同的状态**(wire 上也要能区分):
	//   - sold_out:盘了,但一点剩余都没有(卖光了);
	//   - rest_day :当天没进猪、也没搬上台 ⇒ 这天整个鲜肉板块没开张。
	//
	// 判定:归属日当天 whole_pigs 无进场 + stock_bags 无"搬上台"流水。
	RestDay       bool           `gorm:"column:rest_day;not null;default:false" json:"rest_day"`
	Cuts          datatypes.JSON `gorm:"column:cuts;type:jsonb;not null" json:"cuts"`
	LLMReviewJSON datatypes.JSON `gorm:"column:llm_review_json;type:jsonb" json:"llm_review_json,omitempty"`
	CreatedAt     time.Time      `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (PorkCutsStocktake) TableName() string { return "pork_cuts_stocktakes" }

// CutSnapshot 盘点单 cuts JSON 数组元素(per-SKU:每 tray/hook 一行)。
//
// 仓管"精确到具体 SKU 和数量"要求按 cube_product_id 维度;同 cut_type
// 下不同 SKU(如 五花肉 / 梅肉 / 五花片)各一条记录。
//
// # ⚠️ ActualRemainKg 现在是**柜台(猪肉台)实盘**(2026-10-24 门店口径拆分)
//
// 「盘点盘的是猪肉台,不盘冷库」⇒ 仓管报的数**只含柜台**。
// 冷库余额**单独一个字段**(ColdStoreKg)给出 ⇒ 前端分开展示。
//
// ⚠️ 引擎里"期末 = 柜台实盘 + 冷库余额"仍然成立(两者相加 = 全店结余),
// 但**接口给门店看的是柜台数**。
type CutSnapshot struct {
	CutType       CutType `json:"cut_type"`
	CubeProductID string  `json:"cube_product_id"` // 必填;由 (branch, cut_type) 在 branch_cut_mapping 校验
	// ActualRemainKg 是**柜台(猪肉台)实盘**(不含冷库)。
	ActualRemainKg decimal.Decimal `json:"actual_remain_kg"`
	// ColdStoreKg 是该 SKU 的**冷库余额**(与柜台实盘分开给出)。
	ColdStoreKg decimal.Decimal `json:"cold_store_kg,omitempty"`
	// TotalRemainKg 是全店结余(= 柜台实盘 + 冷库余额)。
	TotalRemainKg    decimal.Decimal `json:"total_remain_kg,omitempty"`
	ExpectedRemainKg decimal.Decimal `json:"expected_remain_kg"`
	VarianceKg       decimal.Decimal `json:"variance_kg"`
}

// ---- 表4:line_sales_by_pig ----

// LineSalesByPig POS 销售事件中 fresh_type=meat 行的本地聚合宽表(FIELD_SPEC §1.6 未列,
// DESIGN.md:577 明确要求)。
//
// UNIQUE(branch_id, pos_line_id) — 同一 pos_line 二次投递 idempotent (do nothing)。
// SaleDate 是 datatypes.Date(只存日期部分),不存时分秒,与 BI 按日聚合对齐。
// CubeProductID 是冗余(sale.completed 可能不含 cube product id;从 branch_cut_mapping 补) ,
// 由 OnSaleCompleted 写入。
// UnitPriceYuan 从 sale.completed.unit_price_yuan 直接拷贝,供后续 per-SKU 单价 / 毛利计算。
// OrderStatus "S"=销售 "R"=退货;POS 已自翻符号(负 qty/amount);毛利计算时 R 行不进 revenue。
type LineSalesByPig struct {
	ID            uint64          `gorm:"primaryKey;column:id;autoIncrement" json:"id"`
	BranchID      string          `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_date,priority:1" json:"branch_id"`
	SaleDate      datatypes.Date  `gorm:"column:sale_date;not null;index:idx_branch_date,priority:2" json:"sale_date"`
	PigID         string          `gorm:"column:pig_id;type:varchar(64);not null;index" json:"pig_id"`
	CutType       CutType         `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`
	QtyKg         decimal.Decimal `gorm:"column:qty_kg;type:decimal(12,3);not null" json:"qty_kg"`
	AmountYuan    decimal.Decimal `gorm:"column:amount_yuan;type:decimal(14,2);not null" json:"amount_yuan"`
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:decimal(10,2);not null;default:0" json:"unit_price_yuan"`
	Source        string          `gorm:"column:source;type:varchar(32);not null;default:'pos'" json:"source"` // pos | erp
	PosLineID     string          `gorm:"column:pos_line_id;type:varchar(64);uniqueIndex:idx_pos_line_branch,priority:1" json:"pos_line_id"`
	PosBranchID   string          `gorm:"column:pos_branch_id;type:varchar(64);uniqueIndex:idx_pos_line_branch,priority:2" json:"pos_branch_id"`
	CubeProductID string          `gorm:"column:cube_product_id;type:varchar(64);index" json:"cube_product_id,omitempty"`
	OrderStatus   string          `gorm:"column:order_status;type:varchar(2);not null;default:'S'" json:"order_status"` // "S" | "R"
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (LineSalesByPig) TableName() string { return "line_sales_by_pigs" }

// ---- 表5:branch_cut_mapping ----

// BranchCutMapping 门店 cut 部位 ↔ cube product 对应表。
//
// # 唯一约束(2026-10-23 改)
//
// 原来是 UNIQUE(branch_id, cut_type) —— **一个部位在一个店只能挂一个 SKU**。
// 但同一份设计里 CutSnapshot 明写"同 cut_type 下不同 SKU(如 五花肉 / 精品五花肉 /
// 团购五花肉)各一条记录",盘点入参也是按 (cut_type, cube_product_id) 三元组校验的
// ⇒ 旧约束让 per-SKU 盘点**只能引用一个 SKU**,第二个 SKU 直接报
// "(branch, cut) 映射已存在"。
//
// 真源库实测(2026-10-23,近 14 个完整营业日)这种一对多在两家店里都是常态:
//
//	佳乐(01):belly → 腩肉(五花肉) + 精品五花肉;rib → 前排(硬排) + 精品排骨
//	富百家(00):rib → 精选腩排 + 精选排骨;shoulder_blade → 梅花肉 + 梅肉/腱子肉;
//	           trotter → 猪脚 + 猪手;intestine → 大肠 + 生肠
//
// ⇒ 改成:
//   - UNIQUE(branch_id, cube_product_id) —— **一个 SKU 在一个店只对应一个部位**
//     (resolvePigByCubeProduct 按 (branch, cube_product_id) 反查部位,必须唯一);
//   - INDEX(branch_id, cut_type) —— 非唯一,一个部位可挂多个 SKU。
//
// CubeProductName 冗余缓存 cube.GetProduct 的 name,避免每次 join cubeclient(高频读)。
//
// 初始化:拓店 / 改品时由仓管 admin 通过 POST/PUT /branch-cut-mappings 维护。
type BranchCutMapping struct {
	ID              string    `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID        string    `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_branch_sku,priority:1;index:idx_branch_cut_lookup,priority:1" json:"branch_id"`
	CutType         CutType   `gorm:"column:cut_type;type:varchar(32);not null;index:idx_branch_cut_lookup,priority:2" json:"cut_type"`
	CubeProductID   string    `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_branch_sku,priority:2" json:"cube_product_id"`
	CubeProductName string    `gorm:"column:cube_product_name;type:varchar(255)" json:"cube_product_name,omitempty"`
	CreatedAt       time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (BranchCutMapping) TableName() string { return "branch_cut_mappings" }

// ---- 表5b:promo_skus ----

// PromoSku 特价档货号登记(**长期配置**,像部位 SKU 映射一样,不需要每天动)。
//
// # 为什么需要这张表
//
// 2026-10-23 实测:特价档是"**价格槽位**",装的是当天滞销的那个部位
// (成交价逐日变:03001 名义 5 元实际卖 9 元/斤;03007 名义 20 元
// 10-13~10-20 卖 15 元、10-21 卖 20 元)
// ⇒ **不能静态映射到某个部位**。
//
// 但"这些货号是特价"这件事是**稳定**的(佳乐 9 个货号长期存在)⇒ 登记一次即可。
//
// # 作用
//
// 登记过的货号,其 POS 销售行**不再被丢弃**(`resolvePigByCubeProduct` 查不到
// 部位映射时原来直接 `continue` 丢整行)⇒ 金额与重量进得来。
// 未指定部位时归到 `promo` 桶,毛利报告单列"特价(未归属)"。
type PromoSku struct {
	ID            string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_promo_branch_sku,priority:1" json:"branch_id"`
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_promo_branch_sku,priority:2" json:"cube_product_id"`
	ProductName   string `gorm:"column:product_name;type:varchar(255)" json:"product_name,omitempty"`
	// PriceTierText 是档位说明(如 "特价猪肉8元"、"特价猪肉20元"),可空 —— 只给人看。
	PriceTierText string    `gorm:"column:price_tier_text;type:varchar(64)" json:"price_tier_text,omitempty"`
	Note          string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	Active        bool      `gorm:"column:active;not null;default:true" json:"active"`
	CreatedAt     time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (PromoSku) TableName() string { return "promo_skus" }

// ---- 表6:waste_logs ----
// WasteLog 报损日志(实际持久化,避免 dapr 失败丢数据)。
//
// ID 格式:`WL<yyyymmdd><8-hex>`(同 stocktake / fresh-meat 其它表)。
// (PigID, CutType) 可空:报损可能是批量处理(如批次过期)、不是单 SKU。
// CubeProductID 可空;若填,直接记实际报废的 cube SKU。
// RecordedAt 由 caller 传(saleDate-like);若空,service 用 ttl=now。
//
// # WasteType:报损 vs **搭赠**(2026-10-31 owner)
//
//   - `loss`(默认):真损耗(过期、破损、加工废料)⇒ 计入「报损成本」。
//   - `gift`:**搭赠**——买肉送猪肝这类「有成本、无收入」的出库。
//
// ⚠️ 为什么必须分开(而不是复用 reason 文本):
//
//	搭赠**不是损耗**。混进报损会污染「切分损耗 / 隔夜失重」的损耗统计,
//	也会让「报损」这个口径失真(门店看报损金额会以为在亏损耗)。
//	⇒ 它需要**自己的一条成本线**(搭赠成本),并**扣毛利**(owner 2026-10-31 拍板)。
//
// ⚠️ 搭赠为什么必须有:搭赠的货**从库存里出去了但没有任何销售记录**
// (没过称、没过账)⇒ 链上看就是「盘亏」⇒ 会被误判成短卖 ⇒ 缺口算到别的出口头上。
type WasteLog struct {
	ID            string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string          `gorm:"column:branch_id;type:varchar(64);not null;index:idx_branch_recorded,priority:1" json:"branch_id"`
	PigID         string          `gorm:"column:pig_id;type:varchar(64);index" json:"pig_id,omitempty"`
	CutType       CutType         `gorm:"column:cut_type;type:varchar(32)" json:"cut_type,omitempty"`
	CubeProductID string          `gorm:"column:cube_product_id;type:varchar(64)" json:"cube_product_id,omitempty"`
	QtyKg         decimal.Decimal `gorm:"column:qty_kg;type:decimal(10,3);not null" json:"qty_kg"`
	Reason        string          `gorm:"column:reason;type:varchar(255);not null" json:"reason"`
	// WasteType 是**出口类型**:`loss`(报损,默认) | `gift`(搭赠)。
	//
	// ⚠️ 默认值给 'loss' ⇒ 老行不用回填(它们全是报损)⇒ 加列安全。
	WasteType  string    `gorm:"column:waste_type;type:varchar(16);not null;default:'loss'" json:"waste_type"`
	RecordedAt time.Time `gorm:"column:recorded_at;not null;index:idx_branch_recorded,priority:2" json:"recorded_at"`
	OperatorID string    `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	CreatedAt  time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// ---- waste_logs 的出口类型 ----

const (
	// WasteTypeLoss 报损(真损耗)。老数据与默认值。
	WasteTypeLoss = "loss"
	// WasteTypeGift 搭赠(买肉送猪肝这类「有成本、无收入」的出库)。
	WasteTypeGift = "gift"
)

// ValidWasteType 判断出口类型是否合法(空 = loss,兼容老客户端)。
func ValidWasteType(t string) bool {
	switch t {
	case "", WasteTypeLoss, WasteTypeGift:
		return true
	}
	return false
}

// TableName 显式表名。
func (WasteLog) TableName() string { return "waste_logs" }

// ---- 表7b:stock_bags(结转记录 / 袋子台账)----
//
// StockBag 一行**结转记录**:门店日终盘点时,按 SKU 把剩余的鲜肉**打包过秤**
// 后登记的一条「归属日 + SKU + 重量」。
//
// ⚠️ **2026-11-02 owner:标签号玩法整套下线** —— 搬台不再「按标签整袋搬」,
// 盘点也不再填标签号 ⇒ 库里再没有新行带标签 ⇒ 这张表退化成
// 「按日结转台账」(FIFO 队列 + 猪归属快照)。
//
// # 为什么还要保留"袋子"这个粒度
//
// ① **搬上台要有流水**:每日毛利的"分摊重量"需要知道当日搬上台了多少
//
//	(搬上台的是**老货**,不能当成当日新分割的货再摊一次猪成本)。
//	⇒ 位置表只记余额、不留痕 ⇒ 结转记录就是那条流水。
//
// ② **每行要有归属**:这批货是当天哪几头猪的产出 ⇒ 卖掉时毛利能**回归**
//
//	到产出它的那头猪(而不是今天碰巧在场的猪)。
//
// ③ **FIFO 队列**:按产生日排队 ⇒ 卖掉一笔货时,收入按「结转先后顺序」摊回猪。
//
// # label 列的历史遗留
//
// 标签号(袋标)曾是这一行的身份(也是「按标签整袋搬」的钥匙)⇒ 现在不再产生
// ⇒ 新行的 label 为 NULL。**列与部分唯一索引都保留**(历史数据 + 迁移不动),
// 但**代码不再读也不再写**它(见 internal/fresh-meat/migrate.go 的注释)。
type StockBag struct {
	// ID 是**确定性**代理主键:同一 (门店, 产生日, SKU) 永远是同一行
	// ⇒ 重复提交同一天的盘点 = 覆盖,不会多出一行。
	//
	// ⚠️ 同一 (归属日, SKU) 只登记**一行**(同 SKU 多行 ⇒ 合并求和)。
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	// CubeProductID 是这袋货的 SKU(= 思迅货号)。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;index" json:"cube_product_id"`
	// CutType 是这个 SKU 归属的部位(冗余一份,便于按部位汇总)。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`
	// QtyKg 是这袋的**过秤重量**(不装袋的行 = 当天结转的散货重量)。
	QtyKg decimal.Decimal `gorm:"column:qty_kg;type:decimal(12,3);not null" json:"qty_kg"`
	// ConsumedKg 是这条结转记录**已经被 FIFO 卖掉**的重量(2026-10-24)。
	//
	// ⚠️ 为什么必须落库:毛利按 FIFO 回归到猪时,每卖一笔就要扣掉对应批次的
	// 「还没卖掉的重量」⇒ 不落库就会**每次重算都把同一批货再摊一次**。
	// ⇒ 结存 = qty_kg − consumed_kg。
	ConsumedKg decimal.Decimal `gorm:"column:consumed_kg;type:decimal(14,3);not null;default:0" json:"consumed_kg"`
	// OriginDate 是这袋货**产生**的日期(哪天的盘点结转出来的)。
	//
	// ⚠️ 它同时是"先进先出"的排序依据:冷库里的货按它排队,
	// 结算/出库时先出最早的那批。
	OriginDate datatypes.Date `gorm:"column:origin_date;not null;index" json:"origin_date"`
	// Location 是这袋当前在哪:cold_store(冷库)| counter(猪肉台)。
	Location string `gorm:"column:location;type:varchar(16);not null;default:'cold_store'" json:"location"`
	// Status:active(在用)| released(已搬上台)。
	//
	// ⚠️ 2026-11-02 owner:标签号玩法下线 ⇒ 不再有"搬上台 ⇒ 释放标签号"这条路径
	//   ⇒ 新行一律 active;released 只剩历史行。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'active'" json:"status"`
	// PigAlloc 是**归属快照**:这袋货由哪几头猪产出、各多少 kg。
	//
	// ⚠️ 存成 JSON 是为了"盘点那一刻就把归属钉死"——后来猪被删/改名都不影响它。
	//
	// 形如:[{"pig_id":"WP...","qty_kg":"12.5"}]
	PigAlloc datatypes.JSON `gorm:"column:pig_alloc;type:jsonb" json:"pig_alloc,omitempty"`
	// Source 是这行结转记录的**来源**(2026-10-25)。
	//
	// ⚠️ 为什么需要它:结转记录有两种来路 ——
	//	stocktake    —— 日终盘点自动登记(主路径)
	//	bulk_manual —— 大宗手工兜底(盘点没录该部位时)
	//
	// ⇒ 「同一天重提交盘点时清掉这次没出现的旧行」必须**只清 stocktake 的**,
	// 否则会把**手工补录**的冷库货悄悄删掉(货还在冷库、账没了)。
	//
	// ⚠️ 老数据没有这一列 ⇒ 默认按 'stocktake' 处理(历史行全是盘点来的)。
	Source string `gorm:"column:source;type:varchar(16);not null;default:'stocktake'" json:"source"`
	// UnitCostYuan 是**入冷库那一刻**快照的移动加权单价(元/kg)—— 这就是"记录分摊成本"。
	//
	// ⚠️ 为什么要落在袋子上:大宗结算可能隔好几天才结账(单价那时才定),
	//   若结算时用"当日单价"会把成本算歪 ⇒ 必须钉在**入库那一刻**。
	UnitCostYuan decimal.Decimal `gorm:"column:unit_cost_yuan;type:decimal(16,4);not null;default:0" json:"unit_cost_yuan"`
	// MovedAt 是最近一次**搬上台**的时间(用于"当日搬上台了多少"的流水口径)。
	MovedAt   *time.Time `gorm:"column:moved_at" json:"moved_at,omitempty"`
	CreatedAt time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (StockBag) TableName() string { return "stock_bags" }

// ---- 表8:sku_stock_positions ----

// StockLocation 库存位置。
//
// 门店只有两个存放位置(2026-10-23 与门店确认):
//
//	counter    —— 猪肉台:当日在售、顾客看得见的货
//	cold_store —— 冷库:过夜存放 / 分割副产(板油) / 待统收
//
// 「全店」是 **查询口径**(= counter + cold_store),不是第三个真实位置。
type StockLocation string

const (
	// StockLocationCounter 猪肉台。
	StockLocationCounter StockLocation = "counter"
	// StockLocationColdStore 冷库。
	StockLocationColdStore StockLocation = "cold_store"
)

// StockScope 查询范围。
//
// ⚠️ **不提供"只查冷库"** —— 冷库单独看没有业务含义(2026-10-23 门店明确)。
type StockScope string

const (
	// StockScopeCounter 只查猪肉台(**默认**)。
	StockScopeCounter StockScope = "counter"
	// StockScopeAll 全店 = 猪肉台 + 冷库。
	StockScopeAll StockScope = "all"
)

// SkuStockPosition 某店某 SKU 在某位置的库存数量(kg)。
//
// 位置余额由**流水**维护(而不是每次实盘覆盖):
//
//	入冷库: 昨日柜台实盘(过夜) / 分割副产入冷库
//	出冷库: 搬上台 / 酒店统收
//
// ⇒ 冷库可以长期压货(门店可能几天不上台),而"搬上台"这一步
// 由仓管决定搬多少(可部分、可不上),必须记录 ⇒ 冷库余额才准。
//
// 全店余额 = counter + cold_store;**毛利用的是全店口径**。
type SkuStockPosition struct {
	// ID 是**代理主键**(单列 varchar)—— 2026-10-23 加。
	//
	// ⚠️ 为什么不用 (branch_id, cube_product_id, location) 复合主键:
	// core/purge 按**单列**主键定位要删的行(旁表 testrun_rows 记的是主键值)
	// ⇒ 复合主键的表**进不了 PurgeRegistry** ⇒ 契约测试写在位置表上的行
	// **永远清不掉** ⇒ 生产库里会一天天攒下"0 kg 的幽灵行",
	// 而库存查询里会冒出根本不存在的 SKU。
	//
	// ⇒ 改成单列代理主键 + 业务唯一索引(下面三个字段仍然唯一)。
	//
	// ⚠️ ID 必须**由业务键确定性地算出来**(见 service.surrogateID):
	// ON CONFLICT 冲突时保留的是**旧行的主键**,而溯源记账记的是
	// "本次 INSERT 语句里的主键" ⇒ 若主键随机,记下来的就是一个**不存在的 id**
	// ⇒ purge 拿着它删不到任何行 ⇒ 残留照旧。
	ID            string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string          `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_sku_pos_branch_sku_loc,priority:1" json:"branch_id"`
	CubeProductID string          `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_sku_pos_branch_sku_loc,priority:2" json:"cube_product_id"`
	Location      StockLocation   `gorm:"column:location;type:varchar(16);not null;uniqueIndex:idx_sku_pos_branch_sku_loc,priority:3" json:"location"`
	QtyKg         decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null;default:0" json:"qty_kg"`
	UpdatedAt     time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (SkuStockPosition) TableName() string { return "sku_stock_positions" }

// ---- 表8:sku_cost_states ----

// SkuCostState 某店某 SKU 的**全店移动加权成本状态**(当日有效)。
//
// 这是 SKU 级移动加权平均的"账" —— 昨天的单价就是今天的期初金额基准。
// 每天日终核算时按当日实盘刷新(见 service/moving_average.go)。
//
// ⚠️ 为什么是**全店**一个单价而不是分位置:
// 转库(冷库 ⇄ 猪肉台)只是物理位置变化,**成本跟着货走**;
// 分位置算两套单价会让"搬上台"变成一次成本重算,且报表复杂度翻倍。
type SkuCostState struct {
	// ID 是**代理主键**(单列 varchar)—— 2026-10-23 加,理由同 SkuStockPosition:
	// 复合主键的表进不了 purge 清单 ⇒ 契约测试写的行永远清不掉。
	// ⚠️ 必须由业务键**确定性**算出(见 service.surrogateID),否则
	// upsert 记账记的是"本次 INSERT 里的 id"而非旧行的 id ⇒ purge 删不到。
	ID                string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID          string          `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_sku_cost_state_branch_sku,priority:1" json:"branch_id"`
	CubeProductID     string          `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_sku_cost_state_branch_sku,priority:2" json:"cube_product_id"`
	QtyKg             decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null;default:0" json:"qty_kg"`
	AmountYuan        decimal.Decimal `gorm:"column:amount_yuan;type:decimal(16,2);not null;default:0" json:"amount_yuan"`
	MovingAvgUnitCost decimal.Decimal `gorm:"column:moving_avg_unit_cost;type:decimal(16,4);not null;default:0" json:"moving_avg_unit_cost"`
	LastBizDate       datatypes.Date  `gorm:"column:last_biz_date;not null" json:"last_biz_date"`
}

// TableName 显式表名。
func (SkuCostState) TableName() string { return "sku_cost_states" }

// ---- 表9:sku_cost_daily ----

// SkuCostDaily SKU 成本**日快照**。
//
// 两个用途:
//  1. **可审计** —— 每天怎么算出来的(期初/入库/销售/期末/加权单价)都能回看;
//  2. **长期毛利** —— 任意期间(周/月/季)的收入与成本从这里累计。
//
// UNIQUE(branch_id, biz_date, cube_product_id) ⇒ 同一天重算会覆盖,不会堆重复行。
type SkuCostDaily struct {
	ID            string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID      string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_sku_cost_daily,priority:1" json:"branch_id"`
	BizDate       datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_sku_cost_daily,priority:2" json:"biz_date"`
	CubeProductID string         `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_sku_cost_daily,priority:3" json:"cube_product_id"`
	CutType       CutType        `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`

	OpeningKg      decimal.Decimal `gorm:"column:opening_kg;type:decimal(14,3);not null;default:0" json:"opening_kg"`
	OpeningAmount  decimal.Decimal `gorm:"column:opening_amount;type:decimal(16,2);not null;default:0" json:"opening_amount"`
	ProducedKg     decimal.Decimal `gorm:"column:produced_kg;type:decimal(14,3);not null;default:0" json:"produced_kg"`
	ProducedAmount decimal.Decimal `gorm:"column:produced_amount;type:decimal(16,2);not null;default:0" json:"produced_amount"`
	SoldKg         decimal.Decimal `gorm:"column:sold_kg;type:decimal(14,3);not null;default:0" json:"sold_kg"`
	SoldAmount     decimal.Decimal `gorm:"column:sold_amount;type:decimal(16,2);not null;default:0" json:"sold_amount"`
	BulkSoldKg     decimal.Decimal `gorm:"column:bulk_sold_kg;type:decimal(14,3);not null;default:0" json:"bulk_sold_kg"`
	BulkSoldAmount decimal.Decimal `gorm:"column:bulk_sold_amount;type:decimal(16,2);not null;default:0" json:"bulk_sold_amount"`
	WasteKg        decimal.Decimal `gorm:"column:waste_kg;type:decimal(14,3);not null;default:0" json:"waste_kg"`
	WasteAmount    decimal.Decimal `gorm:"column:waste_amount;type:decimal(16,2);not null;default:0" json:"waste_amount"`
	ClosingKg      decimal.Decimal `gorm:"column:closing_kg;type:decimal(14,3);not null;default:0" json:"closing_kg"`
	ClosingAmount  decimal.Decimal `gorm:"column:closing_amount;type:decimal(16,2);not null;default:0" json:"closing_amount"`

	MovingAvgUnitCost decimal.Decimal `gorm:"column:moving_avg_unit_cost;type:decimal(16,4);not null;default:0" json:"moving_avg_unit_cost"`
	SalesCostYuan     decimal.Decimal `gorm:"column:sales_cost_yuan;type:decimal(16,2);not null;default:0" json:"sales_cost_yuan"`

	// CounterQty / ColdStoreQty 是期末两位置分布(全店 = 两者之和)。
	CounterQty   decimal.Decimal `gorm:"column:counter_qty;type:decimal(14,3);not null;default:0" json:"counter_qty"`
	ColdStoreQty decimal.Decimal `gorm:"column:cold_store_qty;type:decimal(14,3);not null;default:0" json:"cold_store_qty"`

	// DataSource:actual(有完整实盘) | partial(有盘点但未完成) | estimated(无盘点)
	DataSource string    `gorm:"column:data_source;type:varchar(16);not null;default:'estimated'" json:"data_source"`
	CreatedAt  time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (SkuCostDaily) TableName() string { return "sku_cost_daily" }

// ---- 表9b:cut_cost_daily(部位级成本日快照)----
//
// CutCostDaily 某个营业日**某部位**的成本**日快照** —— 成本链的载体。
//
// # 为什么是**部位**而不是 SKU(2026-10-27 口径决议)
//
// 门店口径(一直如此,见 CutStockDaily / SkuStockOpening 的注释):
// 「鲜肉只管部位数据,不管 SKU;SKU 映射只是为了把销售、数量等数据聚合到其部位」
// ⇒ **库存、结余、盘点、期初结转、大宗结算全是部位口径**。
//
// 而旧的成本链(`sku_cost_daily` / `sku_cost_states`)是 **SKU 主键** ⇒ 两套粒度打架:
// `overlayStockOpenings` 把**部位**的期初 100% 记到该部位的「主 SKU」,
// 同部位其它 SKU 的期初被清零 ⇒ 那个货号账面恒为 0 ⇒ 卖出即**零成本**
// ⇒ 毛利率 100%(毛利虚高),且「应有」被负数裁剪分支抹掉(恒等式对不上)。
//
// ⇒ 成本链改到部位级:这张表就是新链的「上一日结余」来源。
//
// # 与旧表的关系
//
// `sku_cost_daily` / `sku_cost_states` **停止写入**(历史保留只读)。
// ⚠️ 两张表粒度不同,**不要**混查。
type CutCostDaily struct {
	// ID 是**确定性**代理主键(surrogateID("CCD", branch_id, biz_date, cut_type))
	// ⇒ 同一 (门店, 归属日, 部位) 永远只有一行 ⇒ 重算 = 覆盖。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_cut_cost_daily,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_cut_cost_daily,priority:2" json:"biz_date"`
	// CutType 是**部位**(五花肉 / 猪蹄 …)—— 成本链的计算单元。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_cut_cost_daily,priority:3" json:"cut_type"`

	// OpeningKg / OpeningAmount 是**期初**(kg / 元):上一营业日的部位结余。
	OpeningKg     decimal.Decimal `gorm:"column:opening_kg;type:decimal(14,3);not null;default:0" json:"opening_kg"`
	OpeningAmount decimal.Decimal `gorm:"column:opening_amount;type:decimal(16,2);not null;default:0" json:"opening_amount"`
	// ProducedKg / ProducedAmount 是**本期入库**(今日分割产出 kg + 其猪成本分摊额)。
	ProducedKg     decimal.Decimal `gorm:"column:produced_kg;type:decimal(14,3);not null;default:0" json:"produced_kg"`
	ProducedAmount decimal.Decimal `gorm:"column:produced_amount;type:decimal(16,2);not null;default:0" json:"produced_amount"`
	// SoldKg / SoldAmount 是**本期零售销售**。
	SoldKg     decimal.Decimal `gorm:"column:sold_kg;type:decimal(14,3);not null;default:0" json:"sold_kg"`
	SoldAmount decimal.Decimal `gorm:"column:sold_amount;type:decimal(16,2);not null;default:0" json:"sold_amount"`
	// BulkSoldKg / BulkSoldAmount 是**统收**(酒店批量出库)。
	BulkSoldKg     decimal.Decimal `gorm:"column:bulk_sold_kg;type:decimal(14,3);not null;default:0" json:"bulk_sold_kg"`
	BulkSoldAmount decimal.Decimal `gorm:"column:bulk_sold_amount;type:decimal(16,2);not null;default:0" json:"bulk_sold_amount"`
	// WasteKg / WasteAmount 是**报损**。
	WasteKg     decimal.Decimal `gorm:"column:waste_kg;type:decimal(14,3);not null;default:0" json:"waste_kg"`
	WasteAmount decimal.Decimal `gorm:"column:waste_amount;type:decimal(16,2);not null;default:0" json:"waste_amount"`

	// ExpectedClosingKg 是**账面应有**期末量 = 期初 + 入库 − 销售 − 统收 − 报损。
	//
	// ⚠️ **不裁 0**:负数 = **账面短卖**(卖超了库存)⇒ 必须看得见。
	//   旧引擎在这里裁 0 ⇒ 缺口被抹掉 ⇒ 部位"应有"反而变正 ⇒ 门店看到的行自相矛盾。
	ExpectedClosingKg decimal.Decimal `gorm:"column:expected_closing_kg;type:decimal(14,3);not null;default:0" json:"expected_closing_kg"`
	// ClosingKg 是**期末**量(有盘点 = 实盘;无盘点 = 账面推演)。
	//
	// ⚠️ **不为负**(owner 2026-10-30:「期初不能有负数」):
	//   盘到的部位(含 0 kg)用实盘;没盘到的用 max(账面推演, 0)
	//   ⇒ 链上永远是 ≥0 ⇒ 下一天的期初不会是负数。
	//   ⚠️ **分部位的账面不足不算短卖**(见 CutShortSellKg)⇒ 它只是"建议分割与门店
	//   实际卖的不匹配" ⇒ 不进链。
	ClosingKg     decimal.Decimal `gorm:"column:closing_kg;type:decimal(14,3);not null;default:0" json:"closing_kg"`
	ClosingAmount decimal.Decimal `gorm:"column:closing_amount;type:decimal(16,2);not null;default:0" json:"closing_amount"`

	// CutShortSellKg 是**该部位账面不足**的量(kg)—— 只作提示,**不进链**。
	//
	// = max(0, 卖出+统收+报损 + 期末 − 可售量)
	// ⚠️ 典型场景(生产实测 店 00 2026-10-29):AI 的建议分割给 fat 只分了 5.073kg,
	//   门店当天卖了 8.383kg ⇒ 账面差 3.311kg ⇒ **但总量够**(进货 181.7 ≥ 卖出 178.1)
	//   ⇒ 这是**分摊口径**问题,不是真短卖 ⇒ 不该传递到下一天。
	CutShortSellKg decimal.Decimal `gorm:"column:cut_short_sell_kg;type:decimal(14,3);not null;default:0" json:"cut_short_sell_kg"`
	// ShortSellKg 是**当日总量**短卖(kg)—— 只有它才算"短卖"(owner 2026-10-30)。
	//
	// = max(0, Σ(期初+进货) − Σ(卖出+统收+报损) − Σ期末)
	// ⚠️ 生产实测 店 00:10-29 = 0(总量够);10-30 = 62.512(账面只剩 20.9kg 却卖了 83.4kg)。
	// ⇒ >0 ⇒ ① 那部分货**必须计成本**(否则销售成本 = 0 ⇒ 毛利虚高)
	//   ② 结转给下一天(累计短卖)。
	//
	// ⚠️ 2026-11-01 owner：**短卖拦截规则废止**
	//   (原「任何一日出现短卖都不允许审批结算」—— 它的拦截点随审计端点一起下线)
	//   ⇒ 不再拦结算、不再描红 ⇒ 这里只留**成本**与**传递**两件事。
	ShortSellKg decimal.Decimal `gorm:"column:short_sell_kg;type:decimal(14,3);not null;default:0" json:"short_sell_kg"`
	// CarriedShortSellKg 是**从上一期结转过来**的短卖(kg)—— 累计口径。
	CarriedShortSellKg decimal.Decimal `gorm:"column:carried_short_sell_kg;type:decimal(14,3);not null;default:0" json:"carried_short_sell_kg"`

	// MovingAvgUnitCost 是该部位的移动加权平均成本(元/kg)。
	MovingAvgUnitCost decimal.Decimal `gorm:"column:moving_avg_unit_cost;type:decimal(16,4);not null;default:0" json:"moving_avg_unit_cost"`
	// SalesCostYuan / BulkCostYuan / WasteCostYuan 是本期成本三块。
	SalesCostYuan decimal.Decimal `gorm:"column:sales_cost_yuan;type:decimal(16,2);not null;default:0" json:"sales_cost_yuan"`
	BulkCostYuan  decimal.Decimal `gorm:"column:bulk_cost_yuan;type:decimal(16,2);not null;default:0" json:"bulk_cost_yuan"`
	WasteCostYuan decimal.Decimal `gorm:"column:waste_cost_yuan;type:decimal(16,2);not null;default:0" json:"waste_cost_yuan"`
	// RevenueYuan / GrossMarginYuan / GrossMarginPct 是损益三件套。
	RevenueYuan     decimal.Decimal `gorm:"column:revenue_yuan;type:decimal(16,2);not null;default:0" json:"revenue_yuan"`
	GrossMarginYuan decimal.Decimal `gorm:"column:gross_margin_yuan;type:decimal(16,2);not null;default:0" json:"gross_margin_yuan"`
	GrossMarginPct  float64         `gorm:"column:gross_margin_pct;type:numeric(8,4);not null;default:0" json:"gross_margin_pct"`
	// PresaleMarginYuan / PresaleExpectedRevenueYuan 是**预期毛利(预售结转)**。
	PresaleMarginYuan          decimal.Decimal `gorm:"column:presale_margin_yuan;type:decimal(16,2);not null;default:0" json:"presale_margin_yuan"`
	PresaleExpectedRevenueYuan decimal.Decimal `gorm:"column:presale_expected_revenue_yuan;type:decimal(16,2);not null;default:0" json:"presale_expected_revenue_yuan"`

	// DataSource:actual(有完整实盘) | partial(有盘点但未完成) | estimated(无盘点)
	DataSource string `gorm:"column:data_source;type:varchar(16);not null;default:'estimated'" json:"data_source"`
	// CostUncomputable = true ⇒ 该部位当天**成本算不出来**
	// (既没有期初库存、当天也没有进货、又没有任何可用单价)。
	//
	// ⚠️ **这个标记是链的"断点说明"**(2026-10-27 生产实测):
	//   这样的部位,期末金额 = 期末 kg × 单价 0 = **0** ——
	//   而 0 **不是**它的真实价值,是"不知道"。
	//   若下一日把 0 当成上日期末接过来 ⇒ 可售金额被抹平、
	//   可售数量被压小 ⇒ **移动加权单价爆炸** ⇒ 成本变成天文数字
	//   (实测:10-02 的成本从 3061.57 变成 10151.24,毛利率 −191%)。
	//
	// ⇒ 读前一日快照接链时,**必须跳过**标了这个的部位
	//   ⇒ 链在那个部位上自然断开(它本来就没有成本链)⇒ 数字安全。
	CostUncomputable bool      `gorm:"column:cost_uncomputable;not null;default:false" json:"cost_uncomputable"`
	CreatedAt        time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (CutCostDaily) TableName() string { return "cut_cost_daily" }

// ---- 表9c:cut_cost_states(部位级成本状态)----
//
// CutCostState 某店某**部位**的移动加权成本状态(当日有效)。
//
// ⚠️ 这是**部位级**的"账" —— 昨天的单价就是今天的期初金额基准。
//
//	(旧的 `sku_cost_states` 是 SKU 级 ⇒ 多货号部位只有一个货号有单价,
//	 另一个货号卖出时移动加权单价恒为 0 ⇒ 零成本 ⇒ 毛利率 100%。)
type CutCostState struct {
	// ID 是**确定性**代理主键(surrogateID("CCS", branch_id, cut_type))。
	ID       string  `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string  `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_cut_cost_state,priority:1" json:"branch_id"`
	CutType  CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_cut_cost_state,priority:2" json:"cut_type"`
	// QtyKg / AmountYuan 是当前结余(kg / 元)。
	QtyKg             decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null;default:0" json:"qty_kg"`
	AmountYuan        decimal.Decimal `gorm:"column:amount_yuan;type:decimal(16,2);not null;default:0" json:"amount_yuan"`
	MovingAvgUnitCost decimal.Decimal `gorm:"column:moving_avg_unit_cost;type:decimal(16,4);not null;default:0" json:"moving_avg_unit_cost"`
	LastBizDate       datatypes.Date  `gorm:"column:last_biz_date;not null" json:"last_biz_date"`
}

// TableName 显式表名。
func (CutCostState) TableName() string { return "cut_cost_states" }

// ---- 表10:bulk_products ----

// BulkProduct 大宗品项登记表(冷库里"不是零售、攒着统一结算"的东西)。
//
// 典型:板油 / 猪血 / 整根猪肠。
//
// ⚠️ 大宗品项**必须挂一个部位**(`cut_type`):统收结算的收入要落回"产出它的部位",
// 否则那块 kg 的毛利永远是 -100%(有成本没收入)。
type BulkProduct struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null" json:"branch_id"`
	// Code 是业务标识。
	//
	// ⚠️ 2026-10-24(门店口径):大宗品项**只针对部位**,用户不填编码 ⇒
	// 服务端自动填 `code = cut_type`(如 fat)。
	Code string `gorm:"column:code;type:varchar(32);not null" json:"code"`
	// Name 是展示名。2026-10-24 起同样由服务端按部位自动填(部位中文名)。
	Name string `gorm:"column:name;type:varchar(64);not null" json:"name"`
	// CutType 是这个大宗品项对应的**部位**。
	//
	// ⚠️ 2026-10-23:同一部位**不能**登记两个大宗品项
	// (门店要求"从部位里选、不可重复")。
	// 否则会出现"板油"和"猪油"两个品项都挂 fat ⇒ 结存与结算各算一半、说不清。
	// ⇒ 唯一性由 service 层 + DB 约束保证,见 migrate.go。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`
	Unit    string  `gorm:"column:unit;type:varchar(16);not null;default:'kg'" json:"unit"`
	// PosCubeProductID 是**大宗线的 POS 货号**（2026-10-25，门店口径）。
	//
	// ⚠️ **门店不填**（登记大宗品项时只选部位）⇒ 由**装配**(seed)写入。
	//
	// 它只做一件事：**这个货号在 POS 上卖时，不按零售算** ⇒
	// 大宗线（如 `03808 水油`，每 9 天一笔 400~870kg）**不自动进鲜肉域的收入**，
	// 改由门店在「**统收结算**」里**手工登记**（领用冷库重量 + 记收入）。
	//
	// # 为什么必须这样（2026-10-25 拍板）
	//
	// 大宗那笔是**单笔几百公斤**的 POS 行 ⇒ 若按零售算：
	//   - 收入归到「当日某一头猪」⇒ **那头猪毛利虚高**；
	//   - 而它其实是**攒着卖给酒店**的货 ⇒ 口径本来就该走大宗。
	//
	// ⇒ 「两条路互斥」：POS 那条**不收**，只认手工统收。
	// ⇒ 安全网：冷库里那批货还挂着 ⇒ 结存页的「**可领用**」会一直显示它
	//   ⇒ 门店看结存就知道「还有 N kg 没结」。
	//
	// ⚠️ 空 = 没有大宗线的 POS 货号 ⇒ 该部位的 POS 销售照常按零售算。
	PosCubeProductID string    `gorm:"column:pos_cube_product_id;type:varchar(64);index" json:"pos_cube_product_id,omitempty"`
	Note             string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	CreatedAt        time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (BulkProduct) TableName() string { return "bulk_products" }

// ---- 表11:bulk_productions ----

// BulkProduction 大宗品项的**当日产出**(进冷库)。
//
// 例:当天分割出的板油 8kg —— 它不进猪肉台(不零售),直接进冷库,
// 累积几天后由酒店统收(见 BulkSettlement)。
//
// ⚠️ **不指定猪**(2026-10-23 门店决策:录入最省事)。
// 猪归属在**结算时**按当日各猪 `gross_weight_kg` 比例分摊(见 service/bulk.go)。
type BulkProduction struct {
	ID          string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID    string          `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_bulk_prod_branch_day,priority:1" json:"branch_id"`
	ProductCode string          `gorm:"column:product_code;type:varchar(32);not null;uniqueIndex:idx_bulk_prod_branch_day,priority:2" json:"product_code"`
	BizDate     datatypes.Date  `gorm:"column:biz_date;not null;uniqueIndex:idx_bulk_prod_branch_day,priority:3" json:"biz_date"`
	QtyKg       decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null" json:"qty_kg"`
	// SettledQtyKg 是已被结算吃掉的量(结存 = qty_kg − settled_qty_kg)。
	SettledQtyKg decimal.Decimal `gorm:"column:settled_qty_kg;type:decimal(14,3);not null;default:0" json:"settled_qty_kg"`
	// Source 是这条产出的来源(2026-10-23):
	//
	//	stocktake —— **日终盘点自动结转**(主路径,门店不用手工录)
	//	manual    —— 手工兜底(盘点没录该部位时)
	//
	// 同一天同品项只允许**一条** source=stocktake ⇒ 手工录与自动结转互斥。
	//
	// ⚠️ 它**在唯一索引里**(priority 4):手工兜底与自动结转是**两条不同的行**,
	// 若不参与唯一键,自动结转会把手工那条覆盖掉(或反之)⇒ 数据静默丢失。
	Source string `gorm:"column:source;type:varchar(16);not null;default:'manual';uniqueIndex:idx_bulk_prod_branch_day,priority:4" json:"source"`
	// CubeProductID 是该大宗品项**部位的主 SKU**(2026-10-23)。
	//
	// ⚠️ 2026-10-23 修正:大宗库存与收入原来记在伪 SKU `bulk:<code>` 上,
	// 而伪 SKU 不在部位映射里 ⇒ **没有入库金额** ⇒ 移动加权单价恒为 0
	// ⇒ 统收成本算成 0 ⇒ 卖板油的毛利率是 **100%**(实测:卖 10kg 收 60 元,
	// 成本 0、毛利 +60;真实成本 1000 元、毛利应是 −940)。
	// ⇒ 库存与收入都必须记在**真实部位 SKU** 上,成本才匹配得上。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);index" json:"cube_product_id,omitempty"`
	// UnitCostYuan 是**入冷库时**快照的移动加权单价(元/kg)—— 这就是"记录分摊成本"。
	//
	// 冷库里的货会压好几天(每 9 天才卖给酒店一次),若结算时用**当日**单价,
	// 成本会随大盘漂移 ⇒ 快照让"这批 kg 当时值多少"可审计。
	UnitCostYuan decimal.Decimal `gorm:"column:unit_cost_yuan;type:decimal(14,4);not null;default:0" json:"unit_cost_yuan"`
	Note         string          `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	OperatorID   string          `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	CreatedAt    time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (BulkProduction) TableName() string { return "bulk_productions" }

// ---- 表12:bulk_settlements ----

// BulkSettlement 酒店(统购方)统收结算单(**交付离店**口径)。
//
// 门店把冷库里攒了几天的板油等大宗品项一次性交给酒店,
// **几天后才结账**、才知道单价 ⇒ 收入在结算时才确定。
//
// Mode 语义:
//   - delivery(交付离店):结算 = 收入,库存减少(默认)
//   - price_diff(价差结算):货已零售,只结价差(库存不变)—— 备用口径
//
// BulkSettlement 酒店(统购方)统收**结算单表头**(2026-10-23 改成表头+明细)。
//
// # 为什么改成表头 + 明细
//
// 门店原话(2026-10-23):「整个统收结算设计为表头,明细模式,
// 表头有结算日期、对方、备注,明细为大宗品项、结算数量、单价」。
//
// 原来一单只有**一个**品项 ⇒ 一次交酒店两种油就得开两单、金额也散着。
//
// # 表头字段
//
//	ID          单号(结算单号,列表里展示的就是它)
//	SettleDate  结算日期
//	Counterparty 对方(酒店 / 购货方)
//	TotalAmountYuan 总金额 = Σ 明细金额
//	Note        备注
//
// ⚠️ 老字段(product_code / from_date / to_date / qty_kg / unit_price_yuan /
// amount_yuan)是**明细粒度**,2026-10-23 起**废弃**:
// 改成**可空**而不是 DROP 列 ——
// GORM 的 AutoMigrate 不会删列,而 NOT NULL 又会让新 INSERT 缺列直接失败。
// ⇒ 保留列、放宽可空、标注废弃(历史数据仍在,可回查)。
type BulkSettlement struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	// SettleDate 是**结算日期**。
	//
	// ⚠️ 存的是 datatypes.Date(列类型 date),但 **API 输出用 "YYYY-MM-DD" 文本**
	// (见 service.BulkSettlementView.SettleDate)——
	// datatypes.Date 的 JSON 是**完整时间戳**,前端直接显示会不对。
	SettleDate datatypes.Date `gorm:"column:settle_date;not null;index" json:"-"`
	// TotalAmountYuan 是总金额(= Σ 明细金额),列表里直接展示。
	TotalAmountYuan decimal.Decimal `gorm:"column:total_amount_yuan;type:decimal(16,2);not null;default:0" json:"total_amount_yuan"`
	Counterparty    string          `gorm:"column:counterparty;type:varchar(64)" json:"counterparty,omitempty"`
	Mode            string          `gorm:"column:mode;type:varchar(16);not null;default:'delivery'" json:"mode"`
	Note            string          `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	SettledAt       time.Time       `gorm:"column:settled_at;not null" json:"settled_at"`
	OperatorID      string          `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	CreatedAt       time.Time       `gorm:"column:created_at;not null" json:"created_at"`

	// ---- 以下为 2026-10-23 之前的明细粒度字段,已废弃(可空,不再写入)----
	// ⚠️ 保留列是为了历史数据可回查;GORM 的 AutoMigrate 不会删列,
	// 而 NOT NULL 会让新 INSERT 缺列直接失败 ⇒ 只能"放宽可空"。
	DeprecatedProductCode   string           `gorm:"column:product_code;type:varchar(32)" json:"-"`
	DeprecatedFromDate      *datatypes.Date  `gorm:"column:from_date" json:"-"`
	DeprecatedToDate        *datatypes.Date  `gorm:"column:to_date" json:"-"`
	DeprecatedQtyKg         *decimal.Decimal `gorm:"column:qty_kg" json:"-"`
	DeprecatedUnitPriceYuan *decimal.Decimal `gorm:"column:unit_price_yuan" json:"-"`
	DeprecatedAmountYuan    *decimal.Decimal `gorm:"column:amount_yuan" json:"-"`
}

// TableName 显式表名。
func (BulkSettlement) TableName() string { return "bulk_settlements" }

// ---- 表12b:bulk_settlement_items ----

// BulkSettlementItem 结算单的**一条明细**:哪个大宗品项、多少 kg、什么单价。
//
// 一张单可以有多条明细(一次把板油和水油一起交给酒店)。
type BulkSettlementItem struct {
	ID           string          `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID     string          `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	SettlementID string          `gorm:"column:settlement_id;type:varchar(64);not null;index" json:"settlement_id"`
	ProductCode  string          `gorm:"column:product_code;type:varchar(32);not null" json:"product_code"`
	QtyKg        decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null" json:"qty_kg"`
	// UnitPriceYuan 是这条明细的成交单价(元/kg)。
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:decimal(16,4);not null" json:"unit_price_yuan"`
	// AmountYuan = qty_kg × unit_price_yuan。
	AmountYuan decimal.Decimal `gorm:"column:amount_yuan;type:decimal(16,2);not null" json:"amount_yuan"`
	CreatedAt  time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (BulkSettlementItem) TableName() string { return "bulk_settlement_items" }

// ---- 表13:bulk_settlement_allocations ----

// BulkSettlementAllocation 结算量回溯到具体产出记录(以及具体猪)。
//
// 这是"结算收入能落到猪身上"的凭据:每条 allocation 记着
// 哪条产出记录贡献了多少 kg、摊到多少金额、归到哪头猪。
type BulkSettlementAllocation struct {
	ID           string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	SettlementID string `gorm:"column:settlement_id;type:varchar(64);not null;index" json:"settlement_id"`
	// ItemID 是这条分摊来自结算单的**哪一条明细**(2026-10-23 表头/明细改造后才有)。
	//
	// ⚠️ 可空:老数据(单品项时代)没有它。
	ItemID       string `gorm:"column:item_id;type:varchar(64);index" json:"item_id,omitempty"`
	ProductionID string `gorm:"column:production_id;type:varchar(64);not null;index" json:"production_id"`
	// BagID 是这条分摊吃掉的**结转记录(袋子)**的 ID(2026-10-25)。
	//
	// ⚠️ 为什么要有它:大宗结算的 FIFO 队列**就是袋子队列**(2026-10-25 口径)
	// ⇒ 「结算领用了哪几袋、每袋多少」必须能回溯,否则"领用"与"袋子"对不上,
	//   同一批货可能被零售再卖一遍(卖两遍)。
	//
	// ⚠️ 老数据(台账时代)没有它 ⇒ 为空 ⇒ 那时扣的是 bulk_productions。
	BagID string `gorm:"column:bag_id;type:varchar(64);index" json:"bag_id,omitempty"`
	// Location 是领用时这批货所在的**位置**(cold_store|counter)—— 2026-10-25。
	//
	// ⇒ 结算后要按位置扣库存(冷库/柜台各扣各的)⇒ 必须记下来。
	Location       string          `gorm:"column:location;type:varchar(16)" json:"location,omitempty"`
	CubeProductID  string          `gorm:"column:cube_product_id;type:varchar(64)" json:"cube_product_id,omitempty"`
	CutType        CutType         `gorm:"column:cut_type;type:varchar(32);not null" json:"cut_type"`
	PigID          string          `gorm:"column:pig_id;type:varchar(64)" json:"pig_id,omitempty"`
	QtyKg          decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null" json:"qty_kg"`
	AmountYuan     decimal.Decimal `gorm:"column:amount_yuan;type:decimal(16,2);not null" json:"amount_yuan"`
	ProductionDate datatypes.Date  `gorm:"column:production_date;not null" json:"production_date"`
	CreatedAt      time.Time       `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (BulkSettlementAllocation) TableName() string { return "bulk_settlement_allocations" }

// ---- 表14:pig_forecast_baselines ----

// PigForecastBaseline 分割重量的**统计基线**。
//
// # 为什么不用 LLM 做这件事
//
// "300kg 的猪能出多少五花"是**数值回归**问题。LLM 给的是语言先验,不是从样本拟合;
// 而品种/膘情/季节/师傅手法是**结构化特征**,该用统计/回归。
// ⇒ **统计基线优先,LLM 降级为"没有历史样本的冷启动兜底"。**
//
// # 口径
//
// 样本 = (whole_pig 的毛重, pig_cuts 的各部位 kg)。
// 分层 = **体重段**(kg,50 一段)× 部位。
// 统计 = n / mean / p10 / p50 / p90。
//
// ⚠️ 样本数不足(n < MinSamples)时**不用**该基线 ⇒ 回退 LLM。
//
// # 2026-10-30 owner 选的 A 方案:两个**样本池**
//
//	① **实录池** = `pig_cuts`（门店手工补录的分割）—— 最准;
//	② **估计池** = `pig_cut_estimates`（由日终倒推值按毛重比例摊到每头猪的估计）—— 近似。
//
// ⇒ 口径:**实录样本够(≥ MinForecastSamples)就用实录**,不够才用估计。
// ⇒ 用了哪个池子记在 [SampleSource] 里(前端能看出"这条建议是学来的还是录来的")。
type PigForecastBaseline struct {
	ID         string  `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID   string  `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_baseline,priority:1" json:"branch_id"`
	WeightBand string  `gorm:"column:weight_band;type:varchar(16);not null;uniqueIndex:idx_pig_baseline,priority:2" json:"weight_band"`
	CutType    CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_baseline,priority:3" json:"cut_type"`
	// SampleCount 是**实际采用**的那个样本池的样本数(= 可用性判据,见 service.ForecastBaseline.Usable)。
	SampleCount int `gorm:"column:sample_count;not null;default:0" json:"sample_count"`
	// RecordedSampleCount / EstimatedSampleCount 是两个池子**各自**的样本数(留痕用)。
	RecordedSampleCount  int `gorm:"column:recorded_sample_count;not null;default:0" json:"recorded_sample_count"`
	EstimatedSampleCount int `gorm:"column:estimated_sample_count;not null;default:0" json:"estimated_sample_count"`
	// SampleSource:recorded | estimated —— 这条基线**实际采用**的是哪个池子。
	SampleSource string `gorm:"column:sample_source;type:varchar(16);not null;default:'recorded'" json:"sample_source"`
	// SourceBranchID 是这条基线**实际采用的样本来源门店**。
	//
	// ⚠️ 本店自产 = 本店；跨店借用 = 被借方（见 [PigForecastBaselineBorrow]）。
	SourceBranchID string `gorm:"column:source_branch_id;type:varchar(64);not null;default:''" json:"source_branch_id"`
	// ManualGenerated = true ⇒ 这条基线是管理员**按某一天手动生成**的
	// （owner 2026-10-30:「只要那天的数据录入、搬台（含0搬台）、销售、盘点都全就可以」）
	// ⇒ **绕过 MinForecastSamples(5) 的可用性门槛**（见 service.ForecastBaseline.Usable）。
	ManualGenerated bool `gorm:"column:manual_generated;not null;default:false" json:"manual_generated"`
	// GeneratedFromDate 是手动生成时**用的那一天**（YYYY-MM-DD）；空 = 不是手动生成。
	//
	// ⚠️ 用 **varchar** 而不是 datatypes.Date：可选日期用日期类型会落成
	//   0001-01-01（PG 上 date 类型的零值）⇒ 读出来像"公元 1 年"。
	GeneratedFromDate string `gorm:"column:generated_from_date;type:varchar(10);not null;default:''" json:"generated_from_date"`
	// Borrowed = true ⇒ 这一行是**重算时自动借来的**（来源是别的门店，见 SourceBranchID）。
	//
	// # ⚠️ 为什么单独一列，而不是靠 `manual_generated` / `source_branch_id` 推断
	//
	//	有**两种**跨店行，光看 source_branch_id 分不出来：
	//	  ① **自动借用**：重算时按借用配置搬过来的 ⇒ 借用一关就该消失；
	//	  ② **管理员手动跨店选用**：管理员挑了别店某一天生成 ⇒ **永远不许**被覆盖/删。
	//	而 ① 的来源行可能本身就是手动生成的（manual_generated=true）
	//	⇒ 借来的行会**继承** manual_generated ⇒ 靠它判断"管理员指定的"就分不出来了
	//	⇒ 借来的行会变成"钉死"的：既不会被刷新，也不会在关掉借用后消失
	//	⇒ "关掉借用就回到只用本店样本"这条语义就废了。
	//
	//	⇒ 用这一列把 ① 标出来：借用行永远可刷新、可清理；手动行永远受保护。
	Borrowed   bool            `gorm:"column:borrowed;not null;default:false" json:"borrowed"`
	MeanKg     decimal.Decimal `gorm:"column:mean_kg;type:decimal(10,3);not null;default:0" json:"mean_kg"`
	P10Kg      decimal.Decimal `gorm:"column:p10_kg;type:decimal(10,3);not null;default:0" json:"p10_kg"`
	P50Kg      decimal.Decimal `gorm:"column:p50_kg;type:decimal(10,3);not null;default:0" json:"p50_kg"`
	P90Kg      decimal.Decimal `gorm:"column:p90_kg;type:decimal(10,3);not null;default:0" json:"p90_kg"`
	ComputedAt time.Time       `gorm:"column:computed_at;not null" json:"computed_at"`
}

// TableName 显式表名。
func (PigForecastBaseline) TableName() string { return "pig_forecast_baselines" }

// ---- 表16:pig_cut_estimates(分割的**估计样本**) ----
//
// PigCutEstimate 分割的**估计样本**（2026-10-30 owner 选的 A 方案）。
//
// # 它是什么
//
//	当天各部位的**倒推**实际分割(全店恒等式解出来的"今日新分割"),
//	按当日各猪的**毛重比例**摊到每头猪 ⇒ 每头猪每个部位一条估计值。
//	⇒ 用来喂「分割建议基线」(让建议分割学到**本店真实出肉率**,而不是 LLM 的通用先验)。
//
// # 为什么**单独一张表**(不写进 pig_cuts)
//
//	① `pig_cuts` 同时是「**按猪摊成本**」的输入(moving_average.go::computeProducedBySku)
//	   ⇒ 把估计值混进去 ⇒ **污染 FIFO 摊销** ⇒ 成本跟着错;
//	② `pig_cuts` 挂在**具体某头猪**上 ⇒ 门店删猪时会被"有补录"锁死(pig_delete.go);
//	③ 估计值是**部位级总量按比例摊出来的近似** ⇒ 不是门店手工录的事实。
//
// ⇒ 估计样本只喂基线,**不碰**成本与删猪。
//
// # 门店隔离
//
//	唯一键含 branch_id ⇒ **每店一份样本** ⇒ 每店生成**自己的**基线。
type PigCutEstimate struct {
	// ID 是确定性代理键(surrogateID("PCE", branch_id, pig_id, cut_type))
	// ⇒ 同一 (店, 猪, 部位) 只有一行 ⇒ 重建 = 覆盖。
	ID       string  `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string  `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_estimate,priority:1" json:"branch_id"`
	PigID    string  `gorm:"column:pig_id;type:varchar(64);not null;uniqueIndex:idx_pig_estimate,priority:2" json:"pig_id"`
	CutType  CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_estimate,priority:3" json:"cut_type"`
	// WeightKg 是这一头猪这个部位的**估计**分割量(kg)。
	WeightKg decimal.Decimal `gorm:"column:weight_kg;type:decimal(10,3);not null;default:0" json:"weight_kg"`
	// WeightBand 是这头猪的体重段(冗余:分桶时直接用,不用再 join whole_pigs)。
	WeightBand string `gorm:"column:weight_band;type:varchar(16);not null;index" json:"weight_band"`
	// BizDate 是这条估计所属的**营业日**(倒推那天)。
	BizDate datatypes.Date `gorm:"column:biz_date;not null;index" json:"biz_date"`
	// Source 是估计来源(目前只有 stocktake_derived = 由日终倒推摊出来的)。
	Source string `gorm:"column:source;type:varchar(32);not null;default:'stocktake_derived'" json:"source"`
	// ExcludedAt 是**墓碑时间**(2026-10-30 owner 拍板:删除 = 打墓碑,不是物理删)。
	//
	// # ⚠️ 为什么必须打墓碑
	//
	//	引擎重跑那天的成本链会把样本**重新写回来**
	//	⇒ 物理删掉的样本会在下一次重建时**复活** ⇒ "删除"等于没删。
	//
	// ⇒ 非空 = 这条样本已被人工从样本库剔除:
	//	  ① **不参与拟合**(读样本时过滤);
	//	  ② **不被重建覆盖**(重建那天的样本时跳过它,也不清墓碑);
	//	  ③ 可以**恢复**(见 service 的 restore 端点)。
	//
	// ⚠️ 可空列 ⇒ 老行不受影响(AutoMigrate 直接加列)。
	ExcludedAt *time.Time `gorm:"column:excluded_at" json:"excluded_at,omitempty"`
	// ExcludedReason 是删除原因留痕(目前只有 manual = 人工在样本库里删的)。
	ExcludedReason string    `gorm:"column:excluded_reason;type:varchar(32);not null;default:''" json:"excluded_reason,omitempty"`
	CreatedAt      time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (PigCutEstimate) TableName() string { return "pig_cut_estimates" }

// ---- 表17:pig_forecast_baseline_borrows(跨门店选用基线) ----
//
// PigForecastBaselineBorrow 「本店借用别店基线」的配置（2026-10-30 **预留**）。
//
// # 状态：**只建表 + 留模型，业务逻辑未实现**
//
//	owner 原话：「每个门店样本要单独，这样可以生成门店自己的分割建议基线，
//	并实现多门店分级基线对比和跨门店选用（可跟这次做 A 时顺便预留数据模型，
//	但先把具体实现列为待办，等我通知再做）」。
//
// ⇒ 本轮：模型就位（表 + [PigForecastBaseline.SourceBranchID] 落"实际采用的样本来源门店"）
//
//	⇒ 以后做"跨店选用"时**不用再改表结构**。
//
// # 语义（将来实现时按这个来）
//
//	BranchID       = 借用方（本店）
//	WeightBand/CutType = 哪个分层借用
//	SourceBranchID = 被借方（样本来源门店）
//	Enabled        = 关掉就回到"只用本店样本"
//
// # 门店隔离
//
//	唯一键 (branch_id, weight_band, cut_type) ⇒ 每店一份借用配置 ⇒ 互不影响。
type PigForecastBaselineBorrow struct {
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_borrow,priority:1" json:"branch_id"`
	// WeightBand / CutType 是借用哪个分层。
	WeightBand string  `gorm:"column:weight_band;type:varchar(16);not null;uniqueIndex:idx_pig_borrow,priority:2" json:"weight_band"`
	CutType    CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_pig_borrow,priority:3" json:"cut_type"`
	// SourceBranchID 是**被借方**（样本来源门店）。
	SourceBranchID string    `gorm:"column:source_branch_id;type:varchar(64);not null" json:"source_branch_id"`
	Enabled        bool      `gorm:"column:enabled;not null;default:false" json:"enabled"`
	CreatedAt      time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (PigForecastBaselineBorrow) TableName() string { return "pig_forecast_baseline_borrows" }

// ---- 表15:business_days(营业日状态机)----
//
// BusinessDay 某店某个**营业日**走到哪一步了。
//
// # 门店口径(2026-10-24)
//
// 「整个鲜肉板块都是围绕如何计算**每日真实毛利**展开的。倘若不知道前天不做日终盘点,
//
//	那就没办法从冷库搬上台继续售卖;倘若没录入今天的猪,那也无法售卖。」
//
// 「**不补录昨天,就无法录今天。**」
//
// ⇒ 每个营业日的循环是:「录入猪 + 搬上台 → 日终盘点 → 次日…」。
//
// # 状态阶梯
//
//	no_pig         未进猪
//	pig_in         已进猪
//	counter_loaded 已搬台
//	stocktaken     已盘点(售罄 sold_out | 休市 rest_day)
//
// ⚠️ 状态**由实际动作推进**(录猪 / 搬台 / 盘点时 upsert),**不让前端传**。
//
// # 状态与事实标志的关系
//
// Status 取"**走到最远的那一步**"(单调不回退);HasPig / HasCounterLoad /
// HasStocktake 是**事实标志**(某一天可能只搬了台没进猪 ⇒ status=counter_loaded
// 但 has_pig=false)。
type BusinessDay struct {
	// ID 是**确定性**代理主键(surrogateID("BD", branch_id, biz_date))
	// ⇒ 同一 (门店, 营业日) 永远只有一行 ⇒ 重复提交 = 覆盖。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_biz_day_branch_date,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_biz_day_branch_date,priority:2" json:"biz_date"`
	// Status 是状态阶梯上走到的那一步(见上)。
	Status string `gorm:"column:status;type:varchar(16);not null;default:'no_pig'" json:"status"`
	// HasPig:当天有猪进场(录猪动作推进)。
	HasPig bool `gorm:"column:has_pig;not null;default:false" json:"has_pig"`
	// HasCounterLoad:当天有搬上台流水(按 SKU+kg 搬)。
	HasCounterLoad bool `gorm:"column:has_counter_load;not null;default:false" json:"has_counter_load"`
	// HasStocktake:当天提交过盘点单。
	HasStocktake bool `gorm:"column:has_stocktake;not null;default:false" json:"has_stocktake"`
	// StocktakeID 是当天那张盘点单的主键(可空)。
	StocktakeID string `gorm:"column:stocktake_id;type:varchar(64)" json:"stocktake_id,omitempty"`
	// SoldOut:当天盘点『售罄』(一点剩余都没有)。
	SoldOut bool `gorm:"column:sold_out;not null;default:false" json:"sold_out"`
	// RestDay:当天盘点『鲜肉休市』(既没进猪也没搬台)。
	RestDay bool `gorm:"column:rest_day;not null;default:false" json:"rest_day"`
	// PigCount / CounterLoadKg 是当天的流水计数(给前端"走到哪一步"用)。
	PigCount      int             `gorm:"column:pig_count;not null;default:0" json:"pig_count"`
	CounterLoadKg decimal.Decimal `gorm:"column:counter_load_kg;type:decimal(14,3);not null;default:0" json:"counter_load_kg"`
	CreatedAt     time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt     time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (BusinessDay) TableName() string { return "business_days" }

// ---- 表16:counter_loads(搬台流水)----
//
// CounterLoadRecord 一次**搬上台**动作的一条流水。
//
// # 为什么必须有它(2026-10-24)
//
// 「位置表只记余额、不留痕」⇒ 「当日搬上台了多少」查不到
// ⇒ 而休市判定、当日分摊重量、按营业日推导的冷库余额**都要它**。
//
// ⇒ **只有一种搬法**(2026-11-02 owner:标签号玩法下线):按 **SKU + kg** 搬。
//
// (历史行里还可能有 source='bag'(按标签整袋搬),那套玩法已下线。)
//
// # 归属日(BizDate)
//
// 搬台是**有归属日**的动作(与盘点一致)⇒ 补录往期的搬台记在**那一天**,
// 冷库也从那一天扣(而不是从今天扣)。
type CounterLoadRecord struct {
	// ID 是**确定性**代理主键(surrogateID("CLD", branch_id, biz_date, seq))
	// ⇒ 同一 (门店, 归属日, 第几行) 永远只有一行 ⇒ 重复提交 = 覆盖。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_counter_load_branch_date_seq,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_counter_load_branch_date_seq,priority:2" json:"biz_date"`
	// Seq 是这次提交里的**行序号**(0 起)⇒ 同一归属日重复提交同一份 = 覆盖同一批行。
	Seq int `gorm:"column:seq;not null;uniqueIndex:idx_counter_load_branch_date_seq,priority:3" json:"seq"`
	// CubeProductID 是搬上柜台的 SKU(= 思迅货号)。
	//
	// ⚠️ SKU 只是**溯源属性**(那一袋实物是哪个货号);**库存/结余按部位记**
	//   (门店口径:「这个模块只管部位数据,不管 SKU」)⇒ 部位见 CutType。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;index" json:"cube_product_id"`
	// CutType 是搬上柜台的**部位**(SKU 映射 ⇒ 部位)。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null;index" json:"cut_type"`
	// QtyKg 是搬上柜台的重量。
	QtyKg decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null" json:"qty_kg"`
	// ⚠️ **label 列已废弃**(2026-11-02 owner:标签号玩法下线)⇒ 代码不再读写它。
	//   列保留(历史行仍有值)⇒ 迁移不动。
	// Source:sku(按 SKU+kg 搬)。历史行里还可能有 bag(按标签整袋搬,已下线)。
	Source     string    `gorm:"column:source;type:varchar(16);not null;default:'sku'" json:"source"`
	OperatorID string    `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	CreatedAt  time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (CounterLoadRecord) TableName() string { return "counter_loads" }

// ---- 表17:cut_stock_daily(按营业日的结余快照)----
//
// CutStockDaily 某个营业日**日终**某个**部位**的结余。
//
// # 粒度:部位(不是 SKU)(2026-10-24 门店口径)
//
// 「"鲜肉"这个模块,只管部位数据,不管 SKU;SKU 映射只是为了聚合销售、数量等数据
//
//	到其部位……日终盘点也是记录部位。库存,大宗结算都是部位。」
//
// ⇒ 库存/结余一律按**部位**记 ⇒ 每日毛利(按部位)天然对得上。
//
// # 口径
//
// CounterKg   日终**柜台**余额(夜里"过夜搬运"之后 ⇒ 0)
// ColdStoreKg 日终**冷库**余额(= 门店口径的「冷库余额」)
// TotalKg     全店结余(= 两者之和)
//
// ⇒ 审计结转时落这张表(= 冻结产物;审计前实时算、不落表)。
type CutStockDaily struct {
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_cut_stock_daily,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_cut_stock_daily,priority:2" json:"biz_date"`
	// CutType 是**部位**(五花肉 / 梅花肉 …)。
	CutType     CutType         `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_cut_stock_daily,priority:3" json:"cut_type"`
	CounterKg   decimal.Decimal `gorm:"column:counter_kg;type:decimal(14,3);not null;default:0" json:"counter_kg"`
	ColdStoreKg decimal.Decimal `gorm:"column:cold_store_kg;type:decimal(14,3);not null;default:0" json:"cold_store_kg"`
	TotalKg     decimal.Decimal `gorm:"column:total_kg;type:decimal(14,3);not null;default:0" json:"total_kg"`
	CreatedAt   time.Time       `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time       `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (CutStockDaily) TableName() string { return "cut_stock_daily" }

// ---- 表18:sku_stock_openings(期初结转)----
//
// SkuStockOpening 某个营业日**期初**的冷库余额(**按部位**)。
//
// # 为什么按部位(2026-10-24 门店口径)
//
// 「期初结转做成按部位,这维持每日毛利计算每个部位的准确性」
// ⇒ 每日毛利是**按部位**算的 ⇒ 期初也按部位录 ⇒ 部位口径始终对得上。
//
// # 为什么需要期初
//
// 「按营业日推导的冷库余额」是**链式**的:
//
//	冷库(D) = 冷库(D−1) + 结转(D) − 搬上台(D) − 大宗出库(D) + 大宗进库(D)
//
// ⇒ 链的**起点**(期初)必须由门店给 —— 上线前冷库里已经有货,
// 而那些货不在本系统的流水里 ⇒ 期初 = 0 会让历史日的冷库偏低。
//
// ⇒ 「鲜肉-设置」里录一次期初结转 ⇒ 链从那天起算 ⇒ 每一天的余额都对得上。
//
// ⚠️ 生效日(BizDate)= 期初余额**所属的那一天**(链从这天的期初开始)。
type SkuStockOpening struct {
	// ID 是**确定性**代理主键(surrogateID("SSO", branch_id, cut_type))。
	ID       string `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_sku_opening_branch_cut,priority:1" json:"branch_id"`
	// CutType 是期初的**部位**(五花肉 / 梅花肉 …)⇒ 与每日毛利的部位口径一致。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_sku_opening_branch_cut,priority:2" json:"cut_type"`
	// ColdStoreKg 是期初的**冷库**余额(kg)。柜台期初恒为 0(开市前柜台是空的)。
	ColdStoreKg decimal.Decimal `gorm:"column:cold_store_kg;type:decimal(14,3);not null;default:0" json:"cold_store_kg"`
	// UnitPriceYuan 是期初的**单价**(元/kg)—— **门店给**(2026-10-25)。
	//
	// ⚠️ 为什么必须门店给:期初是**上线前就在冷库里的货**,它的**成本**不在本系统的
	// 流水里(本系统 2026-10 才有进猪/搬台流水)⇒ 成本链的第一天若按"倒算的移动
	// 加权单价"取,多半是 **0** ⇒ 那批货**零成本** ⇒ 毛利虚高(卖出去全是利润)。
	// ⇒ 门店盘冷库时知道自己这批货当时**多少钱一斤** ⇒ 让他填。
	UnitPriceYuan decimal.Decimal `gorm:"column:unit_price_yuan;type:decimal(14,4);not null;default:0" json:"unit_price_yuan"`
	// TotalAmountYuan 是**总价**(元) = 重量 × 单价 ⇒ **服务端算,不可填**。
	//
	// ⚠️ 落库而不是每次现算:它是**审计口径的快照**(审计后不可改)，
	// 现场算会跟着重量/单价的后续修改漂移。
	TotalAmountYuan decimal.Decimal `gorm:"column:total_amount_yuan;type:decimal(16,2);not null;default:0" json:"total_amount_yuan"`
	// BizDate 是期初生效日(链从这一天开始)。
	BizDate datatypes.Date `gorm:"column:biz_date;not null" json:"biz_date"`
	// Note 是备注(如"上线期初")。
	Note      string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (SkuStockOpening) TableName() string { return "sku_stock_openings" }

// ---- 表19:biz_audits(审计结转)----
//
// ⚠️ **2026-11-01 owner：审计结转端点已下线**（`/stock/audit` 及其 precheck）。
//
//	模型与表**保留**，但语义收缩为**历史只读**：
//	① 余额链基线(`daily_balance.go` 的 chainBase / closingForDay)以最后一次审计截止日
//	   作为链的起点锚点；② 期初结转清理(`stock_openings.go`)用同一截止日当水位线。
//	⇒ 不会再有新行写入，冻结语义已随端点一起消失(写操作不再被"已冻结"拦截)。
//	⇒ 保留 AutoMigrate + purge 登记，是为了**别删掉老店的历史锚点**，
//	  同时让 purge 能把远古数据清干净。
//
// BizAudit 一次**审计结转**:把截止日期(含)及之前的营业日**冻结**成快照。
//
// # 门店口径(2026-10-24)
//
// 「审计则进行结转,审计指定截止日期;审计前,所有数据从上期结转开始,实时计算,
//
//	不快照,任何日期补录都会导致后续日期的数据发生变化;审计后,产生结转(即快照),
//	审计截止日期和之前的都不能再修改(包括补录);审计条件是截止日期和之前的每日
//	日终盘点都要有,否则不予以审计。」
//
// ⇒ 三段语义:
//
//	审计前:实时算(不快照)⇒ 补录往后 ⇒ **后续日期的毛利跟着变**(这是特性,不是 bug);
//	审计时:截止日期之前的每日日终盘点必须齐全 ⇒ 否则拒绝;通过 ⇒ **落快照(=结转)**;
//	审计后:截止日期及之前**冻结** ⇒ 不能再改(补录/改单/撤单都不行)。
//
// ⇒ 目的:① 实时计算的性能(链只从上次审计往后推);② 不让数据维护工作积压
//
//	(门店在审计前把该补的补完,审计后就冻结了)。
type BizAudit struct {
	// ID 是**确定性**代理主键(surrogateID("BA", branch_id, cutoff_date))。
	ID         string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID   string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_biz_audit_branch_cutoff,priority:1" json:"branch_id"`
	CutoffDate datatypes.Date `gorm:"column:cutoff_date;not null;uniqueIndex:idx_biz_audit_branch_cutoff,priority:2" json:"cutoff_date"`
	// AuditedAt 是审计发生的时间。
	AuditedAt  time.Time `gorm:"column:audited_at;not null" json:"audited_at"`
	OperatorID string    `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	// Note 是备注(如"月度结转")。
	Note      string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (BizAudit) TableName() string { return "biz_audits" }

// ---- 表20:cut_presale_settlements(预期毛利 / 预售结转)----
//
// ⚠️ **2026-11-01 owner：预期毛利(预售结转)端点已下线**（`/stock/presale`）。
//
//	模型与表**保留**，但语义收缩为**历史只读**：
//	账面期末重估(`cut_cost.go`)仍要读本表的 `expected_unit_yuan`
//	(把结余按预期售价重估，否则期末会退回成本口径、账面毛利失真)。
//	⇒ 不会再有新行写入(结转写路径 `settlePresale` / `writePigAllocations` 已随端点退役)；
//	  AutoMigrate + purge 登记保留，老店历史预售价继续参与重估。
//
// CutPresale 审计结转时某个 **SKU** 的「预期毛利(预售)」结转行。
//
// # 为什么需要(2026-10-24 门店口径)
//
// 「审计则进行结转……审计后,产生结转(即快照)」
// + 「本期产生结余,结余部分库存未进行毛利归属计算」
//
// ⇒ 审计日(D)的**结余**如果按**成本价**留在账上,它的毛利就永远没被归属:
//
//	下一期真卖出去时,毛利才出现 ⇒ **本期毛利被低估**、跨期归属错位。
//
// ⇒ 审计时按**预期销售单价**把这批结余**结转**掉(= 预售):
//
//	预期收入 = 结余 kg × 预期销售单价(有促销价按促销价)
//	结余成本 = 结余 kg × 当日移动加权单价(成本口径)
//	预售毛利 = 预期收入 − 结余成本
//
// ⇒ 这笔预售毛利**进审计当期的毛利**,同时把**下一期期初单价**改成预期销售单价
//
//	⇒ 下一期真按市价卖时,成本 = 预期售价 ⇒ 本期毛利 ≈ 0,差额落在下一期
//	⇒ **两期毛利之和 = 实际毛利**(不多算、不少算)。
//
// # 粒度:SKU
//
// 价格是 SKU 级的(同部位不同 SKU 单价不同)、成本引擎也是 SKU 级的
// ⇒ 结转行按 SKU 记;对外按**部位**汇总展示(门店口径:「这个模块只管部位数据」)。
type CutPresale struct {
	// ID 是**确定性**代理主键(surrogateID("CPS", branch_id, biz_date, cube_product_id))
	// ⇒ 同一 (门店, 归属日, SKU) 永远只有一行 ⇒ 重复结转 = 覆盖。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_cut_presale,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_cut_presale,priority:2" json:"biz_date"`
	// CubeProductID 是这行结转的 SKU(= 思迅货号)。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_cut_presale,priority:3" json:"cube_product_id"`
	// CutType 是这行结转的**部位**(五花肉 / 梅花肉 …)。
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null;index" json:"cut_type"`
	// QtyKg 是结转的重量(该 SKU 分到的日终结余)。
	QtyKg decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null;default:0" json:"qty_kg"`
	// CostUnitYuan 是**成本口径**单价(审计日的移动加权单价)。
	CostUnitYuan decimal.Decimal `gorm:"column:cost_unit_yuan;type:decimal(12,4);not null;default:0" json:"cost_unit_yuan"`
	// CostAmountYuan 是结余成本(= QtyKg × CostUnitYuan)。
	CostAmountYuan decimal.Decimal `gorm:"column:cost_amount_yuan;type:decimal(14,2);not null;default:0" json:"cost_amount_yuan"`
	// ExpectedUnitYuan 是**预期销售单价**(SKU 当前售价;有促销价按促销价)。
	//
	// ⚠️ 这是**下一期期初单价**的来源 ⇒ 它是"结转"这件事的**持久事实**,
	//   不能随重算漂移(重算只重算金额,单价以本列为准)。
	ExpectedUnitYuan decimal.Decimal `gorm:"column:expected_unit_yuan;type:decimal(12,4);not null;default:0" json:"expected_unit_yuan"`
	// ExpectedRevenueYuan 是预期收入(= QtyKg × ExpectedUnitYuan)。
	ExpectedRevenueYuan decimal.Decimal `gorm:"column:expected_revenue_yuan;type:decimal(14,2);not null;default:0" json:"expected_revenue_yuan"`
	// MarginYuan 是**预售毛利**(= ExpectedRevenueYuan − CostAmountYuan)。
	//
	// ⚠️ 可以为负(预期售价低于成本 ⇒ 预估亏本)—— 门店自己承担,不拦。
	MarginYuan decimal.Decimal `gorm:"column:margin_yuan;type:decimal(14,2);not null;default:0" json:"margin_yuan"`
	// PriceSource 是单价来源:promo(促销价)| retail(零售价)。
	PriceSource string    `gorm:"column:price_source;type:varchar(16);not null;default:''" json:"price_source"`
	Note        string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	CreatedAt   time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

// TableName 显式表名。
func (CutPresale) TableName() string { return "cut_presale_settlements" }

// ---- 表21:pig_presale_margins(预售毛利按猪分摊)----
//
// PigPresaleMargin 审计结转时**分摊到某头猪**的预售毛利。
//
// # 为什么单独一张表(而不是塞进 line_sales_by_pigs)
//
// `line_sales_by_pigs` 记的是**真实现金收入**(POS 销售 / 统收)。
// 预售是**预期收入**,没有现金进来 ⇒ 塞进去会让"这头猪卖了多少"虚增。
//
// ⇒ 预售毛利单独记 ⇒ 与"正常 FIFO 毛利"(line_sales_by_pigs)并列,
//
//	按 pig_id 相加 = 这头猪在该审计范围内的**总毛利**。
//
// # 归属规则
//
// ① **FIFO**:结余里的货按"产生日先后"回溯到**产出它的那头猪**
//
//	(与 service/pig_fifo.go 的零售毛利回归同一套归属快照);
//
// ② FIFO 摊不完的部分(没有袋子/没有归属)⇒ **兜底**按本次审计范围内
//
//	各猪的**毛重比例**摊(与 applyProducedFallback 同一套口径)。
//
// ⇒ 无论走哪条,Σ 预售毛利(按猪) == 预售毛利(按部位/SKU)—— 总数守恒。
//
// ⚠️ **往日压舱的货归回它自己的猪**(门店口径:「审计+预售计提后所有猪的毛利
//
//	已结算完毕,不会留给下一期」)⇒ 那头猪的毛利在审计当期就算足。
type PigPresaleMargin struct {
	// ID 是**确定性**代理主键(surrogateID("PPM", branch_id, biz_date, sku, pig_id))。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_pig_presale,priority:1" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;uniqueIndex:idx_pig_presale,priority:2" json:"biz_date"`
	// CubeProductID 是这行分摊对应的 SKU。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;uniqueIndex:idx_pig_presale,priority:3" json:"cube_product_id"`
	// PigID 是分摊到的那头猪。
	PigID   string  `gorm:"column:pig_id;type:varchar(64);not null;uniqueIndex:idx_pig_presale,priority:4" json:"pig_id"`
	CutType CutType `gorm:"column:cut_type;type:varchar(32);not null;index" json:"cut_type"`
	// QtyKg 是分摊到这头猪的重量。
	QtyKg decimal.Decimal `gorm:"column:qty_kg;type:decimal(14,3);not null;default:0" json:"qty_kg"`
	// ExpectedRevenueYuan / CostAmountYuan / MarginYuan 是分摊后的三个金额。
	ExpectedRevenueYuan decimal.Decimal `gorm:"column:expected_revenue_yuan;type:decimal(14,2);not null;default:0" json:"expected_revenue_yuan"`
	CostAmountYuan      decimal.Decimal `gorm:"column:cost_amount_yuan;type:decimal(14,2);not null;default:0" json:"cost_amount_yuan"`
	MarginYuan          decimal.Decimal `gorm:"column:margin_yuan;type:decimal(14,2);not null;default:0" json:"margin_yuan"`
	// AllocRule 是归属规则:fifo(按袋子归属)| scope_fallback(按审计范围内猪的毛重比例)。
	AllocRule string    `gorm:"column:alloc_rule;type:varchar(16);not null;default:''" json:"alloc_rule"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (PigPresaleMargin) TableName() string { return "pig_presale_margins" }

// ---- 表22:cut_presale_edits(预售价的人工修改痕迹)----
//
// ⚠️ **2026-11-01 owner：预期毛利(预售结转)端点已下线**（`/stock/presale` 的 PUT）。
//
//	模型与表**保留**，但语义收缩为**历史只读**：改价写路径(`UpdatePresale`)已随端点退役，
//	不会再有新行写入；AutoMigrate + purge 登记保留，老店的改价留痕不被抹掉。
//
// CutPresaleEdit 门店审计人**改预售价**的留痕（append-only 审计轨迹）。
//
// # 为什么要有这张表
//
// 门店口径（2026-10-24）：「取不到也按 0 生成，会经过一轮人工修改，门店审计人负责。」
//
// ⇒ 审计时**取不到售价**的货号按 0 元结转 ⇒ 审计人随后改成真实价。
// ⇒ 这是**修正审计结论**（预售价本来就是审计人拍板的），不是改被冻结的源数据
//
//	⇒ 允许改，但**每次改都要留痕**：谁、何时、从多少改到多少、为什么。
//
// ⚠️ 主键含时间戳 ⇒ 同一秒内改**同一个货号两次**会撞主键 ⇒ 用 upsert 覆盖
//
//	（最后一次的旧价/新价保留），不会丢。
type CutPresaleEdit struct {
	// ID 是**确定性**代理主键（surrogateID("CPE", branch_id, biz_date, sku, 时间戳)）。
	ID       string         `gorm:"primaryKey;column:id;type:varchar(64)" json:"id"`
	BranchID string         `gorm:"column:branch_id;type:varchar(64);not null;index" json:"branch_id"`
	BizDate  datatypes.Date `gorm:"column:biz_date;not null;index" json:"biz_date"`
	// CubeProductID 是被改的货号。
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null;index" json:"cube_product_id"`
	// OldUnitYuan 是改之前的预期单价（元/kg）。
	OldUnitYuan decimal.Decimal `gorm:"column:old_unit_yuan;type:decimal(12,4);not null;default:0" json:"old_unit_yuan"`
	// NewUnitYuan 是改之后的预期单价（元/kg）。
	NewUnitYuan decimal.Decimal `gorm:"column:new_unit_yuan;type:decimal(12,4);not null;default:0" json:"new_unit_yuan"`
	// OperatorID 是谁改的。
	OperatorID string `gorm:"column:operator_id;type:varchar(64);not null" json:"operator_id"`
	// Note 是修改说明。
	Note      string    `gorm:"column:note;type:varchar(255)" json:"note,omitempty"`
	CreatedAt time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

// TableName 显式表名。
func (CutPresaleEdit) TableName() string { return "cut_presale_edits" }

// ---- 输入 DTO ----

// RecordWholePigInput POST /whole-pigs 入参。
//
// 场景区分(用户决策 2026-10-01):
//   - 半头猪:        half_pig=true,offal_included=false
//   - 半头带下水:     half_pig=true,offal_included=true
//   - 1 头整猪:      half_pig=false,offal_included=false (默认)
//   - 2 头整猪同批:  两次调用,purchase_group_id 同值
type RecordWholePigInput struct {
	// EarTag 是猪场耳标 / 批次号。
	//
	// ⚠️ **可空**(2026-10-24 门店口径:有些猪没有耳标/不扫码)
	//   ⇒ 之前这里写着 `binding:"required"`,与前端提示"可留空"、
	//     与 service 的"耳标号可选"注释**自相矛盾**
	//   ⇒ 门店按前端的提示留空 ⇒ 后端 400 + 一串 Go 的 key 路径天书。
	//   ⇒ 数据库侧 ear_tag 只是普通索引(不是唯一键)⇒ 多头猪都留空不冲突。
	EarTag                string          `json:"ear_tag"`
	GrossWeightKg         decimal.Decimal `json:"gross_weight_kg" binding:"required"`
	PurchaseUnitPriceYuan decimal.Decimal `json:"purchase_unit_price_yuan" binding:"required"`
	SupplierID            string          `json:"supplier_id" binding:"required"`
	ArrivedAt             time.Time       `json:"arrived_at" binding:"required"`
	HalfPig               bool            `json:"half_pig"`
	OffalIncluded         bool            `json:"offal_included"`
	PurchaseGroupID       string          `json:"purchase_group_id"`
	// AdviceMode 是**分割建议的生成方式**(auto / baseline / llm;空 = auto)。
	//
	// ⚠️ 2026-10-31 owner:录猪时门店要能选「用什么方式生成建议」
	//   ⇒ 选「只用基线」时后端**不调 LLM**(省一次调用 + 省一次等待)。
	AdviceMode string `json:"advice_mode"`
}

// RecordPigCutInput POST /pig-cuts 入参。
type RecordPigCutInput struct {
	PigID     string          `json:"pig_id" binding:"required"`
	CutType   CutType         `json:"cut_type" binding:"required"`
	Barcode   string          `json:"barcode" binding:"required"`
	WeightKg  decimal.Decimal `json:"weight_kg" binding:"required"`
	ExpiresAt time.Time       `json:"expires_at" binding:"required"`
	// CubeProductID 是这条补录对应的 cube 商品(= 思迅 item_no)。
	//
	// ⚠️ **可空但强烈建议填**:它是 SKU 级移动加权成本的"入库落点" ——
	// 不填时按 cut_type 归到该部位的**主 SKU**(映射表里 cube_product_id 最小的那条),
	// 一个部位挂多个 SKU 时归属会不准。
	CubeProductID string `json:"cube_product_id"`
}

// CutInput POST /pork-cuts-stocktake 入参的 cuts 数组元素(per-SKU 一行)。
//
// ⚠️ 2026-11-02 owner:标签号玩法下线 ⇒ **同一 SKU 只有一行**
// (拆袋是"每袋一个标签号"的衍生物 ⇒ 没有标签号就没有多袋的身份)。
type CutInput struct {
	CutType        CutType         `json:"cut_type" binding:"required"`
	CubeProductID  string          `json:"cube_product_id" binding:"required"` // 必填;后端校验在 branch_cut_mapping 中存在
	ActualRemainKg decimal.Decimal `json:"actual_remain_kg" binding:"required"`
}

// RecordPorkCutsStocktakeInput POST /pork-cuts-stocktake 入参。
type RecordPorkCutsStocktakeInput struct {
	IsComplete bool `json:"is_complete"`
	// Cuts 是盘点明细(per-SKU 一行 = 一袋,或一行散货)。
	//
	// ⚠️ 2026-10-24:**可以是空数组**(当天一点货都没剩 ⇒ 售罄),
	// 但**字段必须存在** —— 缺失时 service 返 400(防止"忘了传 cuts"被当成售罄)。
	// ⇒ 这里**不能**加 binding:"required"(validator 会把空数组也判成缺失)。
	Cuts []CutInput `json:"cuts"`
	// TakenDate 是这张盘点单的**归属日**(YYYY-MM-DD,可选;空 = 今天)。
	//
	// 门店口径(2026-10-24):「每袋有自己的产生日,这个产生日就是盘点归属日
	// (也就是补录指定日)」⇒ 补录往日时传它 ⇒ 盘点单、结转记录、袋子
	// 全部记在**那一天**,而不是今天。
	//
	// ⚠️ 补录往日的**记录层**数据(盘点单 / 按日结转记录 / 袋子)已经照常参与
	// 那一天的毛利回归(FIFO);**余额与成本快照的按营业日推导**尚未实现
	// —— 见 service 注释与交付报告「未做」一节。
	TakenDate string `json:"taken_date,omitempty"`
}

// RecordWasteLogInput POST /waste-logs 入参。
type RecordWasteLogInput struct {
	PigID         string          `json:"pig_id"`
	CutType       CutType         `json:"cut_type"`
	CubeProductID string          `json:"cube_product_id"` // 可选;实际报废的 cube SKU
	QtyKg         decimal.Decimal `json:"qty_kg" binding:"required"`
	Reason        string          `json:"reason" binding:"required"`
	// WasteType 是**出口类型**:`loss`(报损,默认) | `gift`(搭赠)。
	//
	// ⚠️ 空 = loss(老客户端兼容)⇒ 不用改前端也能照旧录报损。
	WasteType  string    `json:"waste_type"`
	RecordedAt time.Time `json:"recorded_at"` // 可空;空则用 service.now()
}

// CreateBranchCutMappingInput POST /branch-cut-mappings 入参。
type CreateBranchCutMappingInput struct {
	CutType       CutType `json:"cut_type" binding:"required"`
	CubeProductID string  `json:"cube_product_id" binding:"required"`
}

// UpdateBranchCutMappingInput PUT /branch-cut-mappings/:id 入参。
type UpdateBranchCutMappingInput struct {
	CutType       CutType `json:"cut_type"`
	CubeProductID string  `json:"cube_product_id"`
}

// ---- 事件 payload(无 tenant_id) ----

// PorkCutsStocktakenEventData 发送给 sales-agg / BI 的盘点完成事件(EVENT-CATALOG §2.8)。
//
// **不携带 tenant_id**(本仓不持久化 tenant;见 service/events.go)。
// OpeningKg / WasteKg 是新键(优化 2026-10-01):key=cut_type,value=decimal.Decimal;下游若不读可忽略。
type PorkCutsStocktakenEventData struct {
	StocktakeID string                     `json:"stocktake_id"`
	BranchID    string                     `json:"branch_id"`
	IsComplete  bool                       `json:"is_complete"`
	Cuts        []CutSnapshot              `json:"cuts"`
	OpeningKg   map[string]decimal.Decimal `json:"opening_kg,omitempty"`
	WasteKg     map[string]decimal.Decimal `json:"waste_kg,omitempty"`
	LLMReviewed bool                       `json:"llm_reviewed"`
	DataSource  string                     `json:"data_source"` // actual | llm_reviewed | history_avg
	CompletedAt time.Time                  `json:"completed_at"`
	OperatorID  string                     `json:"operator_id"`
}

// WasteLogRecordedEventData 报损事件(EVENT-CATALOG §2.6)。
//
// **不携带 tenant_id**。
type WasteLogRecordedEventData struct {
	LogID         string          `json:"log_id"`
	BranchID      string          `json:"branch_id"`
	PigID         string          `json:"pig_id,omitempty"`
	CutType       CutType         `json:"cut_type,omitempty"`
	CubeProductID string          `json:"cube_product_id,omitempty"`
	QtyKg         decimal.Decimal `json:"qty_kg"`
	Reason        string          `json:"reason"`
	// WasteType 让下游能区分「报损」与「搭赠」(2026-10-31)。
	// ⚠️ omitempty:老事件没有这个键 ⇒ 下游读到空 = 报损(行为不变)。
	WasteType  string    `json:"waste_type,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	OperatorID string    `json:"operator_id"`
}
