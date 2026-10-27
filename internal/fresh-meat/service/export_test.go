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
//   对外部测试包可见,但只在测试构建里存在。
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
