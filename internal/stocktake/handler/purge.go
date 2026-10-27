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

// PurgeRegistry 是 stocktake 的可清理实体清单(6 张表都要列)。
//
// ⚠️ 顺序不是正确性要求,是**报告准确性**要求:从表在前,否则主表先删触发
// CASCADE 后从表 Deleted 显示 0,会被读成"这张表没清过"。
// ⚠️ 登记的表必须有**单列主键**(复合主键进不了 purge registry)。
// ⚠️ sweep 不做 approved 闸门(那是给真实单据的保护);本次运行的账已回答
// "哪些行是测试造的"。
func PurgeRegistry() *purge.Registry {
	return purge.NewRegistry(
		purge.Model{Table: "stocktake_lines", Model: &model.StocktakeLine{}},
		purge.Model{Table: "stocktake_line_operations", Model: &model.StocktakeLineOperation{}},
		purge.Model{Table: "stocktake_plan_items", Model: &model.StocktakePlanItem{}},
		purge.Model{Table: "stocktake_branch_defaults", Model: &model.StocktakeBranchDefault{}},
		purge.Model{Table: "stocktake_headers", Model: &model.StocktakeHeader{}},
		// 排除域配置。
		//
		// 登记它只是为了**兜底**：清理按“本次运行记过账的行”定位，
		// 而 seed 装配的配置行是在**没有 run_id 的上下文**里写的
		// ⇒ 不会被记账 ⇒ 本次运行删不到它们 ⇒ 不会被误删。
		// 反过来，若哪天有人加了“运行期改排除域”的端点，漏登记就会留下
		// 永远清不掉的行（且不报错）—— 先登记上，这条隐患就不存在了。
		purge.Model{Table: "stocktake_excluded_scopes", Model: &model.StocktakeExcludedScope{}},
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
