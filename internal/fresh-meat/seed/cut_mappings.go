// Package seed —— 门店「部位 ↔ cube SKU」映射的**初始装配**数据。
//
// # 这份数据是怎么来的(2026-10-23)
//
// 直接查两个生产源库(思迅家族,只读),窗口 = **近 14 个完整自然日**
// (2026-10-09 00:00 ~ 2026-10-22 23:59:59,排除当天未走完的部分):
//
//	佳乐  (branch "01") → sixun-hbposv7-jiale → 类别 1201「佳乐超市猪肉部」
//	富百家(branch "00") → sixun-ysx-fb       → 类别 010303「猪肉」
//
// 逐 SKU 统计"在售天数 / 销量 / 成交价",按**行业标准猪部位命名**归类,
// 得到本文件。DaysSold 保留观测值(14 = 该窗口内**每天都卖**)。
//
// # 归类规则(可复核)
//
//  1. **部位 = 解剖部位**。商品名里的营销前缀(精品/精制/团购/鲜风/特价)
//     不改变部位归属:精制五花扣肉 = 五花、团购排骨 = 排骨、鲜风前腿肉 = 前腿肉。
//  2. **"特价X" 是价格档位,不是部位** ⇒ 不进映射。实测这类 SKU 的**成交价逐日变**
//     (03001 特价猪肉5元 实际卖 9 元/斤;03002 特价猪肉8元 实际卖 7 元/斤;
//     03007 特价猪肉20元 10-13~10-20 卖 15 元、10-21 卖 20 元),
//     说明它装的是"当天临期的某个部位"⇒ 挂到任何固定部位都是错的。
//     代价:这部分销量不计入按部位的毛利(已有 OnSaleCompleted 的 warn 日志可观测)。
//  3. **非鲜肉制品排除**:腊肠/腊肉(腌腊)、火锅丸子、热狗肠(预制)。
//  4. **名字不带部位信息的排除**:佳乐 00024 的商品名是"无"(空名),
//     猜部位等于往毛利分析里灌假数据 ⇒ 不映射,留给门店确认。
//  5. **品种级 ≠ 解剖部位**:隔山肉(本地土猪)是品种,单列 local_pork,
//     且不进整猪分割推演。
//
// # 纳入规则(可复核)
//
//	在售天数 >= 3(= 观测窗口里至少 20% 的营业日都在卖)
//	**或**它是该店该部位的**唯一** SKU(哪怕只卖过 0~2 天)
//	  —— 否则那个部位在这个店就没有任何可盘点的 SKU,门店盘不了点。
//
// 例:富百家 00291 猪皮 / 01404 龙骨 在窗口内 0 天(但它们是该店唯一的
// skin / bone_spine SKU)⇒ 纳入;01417 猪耳朵 1 天(该店唯一 ear SKU)⇒ 纳入。
//
// # 明确排除的 SKU(留给门店确认)
//
//	佳乐 00024 「无」      —— 商品名是空名,猜部位等于往毛利分析灌假数据
//	佳乐 00070/03687/03001/03002/03004/03006/03007/03009/03390 「特价猪肉X元」
//	                       —— 价格档位,逐日换价换货(见上),挂任何固定部位都是错的
//	佳乐 00059/00056 「特价骨头」 —— 同理
//	佳乐 03809 手工腊肠    —— 腌腊制品,不是鲜肉分割部位
//
// 代价:"特价"部分的销量(佳乐近 14 天约 440 斤,占其猪肉销量 ~11%)不计入
// 按部位的毛利。已有 OnSaleCompleted 的 warn 日志可观测到这些被丢弃的行。
//
// # 维护方式
//
// 这份是**初始装配**;之后由仓管通过 POST/PUT /branch-cut-mappings 维护
// (那是本服务设计好的维护路径)。重新跑一次本 seed 是幂等的:
// 同 (branch_id, cube_product_id) 已存在就更新 cut_type / 名称,不会重复插。
package seed

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// Mapping 一条「门店部位 → cube 商品」的映射。
//
// CubeProductID = cube `product.id` = 思迅源库的 `item_no`(货号),
// **不是**称重标签条码(源库 t_rm_saleflow.item_no_Fresh 是**每笔交易重新生成**的
// 称重标签码,不能当稳定键)。
type Mapping struct {
	BranchID string
	CutType  model.CutType
	// CubeProductID 是 cube product.id(= 思迅 item_no)。
	CubeProductID string
	// CubeProductName 是商品档案里的名字(冗余缓存,免得每次 join cube)。
	CubeProductName string
	// DaysSold = 观测窗口(2026-10-09 ~ 2026-10-22,14 个完整营业日)里的在售天数。
	// 14 = 每个营业日都在卖。
	DaysSold int
}

// Daily 该 SKU 在观测窗口里是否**每天都卖**(DaysSold >= 13)。
func (m Mapping) Daily() bool { return m.DaysSold >= 13 }

// ---- 佳乐(branch "01"):类别 1201「佳乐超市猪肉部」,35 个在售 SKU ----

var jialeMappings = []Mapping{
	// 分割肉
	{BranchID: "01", CutType: model.CutLegFront, CubeProductID: "00009", CubeProductName: "前腿肉", DaysSold: 14},
	{BranchID: "01", CutType: model.CutBelly, CubeProductID: "00004", CubeProductName: "腩肉(五花肉)", DaysSold: 14},
	{BranchID: "01", CutType: model.CutBelly, CubeProductID: "00023", CubeProductName: "精品五花肉", DaysSold: 11},
	{BranchID: "01", CutType: model.CutShoulderBlade, CubeProductID: "00008", CubeProductName: "梅花肉", DaysSold: 14},
	{BranchID: "01", CutType: model.CutShoulderBlade, CubeProductID: "00011", CubeProductName: "梅肉(小里脊)", DaysSold: 7},
	{BranchID: "01", CutType: model.CutLean, CubeProductID: "00022", CubeProductName: "精品瘦肉", DaysSold: 14},
	{BranchID: "01", CutType: model.CutLean, CubeProductID: "00002", CubeProductName: "瘦肉", DaysSold: 7},
	{BranchID: "01", CutType: model.CutLegRear, CubeProductID: "00041", CubeProductName: "后腿肉", DaysSold: 13},
	{BranchID: "01", CutType: model.CutRib, CubeProductID: "00007", CubeProductName: "前排(硬排)", DaysSold: 12},
	{BranchID: "01", CutType: model.CutRib, CubeProductID: "00006", CubeProductName: "精品排骨", DaysSold: 10},
	{BranchID: "01", CutType: model.CutTrotter, CubeProductID: "00014", CubeProductName: "猪后脚", DaysSold: 14},
	{BranchID: "01", CutType: model.CutTrotter, CubeProductID: "00015", CubeProductName: "猪前脚", DaysSold: 14},
	{BranchID: "01", CutType: model.CutTongue, CubeProductID: "00018", CubeProductName: "猪利", DaysSold: 9},
	{BranchID: "01", CutType: model.CutEar, CubeProductID: "00021", CubeProductName: "猪耳", DaysSold: 14},
	{BranchID: "01", CutType: model.CutHeadSkin, CubeProductID: "00020", CubeProductName: "猪头皮", DaysSold: 12},
	{BranchID: "01", CutType: model.CutTail, CubeProductID: "00017", CubeProductName: "猪尾", DaysSold: 11},
	{BranchID: "01", CutType: model.CutSkin, CubeProductID: "00019", CubeProductName: "猪皮", DaysSold: 3},
	{BranchID: "01", CutType: model.CutFat, CubeProductID: "00032", CubeProductName: "猪板油", DaysSold: 13},
	// ⚠️ 03808 水油 —— **大宗档**(2026-10-23 门店确认:分割副产品)。
	//
	// 实测:每 9~11 天**整一笔** 400~870kg @1.70~1.80(2026-08~10 共 16 笔),
	// 走**普通收银台**(柜台 9999)⇒ 它是**正常的 POS 零售 SKU**,不是账外统收。
	// 区别只在篮子极大 ⇒ 一笔跨很多头猪 ⇒ 销售落库时要**按当日各猪毛重比例分摊**
	// (见 service/bulk_split.go),否则整笔会归到当日某一头猪头上。
	//
	// ⇒ 它必须**有部位映射**(fat),否则 POS 销售行会被整行丢弃
	// (实测近 2 个月 16 笔 / 9,502kg / ¥16,624 全在丢)。
	// ⇒ 同时登记成**大宗品项**(code=wateroil, 绑 03808)⇒ 走大宗口径的成本与归属。
	{BranchID: "01", CutType: model.CutFat, CubeProductID: "03808", CubeProductName: "水油", DaysSold: 2},
	{BranchID: "01", CutType: model.CutLocalPork, CubeProductID: "00003", CubeProductName: "隔山肉", DaysSold: 12},
	// 骨
	{BranchID: "01", CutType: model.CutBoneSpine, CubeProductID: "00010", CubeProductName: "龙骨", DaysSold: 14},
	{BranchID: "01", CutType: model.CutBoneShank, CubeProductID: "00012", CubeProductName: "筒骨", DaysSold: 14},
	{BranchID: "01", CutType: model.CutBoneScapula, CubeProductID: "00016", CubeProductName: "扇骨", DaysSold: 4},
	{BranchID: "01", CutType: model.CutBoneHead, CubeProductID: "00013", CubeProductName: "猪头骨", DaysSold: 12},
	// 下水
	{BranchID: "01", CutType: model.CutLiver, CubeProductID: "00028", CubeProductName: "猪肝", DaysSold: 14},
	{BranchID: "01", CutType: model.CutHeart, CubeProductID: "00031", CubeProductName: "猪心", DaysSold: 14},
	{BranchID: "01", CutType: model.CutLung, CubeProductID: "00037", CubeProductName: "猪肺", DaysSold: 6},
	{BranchID: "01", CutType: model.CutKidney, CubeProductID: "00027", CubeProductName: "猪腰", DaysSold: 13},
	{BranchID: "01", CutType: model.CutStomach, CubeProductID: "00039", CubeProductName: "猪肚", DaysSold: 12},
	{BranchID: "01", CutType: model.CutStomachHoneycomb, CubeProductID: "00038", CubeProductName: "猪小肚", DaysSold: 8},
	{BranchID: "01", CutType: model.CutIntestine, CubeProductID: "00030", CubeProductName: "猪大肠", DaysSold: 12},
	{BranchID: "01", CutType: model.CutIntestine, CubeProductID: "00029", CubeProductName: "生肠", DaysSold: 3},
	{BranchID: "01", CutType: model.CutIntestineHead, CubeProductID: "00040", CubeProductName: "大肠头", DaysSold: 8},
	{BranchID: "01", CutType: model.CutIntestineSmall, CubeProductID: "00036", CubeProductName: "粉肠", DaysSold: 14},
	{BranchID: "01", CutType: model.CutSpleen, CubeProductID: "00035", CubeProductName: "猪横利", DaysSold: 14},
	{BranchID: "01", CutType: model.CutTendon, CubeProductID: "03541", CubeProductName: "猪鞭", DaysSold: 2},
}

// ---- 富百家(branch "00"):类别 010303「猪肉」,31 个在售 SKU ----

var fubaijiaMappings = []Mapping{
	// 分割肉
	{BranchID: "00", CutType: model.CutBelly, CubeProductID: "01409", CubeProductName: "五花肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutLegFront, CubeProductID: "01408", CubeProductName: "前腿肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutLegRear, CubeProductID: "01407", CubeProductName: "后腿肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutShoulderBlade, CubeProductID: "01406", CubeProductName: "梅花肉", DaysSold: 11},
	{BranchID: "00", CutType: model.CutShoulderBlade, CubeProductID: "00225", CubeProductName: "梅肉/腱子肉/猪面瘦肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutTenderloin, CubeProductID: "01405", CubeProductName: "里脊肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutLean, CubeProductID: "01438", CubeProductName: "新鲜瘦肉", DaysSold: 14},
	{BranchID: "00", CutType: model.CutRib, CubeProductID: "01402", CubeProductName: "精选腩排", DaysSold: 14},
	{BranchID: "00", CutType: model.CutRib, CubeProductID: "01401", CubeProductName: "精选排骨", DaysSold: 14},
	{BranchID: "00", CutType: model.CutTrotter, CubeProductID: "01411", CubeProductName: "猪脚", DaysSold: 13},
	{BranchID: "00", CutType: model.CutTrotter, CubeProductID: "01412", CubeProductName: "猪手", DaysSold: 13},
	{BranchID: "00", CutType: model.CutTongue, CubeProductID: "01421", CubeProductName: "猪舌头", DaysSold: 12},
	{BranchID: "00", CutType: model.CutEar, CubeProductID: "01417", CubeProductName: "猪耳朵", DaysSold: 1},
	{BranchID: "00", CutType: model.CutHeadSkin, CubeProductID: "01416", CubeProductName: "猪头皮", DaysSold: 11},
	{BranchID: "00", CutType: model.CutTail, CubeProductID: "01500", CubeProductName: "猪尾骨", DaysSold: 14},
	{BranchID: "00", CutType: model.CutSkin, CubeProductID: "00291", CubeProductName: "猪皮", DaysSold: 0},
	{BranchID: "00", CutType: model.CutFat, CubeProductID: "01445", CubeProductName: "猪油", DaysSold: 14},
	{BranchID: "00", CutType: model.CutLocalPork, CubeProductID: "01424", CubeProductName: "隔山", DaysSold: 5},
	// 骨
	{BranchID: "00", CutType: model.CutBoneShank, CubeProductID: "01403", CubeProductName: "筒骨", DaysSold: 14},
	{BranchID: "00", CutType: model.CutBoneScapula, CubeProductID: "01422", CubeProductName: "扇骨", DaysSold: 14},
	{BranchID: "00", CutType: model.CutBoneSpine, CubeProductID: "01404", CubeProductName: "龙骨", DaysSold: 0},
	{BranchID: "00", CutType: model.CutBoneRib, CubeProductID: "01426", CubeProductName: "沙骨", DaysSold: 14},
	{BranchID: "00", CutType: model.CutBoneHead, CubeProductID: "01413", CubeProductName: "猪头骨", DaysSold: 13},
	// 下水
	{BranchID: "00", CutType: model.CutLiver, CubeProductID: "01419", CubeProductName: "猪肝", DaysSold: 4},
	{BranchID: "00", CutType: model.CutHeart, CubeProductID: "01423", CubeProductName: "猪心", DaysSold: 14},
	{BranchID: "00", CutType: model.CutLung, CubeProductID: "01418", CubeProductName: "猪肺", DaysSold: 11},
	{BranchID: "00", CutType: model.CutKidney, CubeProductID: "01420", CubeProductName: "猪腰", DaysSold: 10},
	{BranchID: "00", CutType: model.CutStomach, CubeProductID: "01410", CubeProductName: "猪肚", DaysSold: 14},
	{BranchID: "00", CutType: model.CutStomachHoneycomb, CubeProductID: "01442", CubeProductName: "猪小肚", DaysSold: 9},
	{BranchID: "00", CutType: model.CutIntestine, CubeProductID: "01415", CubeProductName: "大肠", DaysSold: 11},
	{BranchID: "00", CutType: model.CutIntestine, CubeProductID: "01464", CubeProductName: "生肠", DaysSold: 14},
	{BranchID: "00", CutType: model.CutIntestineHead, CubeProductID: "01440", CubeProductName: "大肠头", DaysSold: 6},
	{BranchID: "00", CutType: model.CutIntestineSmall, CubeProductID: "01441", CubeProductName: "粉肠", DaysSold: 14},
	{BranchID: "00", CutType: model.CutSpleen, CubeProductID: "01443", CubeProductName: "猪横利", DaysSold: 13},
}

// AllMappings 两个门店的全部初始映射。
var AllMappings = append(append([]Mapping{}, jialeMappings...), fubaijiaMappings...)

// Apply 把 AllMappings 幂等写进 branch_cut_mappings。
//
// 幂等键 = (branch_id, cube_product_id):
//   - 不存在 → 插入(ID 由 (branch, cut, sku) 确定性派生,重复跑不会插两条);
//   - 存在且 cut_type/名称不同 → 更新;
//   - 完全一致 → 跳过(计入 skipped)。
//
// 返回 (created, updated, skipped, err)。
func Apply(db *gorm.DB, now time.Time) (created, updated, skipped int, err error) {
	if db == nil {
		return 0, 0, 0, fmt.Errorf("seed: db 为 nil")
	}
	for _, m := range AllMappings {
		if !m.CutType.Valid() {
			return created, updated, skipped, fmt.Errorf("seed: %s/%s 的 cut_type %q 不在枚举内", m.BranchID, m.CubeProductID, m.CutType)
		}
		var existing model.BranchCutMapping
		q := db.Where("branch_id = ? AND cube_product_id = ?", m.BranchID, m.CubeProductID).First(&existing)
		switch {
		case q.Error == gorm.ErrRecordNotFound:
			rec := model.BranchCutMapping{
				ID:              deterministicID(now, m),
				BranchID:        m.BranchID,
				CutType:         m.CutType,
				CubeProductID:   m.CubeProductID,
				CubeProductName: m.CubeProductName,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if err := db.Create(&rec).Error; err != nil {
				return created, updated, skipped, fmt.Errorf("seed: insert %s/%s: %w", m.BranchID, m.CubeProductID, err)
			}
			created++
		case q.Error != nil:
			return created, updated, skipped, fmt.Errorf("seed: query %s/%s: %w", m.BranchID, m.CubeProductID, q.Error)
		default:
			if existing.CutType == m.CutType && existing.CubeProductName == m.CubeProductName {
				skipped++
				continue
			}
			updates := map[string]any{
				"cut_type":           m.CutType,
				"cube_product_name": m.CubeProductName,
				"updated_at":         now,
			}
			if err := db.Model(&existing).Updates(updates).Error; err != nil {
				return created, updated, skipped, fmt.Errorf("seed: update %s/%s: %w", m.BranchID, m.CubeProductID, err)
			}
			updated++
		}
	}
	return created, updated, skipped, nil
}

// deterministicID 生成 `BCM<yyyymmdd><8-hex>`(与 service.generateID 同形),
// 但后 8 位由 (branch, cut, sku) 哈希派生 ⇒ **重复执行得到同一个 ID**。
func deterministicID(now time.Time, m Mapping) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(m.BranchID + "|" + string(m.CutType) + "|" + m.CubeProductID))
	return fmt.Sprintf("BCM%s%08x", now.UTC().Format("20060102"), h.Sum32())
}
