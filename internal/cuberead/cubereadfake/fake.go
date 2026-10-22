// Package cubereadfake 提供 cuberead.Reader 的**测试替身**。
//
// # 为什么要有它
//
// 2026-10-22 这一批新增了 4 个读域(catalog 档案 / pricing 方案 /
// transaction 流水 / procurement 采购单),每个域都要断言"到底往 cube 发了
// 什么 query"。
//
// ⚠️ cubeclient.DaprCubeClient 收窄 daprInvoker 就是为了这个:
// 依赖面越窄,单测越能证明"发了什么"。
//
// 本替身只实现**一个方法**(LoadCubeQueryPage),并且
//
//	1. 记录每一次调用(model + 完整 query),供断言;
//	2. 由测试自己决定每个 model 返回什么行。
//
// ⇒ 测试可以断言"过滤条件真的进了 query"(而不只是"返回了 200"),
// 这正是 PLAN §4.1 强调的:「查询端点返回了数据"不等于"查询条件生效了」。
//
// ⚠️ 本包**只允许被 _test.go 引用**(与 cubeclientfake 同规矩)。
package cubereadfake

import (
	"context"
	"sync"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// Query 是记录下来的一次查询。
type Query struct {
	// Model 是被查的 cube model 名。
	Model string
	// Query 是完整的 cube 查询(measures / dimensions / filters / limit …)。
	Query cubeclient.CubeQuery
}

// Client 是 cuberead.Reader 的测试替身。
type Client struct {
	mu      sync.Mutex
	queries []Query
	// Handler 按 model 名返回行与总数。
	//
	// ⚠️ 允许返回 error:用来测"cube 挂了 ⇒ 503"这条路径。
	Handler func(model string, q cubeclient.CubeQuery) ([]map[string]any, int, error)
}

var _ interface {
	LoadCubeQueryPage(ctx context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error)
} = (*Client)(nil)

// New 构造一个空的替身(Handler 为 nil ⇒ 一律返空结果)。
func New() *Client {
	return &Client{}
}

// LoadCubeQueryPage 实现 cuberead.Reader。
func (c *Client) LoadCubeQueryPage(
	_ context.Context, modelName string, q cubeclient.CubeQuery,
) ([]map[string]any, int, error) {
	c.mu.Lock()
	c.queries = append(c.queries, Query{Model: modelName, Query: q})
	c.mu.Unlock()
	if c.Handler == nil {
		return nil, 0, nil
	}
	return c.Handler(modelName, q)
}

// Queries 返回记录下来的全部查询(按调用顺序)。
func (c *Client) Queries() []Query {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Query, len(c.queries))
	copy(out, c.queries)
	return out
}

// QueriesFor 返回某个 model 的全部查询。
func (c *Client) QueriesFor(model string) []Query {
	var out []Query
	for _, q := range c.Queries() {
		if q.Model == model {
			out = append(out, q)
		}
	}
	return out
}

// LastQuery 返回最后一个针对该 model 的查询。
func (c *Client) LastQuery(model string) (Query, bool) {
	qs := c.QueriesFor(model)
	if len(qs) == 0 {
		return Query{}, false
	}
	return qs[len(qs)-1], true
}

// FilterValues 返回某个查询里某个 member 的过滤值(扁平化 []any)。
//
// ⚠️ 用来断言"过滤条件真的进了 query"——
// 只断言"返回了 200 + 有字段"是**抓不到**一个被静默忽略的过滤器的。
func (q Query) FilterValues(member string) []any {
	for _, f := range q.Query.Filters {
		if f.Member == member {
			return f.Values
		}
	}
	return nil
}

// HasFilter 报某个 member 上是否挂了过滤。
func (q Query) HasFilter(member string) bool {
	for _, f := range q.Query.Filters {
		if f.Member == member {
			return true
		}
	}
	return false
}
