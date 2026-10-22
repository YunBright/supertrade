// Package handler / product_archive.go —— 商品档案(图1)的 HTTP 端点。
//
// 路由(nginx 已剥过 /api/v1/catalog/):
//
//	GET /product-archive?q=&category_id=&supplier_id=&page=&page_size=
//	    商品档案列表(product:view,per branch)
//	GET /product-archive/:id/supplier-prices
//	    某个商品的「供货信息」(product:view,per branch)
//
// # 与 picker 端点的区别(别把两者混起来)
//
//	picker(/products):面向收银员选货,只要一个价。
//	product-archive   :面向档案管理,要品牌、四个价、供货约定。
//
// ⇒ 两组端点**响应字段不同**,但**分流判据完全相同**
// (internal/cubesource:配了源 ⇒ cube;没配 ⇒ 本域 PG;问不到 ⇒ 503)。
//
// # ⚠️ 分支来源一律走 X-Branch-ID
//
// 路径里**没有** :branch_id(与全局约定一致,见
// .claude/rules/02-handler-routes.md)。
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
)

// listProductArchive GET /product-archive
//
// 查询参数:
//
//	q            关键字(货号 / 自编码 / 名称,大小写无关)
//	category_id  分类(**子树**语义:该分类 + 它的所有子孙)
//	supplier_id  主供应商(精确)
//	page/page_size 分页
func (h *Handler) listProductArchive(c *gin.Context) {
	q := service.ProductArchiveQuery{
		Keyword:    c.Query("q"),
		CategoryID: c.Query("category_id"),
		SupplierID: c.Query("supplier_id"),
		Page:       1,
		PageSize:   service.DefaultPageSize,
	}
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.Page = n
		}
	}
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.PageSize = n
		}
	}
	page, err := h.catalog.ListProductArchive(c.Request.Context(), q)
	if err != nil {
		h.mapProductArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, page)
}

// getProductSupplierPrices GET /product-archive/:id/supplier-prices
func (h *Handler) getProductSupplierPrices(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "bad_request", "message": "商品 id 必填",
		})
		return
	}
	list, err := h.catalog.ListSupplierPrices(c.Request.Context(), id)
	if err != nil {
		h.mapProductArchiveErr(c, err)
		return
	}
	c.JSON(http.StatusOK, list)
}

// mapProductArchiveErr 把商品档案链路的错误映成 HTTP 状态码 + JSON body。
//
// ⚠️ **数据源相关的错先判**:403(没权限)与 503(问不到路由)必须分开,
// 混在一起会让"没权限"显示成"暂不可用"(见 source.go 的注释)。
//
// ⚠️ cube 的 **ErrCubeBadRequest 映 503 而不是 400**:它表示
// "cube 侧没有这个 model / 这个维度"(本仓与 cube 仓的契约不一致),
// 是**服务端**的问题,不是调用方的 ⇒ 400 会让调用方以为是自己参数写错了。
func (h *Handler) mapProductArchiveErr(c *gin.Context, err error) {
	if h.writeSourceErr(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrProductArchiveReaderMissing),
		errors.Is(err, service.ErrLocalArchiveUnavailable):
		// 503:装配错误(读端 / 本地库没装),不是业务结论。
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeForbidden):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code": "forbidden", "message": "没有该门店的数据读权限",
		})
	case errors.Is(err, cubeclient.ErrCubeAuthRejected):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code": "unauthorized", "message": "登录已失效,请重新登录",
		})
	case errors.Is(err, cubeclient.ErrCubeBadRequest):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": "数据源暂不可用(查询契约不匹配)",
		})
	case errors.Is(err, cubeclient.ErrCubeUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "cube_unavailable", "message": "数据源暂不可用,请稍后重试",
		})
	default:
		h.mapErr(c, err)
	}
}
