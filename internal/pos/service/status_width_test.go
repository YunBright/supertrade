package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/YunBright/supertrade/internal/pos/service"
)

// TestStatusValuesFitColumnWidth 锁"**状态值必须塞得进列宽**"。
//
// # 为什么要有这条
//
//	`sales_orders.status` 的列宽是 **varchar(16)**(生产实测)。
//	⇒ 状态值超过 16 字符 ⇒ PG 报
//	  `value too long for type character varying(16) (SQLSTATE 22001)`
//	⇒ 整个事务回滚 ⇒ 端点返 **500 internal_error**
//	⇒ 症状("退款 500")与真因("状态值多写了两个字母")**完全无关**。
//
//	2026-10-22 实测:第一版写的是 `partially_returned`(18 字符)
//	⇒ 生产上部分退货一笔都退不了 ⇒ 现场只看到 500。
//
// ⇒ 凡是会写进**定长 varchar**列的枚举值,都要过这条。
const statusColumnWidth = 16

func TestStatusValuesFitColumnWidth(t *testing.T) {
	for _, s := range []string{
		service.StatusDraft,
		service.StatusPaid,
		service.StatusReturned,
		service.StatusPartiallyReturned,
		service.StatusVoid,
	} {
		assert.LessOrEqual(t, len(s), statusColumnWidth,
			"状态值 %q 长度 %d 超过 sales_orders.status 的列宽 %d ⇒ 生产上会 500",
			s, len(s), statusColumnWidth)
	}
}
