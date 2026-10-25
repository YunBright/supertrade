package seed

// 2026-10-23:特价档货号登记的初始装配。
//
// # 这些货号为什么是"特价档"
//
// 2026-10-23 实测(佳乐,近 14 个完整营业日):特价档是"**价格槽位**",
// 装的是当天滞销的那个部位 —— 成交价**逐日变**:
//
//	03001 特价猪肉5元  名义 5 元 → 实际卖 9 元/斤
//	03002 特价猪肉8元  名义 8 元 → 实际卖 7 元/斤
//	03007 特价猪肉20元 名义 20 元 → 10-13~10-20 卖 15 元、10-21 卖 20 元
//	03390 特价猪肉5Y  名义 4 元 → 卖 3.98 / 4.90 / 4.00
//
// ⇒ **不能静态映射到某个部位**。试过用"当日同价商品"自动反解,匹配上的都是巧合
// (8 元/斤 匹配到的是猪头皮,只是因为该店 8 元/斤到处都是)。
//
// 但"这些货号是特价"这件事是**稳定**的 ⇒ 登记一次即可。
//
// # 纳入规则
//
// 佳乐 `1201` 类目下、名字含"特价"的货号。**排除 `03005 特价牛肉30Y`** ——
// 那是牛肉,不是猪肉(它的毛利归牛肉/牛羊肉域,不是鲜肉域)。
//
// ⚠️ `00056 特价骨头` / `00059 特价骨头A` 是**骨**的促销档,同样登记 ——
// 它们的销售行原本也会被整行丢弃(金额+重量一起丢)。
//
// 富百家 `010303` 实测**没有**特价档 SKU(51 个 SKU 全是常规猪肉部位)⇒ 不登记。
//
// # 2026-10-23 复核(按销售流水重算,窗口 2026-08-24 ~ 10-23)
//
// 判据:「`1201` 猪肉类目**有销售** ∧ **没有部位映射** ∧ **没登记特价**」
// ⇒ 这些销售行正在被整行丢弃(金额 + 重量一起丢)。
//
// 佳乐 353 个 SKU 里 **56 个有销售**,其中 **12 个**既没部位映射也没登记特价,
// 合计 **¥13,774.48 = 猪肉类目成交额的 5.54%**。逐条看下来:
//
//	03687 特价猪肉    110 行 / 184kg / ¥1,102 / 44 天 / 10-23 还在卖  ⇒ **特价,补登记**
//	03005 特价牛肉30Y   3 行 / ¥107 / 2 天                        ⇒ 牛肉,排除
//	03808 水油         7 行 / 4006kg / ¥7,210 / 7 天              ⇒ 不是分割部位(见下)
//	03809 手工腊肠 / 00067 团购排骨 / 06888 扣肉/猪脚 /
//	00034 腊肉 / 00048 茄子辣椒苦瓜酿 / 03609 冻鸭中翅            ⇒ 预制菜/鸡/团购
//	00024 无 / 00005 中井瘦肉 / 00069 精制夹心肉                    ⇒ 量极小或非猪肉
//
// ⇒ **唯一漏登的是 `03687 特价猪肉`**(110 行、¥1,102、44 天、昨天还在卖)。
// 这就是"登记漏一个 = 静默丢单"的典型:它一直在卖,一直在丢。
//
// # ⚠️ `03808 水油` 要单独说(2026-10-23 实测)
//
// 它在 `1201` 猪肉类目里,**每 9 天整一笔**、每次 400~870kg、单价恒为 1.80 元:
//	08-25 / 09-02 / 09-11 / 09-20 / 09-29 / 10-09 / 10-20
//
// ⇒ 这是**大宗批发**的节奏(不是分割副产品的零售节奏)。
// 对比:佳乐 `00032 猪板油` 是**每天零售**卖 2~25kg @2.98 元,
// 富百家 `01445 猪油` 也是**每天零售**卖 ~9.35kg @6.00 元。
//
// ⇒ **大宗台账跟的是"水油"这条线,不是"板油"零售那条线**。
// 它目前**没有部位映射** ⇒ 它的销售行也在被丢弃(近 2 个月 ¥7,210)。
// 水油到底属于"分割副产品"还是"外购油脂商品"⇒ 需门店确认(见 REQUIREMENTS §4.11)。
//
// # 效果
//
// 登记过的货号,其 POS 销售行**不再被丢弃** ⇒
// 佳乐近 14 个营业日约 **5% 的成交金额、10% 的成交重量**回到账上。

// promoSkuSeed 一条特价档货号。
type promoSkuSeed struct {
	BranchID      string
	CubeProductID string
	ProductName   string
	PriceTierText string
}

// promoSkuSeeds 特价档货号初始装配数据。
var promoSkuSeeds = []promoSkuSeed{
	// ---- 佳乐(01)----
	{BranchID: "01", CubeProductID: "00070", ProductName: "特价猪肉", PriceTierText: "特价猪肉8元"},
	{BranchID: "01", CubeProductID: "00269", ProductName: "特价猪肉", PriceTierText: "特价猪肉31元"},
	{BranchID: "01", CubeProductID: "03001", ProductName: "特价猪肉5元", PriceTierText: "特价猪肉5元"},
	{BranchID: "01", CubeProductID: "03002", ProductName: "特价猪肉8元", PriceTierText: "特价猪肉8元"},
	{BranchID: "01", CubeProductID: "03004", ProductName: "特价猪肉10元", PriceTierText: "特价猪肉10元"},
	{BranchID: "01", CubeProductID: "03006", ProductName: "特价猪肉15元", PriceTierText: "特价猪肉15元"},
	{BranchID: "01", CubeProductID: "03007", ProductName: "特价猪肉20元", PriceTierText: "特价猪肉20元"},
	{BranchID: "01", CubeProductID: "03009", ProductName: "特价猪肉22元", PriceTierText: "特价猪肉22元"},
	{BranchID: "01", CubeProductID: "03305", ProductName: "特价猪肉25", PriceTierText: "特价猪肉25元"},
	{BranchID: "01", CubeProductID: "03390", ProductName: "特价猪肉5Y", PriceTierText: "特价猪肉5元"},
	// 03687 特价猪肉 —— 2026-10-23 按销售流水复核时**发现漏登**:
	// 近 2 个月 110 行 / 184kg / ¥1,102 / 44 天,10-23 还在卖。
	// ⇒ 它一直在成交、一直在被整行丢弃(金额 + 重量一起)。
	{BranchID: "01", CubeProductID: "03687", ProductName: "特价猪肉", PriceTierText: "特价猪肉5.98元"},
	{BranchID: "01", CubeProductID: "00056", ProductName: "特价骨头", PriceTierText: "特价骨头"},
	{BranchID: "01", CubeProductID: "00059", ProductName: "特价骨头A", PriceTierText: "特价骨头"},
}

// PromoSkuSeeds 特价档货号装配数据(对外,供装配工具用)。
func PromoSkuSeeds() []PromoSkuSeedEntry {
	out := make([]PromoSkuSeedEntry, 0, len(promoSkuSeeds))
	for _, s := range promoSkuSeeds {
		out = append(out, PromoSkuSeedEntry{
			BranchID:      s.BranchID,
			CubeProductID: s.CubeProductID,
			ProductName:   s.ProductName,
			PriceTierText: s.PriceTierText,
		})
	}
	return out
}

// PromoSkuSeedEntry 一条特价档货号装配记录。
type PromoSkuSeedEntry struct {
	BranchID      string
	CubeProductID string
	ProductName   string
	PriceTierText string
}
