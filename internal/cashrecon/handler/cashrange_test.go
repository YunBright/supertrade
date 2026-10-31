package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestCashRange_OneErrorBody 校验错误分支**只写一次** JSON 错误体。
//
// 回归用例：`?date=20261030` 曾在 date 解析失败后又回落查 from/to，
// 两次 AbortWithStatusJSON 把两个 JSON 拼进同一个 body ⇒ 客户端解析失败、
// error.code 退化成 "Bad Request"（线上契约测试发现的）。
func TestCashRange_OneErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name     string
		query    string
		wantOK   bool
		wantFrom string
		wantTo   string
		wantCode string
	}{
		{"单日", "?date=2026-10-30", true, "2026-10-30", "2026-10-30", ""},
		{"区间", "?from=2026-01-01&to=2026-03-31", true, "2026-01-01", "2026-03-31", ""},
		{"date 格式非法", "?date=20261030", false, "", "", "bad_request"},
		{"date 带斜杠", "?date=2026/10/30", false, "", "", "bad_request"},
		{"只给 from", "?from=2026-01-01", false, "", "", "bad_request"},
		{"只给 to", "?to=2026-03-31", false, "", "", "bad_request"},
		{"全缺", "", false, "", "", "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/flows/cash"+tc.query, nil)

			from, to, ok := cashRange(c)
			require.Equal(t, tc.wantOK, ok, "%s: ok", tc.name)
			if tc.wantOK {
				require.Equal(t, tc.wantFrom, from.Format("2006-01-02"), "%s: from", tc.name)
				require.Equal(t, tc.wantTo, to.Format("2006-01-02"), "%s: to", tc.name)
				return
			}

			require.Equal(t, http.StatusBadRequest, w.Code, "%s: 应 400", tc.name)
			// 关键：整个 body 必须能被**一次**反序列化（两段 JSON 会在这里失败）。
			var body errBody
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body),
				"%s: body 必须是单个 JSON 对象, 实际 %q", tc.name, w.Body.String())
			require.Equal(t, tc.wantCode, body.Code, "%s: error.code", tc.name)
		})
	}
}
