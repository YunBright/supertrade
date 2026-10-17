package fact

import (
	"strings"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// ClassifySupplier 判定一个供应商是否应被排除出"供应商排名"。
//
// # 为什么必须三层而不是一层
//
// ✅ 2026-10-16 实测 ysx(hbposv10) 的 t_bd_supcust_info：
//
//   | 层 | 依据 | 实测命中 |
//   |---|---|---|
//   | 1 | cust_flag != 'S' | 49 个**客户**（运营部、手袋厂、中铁五局搅拌站、银海物业…）|
//   | 2 | type != '1' | 4 个自用/直供（乐园公司、内蒙古伊利集团、新龙净菜、自产）|
//   | 3 | 名称关键词 | 自购蔬菜(019)、水果（自营）(084) —— 类型是 1，语义是自营 |
//
// 只做第 2 层会漏掉 `自购蔬菜`：它 type='1'（普通供应商），
// 但它是门店自营采购单元，**不是外部供应商**。
// 而生鲜（茄子/西红柿/土豆等）全部挂在它下面 ——
// 不排掉它，"哪个供应商单品更好"的榜首会被门店自己的蔬菜采购占满。
//
// # 顺序有讲究：先判"是不是供应商"，再判"是不是自用"
//
// 一个 cust_flag='C' 的客户如果名字里恰好有"自"（例如"自营物业"），
// 先跑名称规则会把它标成"自用"，而真实原因是"它根本不是供应商"。
// 两者对店长的解释完全不同（"这是我们自己人" vs
// "这个单位向我们采购，不是供货方"），所以必须区分。
func ClassifySupplier(rec supplier.Record) (ExclusionRule, string) {
	// ---- 第 1 层：它到底是不是供应商 ----
	//
	// ⚠️ 只认 'C'（明确的客户）在这里短路。'A' **不**短路 ——
	//   ✅ 实测唯一一条 A 记录是 special/内部周转，它同时 type='xx'，
	//   语义明确是内部周转。若 A 直接放行，它会绕过第 2 层，
	//   最终靠"内部"这个名称关键词被排除 —— 判据从"类型异常"
	//   退化成"名字里有个内部"，换个店就漏。
	//
	//   未知取值（空或其它）同样不短路：排除一条真供应商的代价
	//   （丢失优质货源线索）大于多显示一条。
	switch strings.ToUpper(strings.TrimSpace(rec.CustFlag)) {
	case "C":
		return ExcludeNotSupplier, "这是向我们采购的客户，不是供货方"
	}

	// ---- 第 2 层：是不是自用/直供 ----
	//
	// ⚠️ 这里用 != '1' 而不是 == '5'：实测存在 'xx' 这种异常值
	//   （special/内部周转），把它当普通供应商排进榜单是错的，
	//   但它也不该被当作正常供应商对待。
	switch typ := strings.TrimSpace(rec.Type); typ {
	case "1":
		// 普通供应商，继续下一层
	case "":
		// 类型缺失：不排除。很多门店的老数据没有填这个字段。
	default:
		if typ == "5" {
			return ExcludeSelfUseType, "自用/直供渠道（门店或集团内部采购）"
		}
		return ExcludeSelfUseType, "非常规供应商类型（" + typ + "），按自用处理"
	}

	// ---- 第 3 层：名称关键词 ----
	//
	// ⚠️ 关键词表必须**外置可配置**，见 DefaultSelfUseKeywords 的说明。
	//   硬编码的后果是换一个门店就漏排：每家店给自采渠道起的名字都不一样。
	kw := rec.Name
	for _, k := range SelfUseKeywords() {
		if k == "" {
			continue
		}
		if strings.Contains(kw, k) {
			return ExcludeNameKeyword, "名称含自采标识（" + k + "），属门店自营采购单元"
		}
	}
	return "", ""
}

// DefaultSelfUseKeywords 是默认的自用关键词表。
//
// # 为什么不硬编码在判定逻辑里
//
// 因为它**一定会**随门店而变。这些词是从 ysx 实测数据里归纳的
// （自购蔬菜 / 水果（自营）/ 自产 / 乐园公司 / 内部周转），
// 但另一家门店可能叫"厂配"、"内部调拨"、"集采"……
//
// 硬编码的失败模式很隐蔽：不匹配的供应商会正常出现在榜单上，
// 没有任何报错，只是结论慢慢变得不可信。
//
// 所以它是一个**可被配置覆盖的默认值**：运营可以在门店配置里追加关键词，
// 覆盖后立即生效，无需改代码或重新部署。
func DefaultSelfUseKeywords() []string {
	return []string{
		"自购", // 自购蔬菜
		"自营", // 水果（自营）
		"自产", // 自产
		"乐园", // 乐园公司（集团/门店内部）
		"内部", // 内部周转
		"自采",
		"厂配",
		"集采",
	}
}

// keywordOverride 是运行时覆盖的自用关键词。
//
// nil = 用默认值；空切片 = 明确表示"一个都不排除"（运营主动关闭）。
var keywordOverride []string

// SetSelfUseKeywords 覆盖自用关键词表。
//
// ⚠️ 传 nil 恢复默认；传空切片表示**明确禁用**关键词规则。
//   两者必须能区分 —— "没配置"和"配置了空"在上层行为上是不同的。
func SetSelfUseKeywords(kws []string) {
	if kws == nil {
		keywordOverride = nil
		return
	}
	cp := make([]string, len(kws))
	copy(cp, kws)
	keywordOverride = cp
}

// SelfUseKeywords 返回当前生效的自用关键词。
func SelfUseKeywords() []string {
	if keywordOverride != nil {
		return keywordOverride
	}
	return DefaultSelfUseKeywords()
}
