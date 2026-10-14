// purge.go —— stocktake 的兜底清理（契约测试第二层 sweep）。
//
// 第一层清理挂在每个测试的 t.Cleanup 上（DELETE /stocktake-headers/:id），
// 但**进程被 Ctrl-C / kill / panic 时一行都不执行**——而契约测试打的是生产，
// 于是那一次跑造出来的盘点单就永久留下了。
//
// 这个服务的级联最麻烦：4 张从表挂在 stocktake_headers 上，其中 3 张**没有外键**
// （stocktake_line_operations / stocktake_plan_items / stocktake_branch_defaults），
// 删主表带不走它们，少清一张就留下孤儿行，而孤儿行会让"门店默认盘点单"指向一张
// 已删的单。
//
// 旁表记账让这件事变简单：每张表（包括从表）都被独立记账，清理只按各自记下的
// 主键删，于是不需要"父 id ∈ (本次运行的父 id)"那种间接定位，registry 的顺序
// 也不再是正确性要求。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/stocktake/model"
)

// PurgeRegistry 是 stocktake 的可清理实体清单（5 张表都要列，原因见文件头）。
//
// # 顺序：不是正确性要求，是报告准确性要求
//
// 从表排在前面是因为 `stocktake_lines` 声明了 `constraint:OnDelete:CASCADE`：
// 主表先删时 DB 会把从表行级联掉，轮到从表时它已经数不出任何行，于是
// `Deleted["stocktake_lines"]` 显示 0——清理其实成功了，但那个 0 会被读成
// "这张表没清过"。另外 3 张无 FK 的从表顺序无所谓。
//
// 关于 approved 状态：业务删除路径（service.DeleteHeader）对 approved 返 409，
// 那是给**真实**盘点单的保护；sweep 不需要这道闸门——本次运行的账已经回答了
// "哪些行是测试造的"，而生产流量压根不记账。留着 approved 的测试盘点单只会让
// 残留持续累积，那正是这次治理要消灭的东西。
//
// 没有"在途状态"跳过：stocktake 的 header 由用户 API 同步读写，**没有后台
// 流水线在写它**。需要跳过的是 settlement-auditor 的审批流水线。
func PurgeRegistry() *purge.Registry {
	return purge.NewRegistry(
		purge.Model{Table: "stocktake_lines", Model: &model.StocktakeLine{}},
		purge.Model{Table: "stocktake_line_operations", Model: &model.StocktakeLineOperation{}},
		purge.Model{Table: "stocktake_plan_items", Model: &model.StocktakePlanItem{}},
		purge.Model{Table: "stocktake_branch_defaults", Model: &model.StocktakeBranchDefault{}},
		purge.Model{Table: "stocktake_headers", Model: &model.StocktakeHeader{}},
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权，且公网**不可达**（与 /internal/stocktake-plan-headers 同款）：
//  1. nginx `location ^~ /api/v1/stocktake/internal/ { return 404; }`
//     —— 回归锁见 deployer/tests/config/nginx_internal_test.go
//  2. 注册在 RegisterDapr 窗口（auth 中间件之前），调用方走 dapr 内网 invoke。
//
// 判据：免鉴权的删除端点，公网必须由**网络入口**挡死，不能指望应用层记得区分
// "来自 nginx 的公网请求"与"来自 dapr 的内网 invoke"。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
