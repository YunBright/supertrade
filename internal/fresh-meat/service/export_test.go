package service

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// export_test.go —— **仅测试用**的导出(不进生产二进制)。
//
// # 为什么需要
//
// 「按营业日推导的日终结余」(`closingForDay`)是**私有**方法,而它返回的是
// **部位级**的结余(2026-10-27 起)⇒ 外部测试包(package service_test)
// 需要一个入口才能断言「某营业日的冷库余额是多少」。
//
// ⚠️ 标准库的 export_test.go 同款做法:内部测试文件里的**导出**标识符
//
//	对外部测试包可见,但只在测试构建里存在。
//
// # 口径
//
// 返回 (柜台, 冷库) —— 都是**部位(cut_type) → kg**。
// ⚠️ 日终(夜里"过夜搬运"之后)⇒ 柜台已清空 ⇒ 柜台那一张永远是空的。
func ClosingByCutForTest(ctx context.Context, s *Service, branchID string, day time.Time) (
	counter, cold map[string]decimal.Decimal, err error,
) {
	counter, cold, _, err = s.closingForDay(ctx, branchID, day)
	return counter, cold, err
}

// ClampNonNegativeForTest 暴露 [clampNonNegative](负的期末 → 0)。
//
// ⚠️ 为什么它值得单独暴露:「读到负的期末要裁成 0」是 owner「期初不能有负数」
//
//	这条硬要求的**最后一道闸** —— 存量快照里真的还有旧口径写的负数
//	(生产实测 店 00 2026-10-29:8 个部位为负)
//	⇒ 不裁 ⇒ 负期初从快照/锚点漏回链里 ⇒ 只对新建门店成立。
func ClampNonNegativeForTest(kg, amount decimal.Decimal) (decimal.Decimal, decimal.Decimal) {
	return clampNonNegative(kg, amount)
}

// ForecastSplitSampleGapForTest 暴露 [forecastSplitSampleGap](分割样本缺口的判定)。
//
// ⚠️ 为什么它值得单独暴露:
//
//	「两个样本池都空 ⇒ 报缺:分割样本」这条判定**在端到端场景里几乎构造不出来**——
//	引擎很会把销售归到"无名桶"里,倒推值仍为正 ⇒ 总是有样本。
//	⇒ 而判据本身有个**容易写错**的地方:是"**都**空"才算缺,不是"任一为空"。
//	⇒ 把它抽成小函数 + 直接锁,比硬造场景可靠。
//
// 返回:空串 = 不缺;非空 = 缺口文案(门店看到的原话)。
func ForecastSplitSampleGapForTest(recordedN, estimatedN int) string {
	return forecastSplitSampleGap(recordedN, estimatedN)
}
