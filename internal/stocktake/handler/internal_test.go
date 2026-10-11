package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

// buildInternalRouter 只挂 S2S 内部端点(无 claims / X-Branch-ID 中间件,
// 对齐 cmd/stocktake/main.go 的 registerDaprRoutes 窗口语义)。
func buildInternalRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
		&model.StocktakeBranchDefault{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := service.New(db, cubeclientfake.New())
	gin.SetMode(gin.TestMode)
	r := gin.New()
	handler.New(svc).RegisterInternalRoutes(r)
	return r, db
}

func postInternal(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/stocktake-plan-headers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestInternal_CreatePlanHeader_201(t *testing.T) {
	r, db := buildInternalRouter(t)
	w := postInternal(t, r, `{"branch_id":"S001","operator_id":"u-1","remark":"审批建议盘点","product_ids":["P-1","P-2"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var resp handler.CreatePlanHeaderResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "" || resp.BranchID != "S001" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Type != string(model.TypePlan) {
		t.Errorf("type = %s, want plan", resp.Type)
	}
	if resp.Status != string(model.StatusCounting) {
		t.Errorf("status = %s, want counting", resp.Status)
	}
	// 计划商品落库(名称缺省由服务端兜底为 product_id)
	var items []model.StocktakePlanItem
	db.Find(&items, "header_id = ?", resp.ID)
	if len(items) != 2 {
		t.Fatalf("计划商品数 = %d, want 2", len(items))
	}
	for _, it := range items {
		if it.ProductName == "" {
			t.Errorf("product %s 名称应兜底非空", it.ProductID)
		}
	}
}

func TestInternal_CreatePlanHeader_201_NoProducts(t *testing.T) {
	r, _ := buildInternalRouter(t)
	w := postInternal(t, r, `{"branch_id":"S001","operator_id":"u-1","remark":"supplier scope 自行圈品"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201(product_ids 可空), body=%s", w.Code, w.Body.String())
	}
}

func TestInternal_CreatePlanHeader_400_MissingBranch(t *testing.T) {
	r, _ := buildInternalRouter(t)
	w := postInternal(t, r, `{"operator_id":"u-1"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestInternal_CreatePlanHeader_400_MissingOperator(t *testing.T) {
	r, _ := buildInternalRouter(t)
	w := postInternal(t, r, `{"branch_id":"S001"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestInternal_CreatePlanHeader_400_DuplicatedProduct(t *testing.T) {
	r, _ := buildInternalRouter(t)
	w := postInternal(t, r, `{"branch_id":"S001","operator_id":"u-1","product_ids":["P-1","P-1"]}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400(重复商品), body=%s", w.Code, w.Body.String())
	}
}
