// Package model / errors.go —— pricing 的逐行导入错误。
package model

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
