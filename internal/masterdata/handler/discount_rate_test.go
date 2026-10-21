package handler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件锁住"折扣率"的解析口径。
//
// # 为什么单独一个文件
//
// 折扣率直接决定"这个收银员能打几折" ⇒ 它被**静默改成 1** = 折扣权限失效,
// 而且**不报错**、界面上看不出来 ⇒ 必须有守卫测试。

// TestParseRate_InvalidRejected 锁"非法折扣率必须报错,不能静默当 1"。
//
// ⚠️ 旧写法:非法串 → parseAmount 记日志后当 0 → parseRate 再把 0 转成 1
// ⇒ 调用方传 "abc" 会**静默拿到"不打折"**。
func TestParseRate_InvalidRejected(t *testing.T) {
	for _, s := range []string{"abc", "1.5", "-0.1"} {
		_, err := parseRate(s)
		require.Error(t, err, "discount_rate=%q 必须被拒", s)
	}
	// 正常值。
	d, err := parseRate("0.95")
	require.NoError(t, err)
	assert.Equal(t, "0.95", d.String())
}

// TestParseRate_EmptyMeansDefault 锁"空串 = 不传 ⇒ 默认 1(不打折)"。
func TestParseRate_EmptyMeansDefault(t *testing.T) {
	d, err := parseRate("   ")
	require.NoError(t, err, "空串应走默认值")
	assert.Equal(t, "1", d.String())
}

// TestParseRate_ZeroAllowed 锁"0 折是合法输入(照传,不静默改成 1)"。
func TestParseRate_ZeroAllowed(t *testing.T) {
	d, err := parseRate("0")
	require.NoError(t, err)
	assert.Equal(t, "0", d.String())
}
