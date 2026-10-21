package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// localRouterWithDB 造一个能**灌数据**的本地端点 router。
//
// ⚠️ 为什么不用现成的 localRouter：它把 db 藏在内部，测试没法先写数据。
func localRouterWithDB(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	prices := service.NewPriceListService(db)
	promos := service.NewPromotionService(db)
	h := New(service.NewPriceService(nil).WithStore(prices, promos), prices, promos, nil, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/prices", h.getPrices)
	r.GET("/promotions", h.getPromotions)
	r.GET("/prices/export", h.exportLocalPrices)
	return r, db
}

// TestLocalPrices_IncludesPromoPrice —— 批量查价必须**带上促销价**。
//
// # 为什么（2026-10-21 现场发现）
//
// 症状：POS 上「唯得水乡特色S码抽纸（8包装）`6922303199721`
// 显示 原价 15.00 / 单价 15.00 / 小计 15.00，
// 而它**正在特价 9.9**（「长期店特不用补差2026.1.1-12.31日」）。
//
// 根因：这个端点**只查了 price_lists**，促销价**根本没参与**。
// ⇒ 消费方拿到的 `price_yuan` 就是零售价 ⇒ **按原价收钱**。
//
// ⚠️ 这类 bug 不报错、不崩，只是**默默多收钱**。
func TestLocalPrices_IncludesPromoPrice(t *testing.T) {
	r, db := localRouterWithDB(t)
	ctx := t.Context()

	prices := service.NewPriceListService(db)
	promos := service.NewPromotionService(db)

	_, _, err := prices.UpsertPrices(ctx, "10", service.ModeOverwrite,
		[]service.PriceInput{{ProductID: "6922303199721", PriceYuan: "15.00"}})
	require.NoError(t, err)

	_, _, err = promos.UpsertPromotions(ctx, "10", service.ModeOverwrite, "2026-10-21",
		[]service.PromotionInput{{
			ID:            "PS002512190001",
			ProductID:     "6922303199721",
			PlanName:      "长期店特不用补差2026.1.1-12.31日",
			PriceYuan:     "9.90",
			OrigPriceYuan: "15.00",
			BeginDay:      "2026-01-01",
			EndDay:        "2026-12-31",
		}}, false)
	require.NoError(t, err)

	w := getJSON(t, r, "/prices?product_ids=6922303199721&on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var out struct {
		Prices []struct {
			ProductID      string  `json:"product_id"`
			PriceYuan      string  `json:"price_yuan"`
			PromoPriceYuan *string `json:"promo_price_yuan"`
		} `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Prices, 1)

	// ⚠️ decimal 归一化 scale（"15.00" → 15）⇒ 断言按归一化后的值。
	assert.Equal(t, "15", out.Prices[0].PriceYuan, "零售价")
	require.NotNil(t, out.Prices[0].PromoPriceYuan,
		"必须带促销价(否则消费方按原价收钱)")
	assert.Equal(t, "9.9", *out.Prices[0].PromoPriceYuan)
}

// TestLocalPrices_NoPromoMeansNull —— 没有生效促销时，促销价是 **null**（不是 "0"）。
//
// ⚠️ null 与 "0" 必须能区分：促销价 0 元的商品与"没促销"是两件事。
func TestLocalPrices_NoPromoMeansNull(t *testing.T) {
	r, db := localRouterWithDB(t)
	ctx := t.Context()

	prices := service.NewPriceListService(db)
	_, _, err := prices.UpsertPrices(ctx, "10", service.ModeOverwrite,
		[]service.PriceInput{{ProductID: "P-1", PriceYuan: "9.90"}})
	require.NoError(t, err)

	w := getJSON(t, r, "/prices?product_ids=P-1&on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code)

	var out struct {
		Prices []struct {
			PromoPriceYuan *string `json:"promo_price_yuan"`
		} `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Prices, 1)
	assert.Nil(t, out.Prices[0].PromoPriceYuan, "没有促销时 promo_price_yuan 应为 null")
}
