// Package supplier 定义供应商字典的最小数据结构。
//
// # 为什么单独一个包，而不是直接把类型放在 fact 包里
//
// 因为 fact 包要同时被"查询侧"（从 cube 取数）和"判定侧"（排除自用）
// 依赖。把数据形状放在一个中立的小包里，可以让两者都依赖它，
// 而不是互相依赖 —— 否则 fact 会 import cube 适配层，形成环。
//
// Record 只包含**判定与展示**真正需要的字段。
// 不多取字段的理由：每多一个字段就多一条"这个字段在某个门店是空的"
// 的分支，而空值的处理方式往往不一致，最终表现为"排除规则偶尔不生效"。
package supplier

// Record 是一个供应商的字典记录。
//
// ⚠️ 字段名是**思迅语义**，不是本系统语义：
//
//	CustFlag 对应源列 t_bd_supcust_info.supcust_flag，
//	S=供应商 / C=客户 / A=其他。
//
// 保留源字段名而不是改名成 IsSupplier( bool )，是因为那会丢掉
// "未知取值"这个状态 —— 而未知必须被区别对待（见 fact.ClassifySupplier）。
//
// ⚠️ json tag 与思迅源列名一致（cust_flag / supply_way），
//   而不是本系统语义（is_supplier / sale_way）：
//   这些记录会经 HTTP 传给 App 做推荐（handler/ignore_api.go），
//   与源列同名能让运营/开发在两边对照同一个字段。
//   驼峰写法（custFlag）则会被 Go 默认匹配悄悄吃掉 ——
//   encoding/json 不区分大小写匹配，tag 写错不报错，只是永远取不到值。
type Record struct {
	// Code 是供应商编码，对应 t_bd_supcust_info.supcust_no。
	Code string `json:"code"`
	// Name 是供应商名称。
	Name string `json:"name"`
	// Type 对应源列 sup_type。
	//
	// ✅ 2026-10-16 实测 ysx：1=普通供应商(214 条) / 5=自用直供(4 条) / xx=异常(1 条)。
	//   ⚠️ cube supplier schema 里 enum 曾把 1 标成"客户"——**那是错的**，
	//   已在 2026-10-16 修正。写这里的注释是为了防止再被改回去。
	Type string `json:"type"`
	// CustFlag 对应源列 supcust_flag（S/C/A）。
	CustFlag string `json:"cust_flag"`
	// SupplyWay 对应 sale_way，仅用于展示。
	SupplyWay string `json:"supply_way"`
}
