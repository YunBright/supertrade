// Package handler / audit_flow.go —— 机器审核「流程定制」的配置端点。
//
// # 端点
//
//	GET    /audit-flows/suppliers            供应商流程配置列表
//	POST   /audit-flows/suppliers            新建
//	PUT    /audit-flows/suppliers/:id        修改
//	DELETE /audit-flows/suppliers/:id        删除
//
//	GET    /audit-flows/payment-items         付款项流程配置列表
//	POST   /audit-flows/payment-items         新建
//	PUT    /audit-flows/payment-items/:id     修改
//	DELETE /audit-flows/payment-items/:id     删除
//
//	GET    /audit-flows/regex-patterns        正则表达式列表
//	POST   /audit-flows/regex-patterns        新建
//	PUT    /audit-flows/regex-patterns/:id    修改
//	DELETE /audit-flows/regex-patterns/:id    删除
//	POST   /audit-flows/regex-test               正则试跑（不写库）
//
//	GET    /audit-flows/supplier-dict         供应商字典（挑供应商用）
//
// # 权限（**有意不用 settlement:audit**）
//
//	merchant（店长）**持有** settlement:audit（见 auth 的迁移 001）——
//	用它守这些端点 ⇒ 店长能给供应商配「直接通过」⇒ 等于自己给自己开自动放行。
//	⇒ 读走 `supplier:view`、写走 `supplier:manage`，**这两个 merchant 都没有**
//	（迁移里 merchant 的清单只有 profile / inventory / cube / settlement / pricing）。
//
//	⚠️ `supplier:*` 与本模块的语义不是严丝合缝（付款项流程配置并不"管供应商"），
//	但它是现有权限点里**唯一**既语义接近"管理审核策略"、又**店长拿不到**的。
//	要一个专用的 `settlement:flow` 权限点需要动 auth 仓（迁移 + 角色清单 + 契约测试），
//	那是另一次改动 —— 这里先不引入。
package handler

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// 端点使用的 scope。
const (
	// ScopeFlowRead 是读端点的 scope。
	ScopeFlowRead = "supplier:view"
	// ScopeFlowWrite 是写端点的 scope（**merchant 不持有**）。
	ScopeFlowWrite = "supplier:manage"
)

// ---- 请求体 ----

type supplierFlowReq struct {
	SupplierID   string `json:"supplier_id"`
	SupplierName string `json:"supplier_name"`
	Mode         string `json:"mode"`
	LogicKind    string `json:"logic_kind"`
	Enabled      bool   `json:"enabled"`
	Note         string `json:"note"`
}

type itemFlowReq struct {
	ItemType  string `json:"item_type"`
	Mode      string `json:"mode"`
	LogicKind string `json:"logic_kind"`
	RegexID   string `json:"regex_id"`
	Enabled   bool   `json:"enabled"`
	Note      string `json:"note"`
}

type regexPatternReq struct {
	Name       string `json:"name"`
	Pattern    string `json:"pattern"`
	GroupIndex int    `json:"group_index"`
	Enabled    bool   `json:"enabled"`
	Note       string `json:"note"`
}

type regexTestReq struct {
	RegexID string `json:"regex_id"`
	Pattern string `json:"pattern"`
	// GroupIndex 覆盖已保存正则的组号；nil = 用已保存的值。
	GroupIndex *int   `json:"group_index"`
	Sample     string `json:"sample"`
}

// ---- 供应商流程配置 ----

// ListSupplierFlows GET /audit-flows/suppliers
func (h *Handler) ListSupplierFlows(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowRead) {
		return
	}
	rows, err := h.svc.ListSupplierFlows(c.Request.Context(), branchID)
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// CreateSupplierFlow POST /audit-flows/suppliers
func (h *Handler) CreateSupplierFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req supplierFlowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.CreateSupplierFlow(c.Request.Context(), branchID, h.operatorID(c), service.SupplierFlowInput{
		SupplierID:   req.SupplierID,
		SupplierName: req.SupplierName,
		Mode:         req.Mode,
		LogicKind:    req.LogicKind,
		Enabled:      req.Enabled,
		Note:         req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// UpdateSupplierFlow PUT /audit-flows/suppliers/:id
func (h *Handler) UpdateSupplierFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req supplierFlowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.UpdateSupplierFlow(c.Request.Context(), branchID, c.Param("id"), h.operatorID(c), service.SupplierFlowInput{
		SupplierID:   req.SupplierID,
		SupplierName: req.SupplierName,
		Mode:         req.Mode,
		LogicKind:    req.LogicKind,
		Enabled:      req.Enabled,
		Note:         req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// DeleteSupplierFlow DELETE /audit-flows/suppliers/:id
func (h *Handler) DeleteSupplierFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	if err := h.svc.DeleteSupplierFlow(c.Request.Context(), branchID, c.Param("id")); err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id")})
}

// ---- 付款项流程配置 ----

// ListItemFlows GET /audit-flows/payment-items
func (h *Handler) ListItemFlows(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowRead) {
		return
	}
	rows, err := h.svc.ListItemFlows(c.Request.Context(), branchID)
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// CreateItemFlow POST /audit-flows/payment-items
func (h *Handler) CreateItemFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req itemFlowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.CreateItemFlow(c.Request.Context(), branchID, h.operatorID(c), service.ItemFlowInput{
		ItemType:  req.ItemType,
		Mode:      req.Mode,
		LogicKind: req.LogicKind,
		RegexID:   req.RegexID,
		Enabled:   req.Enabled,
		Note:      req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// UpdateItemFlow PUT /audit-flows/payment-items/:id
func (h *Handler) UpdateItemFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req itemFlowReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.UpdateItemFlow(c.Request.Context(), branchID, c.Param("id"), h.operatorID(c), service.ItemFlowInput{
		ItemType:  req.ItemType,
		Mode:      req.Mode,
		LogicKind: req.LogicKind,
		RegexID:   req.RegexID,
		Enabled:   req.Enabled,
		Note:      req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// DeleteItemFlow DELETE /audit-flows/payment-items/:id
func (h *Handler) DeleteItemFlow(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	if err := h.svc.DeleteItemFlow(c.Request.Context(), branchID, c.Param("id")); err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id")})
}

// ---- 正则表达式维护 ----

// ListRegexPatterns GET /audit-flows/regex-patterns
func (h *Handler) ListRegexPatterns(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowRead) {
		return
	}
	rows, err := h.svc.ListRegexPatterns(c.Request.Context(), branchID)
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// CreateRegexPattern POST /audit-flows/regex-patterns
func (h *Handler) CreateRegexPattern(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req regexPatternReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.CreateRegexPattern(c.Request.Context(), branchID, h.operatorID(c), service.RegexPatternInput{
		Name:       req.Name,
		Pattern:    req.Pattern,
		GroupIndex: req.GroupIndex,
		Enabled:    req.Enabled,
		Note:       req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// UpdateRegexPattern PUT /audit-flows/regex-patterns/:id
func (h *Handler) UpdateRegexPattern(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	var req regexPatternReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	row, err := h.svc.UpdateRegexPattern(c.Request.Context(), branchID, c.Param("id"), h.operatorID(c), service.RegexPatternInput{
		Name:       req.Name,
		Pattern:    req.Pattern,
		GroupIndex: req.GroupIndex,
		Enabled:    req.Enabled,
		Note:       req.Note,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, row)
}

// DeleteRegexPattern DELETE /audit-flows/regex-patterns/:id
func (h *Handler) DeleteRegexPattern(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowWrite) {
		return
	}
	if err := h.svc.DeleteRegexPattern(c.Request.Context(), branchID, c.Param("id")); err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": c.Param("id")})
}

// TestRegex POST /audit-flows/regex-test
//
// ⚠️ **纯读**：不写库、不改配置，只是拿一段样例试跑。
// 管理后台的"正则测试"用它验证"9月水电费"能不能抠出"9"。
//
// ⚠️ 路径**不放在** `/audit-flows/regex-patterns/` 下面：
//
//	那个前缀下已经有 `:id`（通配）段，再挂一个静态子路径 gin 会直接 panic
//	（httprouter 的 wildcard/static 冲突）。
func (h *Handler) TestRegex(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowRead) {
		return
	}
	var req regexTestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", "请求体非法: "+err.Error())
		return
	}
	res, err := h.svc.TestRegex(c.Request.Context(), branchID, service.RegexTestInput{
		RegexID:    req.RegexID,
		Pattern:    req.Pattern,
		GroupIndex: req.GroupIndex,
		Sample:     req.Sample,
	})
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// ---- 供应商字典 ----

// SupplierDict GET /audit-flows/supplier-dict
//
// 挑供应商用的字典（id → 名称）。
//
// ⚠️ 字典来自 **cube 的 supplier 字典**（duck model）——
//
//	结算单上的 supplier_id 就是 cube 口径 ⇒ 配置里的 supplier_id 必须用这里的 id。
//	没有 cube 源的门店（如门店 10）走不到供应商分支 ⇒ 这里查不到是正常的。
func (h *Handler) SupplierDict(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if !h.requireScope(c, branchID, ScopeFlowRead) {
		return
	}
	names, err := h.svc.SupplierDict(c.Request.Context(), branchID)
	if err != nil {
		mapFlowErr(c, err)
		return
	}
	// 关键字过滤（按 id 或名称包含）。
	q := strings.TrimSpace(c.Query("q"))
	items := make([]gin.H, 0, len(names))
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		name := names[id]
		if q != "" && !strings.Contains(id, q) && !strings.Contains(name, q) {
			continue
		}
		items = append(items, gin.H{"supplier_id": id, "supplier_name": name})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// operatorID 取当前操作人（auth user id）——审计字段用。
//
// ⚠️ 取 claims.Sub（auth user id），**不信任客户端自称的 operator**。
func (h *Handler) operatorID(c *gin.Context) string {
	cl, _ := claims.FromContext(c.Request.Context())
	if cl == nil {
		return ""
	}
	return cl.Sub
}

// mapFlowErr 把流程配置的 service 错误映射成 HTTP 状态码。
func mapFlowErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrAuditFlowNotFound):
		writeError(c, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, service.ErrAlreadyExists):
		writeError(c, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
	default:
		// 与 mapErr 的 default 同形：500 + 原始错误。
		writeError(c, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
