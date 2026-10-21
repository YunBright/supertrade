// Package cubesync —— 同步编排器的单元测试。
//
// # 测什么(对着症状写)
//
//	① 分页:多页能不能拉全(拉少了 ⇒ 少导商品,报告显示"成功")
//	② 空结果:会不会把"没拉到"当成"没有数据"导进去(覆盖模式会把目标清空)
//	③ **条码分块**:同一商品的条码会不会被切成两块(切了 ⇒ 静默丢条码)
//	④ scope 顺序:引用方是不是在被引用方之后
//	⑤ 契约:导出列名与导入字段名是不是同一份(archivedto.ExportColumns)
//
// 断言全部对着**用户能看见的症状**(导了多少行、少了哪几条),
// 不对着内部实现 —— 否则改个写法就假红。
package cubesync

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// ---- fakes ----

// fakeExporter 是导出方的假实现:按页返回预置的行。
type fakeExporter struct {
	// pages 是每个 scope 的行(顺序即返回顺序,一行一页)。
	pages map[string][]map[string]any
	// calls 记录每个 scope 被拉了几次、每次的 offset。
	calls []exportCall
	// err 非空时直接返回该错误。
	err error
}

type exportCall struct {
	Scope  string
	Offset int
	Limit  int
}

func (f *fakeExporter) Export(_ context.Context, _, scope string, offset, limit int) (*archivedto.ExportPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, exportCall{Scope: scope, Offset: offset, Limit: limit})
	rows := f.pages[scope]
	if offset >= len(rows) {
		return &archivedto.ExportPage{Scope: scope, Rows: nil, Total: len(rows)}, nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return &archivedto.ExportPage{
		Scope: scope,
		Rows:  rows[offset:end],
		Count: end - offset,
		Total: len(rows),
	}, nil
}

// fakeImporter 是导入方的假实现:记录收到的每一批。
//
// ⚠️ **必须线程安全**:定时回调把同步放到**后台**跑
// (一次全量同步实测 ~100 秒,超过 dapr scheduler 的投递超时就会被反复重试),
// 而测试要在主 goroutine 里读 batches ⇒ 不加锁就是 data race。
type fakeImporter struct {
	mu      sync.Mutex
	batches []archivedto.ImportRequest
	// err 非空时直接返回该错误。
	err error
}

func (f *fakeImporter) Import(_ context.Context, _ string, req archivedto.ImportRequest) (*archivedto.ImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.batches = append(f.batches, req)
	res := &archivedto.ImportResult{Mode: req.Mode}
	for _, sc := range req.Scopes {
		row := archivedto.ImportScopeRow{Scope: sc, Received: 1, Inserted: 1}
		switch sc {
		case archivedto.ScopeProducts:
			row.Received = len(req.Data.Products)
			row.Inserted = len(req.Data.Products)
		case archivedto.ScopeCategories:
			row.Received = len(req.Data.Categories)
			row.Inserted = len(req.Data.Categories)
		case archivedto.ScopeSuppliers:
			row.Received = len(req.Data.Suppliers)
			row.Inserted = len(req.Data.Suppliers)
		case archivedto.ScopeItemBarcodes:
			row.Received = len(req.Data.ItemBarcodes)
			row.Inserted = len(req.Data.ItemBarcodes)
		}
		res.ByScope = append(res.ByScope, row)
	}
	return res, nil
}

// snapshot 返回已提交的批次(线程安全)。
func (f *fakeImporter) snapshot() []archivedto.ImportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]archivedto.ImportRequest(nil), f.batches...)
}

// waitForImport 等后台同步跑完并提交了导入。
//
// ⚠️ 定时回调是**异步**的(立刻 ack + 后台跑)⇒
// 测试不能假设"响应返回时导入已经发生"。
func waitForImport(t *testing.T, im *fakeImporter, timeout time.Duration) []archivedto.ImportRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b := im.snapshot(); len(b) > 0 {
			return b
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("后台同步在超时内没有提交任何导入")
	return nil
}

// fakeToken 是机器身份签发器的假实现。
type fakeToken struct{ token string }

func (f fakeToken) AccessToken(context.Context) (string, error) {
	return f.token, nil
}

// newTestSyncer 造一个 Syncer(fake 依赖)。
//
// ⚠️ 路由表:**catalog 拥有四个档案 scope**,prices/promotions 归 pricing,
//
//	  stock 归 inventory,member_types/members 归 member。
//
//		这里几个归属方都指向同一个 fake(测试不关心下游是谁)。
func newTestSyncer(cfg Config, ex Exporter, im Importer) *Syncer {
	s := NewSyncer(cfg, fakeToken{token: "T"}, ex, []Owner{
		{Importer: im, Scopes: DefaultCatalogScopes()},
		{Importer: im, Scopes: DefaultPricingScopes()},
		{Importer: im, Scopes: DefaultInventoryScopes()},
		{Importer: im, Scopes: DefaultMemberScopes()},
	}, nil)
	s.chunkSize = 3 // 小块,便于验证分块行为
	return s
}

// productRow 造一行商品导出数据。
func productRow(id, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "category_id": "C1", "supplier_id": "S1",
		"unit": "件", "status": "1", "sub_code": "SUB" + id, "spec": "500ml",
		"price_yuan": "9.900000", "cost_yuan": "7.100000",
	}
}

// ---- 用例 ----

// TestSync_PullsEveryPage 锁"分页拉全"。
//
// 症状:导出 7 行商品,分页 3 行/页 ⇒ 必须拉 3 页、导 7 行。
// 少拉一页的症状是"少了 4 个商品",而导入报告会说"成功"。
func TestSync_PullsEveryPage(t *testing.T) {
	rows := make([]map[string]any, 0, 7)
	for i := 0; i < 7; i++ {
		rows = append(rows, productRow(fmt.Sprintf("P%02d", i), fmt.Sprintf("商品%d", i)))
	}
	ex := &fakeExporter{pages: map[string][]map[string]any{archivedto.ScopeProducts: rows}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopeProducts}, PageSize: 3}

	rep, err := newTestSyncer(cfg, ex, im).Sync(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if got := rep.Scopes[0].Exported; got != 7 {
		t.Errorf("导出行数应为 7,实际 %d", got)
	}
	inserted, _, _ := rep.Total()
	if inserted != 7 {
		t.Errorf("导入行数应为 7,实际 %d", inserted)
	}
	// 7 行 / 每块 3 行 ⇒ 3 块(3+3+1)
	if got := rep.Scopes[0].Chunks; got != 3 {
		t.Errorf("提交次数应为 3,实际 %d", got)
	}
}

// TestSync_EmptyExportIsRefused 锁"空结果不当事实"。
//
// 症状:导出端点返回 0 行。若当成"这个 scope 没有数据"继续导,
// 覆盖模式会把目标门店的该类数据清空 —— 而报告会写"成功 0 条"。
func TestSync_EmptyExportIsRefused(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{archivedto.ScopeProducts: nil}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopeProducts}, PageSize: 10}

	_, err := newTestSyncer(cfg, ex, im).Sync(context.Background())
	if err == nil {
		t.Fatal("空结果必须报错,而不是当成'没有数据'")
	}
	if !strings.Contains(err.Error(), "空结果") {
		t.Errorf("错误文案应点明是空结果,实际: %v", err)
	}
	if len(im.batches) != 0 {
		t.Errorf("空结果时不应提交任何导入请求,实际提交了 %d 次", len(im.batches))
	}
}

// TestSync_BarcodeGroupNotSplitAcrossChunks 是**本包最要紧的一条**。
//
// 条码在覆盖模式下是"按商品整体替换":
// 同一个商品的两条码被切到两块 ⇒ 后一块会把前一块写的删掉。
// 症状:导入报告说"成功 N 条",但扫某些条码查不到商品。
func TestSync_BarcodeGroupNotSplitAcrossChunks(t *testing.T) {
	// 6 个条码,分属 3 个商品(每个商品 2 个条码),chunkSize=3。
	rows := []map[string]any{
		{"barcode": "B1", "product_id": "P1", "label": ""},
		{"barcode": "B2", "product_id": "P2", "label": ""},
		{"barcode": "B3", "product_id": "P1", "label": ""},
		{"barcode": "B4", "product_id": "P2", "label": ""},
		{"barcode": "B5", "product_id": "P3", "label": ""},
		{"barcode": "B6", "product_id": "P3", "label": ""},
	}
	ex := &fakeExporter{pages: map[string][]map[string]any{archivedto.ScopeItemBarcodes: rows}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopeItemBarcodes}, PageSize: 100}

	if _, err := newTestSyncer(cfg, ex, im).Sync(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	// 逐块检查:同一个 product_id 的条码**不能**出现在两块里。
	seen := map[string]int{}
	for i, b := range im.batches {
		for _, r := range b.Data.ItemBarcodes {
			if prev, ok := seen[r.ProductID]; ok && prev != i {
				t.Errorf("商品 %s 的条码被切到了第 %d 块与第 %d 块 —— "+
					"覆盖模式会互相抹掉(导入报告显示成功,但条码扫不出来)", r.ProductID, prev, i)
			}
			seen[r.ProductID] = i
		}
	}
	// 6 个条码 / 组大小 2 / chunkSize 3 ⇒ 每块最多装 1 组(2 行)⇒ 3 块
	if len(im.batches) != 3 {
		t.Errorf("每块最多装 1 组(2 行),6 行应分 3 块,实际 %d 块", len(im.batches))
	}
}

// TestSync_ScopeWithoutOwnerFails 锁"没有归属方的 scope 必须失败"。
//
// ⚠️ 症状(如果放行):那一类数据**静默不导** ——
//
//	报告里只写着别的 scope 成功了,而"售价没导进去"没人知道。
//
// ⇒ 而且必须在**导出之前**就失败(不然白导一遍 27565 行)。
func TestSync_ScopeWithoutOwnerFails(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeProducts: {productRow("P1", "商品")},
	}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopeProducts}, PageSize: 10}

	// 路由表里**没有** products。
	s := NewSyncer(cfg, fakeToken{token: "T"}, ex, []Owner{
		{Importer: im, Scopes: []string{archivedto.ScopePrices}},
	}, nil)

	_, err := s.Sync(context.Background())
	require.Error(t, err, "没有归属方的 scope 必须失败")
	assert.Contains(t, err.Error(), "没有归属方")
	assert.Empty(t, im.snapshot(), "失败前不该发出任何导入请求")
}

// TestSync_DuplicateOwnerFails 锁"一个 scope 不能有两个归属方"。
//
// ⚠️ 症状(如果放行):后一个覆盖前一个 ⇒ 数据进了错的那张表,
//
//	而报告显示"成功"。
func TestSync_DuplicateOwnerFails(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopePrices}, PageSize: 10}

	s := NewSyncer(cfg, fakeToken{token: "T"}, ex, []Owner{
		{Importer: im, Scopes: []string{archivedto.ScopePrices}},
		{Importer: im, Scopes: []string{archivedto.ScopePrices}},
	}, nil)

	_, err := s.Sync(context.Background())
	require.Error(t, err, "一个 scope 两个归属方必须失败")
	assert.Contains(t, err.Error(), "两个归属方")
}

// TestSync_PricesGoToPricingOwner 锁"售价走 pricing 那个下游"。
//
// ⚠️ 这条是"两个真相源"的守卫:售价若被导进 catalog,
//
//	就是 catalog.products 也存价 ⇒ 与 pricing.price_lists 打架。
func TestSync_PricesGoToPricingOwner(t *testing.T) {
	catalog := &fakeImporter{}
	pricing := &fakeImporter{}
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeProducts: {productRow("P1", "商品")},
		archivedto.ScopePrices:   {{"product_id": "P1", "price_yuan": "9.90"}},
	}}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: []string{archivedto.ScopeProducts, archivedto.ScopePrices}, PageSize: 10}

	s := NewSyncer(cfg, fakeToken{token: "T"}, ex, []Owner{
		{Importer: catalog, Scopes: DefaultCatalogScopes()},
		{Importer: pricing, Scopes: DefaultPricingScopes()},
	}, nil)

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	assert.Len(t, catalog.snapshot(), 1, "catalog 只该收到 products")
	assert.Len(t, pricing.snapshot(), 1, "pricing 只该收到 prices")
	assert.Len(t, pricing.snapshot()[0].Data.Prices, 1, "prices 行必须落在 pricing 那边")
	assert.Empty(t, catalog.snapshot()[0].Data.Prices, "catalog 那边不该有价格行")
}

// TestSync_ScopeOrderFollowsDependencies 锁 scope 顺序。
//
// 分类与供应商是商品的引用对象,条码引用商品 ⇒ 必须先引用方后被引用方。
func TestSync_ScopeOrderFollowsDependencies(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeCategories:   {{"id": "C1", "name": "分类", "parent_id": ""}},
		archivedto.ScopeSuppliers:    {{"id": "S1", "name": "供应商"}},
		archivedto.ScopeProducts:     {productRow("P1", "商品")},
		archivedto.ScopeItemBarcodes: {{"barcode": "B1", "product_id": "P1"}},
		archivedto.ScopePrices:       {{"product_id": "P1", "price_yuan": "9.90"}},
		archivedto.ScopePromotions: {{
			"plan_no": "PS-1", "product_id": "P1", "plan_name": "特价",
			"price_yuan": "8.80", "orig_price_yuan": "9.90",
			"begin_day": "2026-09-01", "end_day": "2026-09-30",
		}},
		archivedto.ScopeStock: {{"product_id": "P1", "qty": "12", "avg_cost_yuan": "7.10"}},
		archivedto.ScopeMemberTypes: {{
			"id": "0", "name": "会员卡", "discount_rate": "1",
			"allow_stored_value": "0", "allow_points_pay": "1",
		}},
		archivedto.ScopeMembers: {{
			"id": "M1", "card_type": "0", "name": "张三", "status": "0",
			"balance_yuan": "10", "points_accumulated": "5", "points_used": "1",
		}},
	}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: archivedto.AllScopes, PageSize: 10}

	rep, err := newTestSyncer(cfg, ex, im).Sync(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	var order []string
	for _, s := range rep.Scopes {
		order = append(order, s.Scope)
	}
	want := []string{
		archivedto.ScopeCategories,
		archivedto.ScopeSuppliers,
		archivedto.ScopeProducts,
		archivedto.ScopeItemBarcodes,
		// ⚠️ 售价与促销排在**最后**:它们引用商品(price 的 product_id)。
		//   先导商品再导价 ⇒ 价不会指向还不存在的商品。
		archivedto.ScopePrices,
		archivedto.ScopePromotions,
		// 库存快照排在最后(它引用商品)。
		archivedto.ScopeStock,
		// ⚠️ 会员类别排在会员**之前**(会员引用类别)。
		archivedto.ScopeMemberTypes,
		archivedto.ScopeMembers,
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("scope 顺序应为 %v,实际 %v", want, order)
	}
}

// TestSync_EmptyScopeSetIsRefused 锁"空 scope 集合 ≠ 不过滤"。
//
// 空集合的语义必须是"一个都不导"并**报错**。
// 若当成"不过滤",症状是"报告说同步成功,但库里一行没动" ——
// 而"没动"与"动过"在报告上长得一模一样。
func TestSync_EmptyScopeSetIsRefused(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{}}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", Mode: "overwrite",
		Scopes: nil, PageSize: 10}

	_, err := newTestSyncer(cfg, ex, im).Sync(context.Background())
	if err == nil {
		t.Fatal("空 scope 集合必须报错")
	}
	if !strings.Contains(err.Error(), "SYNC_SCOPES") {
		t.Errorf("错误文案应点明是 SYNC_SCOPES,实际: %v", err)
	}
	if len(im.batches) != 0 {
		t.Errorf("空 scope 时不应提交任何导入请求,实际 %d 次", len(im.batches))
	}
}

// TestBuildImportData_MatchesExportColumns 是**契约守卫**。
//
// 导出端点吐的列名(archivedto.ExportColumns)必须与导入端点吃的字段名逐字一致。
// 不一致的症状:字段静默变零值 —— 商品名全空、条码全空,而报告显示"成功"。
func TestBuildImportData_MatchesExportColumns(t *testing.T) {
	cases := []struct {
		scope string
		build func(map[string]any) archivedto.ImportData
		row   any // 用来反射出 json tag
	}{
		{archivedto.ScopeCategories,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeCategories, []map[string]any{r})
			},
			archivedto.ImportCategoryRow{}},
		{archivedto.ScopeSuppliers,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeSuppliers, []map[string]any{r})
			},
			archivedto.ImportSupplierRow{}},
		{archivedto.ScopeProducts,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeProducts, []map[string]any{r})
			},
			archivedto.ImportProductRow{}},
		{archivedto.ScopeItemBarcodes,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeItemBarcodes, []map[string]any{r})
			},
			archivedto.ImportBarcodeRow{}},
		{archivedto.ScopeMemberTypes,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeMemberTypes, []map[string]any{r})
			},
			archivedto.ImportMemberTypeRow{}},
		{archivedto.ScopeMembers,
			func(r map[string]any) archivedto.ImportData {
				return buildImportData(archivedto.ScopeMembers, []map[string]any{r})
			},
			archivedto.ImportMemberRow{}},
	}

	for _, tc := range cases {
		cols, ok := archivedto.ExportColumns[tc.scope]
		if !ok {
			t.Fatalf("archivedto.ExportColumns 缺 scope %q", tc.scope)
		}
		// 造一行:每个导出列都给一个可辨识的值。
		row := map[string]any{}
		for i, c := range cols {
			row[c] = fmt.Sprintf("v-%d", i)
		}
		data := tc.build(row)
		got := jsonTagsOf(t, tc.row)
		want := append([]string(nil), cols...)
		if !sameSet(got, want) {
			t.Errorf("scope %q 的导入字段名与导出列名不一致\n  导出列: %v\n  导入字段: %v",
				tc.scope, want, got)
		}
		// 列名对上了还不够:**值**也要落到对应的字段上。
		// 列名对、值错位(比如 name 收到 category_id 的值)同样静默。
		if !valuesLanded(data, cols) {
			t.Errorf("scope %q 的列值没有落到对应字段上", tc.scope)
		}
	}
}

// valuesLanded 检查每个导出列的值都能在整形结果里找到。
func valuesLanded(data archivedto.ImportData, cols []string) bool {
	got := map[string]string{}
	switch {
	case len(data.Categories) > 0:
		r := data.Categories[0]
		got["id"], got["name"], got["parent_id"] = r.ID, r.Name, r.ParentID
	case len(data.Suppliers) > 0:
		r := data.Suppliers[0]
		got["id"], got["name"], got["type"] = r.ID, r.Name, r.Type
		got["contact"], got["phone"], got["email"] = r.Contact, r.Phone, r.Email
		got["address"], got["status"] = r.Address, r.Status
	case len(data.Products) > 0:
		r := data.Products[0]
		got["id"], got["name"], got["category_id"] = r.ID, r.Name, r.CategoryID
		got["supplier_id"], got["unit"], got["status"] = r.SupplierID, r.Unit, r.Status
		got["sub_code"], got["spec"] = r.SubCode, r.Spec
		got["price_yuan"], got["cost_yuan"] = r.PriceYuan, r.CostYuan
	case len(data.ItemBarcodes) > 0:
		r := data.ItemBarcodes[0]
		got["barcode"], got["product_id"], got["label"] = r.Barcode, r.ProductID, r.Label
	case len(data.MemberTypes) > 0:
		r := data.MemberTypes[0]
		got["id"], got["name"], got["discount_rate"] = r.ID, r.Name, r.DiscountRate
		got["allow_stored_value"], got["allow_points_pay"] = r.AllowStoredValue, r.AllowPointsPay
	case len(data.Members) > 0:
		r := data.Members[0]
		got["id"], got["card_face_no"], got["card_type"], got["name"] = r.ID, r.CardFaceNo, r.CardType, r.Name
		got["gender"], got["phone"], got["mobile"], got["birthday"] = r.Gender, r.Phone, r.Mobile, r.Birthday
		got["id_card_no"], got["status"] = r.IDCardNo, r.Status
		got["fee_amount_yuan"], got["limited_use_count"] = r.FeeAmountYuan, r.LimitedUseCount
		got["points_accumulated"], got["points_used"] = r.PointsAccumulated, r.PointsUsed
		got["balance_yuan"], got["consumed_amount_yuan"] = r.BalanceYuan, r.ConsumedAmountYuan
		got["consumed_count"], got["favorite"], got["remark"] = r.ConsumedCount, r.Favorite, r.Remark
		got["branch_no"], got["operator_id"], got["operated_at"] = r.BranchNo, r.OperatorID, r.OperatedAt
		got["valid_from"], got["valid_until"], got["open_id"] = r.ValidFrom, r.ValidUntil, r.OpenID
	default:
		return false
	}
	for i, c := range cols {
		if got[c] != fmt.Sprintf("v-%d", i) {
			return false
		}
	}
	return true
}

// TestStrOf_HandlesNullAndNumbers 锁取值helper 的健壮性。
//
// 契约说值只有"字符串或 null",但契约会变。
// 缺一个 nil 判断就是一次空指针 panic(整个同步崩掉)。
func TestStrOf_HandlesNullAndNumbers(t *testing.T) {
	row := map[string]any{
		"s":   "  abc  ",
		"n":   nil,
		"num": 12,
		"f":   1.5,
		"b":   true,
	}
	if got := strOf(row, "s"); got != "abc" {
		t.Errorf("字符串应去空白,实际 %q", got)
	}
	if got := strOf(row, "n"); got != "" {
		t.Errorf("nil 应得空串,实际 %q", got)
	}
	if got := strOf(row, "missing"); got != "" {
		t.Errorf("缺字段应得空串,实际 %q", got)
	}
	if got := strOf(row, "num"); got != "12" {
		t.Errorf("数字应转字符串,实际 %q", got)
	}
	if got := strOf(row, "f"); got != "1.5" {
		t.Errorf("浮点应转字符串,实际 %q", got)
	}
	if got := strOf(row, "b"); got != "true" {
		t.Errorf("布尔应转字符串,实际 %q", got)
	}
}

// jsonTagsOf 反射出结构体的 json tag 名(按声明顺序)。
func jsonTagsOf(t *testing.T, v any) []string {
	t.Helper()
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// sameSet 比较两个字符串集合(顺序无关)。
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}
