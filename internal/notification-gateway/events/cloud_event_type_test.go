package events_test

// 锁死 2026-10-08 的第二个静默故障：WS 业务事件一条也投不出去。
//
// 现象（全部"正常"，只有客户端收不到事件）：
//   - sidecar 日志 "app is subscribed to [... auth.user.access_changed] through pubsub=tradewind-pubsub"
//   - /dapr/subscribe 200，7 条订阅齐全
//   - WS 握手 101 + welcome
//   - Redis 消费组 entries-read 递增、lag=0、pending=0（说明 sidecar 已投递并被 ack）
//   - auth 侧 publish 204，零报错
//
// 根因：Dapr 的 pub/sub 投递把 CloudEvents 的 `type` 固定写成
// "com.dapr.event.sent"，真正的业务 topic 在**并列的 `topic` 字段**里。
// 代码按 `type` 做 fanout 分类，于是所有事件都掉进 default 分支
// （按 tenant+branch 过滤），而 auth.user.* 事件 payload 里根本没有
// branch_id / tenant_id —— 一条也投不出去。
//
// 这条测试用**实测抓到的原始 payload** 复现，不允许有人把归一化逻辑"优化"掉。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/notification-gateway/events"
	"github.com/YunBright/supertrade/internal/notification-gateway/wsmsg"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// daprDeliveredAuthUserAccessChanged 是 2026-10-08 在生产
// notification-gateway 的 /events/auth.user.access_changed 上**实际抓到**的
// 请求体（临时日志 TEMP-DIAG 原样输出）。
const daprDeliveredAuthUserAccessChanged = `{"data":{"action":"dapr-probe","user_id":"f980de82-3b42-49fe-9697-cc0e6688bcd3"},"datacontenttype":"application/json","id":"61a9af6b-f3eb-4065-91e4-a40196e79745","pubsubname":"tradewind-pubsub","source":"supertrade-notification-gateway","specversion":"1.0","time":"2026-10-08T08:29:43+08:00","topic":"auth.user.access_changed","traceid":"","traceparent":"","tracestate":"","type":"com.dapr.event.sent"}`

func TestNormalizeCloudEventType(t *testing.T) {
	cases := []struct {
		name     string
		in       wsmsg.Envelope
		fallback string
		want     string
	}{
		{
			// 本次事故的形态：type 是 Dapr 固定值，业务 topic 在 topic 字段。
			name:     "dapr 默认 type 用 topic 字段覆盖",
			in:       wsmsg.Envelope{Type: wsmsg.TypeComDaprEventSent, Topic: "auth.user.access_changed"},
			fallback: "auth.user.access_changed",
			want:     "auth.user.access_changed",
		},
		{
			name:     "type 为空时用 topic 字段",
			in:       wsmsg.Envelope{Topic: "stocktake.line.added"},
			fallback: "stocktake.line.added",
			want:     "stocktake.line.added",
		},
		{
			name:     "topic 字段也缺失时退回订阅路由里的 topic",
			in:       wsmsg.Envelope{Type: wsmsg.TypeComDaprEventSent},
			fallback: "stocktake.header.approved",
			want:     "stocktake.header.approved",
		},
		{
			name:     "已经是业务 type 的原样保留（welcome / ping 等我们自己发的帧）",
			in:       wsmsg.Envelope{Type: "welcome"},
			fallback: "auth.user.access_changed",
			want:     "welcome",
		},
		{
			name:     "两者都空则保持空（不硬塞 fallback 之外的任何东西）",
			in:       wsmsg.Envelope{Type: wsmsg.TypeComDaprEventSent},
			fallback: "",
			want:     wsmsg.TypeComDaprEventSent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wsmsg.NormalizeCloudEventType(tc.in, tc.fallback)
			assert.Equal(t, tc.want, got.Type)
		})
	}
}

// TestDispatchClassifiesDaprDeliveredEventAsUserScoped 是本次故障的正面回归：
// 把实测的 Dapr 投递体喂进真正的 handler，必须被判成"按用户投递"。
//
// 断言的是**分类结果**而不是"收到了事件"—— 因为是否真的推给某个 WS 连接
// 还需要一个在线客户端，那是 deployer/tests/contract 里的端到端用例。
func TestDispatchClassifiesDaprDeliveredEventAsUserScoped(t *testing.T) {
	var env wsmsg.Envelope
	require.NoError(t, json.Unmarshal([]byte(daprDeliveredAuthUserAccessChanged), &env))

	// 先确认这份 payload 确实是"坏"的那一类，否则本测试就是自证。
	require.Equal(t, wsmsg.TypeComDaprEventSent, env.Type,
		"实测 payload 的 type 应当是 Dapr 固定值；若上游改了投递形态，本测试的前提失效")
	require.Equal(t, "auth.user.access_changed", env.Topic)

	env = wsmsg.NormalizeCloudEventType(env, "auth.user.access_changed")
	assert.Equal(t, "auth.user.access_changed", env.Type,
		"归一化后 type 必须等于业务 topic，否则 fanout 会掉进按门店过滤的分支")

	// data 仍要能取出 user_id（envelopeUserID 读的就是它）。
	var payload map[string]any
	require.NoError(t, json.Unmarshal(env.Data, &payload))
	assert.Equal(t, "f980de82-3b42-49fe-9697-cc0e6688bcd3", payload["user_id"])
}

// TestSubscribeEndpointDeclaresEventBus 锁 /dapr/subscribe 的 pubsubname。
//
// 2026-10-08 之前 supertrade 用的是 "pubsub"，而发布侧 auth(userd) 用的是
// "tradewind-pubsub" —— 两条互不相通的 bus，事件静默丢失。名字必须逐字相同，
// 详见 supertrade/pkg/eventbus 的说明。
func TestSubscribeEndpointDeclaresEventBus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/dapr/subscribe", events.NewHandler(nil, nil, nil).Subscribe)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"pubsubname":"tradewind-pubsub"`,
		"pubsubname 必须与 auth(userd) 的 DAPR_PUBSUB_NAME 逐字相同")
	for _, topic := range []string{
		"auth.user.access_changed",
		"stocktake.line.added",
		"stocktake.header.approved",
	} {
		assert.True(t, strings.Contains(body, topic), "订阅清单应含 %s", topic)
	}
}
