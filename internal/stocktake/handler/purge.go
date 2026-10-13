// purge.go —— stocktake 的兜底清理（契约测试第二层 sweep）。
//
// # 它补的是什么洞
//
// 第一层清理挂在每个测试的 t.Cleanup 上（DELETE /stocktake-headers/:id）。
// 那层精确，但**进程被 Ctrl-C / kill / panic 时一行都不执行** —— 而契约
// 测试打的是生产，于是那一次跑造出来的盘点单就永久留下了。
//
// # 为什么这个服务的级联最麻烦
//
// stocktake 有 4 张从表挂在 stocktake_headers 上，其中 3 张**没有外键**：
//
//	stocktake_lines             有 constraint:OnDelete:CASCADE
//	stocktake_line_operations   无 FK
//	stocktake_plan_items        无 FK
//	stocktake_branch_defaults   无 FK
//
// 无 FK 意味着删主表**不会**带走它们 —— 少了哪张就留下孤儿行，而孤儿行会让
// "门店默认盘点单"指向一张已删的单（P1-2 已在业务删除路径上踩过）。
//
// # 旁表记账把这件事变简单了
//
// 溯源在旁表 testrun_rows 里按 (run_id, 表名, 主键) 记账，每张表
// （**包括从表**）都被独立记账。清理只按各自记下的主键删，于是：
//
//  1. 不需要"父 id ∈ (本次运行的父 id)"那种间接定位；
//  2. **registry 的顺序不再是语义**。第一版里"从表必须排在主表之前"
//     是硬不变量，违反时表现是**从表一行都没删且不报错**，报告干干净净。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/stocktake/model"
)

// PurgeRegistry 是 stocktake 的可清理实体清单。
//
// 5 张表全都要列（理由见文件头）。
//
// # 顺序：不是正确性要求，是报告准确性要求
//
// 第一版里"从表必须排在主表之前"是**正确性**硬不变量（违反 → 从表一行都没删
// 且不报错）。旁表记账让那条约束消失了 —— 每张表都按自己的账定位。
//
// 但顺序仍然影响**报告**：`stocktake_lines` 声明了 `constraint:OnDelete:CASCADE`，
// 主表先删时 DB 会把从表行级联掉，于是轮到从表时它已经数不出任何行 ——
// `Deleted["stocktake_lines"]` 显示 0，而行其实已经没了。
//
// 清理结果没错，但那个 0 会被读成"这张表没清过"。所以从表仍然排在前面。
// 另外 3 张无 FK 的从表顺序无所谓。
//
// 关于 approved 状态：业务删除路径（service.DeleteHeader）对 approved 返
// 409，那是给**真实**盘点单的保护。sweep 不需要这道闸门 ——
// 本次运行的账已经回答了"哪些行是测试造的"，而生产流量压根不记账。
// 留着 approved 的测试盘点单只会让残留持续累积，那正是这次治理要消灭的东西。
//
// 为什么没有"在途状态"跳过：stocktake 的 header 由用户 API 同步读写，
// **没有后台流水线在写它**。需要跳过的是 settlement-auditor 的审批流水线。
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
// 免鉴权，且公网**不可达**（两道防线，与 /internal/stocktake-plan-headers 同款）：
//  1. nginx `location ^~ /api/v1/stocktake/internal/ { return 404; }`
//     —— 回归锁见 deployer/tests/config/nginx_internal_test.go
//  2. 注册在 RegisterDapr 窗口（auth 中间件之前），调用方走 dapr 内网 invoke。
//
// 判据：免鉴权的删除端点，公网必须由**网络入口**挡死，不能指望应用层
// 记得区分"来自 nginx 的公网请求"与"来自 dapr 的内网 invoke"。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
