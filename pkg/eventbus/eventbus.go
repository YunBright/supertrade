// Package eventbus 收敛全平台 Dapr pubsub component 的名字。
//
// # 为什么必须有唯一真相源
//
// Dapr 的 pubsub **component name 是路由键**：发布是
// `POST /v1.0/publish/<name>/<topic>`，订阅方必须在 `/dapr/subscribe` 里
// 声明同一个 `<name>`。名字不同就是两条互不相通的 bus —— 即使两者指向
// 同一个 Redis Streams（流名由 `<name>-<topic>` 派生，也因此天然隔离）。
//
// 这个 bug 在本仓库真实发生过，而且**每一层单独看都正常**，因此完全静默：
//
//	sidecar 日志    "app is subscribed to [... auth.user.access_changed]
//	                 through pubsub=pubsub"          ✅
//	/dapr/subscribe 200，7 条订阅齐全                    ✅
//	WS 握手          101 + welcome，client registered    ✅
//	auth 侧 publish  成功，零报错                        ✅
//	→ 事件永远到不了 WS，因为不在同一条 bus 上。
//
// 2026-10-08 由 `deployer/tests/contract/notification_ws_test.go` 的
// TestNotificationWSDeliversAccessChanged 抓到（该断言 pubsubname 必须是
// [Name]，并跑通端到端投递）。
//
// # 全平台约定
//
// 唯一 bus 名是 [Name]。谁在发、谁在收，都用这一个常量，不要就地写字符串字面量。
//
//	auth (userd)         DAPR_PUBSUB_NAME=tradewind-pubsub   ← 生产 .env
//	auth                 components/pubsub.yaml  metadata.name
//	wechat-bot           components/pubsub.yaml  metadata.name
//	supertrade 的 3 个服务  dapr/components/<svc>/pubsub.yaml  metadata.name
//
// 新增 supertrade 服务时，把它的 component 文件 metadata.name 也写成 [Name]，
// 并且 systemd unit 要带 `--resources-path <app>/components`，否则 sidecar
// 一个 component 都加载不了（此时 sidecar 静默不订阅任何 topic）。
package eventbus

// Name 是全平台唯一的 Dapr pubsub component 名。
//
// 发布侧与订阅侧必须逐字相同，否则事件没有任何订阅者，且**不会报错**。
// 它同时是 Redis Streams 的流名前缀，改名会让旧流停止被消费 —— 这些是
// 瞬时事件流，直接过期即可，不需要迁移。
const Name = "tradewind-pubsub"