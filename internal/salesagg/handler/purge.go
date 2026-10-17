// purge.go —— sales-agg 门店对比模板的兜底清理（契约测试第二层 sweep）。
//
// 第一层清理挂在每个测试的 t.Cleanup 上（DELETE /compare-templates/:id），
// 但进程被 Ctrl-C / kill / panic 时一行都不执行 —— 而契约测试打的是生产，
// 那一次跑造出来的模板就永久留下了。
//
// 模板无子表、无在途状态（没有后台流水线在写它），registry 里只有
// 一张表、不需要 KeepSQL —— 比 settlement-auditor 那份简单得多。
package handler

import (
	"github.com/YunBright/core/purge"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/salesagg/compare"
)

// PurgeRegistry 是 sales-agg 的可清理实体清单（只有模板一张表）。
func PurgeRegistry() *purge.Registry {
	return purge.NewRegistry(
		purge.Model{Table: compare.Template{}.TableName(), Model: &compare.Template{}},
	)
}

// RegisterPurgeRoutes 挂 POST /internal/datastore/purge。
//
// 免鉴权，且公网**不可达**：nginx 有
// `location ^~ /api/v1/sales-agg/internal/ { return 404; }`
// （回归锁见 deployer/tests/config/nginx_internal_test.go）。
// 与 stocktake / settlement-auditor 同一范式 —— 端点注册在 RegisterDapr
// 窗口（auth 中间件之前），因为收尾清理时未必还持有 token。
func RegisterPurgeRoutes(r *gin.Engine, db *gorm.DB) {
	r.POST("/internal/datastore/purge", purge.Handler(db, PurgeRegistry()))
}
