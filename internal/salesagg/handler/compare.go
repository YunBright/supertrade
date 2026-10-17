package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesagg/compare"
	"github.com/YunBright/supertrade/internal/salesagg/service"
)

// CompareHandler 是门店对比 handler（模板 CRUD + 逐店取数）。
//
// 与报表的 Handler 分开而不是塞进同一个 struct：报表是「带 X-Branch-ID
// 的单店即席查询」，对比是「忽略 X-Branch-ID、按 token claims 逐门店
// 鉴权」的另一套鉴权模型 —— 分开让两者的依赖面与安全假设各自可见。
type CompareHandler struct {
	svc   *service.Service
	store *compare.Store
	now   func() time.Time
}

// NewCompare 构造 CompareHandler。
func NewCompare(svc *service.Service, store *compare.Store) *CompareHandler {
	return &CompareHandler{svc: svc, store: store, now: time.Now}
}

// RegisterRoutes 注册门店对比路由。
//
// 路径是 nginx 剥掉 /api/v1/sales-agg/ 前缀后本服务看到的路径。
// X-Branch-ID 中间件仍在链上，但这组 handler 一律**不读**它：
// 模板是全局资源；取数授权只认 token claims。
func (h *CompareHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/compare-templates", h.ListTemplates)
	r.POST("/compare-templates", h.CreateTemplate)
	r.GET("/compare-templates/:id", h.GetTemplate)
	r.PUT("/compare-templates/:id", h.UpdateTemplate)
	r.DELETE("/compare-templates/:id", h.DeleteTemplate)
	r.GET("/compare-templates/:id/data", h.TemplateData)
	r.POST("/compare/preview", h.Preview)
}

// caller 是从 claims 提取的调用方身份与门店授权。
//
// RequireAudience 中间件已保证 claims 存在（缺 → 401），这里的检查是
// 防御性的：万一将来有人把这组路由误挂进免鉴权窗口，宁可 401 也不能让
// 「无 claims = 全部门店可见」静默发生。
type caller struct {
	sub              string
	accessibleStores map[string]bool
}

func callerFrom(c *gin.Context) (caller, bool) {
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		return caller{}, false
	}
	allowed := make(map[string]bool, len(cl.AccessibleBranches))
	for _, b := range cl.AccessibleBranches {
		allowed[b] = true
	}
	return caller{sub: cl.Sub, accessibleStores: allowed}, true
}

func (h *CompareHandler) unauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code":    "unauthenticated",
		"message": "缺少已签 token",
	})
}

func (h *CompareHandler) invalid(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"code":    "compare_template_invalid",
		"message": msg,
	})
}

// fail 把 compare / service 层错误映射为 HTTP 状态。
func (h *CompareHandler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, compare.ErrInvalid):
		h.invalid(c, err.Error())
	case errors.Is(err, compare.ErrNotFound):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"code":    "compare_template_not_found",
			"message": err.Error(),
		})
	case errors.Is(err, compare.ErrConflict):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "compare_template_conflict",
			"message": err.Error(),
		})
	case errors.Is(err, service.ErrInvalidDimension):
		h.invalid(c, err.Error())
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":    "internal",
			"message": err.Error(),
		})
	}
}

// ListTemplates GET /compare-templates
//
// 全量列表（updated_at 倒序），不分页 —— 模板是 wiki 式共享资源，
// 数量级是个位数到几十。
func (h *CompareHandler) ListTemplates(c *gin.Context) {
	if _, ok := callerFrom(c); !ok {
		h.unauthorized(c)
		return
	}
	list, err := h.store.List(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	out := make([]compare.TemplateJSON, 0, len(list))
	for i := range list {
		tj, err := list[i].ToJson()
		if err != nil {
			h.fail(c, err)
			return
		}
		out = append(out, tj)
	}
	c.JSON(http.StatusOK, gin.H{"templates": out})
}

// GetTemplate GET /compare-templates/:id
func (h *CompareHandler) GetTemplate(c *gin.Context) {
	if _, ok := callerFrom(c); !ok {
		h.unauthorized(c)
		return
	}
	h.respondTemplate(c, c.Param("id"))
}

// respondTemplate 取模板并按契约形状响应（Get / Update 共用）。
func (h *CompareHandler) respondTemplate(c *gin.Context, id string) {
	t, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		h.fail(c, err)
		return
	}
	tj, err := t.ToJson()
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, tj)
}

// CreateTemplate POST /compare-templates
func (h *CompareHandler) CreateTemplate(c *gin.Context) {
	cl, ok := callerFrom(c)
	if !ok {
		h.unauthorized(c)
		return
	}
	var in compare.SaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	if err := compare.ValidateSave(in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	t, err := compare.TemplateFromInput(in)
	if err != nil {
		h.fail(c, err)
		return
	}
	t.CreatedBy = cl.sub
	t.UpdatedBy = cl.sub
	// created_by_name 留空：JWT claims 没有 name 字段（authkit Claims 无此
	// 声明），前端对空名回退显示 ID —— 不为此在 token 里塞 PII。
	if err := h.store.Create(c.Request.Context(), t); err != nil {
		h.fail(c, err)
		return
	}
	tj, err := t.ToJson()
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, tj)
}

// UpdateTemplate PUT /compare-templates/:id
//
// 整体替换 + 乐观锁：body 必须带 revision（缺失 → 400，而不是静默按
// "最新版本"更新 —— 那会让并发编辑互相覆盖且无人察觉）。
func (h *CompareHandler) UpdateTemplate(c *gin.Context) {
	cl, ok := callerFrom(c)
	if !ok {
		h.unauthorized(c)
		return
	}
	var in compare.SaveInput
	if err := c.ShouldBindJSON(&in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	if err := compare.ValidateSave(in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	if in.Revision == nil {
		h.invalid(c, "缺少 revision（乐观锁）")
		return
	}
	t, err := compare.TemplateFromInput(in)
	if err != nil {
		h.fail(c, err)
		return
	}
	t.ID = c.Param("id")
	t.UpdatedBy = cl.sub
	if err := h.store.Update(c.Request.Context(), t, *in.Revision); err != nil {
		h.fail(c, err)
		return
	}
	// 回读响应：created_* / updated_at 以库为准，不在内存里拼装。
	h.respondTemplate(c, t.ID)
}

// DeleteTemplate DELETE /compare-templates/:id（物理删除）
func (h *CompareHandler) DeleteTemplate(c *gin.Context) {
	if _, ok := callerFrom(c); !ok {
		h.unauthorized(c)
		return
	}
	if err := h.store.Delete(c.Request.Context(), c.Param("id")); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// TemplateData GET /compare-templates/:id/data?preset=|from=&to=
//
// 取存档模板 + 日期参数，走与 preview 完全相同的取数路径。
func (h *CompareHandler) TemplateData(c *gin.Context) {
	cl, ok := callerFrom(c)
	if !ok {
		h.unauthorized(c)
		return
	}
	t, err := h.store.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.fail(c, err)
		return
	}
	tj, err := t.ToJson()
	if err != nil {
		h.fail(c, err)
		return
	}
	from, to, bad := h.resolvePeriod(c.Query("preset"), c.Query("from"), c.Query("to"))
	if bad != nil {
		h.invalid(c, bad.Error())
		return
	}
	h.fetchData(c, cl, tj.Dimension, tj.Stores, tj.Rows, from, to)
}

// Preview POST /compare/preview
//
// 草稿取数：配置内联在 body 里，不落库 —— 用户没保存也能看数。
// rows 允许 0 行（空草稿先看各店列通不通）。
func (h *CompareHandler) Preview(c *gin.Context) {
	cl, ok := callerFrom(c)
	if !ok {
		h.unauthorized(c)
		return
	}
	var in compare.PreviewBody
	if err := c.ShouldBindJSON(&in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	if err := compare.ValidatePreview(in); err != nil {
		h.invalid(c, err.Error())
		return
	}
	from, to, bad := h.resolvePeriod(in.Preset, in.From, in.To)
	if bad != nil {
		h.invalid(c, bad.Error())
		return
	}
	h.fetchData(c, cl, in.Dimension, in.Stores, in.Rows, from, to)
}

// resolvePeriod 解析取数日期区间。
//
// from/to 都非空 → 自定义区间（必须成对、YYYY-MM-DD、from ≤ to）；
// 否则 preset（缺省 today，与报表页口径一致；未知值由 ResolveRange
// 退回 today —— 与 /reports/sales 行为一致，不另立规则）。
// 第三返回值非 nil 表示入参不合法（→ 400 compare_template_invalid）。
func (h *CompareHandler) resolvePeriod(preset, from, to string) (string, string, error) {
	if from != "" || to != "" {
		if from == "" || to == "" {
			return "", "", errors.New("from 与 to 必须成对出现")
		}
		f, ferr := time.Parse("2006-01-02", from)
		t, terr := time.Parse("2006-01-02", to)
		if ferr != nil || terr != nil {
			return "", "", errors.New("日期格式必须是 YYYY-MM-DD")
		}
		if f.After(t) {
			return "", "", errors.New("from 不能晚于 to")
		}
		return from, to, nil
	}
	if strings.TrimSpace(preset) == "" {
		preset = string(service.PresetToday)
	}
	f, t := service.ResolveRange(service.Preset(preset), h.now())
	return f, t, nil
}

// fetchData 是 data 与 preview 共用的取数装配：把 wire 行转成 service
// 入参，随后全部交 CompareData。HTTP 恒 200 —— 单店失败已内联为列状态。
func (h *CompareHandler) fetchData(
	c *gin.Context, cl caller, dimension string, stores []string, rows []compare.RowJSON, from, to string,
) {
	in := service.CompareInput{
		Dimension: dimension,
		Stores:    stores,
		Allowed:   cl.accessibleStores,
		From:      from,
		To:        to,
		Rows:      make([]service.CompareRow, 0, len(rows)),
	}
	for _, r := range rows {
		items := make(map[string]string, len(r.Items))
		for k, v := range r.Items {
			if v == nil {
				continue
			}
			items[k] = *v
		}
		in.Rows = append(in.Rows, service.CompareRow{Key: r.Key, Label: r.Label, Items: items})
	}
	out, err := h.svc.CompareData(c.Request.Context(), in)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}
