// Package main 是 pricing dapr app 入口(售价 / 促销 / 会员 / 改价审批)。
//
// 启动:`dapr run --app-id supertrade-pricing --app-port 8111 -- go run ./cmd/pricing`
//
// # 2026-10-15:从空壳变成第一个真实端点
//
// 此前本服务只有一个 `GET /price-lists` 返空数组,且**没有 systemd unit、
// 没有 nginx 路由、从未部署过**。本次为 POP 价签打印落地促销查询,
// 把它变成一个真正在跑的 dapr app。
//
// # 2026-10-21:开始**拥有表**(price_lists / promotions)
//
// 此前 pricing **不需要 POSTGRES_DSN**:它不拥有任何表,全部数据实时来自
// cube(product 档案 + promotion 促销)。
//
// 现在拥有表,原因是**自营门店(门店 10)没有 cube 源**
// (`branch_cube_sources` 实测只有 `00` / `01`)
// ⇒ 门店 10 的售价/促销**没有别的地方可取**。
//
// ⇒ 读路径变成"**配了数据源 ⇒ cube;没配 ⇒ 本地 PG**":
//   - 门店 00 / 01(富百家 / 佳乐,配了数据源):cube(实时价与促销)
//   - 门店 10(自营,没配数据源):本域 PG
//
// ⚠️ 判据**不是**"本地有没有数据"(旧判据):
// 门店 00 的价格已被主同步实例(00→00)灌进本地 PG(27565 行,快照 2026-10-20 19:24)
// ⇒ 旧判据让查价命中本地 ⇒ 返回**两天前的快照价**。
// ⇒ 判据统一到"配没配数据源"(与 member / catalog / inventory 同一套,
// 见 internal/cubesource)。
//
// ⚠️ **POSTGRES_DSN 缺失即拒绝启动** ——
// 缺数据库却照常起来,症状是"服务正常、查价永远走 cube",
// 而门店 10 的 cube 路径必然 503 ⇒ 现场看到的是「库存服务暂不可用」,
// 真因(DSN 没配)不出现在任何日志里。
//
// 端口分配(8101~8110 已被占用,8111 起):
//
//	notification-gateway 8101   master-data        8102   catalog        8103
//	inventory            8105   stocktake          8106   cube-router    8107
//	fresh-meat           8108   settlement-auditor 8109   sales-agg      8110
//	pricing              8111
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/db"
	"github.com/YunBright/supertrade/internal/pricing/handler"
	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
)

// appDeps 持有运行时单例(在 initApp 里建立,registerRoutes 里消费)。
var (
	appCube       cubeclient.Client
	appPrice      *service.PriceService
	appPriceLists *service.PriceListService
	appPromos     *service.PromotionService
	appUser       *userinfo.Client
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "supertrade-pricing",
		Port:     cmdbootstrap.AppPort(":8111"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

func initApp() error {
	var err error
	if appCube, err = cubehttp.NewClient(); err != nil {
		return fmt.Errorf("cube client: %w", err)
	}

	// ---- 本地价格表(pricing 从 2026-10-21 起拥有表)----
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("pricing: POSTGRES_DSN 必填(pricing 拥有 price_lists / promotions 表;无 fallback)")
	}
	appDB, err := db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.PriceList{},
		&model.Promotion{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}
	appPriceLists = service.NewPriceListService(appDB)
	appPromos = service.NewPromotionService(appDB)

	// 读路径:**配了数据源 ⇒ cube;没配 ⇒ 本地 PG**(判据唯一,见 internal/cubesource)。
	//
	// ⚠️ **解析器与 cube 读端必须一起注入**,且**复用同一个 client**:
	// 只注入解析器 ⇒ 走 cube 但没有读端;只注入读端 ⇒ 有数据源的门店仍走本地
	// ⇒ 症状与"没接数据源"完全一样(查价返回旧快照),而且**没有任何报错**。
	//
	// ⚠️ cubeclient.Client 接口**不含** ResolveBranchSource / LoadCubeQueryPage
	// (那是被"查一个商品"用的窄接口)⇒ 这里要拿到**具体类型**。
	// 断言失败就**拒绝启动**:带着半套装配跑起来 = 静默走错路。
	daprCube, ok := appCube.(*cubeclient.DaprCubeClient)
	if !ok {
		return fmt.Errorf("pricing: cube client 类型不是 *cubeclient.DaprCubeClient,装配有误")
	}
	appPrice = service.NewPriceService(appCube).WithStore(appPriceLists, appPromos)
	// 解析器带 60s 缓存 + stale-on-error:每次读都打一次 dapr 往返不划算,
	// 而一次网络抖动不该让整个门店的服务挂掉。
	appPrice.SetCubeSource(
		cubesource.NewCachedResolver(daprCube, cubesource.DefaultTTL, slog.Default()),
		daprCube,
	)
	slog.Info("pricing: 门店数据源路由已启用", "app_id", cubeclient.DefaultCubeAppID())

	if appUser, err = userinfo.New("userd"); err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(与 catalog 同一个理由:实测首次 7~12s
	// 会撞 userinfo.Client.timeout 默认 10s)。提前烧掉,业务首次即 warm。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok")
	}
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appPrice == nil {
		panic("pricing: appPrice 未初始化")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	handler.New(appPrice, appPriceLists, appPromos, appUser, nil).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 caller's Authorization / X-Branch-ID 注入 ctx,
// 让 cubeclient / userinfo 内部调用自动 forward 给下游
// (sidecar 把 outgoing gRPC metadata 转成 outgoing HTTP 头)。
//
// 与 catalog/main.go、inventory/main.go 同构。空 header(内部 job 路径)下游会 401,符合预期。
//
// # ⚠️ X-Branch-ID 必须一起注入(2026-10-16)
//
// middleware.XBranchID() 写的是 **gin.Context**,而 cubeclient.LoadCubeQuery 从
// **request.Context** 读门店再拼 outgoing metadata。少注入一次,cube-router 就收不到
// X-Branch-ID,返 400 `branch_required`,sidecar 把它折叠成 gRPC `Internal: Bad Request`,
// pricing 的错误映射再当成上游故障翻成 503 `cube_unavailable` —— 现场表现是
// "库存服务暂不可用",而 cube / cube-router / 源库全部健康。
//
// 同一天在 catalog 上踩到的同一个坑(它的 /products/search 对任何条码都失败)。
// 代价是一次真机部署 + 一次端到端验证,所以这里在写第一行 handler 时就带上。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
		}
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
