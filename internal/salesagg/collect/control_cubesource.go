// control_cubesource.go — cube-router 错误词汇 → 本包哨兵的**唯一**翻译点。
//
// 单独成文件是为了让"跨包耦合"这件事在一个地方一目了然：
// 将来 cube-router 改了表结构/错误，只有这个文件需要跟着改。
package collect

import (
	"errors"

	cubermodel "github.com/YunBright/supertrade/internal/cube-router/model"
)

// translateCubeSourceErr 把 cube-router 的哨兵翻译成本包认识的两个。
//
// # 为什么"查错了"与"查到了但禁用"必须是两个哨兵
//
// 前端对这两种情况的引导完全不同：
//
//	ErrCubeSourceNotConfigured → 弹「去配置数据源」链接（这是用户的下一步）
//	ErrCubeSourceDisabled      → 弹「联系管理员启用」（用户点链接也没用）
//
// 合并成一句"数据源不可用"的话，用户会反复去点配置链接，
// 而配置早就存在了 —— 他会以为这个功能坏了。
func translateCubeSourceErr(err error) error {
	switch {
	case errors.Is(err, cubermodel.ErrNotFound):
		return ErrCubeSourceNotConfigured
	case errors.Is(err, cubermodel.ErrDisabled):
		return ErrCubeSourceDisabled
	default:
		// ⚠️ 其它错误**原样透出**而不是当成"未配置"：
		//   DB 故障被当成"去配置数据源"的话，用户会去点一个点了也没用的链接，
		//   而真因（数据库不可达）被完全掩盖。
		return err
	}
}
