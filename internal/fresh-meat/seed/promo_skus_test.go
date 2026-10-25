package seed_test

// 2026-10-23:特价档货号装配数据的**回归锁**。
//
// # 为什么这些数据要钉住
//
// 特价档货号登记的**后果是双向**的:
//
//	漏登 ⇒ 该货号的 POS 销售行**整行被丢弃**(金额 + 重量一起丢)
//	       实测:佳乐猪肉类目 12 个"有销售但既没部位映射也没登记"的 SKU,
//	             合计 ¥13,774 = 猪肉类目成交额的 **5.54%**。
//	错登 ⇒ 把**非特价**(比如牛肉、预制菜)登记成特价 ⇒ 它们的销售行
//	         会被归到 promo 桶(不摊到具体部位)⇒ 部位毛利失真。
//
// ⇒ 名单必须**逐条有据**,不能靠"名字里带特价就都加上"。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/seed"
)

func TestPromoSkuSeeds_Shape(t *testing.T) {
	seeds := seed.PromoSkuSeeds()
	if len(seeds) == 0 {
		t.Fatal("PromoSkuSeeds 不应为空")
	}

	seen := map[string]bool{}
	bySKU := map[string]seed.PromoSkuSeedEntry{}
	for _, s := range seeds {
		key := s.BranchID + "|" + s.CubeProductID
		if seen[key] {
			t.Errorf("重复的 (branch, sku): %s", key)
		}
		seen[key] = true
		if s.BranchID == "" {
			t.Errorf("%s: branch_id 不能为空", key)
		}
		if s.CubeProductID == "" {
			t.Errorf("%s: cube_product_id 不能为空", key)
		}
		bySKU[s.BranchID+"|"+s.CubeProductID] = s
	}

	// 富百家(00)实测**没有**特价档 SKU(010303 猪肉类目 51 个 SKU 全是常规部位)。
	// ⇒ 不该给 00 登记任何特价货号。
	for _, s := range seeds {
		if s.BranchID == "00" {
			t.Errorf("branch 00 不该有特价档货号(%s): 富百家 010303 实测无特价档", s.CubeProductID)
		}
	}

	// ⚠️ `03005 特价牛肉30Y` 是**牛肉** —— 它的毛利归牛羊肉域,不是鲜肉域。
	// 登记进来会让牛肉的销售额混进猪肉的毛利报告。
	if _, ok := bySKU["01|03005"]; ok {
		t.Errorf("01|03005 特价牛肉30Y 不该登记(它是牛肉,不是猪肉)")
	}

	// ★ 2026-10-23 按销售流水复核时发现的**漏登**:
	// 03687 特价猪肉 —— 近 2 个月 110 行 / 184kg / ¥1,102 / 44 天,10-23 还在卖。
	// ⇒ 它一直在成交、一直在被整行丢弃。
	if _, ok := bySKU["01|03687"]; !ok {
		t.Errorf("01|03687 特价猪肉 必须登记(实测 110 行 / ¥1,102 / 44 天,一直在被丢弃)")
	}

	// 佳乐 12 个已登记 + 03687 = 13 个。
	jl := 0
	for _, s := range seeds {
		if s.BranchID == "01" {
			jl++
		}
	}
	if jl != 13 {
		t.Errorf("佳乐特价档货号应为 13 个(12 个原有 + 03687), got %d", jl)
	}
}
