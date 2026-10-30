package service

// 2026-10-29:部位级「建议分割 / 实际分割 / 调整后权重」的**算法锁**。
//
// # 为什么是内部测试包(package service)
//
//	这几个函数是**纯计算**(不碰数据库)⇒ 直接测它们最省事、也最不容易被"接线错误"掩盖。
//
// # 锁的是**口径**,不是魔法数
//
//	断言写成"总量守恒""超出建议的那个部位等于实际分割""老货不算今日分割"这类
//	**业务口径**,而不是"某个函数返回 17.5"—— 后者会在口径演进时变成噪音。

import (
	"testing"

	"github.com/shopspring/decimal"
)

// dsv 把字符串转成 decimal(测试输入用)。
func dsv(s string) decimal.Decimal {
	x, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return x
}

// kgMap 造一个 部位 → kg 的 map。
func kgMap(pairs ...string) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = dsv(pairs[i+1])
	}
	return out
}

// sumKg 求和。
func sumKg(m map[string]decimal.Decimal) decimal.Decimal {
	var total decimal.Decimal
	for _, v := range m {
		total = total.Add(v)
	}
	return total
}

// ---- 重分配(调整后权重 W)----

// TestRedistributeSplitWeights_NoOneExceeds 锁"没人超出建议 ⇒ W 就等于建议"。
//
// 这条对应 owner 的第一条:「实际分割没超出建议时,成本就按建议分割算」。
func TestRedistributeSplitWeights_NoOneExceeds(t *testing.T) {
	suggested := kgMap("belly", "20", "rib", "30")
	actual := kgMap("belly", "18", "rib", "0")

	w, note := redistributeSplitWeights(suggested, actual)
	if note != "" {
		t.Errorf("不该有告警: %s", note)
	}
	if !w["belly"].Equal(dsv("20")) || !w["rib"].Equal(dsv("30")) {
		t.Errorf("W 应等于建议: got belly=%s rib=%s", w["belly"], w["rib"])
	}
}

// TestRedistributeSplitWeights_OverUsesCompressiblePool 锁"超出建议 ⇒ 先按可压缩量扣"。
//
// belly 实出 50(建议 20)⇒ 超出 30;rib 实出 0(建议 30)⇒ 可压缩 30
// ⇒ rib 正好让出 30(全让)⇒ W: belly=50, rib=0 ⇒ ΣW == ΣS(守恒)。
func TestRedistributeSplitWeights_OverUsesCompressiblePool(t *testing.T) {
	suggested := kgMap("belly", "20", "rib", "30")
	actual := kgMap("belly", "50", "rib", "0")

	w, note := redistributeSplitWeights(suggested, actual)
	if note != "" {
		t.Errorf("不该有告警: %s", note)
	}
	if !w["belly"].Equal(dsv("50")) {
		t.Errorf("超出建议的部位应直接取实际分割 50, got %s", w["belly"])
	}
	if !w["rib"].Equal(dsv("0")) {
		t.Errorf("可压缩量 30 应被扣光, got %s", w["rib"])
	}
	if !sumKg(w).Equal(sumKg(suggested)) {
		t.Errorf("总量应守恒: ΣW=%s ≠ ΣS=%s", sumKg(w), sumKg(suggested))
	}
}

// TestRedistributeSplitWeights_OverProportionalFallback 锁"可压缩量不够 ⇒ 按建议比例摊"。
//
// belly 实出 100(建议 20)⇒ 超出 80;rib 建议 30(实出 0)⇒ 可压缩 30
// ⇒ 第一轮扣光 30,还差 50 ⇒ 第二轮按当前 W 的比例摊(只有 rib 有 W=0)⇒ 仍差 50
// ⇒ **守恒不了** ⇒ 必须出告警(不许静默),且 W 不能变成负数。
func TestRedistributeSplitWeights_OverProportionalFallback(t *testing.T) {
	suggested := kgMap("belly", "20", "rib", "30")
	actual := kgMap("belly", "100", "rib", "0")

	w, note := redistributeSplitWeights(suggested, actual)
	if note == "" {
		t.Errorf("★ 守恒不了时必须出告警(不许静默)")
	}
	if !w["belly"].Equal(dsv("100")) {
		t.Errorf("超出建议的部位应直接取实际分割 100, got %s", w["belly"])
	}
	if w["rib"].IsNegative() {
		t.Errorf("W 不能是负数: %s", w["rib"])
	}
	// ⚠ 这个场景**守恒不了**:可压缩量只有 30,超出却有 80 ⇒ 差 50 无处可去
	//   ⇒ ΣW > ΣS(差额必须以告警的形式露出来,不能悄悄吞掉)。
	if !sumKg(w).Equal(dsv("100")) {
		t.Errorf("ΣW 应 = 实际分割 100 + 0, got %s", sumKg(w))
	}
	if sumKg(w).Sub(sumKg(suggested)).LessThanOrEqual(dsv("0")) {
		t.Errorf("守恒不了时 ΣW 应大于 ΣS(差额要看得见): ΣW=%s ΣS=%s", sumKg(w), sumKg(suggested))
	}
}

// TestRedistributeSplitWeights_ProportionalSplitIsFair 锁"两个部位都有可压缩量 ⇒ 按比例摊"。
//
// belly 超出 10;rib 可压缩 20、trotter 可压缩 20 ⇒ 各摊 5。
func TestRedistributeSplitWeights_ProportionalSplitIsFair(t *testing.T) {
	suggested := kgMap("belly", "20", "rib", "20", "trotter", "20")
	actual := kgMap("belly", "30", "rib", "0", "trotter", "0")

	w, note := redistributeSplitWeights(suggested, actual)
	if note != "" {
		t.Errorf("不该有告警: %s", note)
	}
	if !w["rib"].Equal(dsv("15")) || !w["trotter"].Equal(dsv("15")) {
		t.Errorf("应各摊 5(可压缩量 20:20): got rib=%s trotter=%s", w["rib"], w["trotter"])
	}
	if !sumKg(w).Equal(sumKg(suggested)) {
		t.Errorf("总量应守恒: ΣW=%s ≠ ΣS=%s", sumKg(w), sumKg(suggested))
	}
}

// TestRedistributeSplitWeights_GuardNoSuggestion 锁**守卫①**:没有建议分割 ⇒ 不重分配。
//
// 生产实测(店 00 / 2026-10-29):当天没有进猪 ⇒ S 全是 0,但销量 27.58
// ⇒ 倒推出的 A = 27.58 > S = 0 ⇒ 若拿它当入库量,账面的"短卖"(−27.58)会被撑成 0
// ⇒ **把损失掩盖掉了**。⇒ ΣS <= 0 时必须原样返回(全 0),不重分配。
func TestRedistributeSplitWeights_GuardNoSuggestion(t *testing.T) {
	suggested := map[string]decimal.Decimal{}
	actual := kgMap("belly", "27.5813")

	w, note := redistributeSplitWeights(suggested, actual)
	if note != "" {
		t.Errorf("守卫①不是异常,不该告警: %s", note)
	}
	if !w["belly"].IsZero() {
		t.Errorf("★ 守卫①:没有建议分割时 W 必须是 0(别把短卖撑成 0), got %s", w["belly"])
	}
}

// TestRedistributeSplitWeights_GuardNoPool 锁**守卫②**:无处可挪 ⇒ 不重分配 + 告警。
//
// belly 有建议 20 且实出 50(超出);其它部位都没有建议(可压缩量 = 0)
// ⇒ 没处可挪 ⇒ W 保持 = 建议(20),并**必须**出告警(门店要看得见)。
func TestRedistributeSplitWeights_GuardNoPool(t *testing.T) {
	suggested := kgMap("belly", "20")
	actual := kgMap("belly", "50")

	w, note := redistributeSplitWeights(suggested, actual)
	if note == "" {
		t.Errorf("★ 守卫②必须出告警(不能让门店以为进货 = 实际切出来的量)")
	}
	if !w["belly"].Equal(dsv("20")) {
		t.Errorf("★ 守卫②:W 应保持建议 20(不重分配), got %s", w["belly"])
	}
}

// ---- 实际分割(来源优先级 + 两分支公式)----

// TestDeriveActualSplits_RecordedWins 锁"实录优先"。
//
// belly 有实录 40 ⇒ A = 40,来源 recorded(**不与倒推混算**:即使倒推值是别的数)。
func TestDeriveActualSplits_RecordedWins(t *testing.T) {
	recorded := kgMap("belly", "40")
	actual, source := deriveActualSplits(recorded, true,
		kgMap("belly", "10"), kgMap(), kgMap(), kgMap(), kgMap(), kgMap(), kgMap())
	if !actual["belly"].Equal(dsv("40")) {
		t.Errorf("A 应取实录 40, got %s", actual["belly"])
	}
	if source["belly"] != ActualSplitSourceRecorded {
		t.Errorf("来源应是 recorded, got %q", source["belly"])
	}
}

// TestDeriveActualSplits_DerivedWithStocktake 锁"有实盘 ⇒ 全店恒等式"。
//
// A = 全店期末结余(猪肉台实盘 60 + 冷库 50)+ 销售 30 + 统收 5 + 报损 2 − 全店期初 100 = 47。
//
// ⚠ 注意是**全店期末结余**(含冷库),不是"台面"—— 台面恒等式里推不出"今日分割"。
func TestDeriveActualSplits_DerivedWithStocktake(t *testing.T) {
	actual, source := deriveActualSplits(kgMap(), true,
		kgMap("belly", "30"), kgMap("belly", "5"), kgMap("belly", "2"),
		kgMap("belly", "60"), kgMap("belly", "50"), kgMap("belly", "100"), kgMap())
	if !actual["belly"].Equal(dsv("47")) {
		t.Errorf("A 应 = (60+50)+30+5+2−100 = 47, got %s", actual["belly"])
	}
	if source["belly"] != ActualSplitSourceDerived {
		t.Errorf("来源应是 derived, got %q", source["belly"])
	}
}

// TestDeriveActualSplits_DerivedWithoutStocktake 锁"无实盘 ⇒ 台面流量倒推(近似)"。
//
// A = max(0, 销售 30 + 统收 5 + 报损 2 − 全店期初 100 − 搬上台 20) = 0。
func TestDeriveActualSplits_DerivedWithoutStocktake(t *testing.T) {
	actual, source := deriveActualSplits(kgMap(), false,
		kgMap("belly", "30"), kgMap("belly", "5"), kgMap("belly", "2"),
		kgMap(), kgMap(), kgMap("belly", "100"), kgMap("belly", "20"))
	if !actual["belly"].IsZero() {
		t.Errorf("A 应被压到 0, got %s", actual["belly"])
	}
	if source["belly"] != ActualSplitSourceDerived {
		t.Errorf("来源应是 derived, got %q", source["belly"])
	}
}

// TestDeriveActualSplits_CounterLoadIsOldGoods 锁"**搬上台的老货不算今日分割**"。
//
// 无实盘:销售 130 + 期初 100 + 搬上台 20 ⇒ A = 130 − 100 − 20 = 10。
// (不扣搬上台的话会算成 30 ⇒ 那 20kg 是前几天的老货。)
func TestDeriveActualSplits_CounterLoadIsOldGoods(t *testing.T) {
	actual, _ := deriveActualSplits(kgMap(), false,
		kgMap("belly", "130"), kgMap(), kgMap(),
		kgMap(), kgMap(), kgMap("belly", "100"), kgMap("belly", "20"))
	if !actual["belly"].Equal(dsv("10")) {
		t.Errorf("★ A 应 = 130−100−20 = 10(搬上台是老货,要扣掉), got %s", actual["belly"])
	}
}

// ---- 入库量/入库金额按 W 重分 ----

// TestApplySplitWeights_ResplitsPigCost 锁"金额按 W 的**比例**重分"。
//
// belly 30kg / rib 10kg(共 40kg);可摊猪成本 400 ⇒ belly 300 / rib 100。
//
// ⚠ 如果按原比例同比缩放,单价不变 ⇒ 毛利不变 ⇒ 等于没做 ⇒ 这条锁住"按比例重分"。
func TestApplySplitWeights_ResplitsPigCost(t *testing.T) {
	split := &cutSplitWeights{Adjusted: kgMap("belly", "30", "rib", "10")}
	producedKg := kgMap("belly", "20", "rib", "20")
	producedAmount := kgMap("belly", "200", "rib", "200")

	kg, amt := applySplitWeights(split, producedKg, producedAmount)
	if !kg["belly"].Equal(dsv("30")) || !kg["rib"].Equal(dsv("10")) {
		t.Errorf("入库量应等于 W: got belly=%s rib=%s", kg["belly"], kg["rib"])
	}
	if !amt["belly"].Equal(dsv("300")) || !amt["rib"].Equal(dsv("100")) {
		t.Errorf("入库金额应按 W 比例重分: got belly=%s rib=%s", amt["belly"], amt["rib"])
	}
	if !sumKg(amt).Equal(dsv("400")) {
		t.Errorf("当日总摊销必须不变: got %s, want 400", sumKg(amt))
	}
}

// TestApplySplitWeights_GuardKeepsTodayBehaviour 锁**守卫①**在引擎侧的效果。
//
// ΣW == 0 ⇒ 入库量/入库金额**完全不动**(与今天逐字一致)⇒ 短卖不会被掩盖。
func TestApplySplitWeights_GuardKeepsTodayBehaviour(t *testing.T) {
	split := &cutSplitWeights{Adjusted: map[string]decimal.Decimal{}}
	producedKg := kgMap("belly", "40")
	producedAmount := kgMap("belly", "800")

	kg, amt := applySplitWeights(split, producedKg, producedAmount)
	if !kg["belly"].Equal(dsv("40")) || !amt["belly"].Equal(dsv("800")) {
		t.Errorf("守卫①:入库量/金额必须保持今天的值, got kg=%s amt=%s", kg["belly"], amt["belly"])
	}
}
