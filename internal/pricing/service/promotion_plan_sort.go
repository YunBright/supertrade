// Package service / promotion_plan_sort.go —— 促销方案(图4)的**排序**契约。
//
// # 一处定义三个名字
//
// 每个可排序字段同时有三个名字:
//
//	Field       —— wire 值(前端 query 参数 `sort` 的取值)
//	CubeDim     —— cube 的全限定维度名
//	LocalColumn —— 本域 PG 的列名
//
// ⚠️ **必须一处定义**:两份映射表必然漂移,而漂移的症状是
// "走 cube 的门店能排序、走本地 PG 的门店排序静默失效"——
// 两边都是 200,顺序还不一样,**没有任何报错**。
//
// # 为什么**不**开放"按审核状态排序"
//
// 审核状态是**翻译后**的值:本域 PG 存的是 draft / approved / stopped,
// cube 侧存的是**源码** approve_flag('1' / '2' / '3')。
// ⇒ 按状态排序时两边的顺序不一样(源码序 1<2<3,译文序 approved<draft<stopped)
// ⇒ 同一个请求在两种门店得到两种顺序,而且两边都"看起来对"。
// ⇒ 不开放这个排序字段(要按状态排,前端按 approve_status 过滤更准)。
//
// # 为什么 NULL 的位置要显式对齐
//
// SQL Server(cube 侧)把 NULL 当**最小值**:ASC 时 NULL 在前、DESC 时在后。
// PostgreSQL(本域 PG)默认把 NULL 当**最大值**:正好相反。
// ⇒ 不处理的话"按审核日期排"在两种门店会给出**相反**的结果
//   (未审核的方案没有审核时间 ⇒ 空值占多数)。
//
// ⇒ 本域 PG 的 ORDER BY 用 CASE 把 NULL 摆到与 T-SQL 一样的位置,
//   见 [promotionPlanLocalOrderExpr]。
package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// 促销方案的**可排序字段**(wire 值 = 前端 query 参数 `sort` 的取值)。
//
// ⚠️ 这些常量是**跨仓契约**:前端 `lib/domain/pricing/promotion_plan_sort.dart`
// 与 deployer 的契约测试都按字面使用 ⇒ 改名要三处一起改。
const (
	// PromotionPlanSortPlanNo 按方案编号排。
	PromotionPlanSortPlanNo = "plan_no"
	// PromotionPlanSortPlanName 按方案名称排。
	PromotionPlanSortPlanName = "plan_name"
	// PromotionPlanSortBeginDate 按开始日期排。
	PromotionPlanSortBeginDate = "begin_date"
	// PromotionPlanSortEndDate 按结束日期排。
	PromotionPlanSortEndDate = "end_date"
	// PromotionPlanSortCreatedAt 按制单日期排(**默认**)。
	PromotionPlanSortCreatedAt = "created_at"
	// PromotionPlanSortCreatedBy 按制单人排。
	PromotionPlanSortCreatedBy = "created_by"
	// PromotionPlanSortApprovedAt 按审核日期排。
	PromotionPlanSortApprovedAt = "approved_at"
	// PromotionPlanSortStoppedAt 按终止日期排。
	PromotionPlanSortStoppedAt = "stopped_at"
)

const (
	// PromotionPlanDefaultSort 是默认排序字段:**制单日期**。
	//
	// 方案单是"越新越要管"的表 ⇒ 默认看最新的那一批。
	PromotionPlanDefaultSort = PromotionPlanSortCreatedAt
	// PromotionPlanDefaultOrder 是默认排序方向:**倒序**(最新的排前面)。
	PromotionPlanDefaultOrder = "desc"
)

// promotionPlanSortColumn 是"一个可排序字段的三个名字"。
type promotionPlanSortColumn struct {
	// Field 是 wire 值(前端 `sort` 参数的取值)。
	Field string
	// CubeDim 是 cube 的**全限定**维度名。
	//
	// ⚠️ cube 的 order 成员必须**同时出现在 SELECT 里**
	// (ORDER BY 只能引用输出列,否则 cube 返 400)⇒ 这里列的每个维度
	// 都必须同时在 promotionPlanDims() 里。
	CubeDim string
	// LocalColumn 是本域 PG 的列名。
	LocalColumn string
}

// promotionPlanSortColumns 是排序字段白名单。
//
// ⛔ 不接受任意列名 / 表达式(那是 SQL 注入面)⇒ 只认这张表。
var promotionPlanSortColumns = []promotionPlanSortColumn{
	{Field: PromotionPlanSortPlanNo, CubeDim: "promotion_plan.plan_no", LocalColumn: "plan_no"},
	{Field: PromotionPlanSortPlanName, CubeDim: "promotion_plan.plan_name", LocalColumn: "plan_name"},
	{Field: PromotionPlanSortBeginDate, CubeDim: "promotion_plan.begin_date", LocalColumn: "begin_date"},
	{Field: PromotionPlanSortEndDate, CubeDim: "promotion_plan.end_date", LocalColumn: "end_date"},
	{Field: PromotionPlanSortCreatedAt, CubeDim: "promotion_plan.created_at", LocalColumn: "created_at"},
	{Field: PromotionPlanSortCreatedBy, CubeDim: "promotion_plan.created_by", LocalColumn: "created_by"},
	{Field: PromotionPlanSortApprovedAt, CubeDim: "promotion_plan.approved_at", LocalColumn: "approved_at"},
	{Field: PromotionPlanSortStoppedAt, CubeDim: "promotion_plan.stopped_at", LocalColumn: "stopped_at"},
}

// PromotionPlanSortFields 返回全部可排序字段(wire 值,升序)。
//
// 供上层做白名单校验、拼错误提示与文档化。
func PromotionPlanSortFields() []string {
	out := make([]string, 0, len(promotionPlanSortColumns))
	for _, c := range promotionPlanSortColumns {
		out = append(out, c.Field)
	}
	sort.Strings(out)
	return out
}

// promotionPlanSortColumnOf 取排序字段对应的三个名字。
//
// ⚠️ 空 = 默认字段(制单日期);非法字段 = **400**,不是静默回落。
//
// 静默回落的后果:用户点了"按方案名称排",列表还是按日期排,
// 而且**没有任何提示**——与"参数被忽略"长得一模一样。
func promotionPlanSortColumnOf(field string) (promotionPlanSortColumn, error) {
	f := strings.TrimSpace(field)
	if f == "" {
		f = PromotionPlanDefaultSort
	}
	for _, c := range promotionPlanSortColumns {
		if c.Field == f {
			return c, nil
		}
	}
	return promotionPlanSortColumn{}, fmt.Errorf("%w: 排序字段 %q 不允许(可用: %s)",
		model.ErrInvalidInput, field, strings.Join(PromotionPlanSortFields(), ", "))
}

// promotionPlanSortDirection 归一排序方向。
//
// ⚠️ 空 = 默认(倒序);非 asc / desc = **400**。
//
// 方向是用户可控文本,静默回落成默认方向 = 用户以为按升序排、实际按降序。
func promotionPlanSortDirection(order string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(order)) {
	case "":
		return PromotionPlanDefaultOrder, nil
	case "asc":
		return "asc", nil
	case "desc":
		return "desc", nil
	default:
		return "", fmt.Errorf("%w: 排序方向 %q 不允许(只支持 asc / desc)",
			model.ErrInvalidInput, order)
	}
}

// promotionPlanSortTieBreakerDim 是排序的**兜底键**(cube 侧全限定维度名)。
//
// 排序列会并列 ⇒ 没有兜底键就不是全序 ⇒ 翻页会重复 / 漏行
// (症状是"第 2 页又看到第 1 页的某条")。
const promotionPlanSortTieBreakerDim = "promotion_plan.plan_no"

// promotionPlanCubeOrder 造 cube 的排序项。
//
// ⚠️ 末尾追加 plan_no 兜底:排序列会并列 ⇒ 没有兜底键就不是全序
// ⇒ 翻页会重复 / 漏行(症状是"第 2 页又看到第 1 页的某条")。
//
// ⚠️ **兜底键不能与排序列相同**(2026-10-23 修):
// MSSQL 的 ORDER BY 里同一列出现两次直接报
// "在排序依据列表中多次指定了某一列"(错误 169)
// ⇒ 整条查询 503 `cube_unavailable`。
// 症状:按"方案编号"排序时列表**整个打不开**,而按其它列排一切正常
// (⇒ 看起来像"编号列坏了",真实原因是兜底键撞了排序列)。
//
// ⚠️ 每个成员都**必须同时在 SELECT 里**——cube 的硬约束:
// ORDER BY 只能引用输出列,否则 GROUP BY 查询会在 DB 层报
// "not a grouped column"(一个调用方看不懂的 400)。
func promotionPlanCubeOrder(sortCol promotionPlanSortColumn, dir string) []cubeclient.CubeOrder {
	order := []cubeclient.CubeOrder{{ID: sortCol.CubeDim, Order: dir}}
	if sortCol.CubeDim != promotionPlanSortTieBreakerDim {
		order = append(order, cubeclient.CubeOrder{ID: promotionPlanSortTieBreakerDim, Order: "asc"})
	}
	return order
}

// promotionPlanLocalOrderExpr 造本域 PG 的 ORDER BY 片段。
//
// # 为什么 NULL 的位置要显式摆
//
// SQL Server(cube 侧)把 NULL 当**最小值**:ASC 时 NULL 在前、DESC 时在后。
// PostgreSQL(本域 PG)默认把 NULL 当**最大值**:正好相反。
//
// ⇒ 用 CASE 把 NULL 摆到与 T-SQL 一样的位置:
//
//	CASE WHEN col IS NULL THEN 0 ELSE 1 END ASC   -- ASC:NULL 在前
//	CASE WHEN col IS NULL THEN 1 ELSE 0 END ASC   -- DESC:NULL 在后
//
// (SQLite 也认这套语法 ⇒ 单测(SQLite)与生产(PG)行为一致。)
//
// ⚠️ 末尾追加 plan_no 兜底(与 cube 侧同款:并列 ⇒ 不是全序 ⇒ 翻页重复/漏行)。
//
// ⚠️ 兜底键**不能与排序列相同**(与 cube 侧同款理由):
// 重复列在 MSSQL 上是硬错误(169),在 PG 上虽不报错但纯属多余 ⇒ 两边都跳过。
func promotionPlanLocalOrderExpr(col, dir string) string {
	tie := ""
	if col != "plan_no" {
		tie = ", plan_no ASC"
	}
	if strings.EqualFold(dir, "asc") {
		return fmt.Sprintf(
			"CASE WHEN %s IS NULL THEN 0 ELSE 1 END ASC, %s ASC%s", col, col, tie)
	}
	return fmt.Sprintf(
		"CASE WHEN %s IS NULL THEN 1 ELSE 0 END ASC, %s DESC%s", col, col, tie)
}
