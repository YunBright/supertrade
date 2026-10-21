// Package handler / local.go —— 本地表的批量读侧。
//
// # 为什么要有"批量"端点
//
// POS 扫一屏商品要一次拿一批价。逐个调 `/quotes` 是 N 次 HTTP 往返,
// 而每次往返都要过一次 RBAC(userd 实时拉 per-branch 权限矩阵)。
// ⇒ 批量端点一次查完,N 次 RBAC 变成 1 次。
//
// # 金额一律字符串
//
// JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉。
package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// MaxLocalBatchSize 是批量查价一次最多带多少个商品。
//
// ⚠️ 上限不是随便定的:`product_id IN (...)` 会拼成一条 SQL,
// 一万个 id 会让这条语句的解析时间超过网络往返本身。
// 超出时**截断**而不是报错 —— 调用方(POS)分批拿即可,
// 而报错会让"一屏商品太多"变成一次失败。
const MaxLocalBatchSize = 500

// localPriceDTO 是一行本地售价。
type localPriceDTO struct {
	ProductID string `json:"product_id"`
	// PriceYuan 是零售价(字符串)。
	PriceYuan string `json:"price_yuan"`
	// PromoPriceYuan 是**当前生效促销价**;nil = 没有生效促销。
	//
	// ⚠️ **为什么零售价与促销价要分开给**:
	//   思迅的促销单会**覆盖**零售价(实测 `6922303199721`:
	//   零售 15.00 / 促销 9.90,"长期店特不用补差2026.1.1-12.31日")。
	//
	//   ⇒ 只给一个"价"的话,消费方要么按原价收钱(多收),
	//     要么自己再查一次 promotions(多一次往返)。
	//   ⇒ 两个都给,**成交价 = min(零售价, 促销价)**,由消费方决定
	//     (POS 收银用 min;价签打印用零售价划线 + 促销价)。
	//
	//   ⚠️ 同一商品多个促销 ⇒ 取**最低**的那个
	//     (与 cube promotion model 的 min_promo_price 语义一致)。
	PromoPriceYuan *string `json:"promo_price_yuan,omitempty"`
	// MemberPriceYuan 是会员价;nil = 没有会员价(不是"会员价 0 元")。
	MemberPriceYuan *string `json:"member_price_yuan,omitempty"`
	// CostYuan 是成本价;nil = 不知道成本。
	CostYuan *string `json:"cost_yuan,omitempty"`
}

// localPromoDTO 是一行当前生效促销。
type localPromoDTO struct {
	ProductID string `json:"product_id"`
	PlanNo    string `json:"plan_no"`
	PlanName  string `json:"plan_name,omitempty"`
	// PriceYuan 是促销价(字符串)。
	PriceYuan string `json:"price_yuan"`
	// OrigPriceYuan 是原价;nil = 没有原价。
	OrigPriceYuan *string `json:"orig_price_yuan,omitempty"`
	BeginDay      string  `json:"begin_day"`
	EndDay        string  `json:"end_day"`
}

// getLocalPrices GET /prices/local?product_ids=a,b,c
//
// 返回该门店本地表里这些商品的**当前生效售价**。
//
// ⚠️ **没维护价的商品不在返回里**(它没有行)——
//
//	调用方据此区分"没维护售价"与"售价 0 元"。
//
// # ⚠️ 这两个端点**只读本地表**,不按数据源路由
//
//	端点名字里的 "local" 就是承诺:批量查**本域 PG**里的售价/促销。
//	要"按门店有无数据源路由"的实时价,走 /quotes(单个)与 /prices/export(全店)。
//
// ⇒ 门店 00(配了数据源)在这两个端点上拿到的仍是本地**快照**,
// 而 /quotes 拿的是 cube 实时价 —— 这是**已知的不一致**,记在这里备查:
// 现场问"为什么批量查和单个查不一样"时,答案就在这两行。
func (h *Handler) getLocalPrices(c *gin.Context) {
	if h.priceLists == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "本地价格表未装配",
		})
		return
	}
	ids := parseProductIDs(c.Query("product_ids"))
	if len(ids) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_product_ids", "message": "product_ids query 必填(逗号分隔)",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	at := h.localNow()

	rows, err := h.priceLists.ActivePrices(c.Request.Context(), branchID, "", ids, at)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	// ⚠️ **促销要单独查**:售价表里没有促销价,
	//   而促销是**时效数据**(按基准日生效)⇒ 必须按 at 取当前生效的那些。
	//
	//   症状(2026-10-21 现场):POS 显示 15.00 而实际促销价 9.90 ——
	//   因为这个端点只查了 price_lists,促销价**根本没参与**。
	promos := map[string]model.Promotion{}
	if h.promos != nil {
		promos, err = h.promos.ActivePromotions(c.Request.Context(), branchID, ids, at)
		if err != nil {
			h.mapErr(c, err)
			return
		}
	}
	out := make([]localPriceDTO, 0, len(rows))
	for _, r := range rows {
		dto := localPriceDTO{
			ProductID: r.ProductID,
			PriceYuan: r.PriceYuan.String(),
		}
		if p, ok := promos[r.ProductID]; ok && !p.PriceYuan.IsZero() {
			s := p.PriceYuan.String()
			dto.PromoPriceYuan = &s
		}
		if !r.MemberPriceYuan.IsZero() {
			s := r.MemberPriceYuan.String()
			dto.MemberPriceYuan = &s
		}
		if !r.CostYuan.IsZero() {
			s := r.CostYuan.String()
			dto.CostYuan = &s
		}
		out = append(out, dto)
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id": branchID,
		"as_of":     at.Format("2006-01-02"),
		"prices":    out,
		"count":     len(out),
	})
}

// getLocalPromotions GET /promotions/local?product_ids=a,b,c&on_date=yyyy-mm-dd
//
// 返回该门店本地表里这些商品**当前生效**的促销。
//
// ⚠️ 同一商品多个生效促销 ⇒ **只返促销价最低的那条**
// (与 cube promotion model 的 min_promo_price 语义一致)。
func (h *Handler) getLocalPromotions(c *gin.Context) {
	if h.promos == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "本地促销表未装配",
		})
		return
	}
	ids := parseProductIDs(c.Query("product_ids"))
	if len(ids) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "missing_product_ids", "message": "product_ids query 必填(逗号分隔)",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	at := h.localNow()
	if day := strings.TrimSpace(c.Query("on_date")); day != "" {
		parsed, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code": "bad_on_date", "message": "on_date 必须是 yyyy-mm-dd",
			})
			return
		}
		at = parsed
	}

	rows, err := h.promos.ActivePromotions(c.Request.Context(), branchID, ids, at)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	out := make([]localPromoDTO, 0, len(rows))
	for _, r := range rows {
		dto := localPromoDTO{
			ProductID: r.ProductID,
			PlanNo:    r.ID,
			PlanName:  r.PlanName,
			PriceYuan: r.PriceYuan.String(),
			BeginDay:  r.BeginAt.Format("2006-01-02"),
			EndDay:    r.EndAt.Format("2006-01-02"),
		}
		if !r.OrigPriceYuan.IsZero() {
			s := r.OrigPriceYuan.String()
			dto.OrigPriceYuan = &s
		}
		out = append(out, dto)
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id":  branchID,
		"as_of":      at.Format("2006-01-02"),
		"promotions": out,
		"count":      len(out),
	})
}

// exportLocalPrices GET /prices/export?on_date=yyyy-mm-dd
//
// # 是什么
//
// 给 POS 客户端的**离线价格包**:一次给全该门店默认价目表
// (id='default')的生效零售价 + 该基准日生效的促销。
//
// # ⚠️ 与 getLocalPrices 的差别
//
//	getLocalPrices   必须带 product_ids(上限 500,超限**静默截断**)
//	exportLocalPrices 不带 product_ids,给**全店**
//
// ⚠️ 500 的上限是 `product_id IN (...)` 拼一条 SQL 的代价;
// 导出走的是"整表扫",不受那个上限约束。
//
// # ⚠️ 路径与响应形状**不许变**(前端 / POS 离线包在用)
//
//	{"branch_id","as_of","prices":[…],"count","total"}
//
// 只有**数据来源**按"配没配数据源"切(有源 ⇒ cube 实时;没源 ⇒ 本地 PG),
// 判据与查价同一套(见 service.ExportPriceRows)。
//
// # on_date
//
//   - 不传 = 今天(基准日的 00:00:00,与 localNow 同一口径)
//   - 传未来日期 = 取**那一天**生效的零售价与促销
//     (客户端可以提前把未来的促销价存下来,到日子自动生效)
//
// ⚠️ 非法 on_date ⇒ 400 bad_on_date(绝不"尽力解析"成一个大概的日期 ——
// 那会让促销在错误的日期生效,印出低于真价的价签)。
func (h *Handler) exportLocalPrices(c *gin.Context) {
	if h.prices == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code": "not_ready", "message": "本地价格表未装配",
		})
		return
	}
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "branch_required", "message": "X-Branch-ID header 必填",
		})
		return
	}
	at := h.localNow()
	if day := strings.TrimSpace(c.Query("on_date")); day != "" {
		parsed, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code": "bad_on_date", "message": "on_date 必须是 yyyy-mm-dd",
			})
			return
		}
		at = parsed
	}
	// ⚠️ 数据来源由 service 决定(有源 ⇒ cube 实时;没源 ⇒ 本地 PG)。
	rows, err := h.prices.ExportPriceRows(c.Request.Context(), branchID, at)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"branch_id": branchID,
		// as_of 回给调用方:让"这批价格是按哪一天判的"从响应里就能看出来。
		"as_of":  at.Format("2006-01-02"),
		"prices": rows,
		"count":  len(rows),
		"total":  len(rows),
	})
}

// localNow 返回当前时刻(基准日的 00:00:00)。
//
// ⚠️ 促销的生效判定是**按天**的,所以基准时刻取当天零点 ——
// 取"此刻"会让"今天上午 10 点"与"今天零点"在跨日边界上判出不同结果。
func (h *Handler) localNow() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// parseProductIDs 解析逗号分隔的商品 id 列表。
//
// ⚠️ 空集合**不等于不过滤** ——
// 这里返回空切片,调用方据此报 400(而不是"查全部")。
// 若把它当成"不过滤",一条 `product_id IN ()` 会得到 0 行,
// 而调用方以为"这个门店一个价都没有"。
func parseProductIDs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= MaxLocalBatchSize {
			break
		}
	}
	return out
}
