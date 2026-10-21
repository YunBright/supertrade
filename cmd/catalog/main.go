// Package main 是 catalog dapr app 入口(SKU / 品类 / 单位 / 条码 / 供应商)。
//
// 启动:`dapr run --app-id catalog --app-port 8103 -- go run ./cmd/catalog`
//
// 本期定位(2026-09 重构后):
//   - 拥有本地 suppliers 表 + products 表(本系统数据)
//   - products/search 走"本地表 + cube 兜底"双层(数据迁移期)
//   - cube 转发仍保留(兜底用)—— 由 cubeclient 直接调用,不走 cube-router
//     (cube-router 收口的是 stocktake.SearchProducts 等高级查询;catalog
//     本期直接调 cube 即可)
//
// 端口分配:
//
//	catalog      :8103    inventory :8105    stocktake    :8106
//
// 配置:
//
//	POSTGRES_DSN    必填(只支持 PostgreSQL)
//	CUBE_APP_ID      = "supertrade-cube-router" // 默认(多源路由);CUBE_QUERY_PATH = "v1/load"
//	CUBE_APP_ID      = "cube-router"   // 默认 (走 cube-router 多源路由)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/catalog/db"
	"github.com/YunBright/supertrade/internal/catalog/handler"
	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubehttp"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID:    "catalog",
		Port:     cmdbootstrap.AppPort(":8103"),
		OnStart:  initApp,
		Register: registerRoutes,
	})
}

// appDeps 持有运行时单例。
var (
	appDB   *gorm.DB
	appSup  *service.SupplierService
	appProd *service.ProductService
	appCat  *service.CategoryService
	appBar  *service.ProductBarcodeService
	appArc  *service.ArchiveService
	appCube cubeclient.Client
	appUser *userinfo.Client
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("catalog: POSTGRES_DSN 必填(只支持 PostgreSQL)")
	}

	var err error
	appDB, err = db.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.Supplier{},
		&model.Product{},
		&model.ProductBarcode{},
		&model.Category{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	appSup = service.NewSupplierService(appDB)
	appProd = service.NewProductService(appDB)
	appCat = service.NewCategoryService(appDB)
	appBar = service.NewProductBarcodeService(appDB)
	appArc = service.NewArchiveService(appDB)

	// cube client 用作 products/search 兜底(本期未实装本地 product CRUD)
	if appCube, err = cubehttp.NewClient(); err != nil {
		return fmt.Errorf("cube client: %w", err)
	}
	appUser, err = userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w", err)
	}

	// Warmup userd 跨主机冷握手(Consul DNS + mTLS + HTTP/2 SETTINGS + userd 进程
	// DB pool),实测首次 7~12s 撞 userinfo.Client.timeout 默认 10s。提前烧掉,
	// 业务请求进来时已 warm。失败仅 warn,业务首次走 cold path(已知行为)。
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := appUser.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败,业务首次 userd 调用将走 cold path",
			"err", err)
	} else {
		slog.Info("userinfo warmup ok (userd 跨主机链路预热完成)")
	}
	return nil
}

func registerRoutes(r *gin.Engine) {
	if appSup == nil {
		panic("catalog: appSup 未初始化")
	}
	// 全局挂 X-Branch-ID 解析(供 supplier / product handler 用)
	r.Use(middleware.XBranchID())
	// 把 caller 的 Authorization header 注入 ctx,后续 cubeclient / userinfo 内部
	// 自动 forward 给下游(cube-gateway / userd)。userd 端 middleware.http.bearer
	// 必需要 Authorization 头,无 token 会 401 Unauthenticated。
	r.Use(forwardBearerToOutgoing())
	handler.New(appSup, appProd, appCat, appBar, appArc, appCube, appUser, nil).RegisterRoutes(r)
}

// forwardBearerToOutgoing 把 gin request 的 Authorization / X-Branch-ID header 注入 ctx。
//
// 用 cubeclient.WithBearer + userinfo.WithBearer 双写,两者各自的 dapr 调用路径里
// 会读 ctx 拼到 outgoing gRPC metadata → sidecar 转 outgoing HTTP Authorization 给
// 目标 app。这是跨 dapr app 调用透传 caller JWT 的标准做法(同 cube-gateway 调用的
// cubeclient.WithBearer 模式)。
//
// 空 header 表示无 token(内部 job 路径),下游会以 401 拒绝,符合预期。
//
// # ⚠️ X-Branch-ID 必须一起注入(2026-10-16 修复的生产故障)
//
// middleware.XBranchID() 只把门店写进 **gin.Context**,不动 Go 的 request.Context。
// 而 cubeclient.LoadCubeQuery 是从 **request.Context** 里读门店,再拼成 outgoing
// gRPC metadata `x-branch-id` 发给 cube-router。两者不是同一个 context ——
// 少注入一次,cube-router 就收不到 X-Branch-ID,直接返 400 `branch_required`,
// dapr sidecar 把这个 400 折叠成 gRPC `Internal: Bad Request`,catalog 的错误
// 分类器再把它当成基础设施故障翻成 502 `cube_error`。
//
// 对外表现:`GET /catalog/products/search?barcode=X` 对**任何**条码都失败,
// 报"库存服务暂不可用",而 cube / cube-router / 源库全部健康 ——
// curl 直连 cube-router 打**同样的 body** 却 200。
//
// 这是 catalog 独有的:inventory / stocktake / sales-agg / settlement-auditor
// 四个服务都注入了(见各自 handler 的 cubeclient.WithBranchID 调用点),
// 只有 catalog 漏了,于是盘点扫码能查商品、价签扫码查不了。
//
// 为什么不把 branchID 作为函数参数传下去:SearchProductsByBarcode 的 branchID
// 参数只用于填返回的 DTO,真正决定"路由到哪家门店的 cube"的是这个 outgoing
// metadata,两者必须同源,否则会出现"DTO 说 01、实际查了 01 的 cube"之外的
// 不一致组合(请求头与单记录不一致时,stocktake 的注释已记录过这个坑)。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
		}
		// 原样透传(含 `*` / `01,02` 多店),解析语义交给 cube-router。
		if branchID := c.Request.Header.Get("X-Branch-ID"); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
