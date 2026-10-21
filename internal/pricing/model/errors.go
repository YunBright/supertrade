// Package model / errors.go —— pricing 的逐行导入错误 + 读路径的业务错误。
package model

import "errors"

// ---- 门店数据源路由(pricing 读路径的判据) ----
//
// ⚠️ 这三个错误对应**三件必须区分开的事**,混在一起就会把故障说成业务结论:
//
//	ErrCubeSourceUnavailable  问不到"这个门店配了哪个数据源"(链路故障)⇒ 503
//	ErrCubeSourceDisabled     配了但被管理员关停 ⇒ 503
//	ErrExternalSourceReadOnly 该门店的价格在外部系统里,本域只读 ⇒ 409
var (
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(503)。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店),
	// 问不到是**链路故障** ⇒ 后者必须让用户看到"暂不可用",
	// 而不是让它悄悄回落成本地(那会把一次故障显示成
	// 「这个商品没维护售价」——正是本次要修的那个症状)。
	ErrCubeSourceUnavailable = errors.New("pricing: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用(503)。
	ErrCubeSourceDisabled = errors.New("pricing: 该门店的数据源已停用")
	// ErrExternalSourceReadOnly:该门店的价格来自外部系统,本域只读(409)。
	ErrExternalSourceReadOnly = errors.New("pricing: 该门店的价格来自外部系统,本域只读")
	// ErrUnsupportedExportScope:导出端点不支持该资料域(400)。
	ErrUnsupportedExportScope = errors.New("pricing: 不支持的导出资料域")
	// ErrLocalStoreNotReady:本地价格表服务未装配(503)。
	//
	// ⚠️ 这是**部署错误**,不是业务结论:自营门店的价格只在本域 PG 里。
	// 报成"没维护售价"会把排查方向引到"数据没导"上,而真因是"服务没装表"。
	ErrLocalStoreNotReady = errors.New("pricing: 本地价格表未装配")
)

// ImportRowError 是**逐行**的导入失败明细。
//
// ⚠️ 逐行错误**不中断整体** —— 一批 27565 行的导入,
// 因为第 3 行有一条脏数据就整体回滚,等于没导。
//
// ⚠️ 金额解析失败**必须**进这里而不是被当成 0 ——
//
//	"售价 0 元"会被 POS 照印出去,而"这行没导进去"只会在报告里出现。
type ImportRowError struct {
	// Scope 是资料域(prices / promotions)。
	Scope string `json:"scope"`
	// ID 是出错的行标识(商品货号 / 促销单号)。
	ID string `json:"id"`
	// Error 是错误文案。
	Error string `json:"error"`
}
