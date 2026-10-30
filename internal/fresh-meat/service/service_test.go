// Package service_test 是 fresh-meat 服务的单元测试。
//
// 用 SQLite in-memory + 一个 recording publisher 验证:
//   - 5 张表 CRUD 正确性
//   - 业务校验(branch 不一致 / (branch, cut_type) 未配映射)
//   - purchase_cost 精度(decimal:412 × 28 = 11536)
//   - 事件 payload 无 tenant_id(防误带)
//   - publisher nil → 业务仍成功(noop)
package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/internal/fresh-meat/testdb"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// recordingPublisher 录 publish 调用,便于断言。
type recordingPublisher struct {
	mu     sync.Mutex
	calls  []recordedCall
	logger *recordingLogger
}

type recordedCall struct {
	Topic   string
	Payload any
}

// 静默满足 Publisher 接口(只取 payload + topic)。
func (p *recordingPublisher) Publish(_ context.Context, topic string, data any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, recordedCall{Topic: topic, Payload: data})
	if p.logger != nil {
		p.logger.info("publish", "topic", topic)
	}
	return nil
}

// recordingLogger 把日志消息收集起来,便于断言。
type recordingLogger struct {
	mu   sync.Mutex
	msgs []string
}

func (l *recordingLogger) info(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
}

// setupTestService 起 SQLite + 录 publisher + 固定时钟。
//
// 默认注入**同步**的后台执行器(分割建议同步跑完)⇒ 断言确定性好、不用 sleep。
func setupTestService(t *testing.T) (*service.Service, *recordingPublisher) {
	return setupTestServiceMode(t, true)
}

// setupTestServiceAsync 起 SQLite + 录 publisher + 固定时钟,
// 但**不**覆盖后台执行器 ⇒ 分割建议按生产的**异步**方式生成。
//
// ⚠️ 用于验证"录猪不等 LLM""客户端断开不影响建议"这类**时序**行为
// (2026-10-26 门店反馈的真因就在这里)。
func setupTestServiceAsync(t *testing.T) (*service.Service, *recordingPublisher) {
	return setupTestServiceMode(t, false)
}

func setupTestServiceMode(t *testing.T, syncAdvice bool) (*service.Service, *recordingPublisher) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.WholePig{},
		&model.PigCut{},
		&model.PorkCutsStocktake{},
		&model.LineSalesByPig{},
		&model.BranchCutMapping{},
		&model.WasteLog{},
		// 2026-10-23:SKU 级移动加权成本 + 两位置库存 + 特价档货号。
		&model.SkuStockPosition{},
		&model.SkuCostState{},
		&model.SkuCostDaily{},
		&model.PromoSku{},
		// 2026-10-23:大宗台账 + 分割重量统计基线。
		&model.StockBag{},
		&model.BulkProduct{},
		&model.BulkProduction{},
		&model.BulkSettlement{},
		&model.BulkSettlementItem{},
		&model.BulkSettlementAllocation{},
		&model.PigForecastBaseline{},
		// 2026-10-24:营业日状态机 + 搬台流水 + 按营业日的结余快照 + 期初结转 + 审计结转。
		&model.BusinessDay{},
		&model.CounterLoadRecord{},
		&model.CutStockDaily{},
		&model.SkuStockOpening{},
		&model.BizAudit{},
		// 2026-10-24:预期毛利(预售)结转 + 预售毛利按猪分摊。
		&model.CutPresale{},
		&model.PigPresaleMargin{},
		&model.CutPresaleEdit{},
		// 2026-10-27:成本链改部位级(部位级成本日快照 + 部位级成本状态)。
		&model.CutCostDaily{},
		&model.CutCostState{},
		// 2026-10-30:分割基线的估计样本(A 方案)+ 跨门店选用预留表。
		&model.PigCutEstimate{},
		&model.PigForecastBaselineBorrow{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := service.New(db)
	// 注入固定时间,便于断言。
	// 固定到 Asia/Shanghai 14:15(相当于 06:15 UTC);新买的猪若用 +08:00 偏移能落在当日窗口。
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	fixed := time.Date(2026, 10, 1, 14, 15, 0, 0, bizLoc)
	svc.SetClock(func() time.Time { return fixed })

	pub := &recordingPublisher{}
	svc.SetPublisher(pub)
	// ⚠️ 分割建议改成**异步**生成(2026-10-26)⇒ 单测默认注入**同步**执行器:
	//   "录猪后建议已就绪"是确定性的,不用 sleep 轮询。
	//   (生产保持异步 —— 见 service.SetAdviceRunner 的说明。)
	//   要验证异步时序 ⇒ 用 setupTestServiceAsync。
	if syncAdvice {
		svc.SetAdviceRunner(func(f func()) { f() })
	}
	// ⚠️ 成本链物化默认**禁用**(2026-10-27):物化是后台任务,
	//   若开着,它在断言之后才落库 ⇒ 后面的用例会看到脏数据(竞态)。
	//   要验证"物化确实发生了"⇒ 单独用例注入**同步**执行器
	//   (见 cut_chain_test.go 的 TestCutChain_WritePathMaterializesChain)。
	svc.SetChainMaterializer(func(f func()) {})
	// 把 db 挂到 testDBSingleton,Stage 4 测试需要直接写 llm_advice_json 模拟。
	testDBSingleton = db
	return svc, pub
}

// testDBSingleton 仅 Stage 4 测试用(直接写 whole_pig.llm_advice_json 模拟 LLM 已返 advice)。
// 生产代码不引用;test 之间不并发(每个测试开新 SQLite,无并发)。
var testDBSingleton *gorm.DB

// d 把 string → decimal.Decimal(便于测试输入)。
func d(s string) decimal.Decimal {
	x, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return x
}

// mustNoErr 断言无 error。
func mustNoErr(t *testing.T, err error, label string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// reloadPig 从库里读回整猪。
//
// ⚠️ 分割建议是**异步**写回的(2026-10-26)⇒ 断言建议相关字段时
// **必须查库**,不能只看 RecordWholePig 的返回值(返回值是"刚落库那一刻"的状态,
// data_source 还是 stub、advice_status 是 generating)。
func reloadPig(t *testing.T, svc *service.Service, branchID, pigID string) *model.WholePig {
	t.Helper()
	pig, err := svc.GetWholePig(context.Background(), branchID, pigID)
	mustNoErr(t, err, "GetWholePig")
	return pig
}

// ---- 整猪 ----

func TestRecordWholePig_OK_PurchaseCostPrecision(t *testing.T) {
	svc, pub := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag:                "EB-2026-001",
			GrossWeightKg:         d("412"),
			PurchaseUnitPriceYuan: d("28"),
			SupplierID:            "SUP-101",
			ArrivedAt:             time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 412 × 28 = 11536.000,精度 decimal(12,2) 留 2 位。
	if !pig.PurchaseCostYuan.Equal(d("11536.00")) {
		t.Errorf("purchase_cost = %s, want 11536.00", pig.PurchaseCostYuan)
	}
	// 默认 predictFn = defaultPredictFn → data_source 应为 history_avg
	// (单测没注入真 LLM,降级路径必触发;`stub` 语义在 production 已不可达)。
	//
	// ⚠️ 建议是**异步**写回的 ⇒ 断言必须**查库**(见 reloadPig)。
	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceHistoryAvg {
		t.Errorf("data_source = %q, want history_avg(单测无 LLM,降级路径)", row.DataSource)
	}
	if row.AdviceStatus != model.AdviceStatusReady {
		t.Errorf("advice_status = %q, want ready", row.AdviceStatus)
	}
	if pig.ID == "" || pig.ID[:2] != "WP" {
		t.Errorf("id 前缀应是 WP, got %q", pig.ID)
	}
	if len(pub.calls) != 0 {
		t.Errorf("RecordWholePig 不应发任何事件, got %d calls", len(pub.calls))
	}
}

func TestRecordWholePig_InvalidInput(t *testing.T) {
	svc, _ := setupTestService(t)
	cases := []struct {
		name string
		in   model.RecordWholePigInput
	}{
		{"missing supplier_id", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("28"),
			ArrivedAt: time.Now().UTC(),
		}},
		{"zero gross_weight", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: decimal.Zero, PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}},
		{"negative gross_weight", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("-1"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}},
		{"missing arrived_at", model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RecordWholePig(context.Background(), "S001", tc.in, "u-1")
			if err == nil {
				t.Errorf("应报错, got nil")
			}
		})
	}
}

// TestRecordWholePig_EarTagOptional 锁「耳标号可选」(2026-10-24 门店口径)。
//
// 有些猪没有耳标/不扫码 ⇒ 耳标号不能是必填。
//
// ⚠️ 数据库侧 ear_tag 只是普通索引(不是唯一键)⇒ 多头猪都留空不冲突。
func TestRecordWholePig_EarTagOptional(t *testing.T) {
	svc, _ := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001", model.RecordWholePigInput{
		GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("28"),
		SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
	}, "u-1")
	mustNoErr(t, err, "耳标号留空应能录入")
	if pig.EarTag != "" {
		t.Errorf("ear_tag 应为空, got %q", pig.EarTag)
	}
}

func TestGetWholePig_BranchMismatch(t *testing.T) {
	svc, _ := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 跨店查询 → ErrBranchMismatch
	_, err = svc.GetWholePig(context.Background(), "S002", pig.ID)
	if !errors.Is(err, service.ErrBranchMismatch) {
		t.Errorf("跨店应返 ErrBranchMismatch, got %v", err)
	}

	// 同店查询 → OK
	got, err := svc.GetWholePig(context.Background(), "S001", pig.ID)
	mustNoErr(t, err, "GetWholePig same branch")
	if got.ID != pig.ID {
		t.Errorf("id 不一致: got %s want %s", got.ID, pig.ID)
	}
}

func TestListWholePigsByDay(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-A", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: day.Add(6 * time.Hour),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig A")
	_, err = svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-B", GrossWeightKg: d("420"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: day.Add(7 * time.Hour),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig B")

	rows, err := svc.ListWholePigsByDay(context.Background(), "S001", day)
	mustNoErr(t, err, "ListWholePigsByDay")
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}

// ---- pig_cuts ----

func TestRecordPigCut_RequiresMapping(t *testing.T) {
	svc, _ := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 未配 mapping → ErrInvalidInput
	_, err = svc.RecordPigCut(context.Background(), "S001",
		model.RecordPigCutInput{
			PigID: pig.ID, CutType: model.CutBelly,
			Barcode: "BC-001", WeightKg: d("20"),
			ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
		}, "u-1")
	if err == nil {
		t.Errorf("未配 mapping 应报错")
	}
}

func TestRecordPigCut_BranchMismatch(t *testing.T) {
	svc, _ := setupTestService(t)
	// 先在 S001 录一头 + 配一个 mapping。
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{
			CutType: model.CutBelly, CubeProductID: "P-2001",
		})
	mustNoErr(t, err, "CreateBranchCutMapping")

	// 跨店 pig_cut → ErrBranchMismatch
	_, err = svc.RecordPigCut(context.Background(), "S002",
		model.RecordPigCutInput{
			PigID: pig.ID, CutType: model.CutBelly,
			Barcode: "BC-001", WeightKg: d("20"),
			ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
		}, "u-1")
	if !errors.Is(err, service.ErrBranchMismatch) {
		t.Errorf("跨店 pig_cut 应返 ErrBranchMismatch, got %v", err)
	}

	// 同店 OK
	cut, err := svc.RecordPigCut(context.Background(), "S001",
		model.RecordPigCutInput{
			PigID: pig.ID, CutType: model.CutBelly,
			Barcode: "BC-001", WeightKg: d("20"),
			ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
		}, "u-1")
	mustNoErr(t, err, "RecordPigCut same branch")
	if cut.PigID != pig.ID || cut.CutType != model.CutBelly {
		t.Errorf("cut 不一致: %+v", cut)
	}
}

func TestListPigCutsByPig(t *testing.T) {
	svc, _ := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-1", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Now().UTC(),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	for _, ct := range []model.CutType{model.CutBelly, model.CutRib, model.CutTrotter} {
		// ⚠️ 每个部位用**自己的** cube_product_id:
		// 2026-10-23 起 branch_cut_mappings 的唯一约束是
		// UNIQUE(branch_id, cube_product_id) —— **一个 SKU 在一个店只对应一个部位**
		// (resolvePigByCubeProduct 按 (branch, cube_product_id) 反查部位,必须唯一)。
		// 同一个 SKU 挂多个部位会让"这笔销售算哪个部位"变成不确定 ⇒ 约束层直接禁掉。
		_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
			model.CreateBranchCutMappingInput{CutType: ct, CubeProductID: "P-" + string(ct)})
		mustNoErr(t, err, "CreateBranchCutMapping "+string(ct))
		_, err = svc.RecordPigCut(context.Background(), "S001",
			model.RecordPigCutInput{
				PigID: pig.ID, CutType: ct,
				Barcode: "BC-" + string(ct), WeightKg: d("10"),
				ExpiresAt: time.Now().UTC().Add(48 * time.Hour),
			}, "u-1")
		mustNoErr(t, err, "RecordPigCut "+string(ct))
	}
	cuts, err := svc.ListPigCutsByPig(context.Background(), "S001", pig.ID)
	mustNoErr(t, err, "ListPigCutsByPig")
	if len(cuts) != 3 {
		t.Errorf("cuts = %d, want 3", len(cuts))
	}
}

// ---- pork_cuts_stocktake ----

func TestRecordPorkCutsStocktake_PublishesWhenComplete(t *testing.T) {
	svc, pub := setupTestService(t)
	// 配 belly / rib mapping(CubeProductID 必填校验依赖)
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutRib, CubeProductID: "P-RIB"})
	mustNoErr(t, err, "CreateBranchCutMapping rib")
	st, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("56.5")},
				{CutType: model.CutRib, CubeProductID: "P-RIB", ActualRemainKg: d("30.0")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
	if !st.IsComplete {
		t.Errorf("is_complete 应为 true")
	}
	if len(pub.calls) != 1 {
		t.Fatalf("is_complete=true 应发 1 个事件, got %d", len(pub.calls))
	}
	if pub.calls[0].Topic != service.TopicPorkCutsStocktaken {
		t.Errorf("topic = %q, want %q", pub.calls[0].Topic, service.TopicPorkCutsStocktaken)
	}
	// payload 应有 stocktake_id
	data, ok := pub.calls[0].Payload.(*model.PorkCutsStocktakenEventData)
	if !ok {
		t.Fatalf("payload 类型 = %T, want *PorkCutsStocktakenEventData", pub.calls[0].Payload)
	}
	if data.StocktakeID != st.ID {
		t.Errorf("stocktake_id 不一致: %s vs %s", data.StocktakeID, st.ID)
	}
}

func TestRecordPorkCutsStocktake_PartialDoesNotPublish(t *testing.T) {
	svc, pub := setupTestService(t)
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	_, err = svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: false,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("56.5")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")
	if len(pub.calls) != 0 {
		t.Errorf("is_complete=false 不应发事件, got %d", len(pub.calls))
	}
}

func TestGetLatestPorkCutsStocktake(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// 配 belly mapping
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	// 录 2 次,后一次应排在前
	for i := 0; i < 2; i++ {
		_, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
			model.RecordPorkCutsStocktakeInput{
				IsComplete: true,
				Cuts: []model.CutInput{
					{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("50")},
				},
			}, "u-1")
		mustNoErr(t, err, "RecordPorkCutsStocktake")
		time.Sleep(10 * time.Millisecond) // 让 taken_at 不一样
	}
	latest, err := svc.GetLatestPorkCutsStocktake(context.Background(), "S001", day)
	mustNoErr(t, err, "GetLatestPorkCutsStocktake")
	if latest == nil || latest.ID == "" {
		t.Errorf("latest 应非空")
	}
}

// ---- waste_log ----

func TestRecordWasteLog_Publishes(t *testing.T) {
	svc, pub := setupTestService(t)
	out, err := svc.RecordWasteLog(context.Background(), "S001",
		model.RecordWasteLogInput{
			PigID: "WP-TEST", CutType: model.CutBelly, QtyKg: d("2.5"), Reason: "破损",
		}, "u-1")
	mustNoErr(t, err, "RecordWasteLog")
	if out == nil || out.ID == "" {
		t.Fatalf("RecordWasteLog 应返回持久化记录, got %v", out)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("应发 1 个事件, got %d", len(pub.calls))
	}
	if pub.calls[0].Topic != service.TopicWasteLogRecorded {
		t.Errorf("topic = %q, want %q", pub.calls[0].Topic, service.TopicWasteLogRecorded)
	}
}

// ---- branch_cut_mapping CRUD ----

func TestBranchCutMapping_CRUD(t *testing.T) {
	svc, _ := setupTestService(t)
	// Create
	m, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	if m.BranchID != "S001" || m.CutType != model.CutBelly || m.CubeProductID != "P-2001" {
		t.Errorf("mapping 字段不一致: %+v", m)
	}

	// 冲突 → ErrBranchCutMappingConflict
	//
	// ⚠️ 2026-10-23 起冲突的判定键变了:唯一约束是
	// UNIQUE(branch_id, cube_product_id)(一个 SKU 在一个店只对应一个部位),
	// 而**不是** UNIQUE(branch_id, cut_type)。
	// ⇒ 同一个部位挂**不同** SKU 是**允许的**(见下面 TestBranchCutMapping_SameCutManySKUs);
	// 同一个 SKU 挂**不同**部位才是冲突(会让销售归属变得不确定)。
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	if !errors.Is(err, service.ErrBranchCutMappingConflict) {
		t.Errorf("同 SKU 重复挂载应返 ErrBranchCutMappingConflict, got %v", err)
	}

	// List
	rows, err := svc.ListBranchCutMappings(context.Background(), "S001")
	mustNoErr(t, err, "ListBranchCutMappings")
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}

	// Update cube_product_id
	upd, err := svc.UpdateBranchCutMapping(context.Background(), "S001", m.ID,
		model.UpdateBranchCutMappingInput{CubeProductID: "P-9999"})
	mustNoErr(t, err, "UpdateBranchCutMapping")
	if upd.CubeProductID != "P-9999" {
		t.Errorf("cube_product_id = %s, want P-9999", upd.CubeProductID)
	}

	// Delete
	if err := svc.DeleteBranchCutMapping(context.Background(), "S001", m.ID); err != nil {
		t.Fatalf("DeleteBranchCutMapping: %v", err)
	}

	// 再次 list 应为空
	rows, err = svc.ListBranchCutMappings(context.Background(), "S001")
	mustNoErr(t, err, "ListBranchCutMappings after delete")
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// TestBranchCutMapping_SameCutManySKUs 锁定 2026-10-23 的约束变更。
//
// 一个部位在一个店可以挂**多个** SKU —— 真源库实测这是常态:
//
//	佳乐(01)  : belly = 腩肉(五花肉) + 精品五花肉;rib = 前排(硬排) + 精品排骨
//	富百家(00): rib  = 精选腩排 + 精选排骨;shoulder_blade = 梅花肉 + 梅肉/腱子肉;
//	            trotter = 猪脚 + 猪手;intestine = 大肠 + 生肠
//
// 且日终盘点要能**按 SKU 分别**引用同一个部位下的多个 SKU ——
// 旧约束(UNIQUE(branch_id, cut_type))下第二个 SKU 直接报
// "(branch, cut) 映射已存在" / "未在 branch_cut_mappings 配置",
// per-SKU 盘点形同虚设。
func TestBranchCutMapping_SameCutManySKUs(t *testing.T) {
	svc, _ := setupTestService(t)

	// 同一部位挂两个 SKU —— 都必须成功
	for _, sku := range []string{"P-BELLY-1", "P-BELLY-2"} {
		_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
			model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: sku})
		mustNoErr(t, err, "CreateBranchCutMapping "+sku)
	}

	rows, err := svc.ListBranchCutMappings(context.Background(), "S001")
	mustNoErr(t, err, "ListBranchCutMappings")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2(同部位应能挂多个 SKU)", len(rows))
	}

	// 日终盘点按 SKU 分别引用同一个部位下的两个 SKU —— 必须都接受
	st, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-BELLY-1", ActualRemainKg: d("30")},
				{CutType: model.CutBelly, CubeProductID: "P-BELLY-2", ActualRemainKg: d("26.5")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake 同部位多 SKU")

	var snaps []model.CutSnapshot
	if err := json.Unmarshal(st.Cuts, &snaps); err != nil {
		t.Fatalf("cuts JSON 解析: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("cuts = %d, want 2", len(snaps))
	}
	seen := map[string]bool{}
	for _, s := range snaps {
		if s.CutType != model.CutBelly {
			t.Errorf("cut_type = %s, want belly", s.CutType)
		}
		seen[s.CubeProductID] = true
	}
	if !seen["P-BELLY-1"] || !seen["P-BELLY-2"] {
		t.Errorf("盘点行应同时含两个 SKU, got %v", seen)
	}
}

// TestBranchCutMapping_RejectsUnknownCut 锁定 2026-10-23 补的入参校验:
// cut_type 必须在枚举内(以前是"建了再在盘点时报映射未配置",报错点离原因很远)。
func TestBranchCutMapping_RejectsUnknownCut(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutType("not_a_real_cut"), CubeProductID: "P-X"})
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("未知 cut_type 应返 ErrInvalidInput, got %v", err)
	}
}

// ---- 事件 payload 无 tenant_id 兜底测试 ----

func TestEventPayload_NoTenantID(t *testing.T) {
	svc, pub := setupTestService(t)

	// 配 belly mapping
	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	// 录盘点(is_complete=true → 发 pork.cuts.stocktaken)
	_, err = svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("50")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	// 报损 → 发 waste.log.recorded
	_, err = svc.RecordWasteLog(context.Background(), "S001",
		model.RecordWasteLogInput{CutType: model.CutBelly, QtyKg: d("1.0"), Reason: "测试"},
		"u-1")
	mustNoErr(t, err, "RecordWasteLog")

	if len(pub.calls) != 2 {
		t.Fatalf("应发 2 个事件, got %d", len(pub.calls))
	}
	for _, call := range pub.calls {
		raw, err := json.Marshal(call.Payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", call.Topic, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", call.Topic, err)
		}
		if _, ok := m["tenant_id"]; ok {
			t.Errorf("%s payload 含 tenant_id 字段: %s", call.Topic, raw)
		}
	}
}

// ---- OnSaleCompleted 测试 ----

// seedPigForSaleTests 录一头猪 + 配一个 belly 映射(为 resolvePigByCubeProduct 准备)。
func seedPigForSaleTests(t *testing.T, svc *service.Service) model.WholePig {
	t.Helper()
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-SALE", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	return *pig
}

func TestOnSaleCompleted_MeatLinesOnly(t *testing.T) {
	svc, _ := setupTestService(t)
	pig := seedPigForSaleTests(t, svc)

	// 模拟 POS 推送:3 行,只有 2 行是 meat。
	inserted, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-001",
		BranchID:    "S001",
		OperatorID:  "u-pos",
		CompletedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("3.5"), AmountYuan: d("120"), PigID: pig.ID, CutType: model.CutBelly},
			{LineID: "L-2", SKUID: "TOMATO", FreshType: "produce",
				Qty: d("2.0"), AmountYuan: d("30")}, // produce → drop
			{LineID: "L-3", SKUID: "P-2001", FreshType: "meat",
				Qty: d("0.5"), AmountYuan: d("18"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	if inserted != 2 {
		t.Errorf("inserted = %d, want 2(只 meat 行)", inserted)
	}

	rows, err := svc.ListLineSalesByPig(context.Background(), "S001", pig.ID, nil, time.Time{}, time.Time{})
	mustNoErr(t, err, "ListLineSalesByPig")
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
}

func TestOnSaleCompleted_DedupByPosLineID(t *testing.T) {
	svc, _ := setupTestService(t)
	pig := seedPigForSaleTests(t, svc)

	line := service.SaleLine{
		LineID: "L-DUP", SKUID: "P-2001", FreshType: "meat",
		Qty: d("3.5"), AmountYuan: d("120"), PigID: pig.ID, CutType: model.CutBelly,
	}
	payload := service.SaleCompletedPayload{
		SaleID: "SO-DUP", BranchID: "S001",
		CompletedAt: time.Now().UTC(),
		Lines:       []service.SaleLine{line},
	}
	// 第一次投递
	if _, err := svc.OnSaleCompleted(context.Background(), payload); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// 第二次投递(dapr at-least-once 重投)
	if _, err := svc.OnSaleCompleted(context.Background(), payload); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	rows, err := svc.ListLineSalesByPig(context.Background(), "S001", pig.ID, nil, time.Time{}, time.Time{})
	mustNoErr(t, err, "ListLineSalesByPig")
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1(UNIQUE 约束去重)", len(rows))
	}
}

func TestOnSaleCompleted_FallbackByBranchCutMapping(t *testing.T) {
	svc, _ := setupTestService(t)
	pig := seedPigForSaleTests(t, svc) // 当日 S001 唯一一头猪

	// L-X 缺 pig_id 但有 cube_product_id=P-2001 → 应通过 mapping 解析到 pig.ID
	inserted, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-FALLBACK",
		BranchID:    "S001",
		CompletedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		Lines: []service.SaleLine{
			{LineID: "L-X", SKUID: "P-2001", FreshType: "meat",
				Qty: d("2.0"), AmountYuan: d("70")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	if inserted != 1 {
		t.Errorf("inserted = %d, want 1(fallback)", inserted)
	}

	rows, err := svc.ListLineSalesByPig(context.Background(), "S001", pig.ID, nil, time.Time{}, time.Time{})
	mustNoErr(t, err, "ListLineSalesByPig")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].CutType != model.CutBelly {
		t.Errorf("cut_type = %q, want belly", rows[0].CutType)
	}
	if rows[0].CubeProductID != "P-2001" {
		t.Errorf("cube_product_id = %q, want P-2001", rows[0].CubeProductID)
	}
}

func TestOnSaleCompleted_DropsWhenNoMappingNoPigID(t *testing.T) {
	svc, _ := setupTestService(t)
	seedPigForSaleTests(t, svc)

	// L-Y 缺 pig_id 且 cube_product_id 也不在 mapping → drop
	inserted, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-DROP",
		BranchID:    "S001",
		CompletedAt: time.Now().UTC(),
		Lines: []service.SaleLine{
			{LineID: "L-Y", SKUID: "P-NONEXIST", FreshType: "meat",
				Qty: d("2.0"), AmountYuan: d("70")},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")
	if inserted != 0 {
		t.Errorf("inserted = %d, want 0(drop)", inserted)
	}
}

func TestOnSaleCompleted_MissingBranchID(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID: "SO-X", BranchID: "",
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P", FreshType: "meat", Qty: d("1"), AmountYuan: d("1")},
		},
	})
	if err == nil {
		t.Errorf("branch_id 空应报错")
	}
}

// ---- OnAccessChanged ----

func TestOnAccessChanged_InvalidatesCache(t *testing.T) {
	svc, _ := setupTestService(t)
	// 写入假缓存
	svc.InvalidateScopeCache("u-1", "S001") // 没缓存,noop,验证 API 存在
	// InvalidateScopeCache 静默接受空 / 不存在的 cache;不能报错。
	// (userd 不注入时 GetEffectiveScopes 才会返 ErrUserInfoUnavailable)
	svc.OnAccessChanged(context.Background(), "u-1", "S001")
}

// ---- Stage 3 LLM 集成 ----

// fakePredictFn 让 service 调一个**模拟 LLM** 的 predict。
type fakePredictFn struct {
	calls int
	// fn 允许返回 (result, err);若 err != nil,service 内部走 defaultPredictFn(history_avg)。
	fn func(ctx context.Context, pig *model.WholePig, history []model.WholePig) (*service.LLMResult, error)
}

func (f *fakePredictFn) Predict(ctx context.Context, pig *model.WholePig, history []model.WholePig) (*service.LLMResult, error) {
	f.calls++
	if f.fn == nil {
		return defaultLLMResult(), nil
	}
	return f.fn(ctx, pig, history)
}

// defaultLLMResult 是 fakePredictFn 不传 fn 时返的固定 LLM 成功响应。
func defaultLLMResult() *service.LLMResult {
	return &service.LLMResult{
		Cuts: []service.PredictedCut{
			{CutType: model.CutBelly, ExpectedKg: d("20.5")},
			{CutType: model.CutRib, ExpectedKg: d("15.0")},
		},
		RawJSON: []byte(`{"cuts":[{"cut_type":"belly","expected_kg":20.5},{"cut_type":"rib","expected_kg":15}]}`),
		Source:  "llm",
	}
}

func TestRecordWholePig_LLM_Success(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{fn: func(_ context.Context, pig *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return defaultLLMResult(), nil
	}}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-LLM", GrossWeightKg: d("412"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	if fake.calls != 1 {
		t.Errorf("predictFn calls = %d, want 1", fake.calls)
	}
	// ⚠️ 建议是**异步**写回的 ⇒ 查库断言(见 reloadPig)。
	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceLLM {
		t.Errorf("data_source = %q, want llm", row.DataSource)
	}
	if len(row.LLMAdviceJSON) == 0 {
		t.Errorf("llm_advice_json 应非空")
	}
	if row.AdviceStatus != model.AdviceStatusReady {
		t.Errorf("advice_status = %q, want ready", row.AdviceStatus)
	}
}

func TestRecordWholePig_LLM_Timeout_FallbackHistory(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{fn: func(_ context.Context, pig *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return nil, errors.New("LLM timeout after 30s")
	}}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-TO", GrossWeightKg: d("412"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig must not error on LLM fail")
	if fake.calls != 1 {
		t.Errorf("predictFn 调用 1 次后降级,got %d", fake.calls)
	}
	// ⚠️ 建议是**异步**写回的 ⇒ 查库断言。
	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceHistoryAvg {
		t.Errorf("data_source = %q, want history_avg", row.DataSource)
	}
	// llm_advice_json 应记录"fallback_used" 元数据(由 defaultPredictFn 写入)
	if len(row.LLMAdviceJSON) == 0 {
		t.Errorf("history_avg fallback 也应写 llm_advice_json(占位)")
	}
}

func TestRecordWholePig_LLM_5xx_FallbackHistory(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{fn: func(_ context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return nil, errors.New("dapr converse: status 502")
	}}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-5xx", GrossWeightKg: d("412"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// ⚠️ 建议是**异步**写回的 ⇒ 查库断言。
	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceHistoryAvg {
		t.Errorf("data_source = %q, want history_avg", row.DataSource)
	}
}

func TestRecordWholePig_DefaultPredictFn_HistoryAvg(t *testing.T) {
	svc, _ := setupTestService(t)
	// 没调 SetPredictFn,默认走 defaultPredictFn(history_avg 降级)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-DEF", GrossWeightKg: d("412"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// ⚠️ 建议是**异步**写回的 ⇒ 查库断言。
	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceHistoryAvg {
		t.Errorf("data_source = %q, want history_avg(默认降级路径)", row.DataSource)
	}
}

// ---- Stage 4 服务端 expected_remain_kg ----

func TestRecordPorkCutsStocktake_ServerSideExpectedRemainder(t *testing.T) {
	svc, _ := setupTestService(t)

	// 准备:当日一头猪 + pig_cuts belly=20kg + 1 笔 sale belly=3.5kg
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-D", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: day,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// 配 belly mapping → pig_cuts 录入
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	_, err = svc.RecordPigCut(context.Background(), "S001",
		model.RecordPigCutInput{
			PigID: pig.ID, CutType: model.CutBelly,
			Barcode: "BC-1", WeightKg: d("20"),
			ExpiresAt: day.Add(48 * time.Hour),
		}, "u-1")
	mustNoErr(t, err, "RecordPigCut belly 20kg")

	// 销售聚合 belly=3.5kg(模拟 OnSaleCompleted 已落库)
	_, err = svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-D",
		BranchID:    "S001",
		CompletedAt: day.Add(4 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-BELLY", SKUID: "P-2001", FreshType: "meat",
				Qty: d("3.5"), AmountYuan: d("120"),
				PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")

	// 仓管录盘点:belly actual=16.0kg(估算)
	st, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("16.0")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	// 解析 + 断言
	var parsedCuts []model.CutSnapshot
	mustNoErr(t, json.Unmarshal(st.Cuts, &parsedCuts), "unmarshal cuts")
	if len(parsedCuts) != 1 {
		t.Fatalf("cuts = %d, want 1", len(parsedCuts))
	}
	cut := parsedCuts[0]
	if cut.CutType != model.CutBelly {
		t.Errorf("cut_type = %q, want belly", cut.CutType)
	}
	// expected = 20(pig_cuts) - 3.5(sales) = 16.5
	if !cut.ExpectedRemainKg.Equal(d("16.5")) {
		t.Errorf("expected_remain_kg = %s, want 16.5", cut.ExpectedRemainKg)
	}
	// actual = 16.0(仓管录入)
	if !cut.ActualRemainKg.Equal(d("16.0")) {
		t.Errorf("actual_remain_kg = %s, want 16.0", cut.ActualRemainKg)
	}
	// variance = actual - expected = -0.5
	if !cut.VarianceKg.Equal(d("-0.5")) {
		t.Errorf("variance_kg = %s, want -0.5", cut.VarianceKg)
	}
}

func TestRecordPorkCutsStocktake_LLMAdviceFallback(t *testing.T) {
	svc, _ := setupTestService(t)

	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	// 一头猪,但没 pig_cuts 录入;只用 llm_advice 推演。
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-NL", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: day,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// 注入 llm_advice_json:belly=18,rib=12
	if err := testDBSingleton.Model(pig).Update("llm_advice_json",
		[]byte(`{"cuts":[{"cut_type":"belly","expected_kg":18},{"cut_type":"rib","expected_kg":12}]}`)).Error; err != nil {
		t.Fatalf("update llm_advice: %v", err)
	}
	// 配 belly mapping(CubeProductID 必填校验依赖)
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	// 仓管录 belly actual=18(完全符合推演)
	st, err := svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: false, // partial,不发事件
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("18.0")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	var parsedCuts []model.CutSnapshot
	mustNoErr(t, json.Unmarshal(st.Cuts, &parsedCuts), "unmarshal")
	// 仅 belly 录,只 1 条 cuts(仓管只录 belly actual;rib 无输入不入 cuts)。
	if len(parsedCuts) != 1 {
		t.Fatalf("cuts = %d, want 1(per-SKU, 仅 belly 录入)", len(parsedCuts))
	}
	belly := findCut(parsedCuts, model.CutBelly)
	if belly == nil {
		t.Fatalf("belly 不在 cuts 里")
	}
	// pig_cuts 表空 → belly 全靠 llm_advice 兜底
	if !belly.ExpectedRemainKg.Equal(d("18.0")) {
		t.Errorf("belly expected_remain_kg = %s, want 18.0(llm_advice 兜底)", belly.ExpectedRemainKg)
	}
	if !belly.ActualRemainKg.Equal(d("18.0")) {
		t.Errorf("belly actual_remain_kg = %s, want 18.0", belly.ActualRemainKg)
	}
}

// ---- Stage D ComputeGrossMargin ----

// TestComputeGrossMargin_Estimated_NoStocktake 验证无 stocktake 时返 estimated,
// 含 per-cut + total,且 R 行不入 revenue/sold_kg。
func TestComputeGrossMargin_Estimated_NoStocktake(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	// 1 头猪,LLM 给出 belly=20,rib=15(pig_cuts 暂未录,依赖 LLM 兜底)
	fake := &fakePredictFn{fn: func(_ context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return &service.LLMResult{
			Cuts: []service.PredictedCut{
				{CutType: model.CutBelly, ExpectedKg: d("20")},
				{CutType: model.CutRib, ExpectedKg: d("15")},
			},
			RawJSON: []byte(`{"cuts":[{"cut_type":"belly","expected_kg":20},{"cut_type":"rib","expected_kg":15}]}`),
			Source:  "llm",
		}, nil
	}}
	svc.SetPredictFn(fake.Predict)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-GM", GrossWeightKg: d("400"), PurchaseUnitPriceYuan: d("28"),
			SupplierID: "SUP-1", ArrivedAt: day,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 配 belly / rib mapping(sale 行需要)
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")
	_, err = svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutRib, CubeProductID: "P-2002"})
	mustNoErr(t, err, "CreateBranchCutMapping rib")

	// 销售:belly 5kg×34=170 / rib 3kg×28=84,以及退货 belly 1kg×34=-34
	_, err = svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-GM-1",
		BranchID:    "S001",
		CompletedAt: day.Add(2 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-GM-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("5"), AmountYuan: d("170"), PigID: pig.ID, CutType: model.CutBelly},
			{LineID: "L-GM-2", SKUID: "P-2002", FreshType: "meat",
				Qty: d("3"), AmountYuan: d("84"), PigID: pig.ID, CutType: model.CutRib},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted S")
	_, err = svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID:      "SO-GM-R",
		BranchID:    "S001",
		OrderStatus: "R",
		CompletedAt: day.Add(3 * time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-GM-R", SKUID: "P-2001", FreshType: "meat",
				Qty: d("-1"), AmountYuan: d("-34"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted R")

	// 报损:belly 0.2
	_, err = svc.RecordWasteLog(context.Background(), "S001",
		model.RecordWasteLogInput{
			CutType: model.CutBelly, CubeProductID: "P-2001",
			QtyKg: d("0.2"), Reason: "过期",
		}, "u-1")
	mustNoErr(t, err, "RecordWasteLog")

	// 调毛利
	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	if report.DataSource != "estimated" {
		t.Errorf("data_source = %q, want estimated(无 stocktake)", report.DataSource)
	}
	if report.IsStocktakeComplete {
		t.Errorf("is_stocktake_complete 应为 false")
	}
	if report.Date != "2026-10-01" {
		t.Errorf("date = %q, want 2026-10-01(bizTZ 当天)", report.Date)
	}

	// per_cut 应含 belly + rib
	var bellyRow, ribRow *service.PerCutGrossMargin
	for i := range report.PerCut {
		switch report.PerCut[i].CutType {
		case model.CutBelly:
			bellyRow = &report.PerCut[i]
		case model.CutRib:
			ribRow = &report.PerCut[i]
		}
	}
	if bellyRow == nil || ribRow == nil {
		t.Fatalf("belly 或 rib 不在 per_cut: %+v", report.PerCut)
	}
	// belly:sold=5(退货 R 不算) / revenue=170(退货不算) / waste=0.2
	if !bellyRow.SoldKg.Equal(d("5")) {
		t.Errorf("belly sold_kg = %s, want 5(R 行不计入)", bellyRow.SoldKg)
	}
	if !bellyRow.RevenueYuan.Equal(d("170")) {
		t.Errorf("belly revenue = %s, want 170(R 行不计入)", bellyRow.RevenueYuan)
	}
	if !bellyRow.WasteKg.Equal(d("0.2")) {
		t.Errorf("belly waste_kg = %s, want 0.2", bellyRow.WasteKg)
	}
	// purchased 来自 LLM 兜底(无 pig_cuts)=20
	if !bellyRow.PurchasedKg.Equal(d("20")) {
		t.Errorf("belly purchased_kg = %s, want 20(LLM 兜底)", bellyRow.PurchasedKg)
	}
	// expected = 0 + 20 - 5 - 0.2 = 14.8
	if !bellyRow.ExpectedRemainKg.Equal(d("14.8")) {
		t.Errorf("belly expected_remain_kg = %s, want 14.8", bellyRow.ExpectedRemainKg)
	}
	// cost allocation:无盘点时按**账面期末**结转(等价于 sold × 加权单价):
	//   pig 总成本 400×28=11200;LLM belly=20,total LLM=20+15=35
	//   belly 入库金额 = 11200 × 20/35 = 6400
	//   加权单价 = 6400 / 20 = 320 元/kg
	//   账面期末 = 20 − 5(卖) − 0.2(报损) = 14.8 → 期末金额 = 14.8 × 320 = 4736
	//   销售成本(零售那 5kg) = 6400 − 4736 − 0 − 64(报损成本) = 1600
	if !bellyRow.SalesCostYuan.Equal(d("1600")) {
		t.Errorf("belly sales_cost = %s, want 1600", bellyRow.SalesCostYuan)
	}
	// ⚠️ 2026-10-23 修正:**报损成本必须进损益**。
	//
	// 原来 gm = 收入 − 销售成本,而销售成本里**已经把报损成本减掉了**
	// (它从"可售金额"里被扣除,不代表成本已入账)
	// ⇒ 报损的 64 元成本**凭空消失** ⇒ 报损不体现为损失、毛利虚高 64 元。
	//
	// 正确:毛利 = 收入 − 销售成本 − 统收成本 − 报损成本
	//      = 170 − 1600 − 0 − 64 = **−1494**
	if !bellyRow.WasteCostYuan.Equal(d("64")) {
		t.Errorf("belly waste_cost = %s, want 64(0.2kg × 320 元/kg)", bellyRow.WasteCostYuan)
	}
	if !bellyRow.GrossMarginYuan.Equal(d("-1494")) {
		t.Errorf("belly gm = %s, want -1494(170 收入 − 1600 销售成本 − 64 报损成本)", bellyRow.GrossMarginYuan)
	}

	// total
	if !report.Total.RevenueYuan.Equal(d("254")) {
		t.Errorf("total revenue = %s, want 254(170+84)", report.Total.RevenueYuan)
	}
	if report.Total.SalesCostYuan.IsZero() {
		t.Errorf("total cost 不应为 0")
	}
}

// TestComputeGrossMargin_Actual_WithStocktake 验证 is_complete=true 时返 actual,
// 含 actual_remain_kg + variance_kg。
func TestComputeGrossMargin_Actual_WithStocktake(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)

	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	// 仓管录盘点 actual=10(无 pig_cuts / LLM → expected=0)
	_, err = svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("10")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake")

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	if report.DataSource != "actual" {
		t.Errorf("data_source = %q, want actual", report.DataSource)
	}
	if !report.IsStocktakeComplete {
		t.Errorf("is_stocktake_complete 应为 true")
	}
	var belly *service.PerCutGrossMargin
	for i := range report.PerCut {
		if report.PerCut[i].CutType == model.CutBelly {
			belly = &report.PerCut[i]
		}
	}
	if belly == nil {
		t.Fatalf("belly 不在 per_cut")
	}
	// 实盘/差异是**指针**:nil = 没有实盘(2026-10-26 起)。
	if belly.ActualRemainKg == nil {
		t.Fatalf("belly actual_remain_kg 不该为空(完整盘点必须有实盘)")
	}
	if !belly.ActualRemainKg.Equal(d("10")) {
		t.Errorf("belly actual_remain_kg = %s, want 10", *belly.ActualRemainKg)
	}
	// expected = 0 + 0 - 0 - 0 = 0;variance = 10 - 0 = 10
	if !belly.ExpectedRemainKg.Equal(decimal.Zero) {
		t.Errorf("belly expected_remain_kg = %s, want 0", belly.ExpectedRemainKg)
	}
	if belly.VarianceKg == nil {
		t.Fatalf("belly variance_kg 不该为空(完整盘点必须有差异)")
	}
	if !belly.VarianceKg.Equal(d("10")) {
		t.Errorf("belly variance_kg = %s, want 10", *belly.VarianceKg)
	}
}

// TestComputeGrossMargin_OpeningCarryForward 验证昨夜库存结转进 opening_kg。
func TestComputeGrossMargin_OpeningCarryForward(t *testing.T) {
	svc, _ := setupTestService(t)

	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	// 昨夜 2026-09-30 is_complete=true belly actual=5
	// 把时钟拨到昨夜再录,使其 taken_at 落在 2026-09-30 窗口内
	bizLoc := time.FixedZone("Asia/Shanghai", 8*3600)
	svc.SetClock(func() time.Time {
		return time.Date(2026, 9, 30, 22, 0, 0, 0, bizLoc)
	})
	_, err = svc.RecordPorkCutsStocktake(context.Background(), "S001",
		model.RecordPorkCutsStocktakeInput{
			IsComplete: true,
			Cuts: []model.CutInput{
				{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("5")},
			},
		}, "u-1")
	mustNoErr(t, err, "RecordPorkCutsStocktake yesterday")

	// 时钟拨回今日 10:00 再查毛利
	svc.SetClock(func() time.Time {
		return time.Date(2026, 10, 1, 10, 0, 0, 0, bizLoc)
	})
	today := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	report, err := svc.ComputeGrossMargin(context.Background(), "S001", today)
	mustNoErr(t, err, "ComputeGrossMargin")
	if report.DataSource != "estimated" {
		t.Errorf("data_source = %q, want estimated(今日未盘)", report.DataSource)
	}

	var belly *service.PerCutGrossMargin
	for i := range report.PerCut {
		if report.PerCut[i].CutType == model.CutBelly {
			belly = &report.PerCut[i]
		}
	}
	if belly == nil {
		t.Fatalf("belly 不在 per_cut")
	}
	if !belly.OpeningKg.Equal(d("5")) {
		t.Errorf("belly opening_kg = %s, want 5(昨夜结转)", belly.OpeningKg)
	}
	// expected = 5 + 0 - 0 - 0 = 5
	if !belly.ExpectedRemainKg.Equal(d("5")) {
		t.Errorf("belly expected_remain_kg = %s, want 5", belly.ExpectedRemainKg)
	}
}

// findCut 在 []CutSnapshot 中按 cut_type 找一条。
func findCut(cuts []model.CutSnapshot, ct model.CutType) *model.CutSnapshot {
	for i := range cuts {
		if cuts[i].CutType == ct {
			return &cuts[i]
		}
	}
	return nil
}
