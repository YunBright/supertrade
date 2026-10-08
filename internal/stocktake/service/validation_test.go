package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/shopspring/decimal"
)

// 本文件覆盖 2026-10-07 补的入参校验。
//
// 补这些用例的起因是一个具体的静默故障:handler 的 addLineReq.ActualQty 带
// `binding:"required"`,但那是作用在 **decimal.Decimal 指针**上的 ——
// validator 的 hasValue 对 struct 落回 `field.IsValid() && !field.IsZero()`,
// 而 decimal.Decimal 是 struct,NewFromInt(0) 得到的值仍非零。
// 结果 0 / -5 全部通过 binding,一路写进 decimal(20,4) 列。
//
// 下面每条用例都锁死"非法输入 → 具体 sentinel error",而不是只锁"返回了 error",
// 否则将来有人把 ErrInvalidQty 换成别的包装,测试仍然绿。

func ptrDiffReason(r model.DiffReason) *model.DiffReason { return &r }

// setupWithCube 起一个用指定 cube client 的 service(默认 mock 之外可注入假 client)。
func setupWithCube(t *testing.T, cube cubeclient.Client) *service.Service {
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
	return service.New(db, cube)
}

// countLines 数某个盘点单下的明细行数(经公开 API,不碰 db 私有字段)。
func countLines(t *testing.T, svc *service.Service, headerID string) int {
	t.Helper()
	h, err := svc.GetHeaderWithLines(context.Background(), headerID)
	if err != nil {
		t.Fatalf("GetHeaderWithLines: %v", err)
	}
	return len(h.Lines)
}

// firstLineQty 取某盘点单第一行的 actual_qty 字符串。
func firstLineQty(t *testing.T, svc *service.Service, headerID string) string {
	t.Helper()
	h, err := svc.GetHeaderWithLines(context.Background(), headerID)
	if err != nil {
		t.Fatalf("GetHeaderWithLines: %v", err)
	}
	if len(h.Lines) == 0 {
		t.Fatalf("盘点单 %s 下没有任何明细行", headerID)
	}
	return h.Lines[0].ActualQty.String()
}

// emptyRecordCubeClient 模拟 cube 的"COUNT(*) 无匹配也返 1 行空记录"行为。
//
// 生产里 DaprCubeClient.GetStock 只在 len(data)==0 时报错;一条 product_id 为空的
// 记录会被 asDecimal 兜底成 decimal.Zero,于是一个本该跨店拦截的请求会静默
// 落库 book_qty=0 → diff_qty = actual_qty → 看起来像"整盘亏空"。
type emptyRecordCubeClient struct {
	*cubeclientfake.Client
}

func (c *emptyRecordCubeClient) GetStock(_ context.Context, _ string, _ string) (*cubeclient.StockSnapshotDTO, error) {
	// ProductID 为空 == cube 返了一条没有维度的空记录
	return &cubeclient.StockSnapshotDTO{}, nil
}

// ---- actual_qty 符号 ----

func TestService_AddLine_RejectsNegativeQty(t *testing.T) {
	svc, _ := setupTestService(t)
	h, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	_, err = svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.NewFromInt(-5),
	})
	if !errors.Is(err, service.ErrInvalidQty) {
		t.Fatalf("负数 actual_qty 应被 ErrInvalidQty 拒绝, got %v", err)
	}
	// 关键:拒绝后不能留下任何行
	if n := countLines(t, svc, h.ID); n != 0 {
		t.Errorf("被拒绝的录入不应落库, got %d line(s)", n)
	}
}

func TestService_AddLine_AcceptsZeroQty(t *testing.T) {
	// 0 是合法实盘值(商品确实盘空),必须放行 —— 这条锁死"不要把校验写成 > 0"。
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.Zero,
	})
	if err != nil {
		t.Fatalf("actual_qty=0 应被接受(盘空), got %v", err)
	}
	// book=100,actual=0 → diff=-100
	if got := line.DiffQty.String(); got != "-100" {
		t.Errorf("diff_qty = %s, want -100", got)
	}
}

// ---- actual_qty 形状(对齐 decimal(20,4))----

func TestService_AddLine_RejectsTooManyDecimals(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	// 5 位小数:PG 会静默 round 成 1.0001,回显与提交值不一致
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.RequireFromString("95.12345"),
	})
	if !errors.Is(err, service.ErrInvalidQty) {
		t.Fatalf("5 位小数应被拒绝, got %v", err)
	}
	if !strings.Contains(err.Error(), "小数") {
		t.Errorf("错误信息应说明是小数位问题, got %q", err.Error())
	}
}

func TestService_AddLine_AcceptsFourDecimals(t *testing.T) {
	// 边界:正好 4 位必须放行
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.RequireFromString("95.1234"),
	}); err != nil {
		t.Fatalf("正好 4 位小数应被接受, got %v", err)
	}
}

func TestService_AddLine_RejectsHugeIntegerPart(t *testing.T) {
	// 17 位整数 > decimal(20,4) 的 16 位上限 → PG numeric overflow → 500。
	// 这里锁死它变成 400 而不是 500。
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.RequireFromString("12345678901234567"),
	})
	if !errors.Is(err, service.ErrInvalidQty) {
		t.Fatalf("超长整数位应被拒绝, got %v", err)
	}
	if !strings.Contains(err.Error(), "整数位") {
		t.Errorf("错误信息应说明是整数位问题, got %q", err.Error())
	}
}

// ---- accumulate 的语义:delta 可负,结果不可负 ----

func TestService_UpdateLine_AccumulateNegativeResultRejected(t *testing.T) {
	// accumulate 传 delta 是允许的(减库存),但累加到负数必须拒绝 ——
	// 否则等于换个入口把库存改成负数。
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	// 10 + (-20) = -10
	_, err = svc.UpdateLine(context.Background(), line.ID, service.UpdateLineInput{
		ActualQty: ptrDec(decimal.NewFromInt(-20)),
		OpType:    model.OpAccumulate,
	})
	if !errors.Is(err, service.ErrInvalidQty) {
		t.Fatalf("accumulate 到负数应被拒绝, got %v", err)
	}
	// 行必须保持原值 10
	if got := firstLineQty(t, svc, h.ID); got != "10" {
		t.Errorf("被拒绝后 actual_qty 应仍为 10, got %s", got)
	}
}

func TestService_UpdateLine_AccumulateNegativeDeltaOK(t *testing.T) {
	// 负 delta 但结果仍为正 —— 合法,必须放行
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, _ := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(10),
	})
	upd, err := svc.UpdateLine(context.Background(), line.ID, service.UpdateLineInput{
		ActualQty: ptrDec(decimal.NewFromInt(-3)),
		OpType:    model.OpAccumulate,
	})
	if err != nil {
		t.Fatalf("负 delta 但结果为正应被接受, got %v", err)
	}
	if upd.ActualQty.String() != "7" {
		t.Errorf("actual_qty = %s, want 7", upd.ActualQty.String())
	}
}

func TestService_UpdateLine_OverwriteRejectsNegative(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, _ := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(10),
	})
	_, err := svc.UpdateLine(context.Background(), line.ID, service.UpdateLineInput{
		ActualQty: ptrDec(decimal.NewFromInt(-1)),
		OpType:    model.OpOverwrite,
	})
	if !errors.Is(err, service.ErrInvalidQty) {
		t.Fatalf("overwrite 成负数应被拒绝, got %v", err)
	}
}

// ---- diff_reason 枚举 ----

func TestService_AddLine_RejectsUnknownDiffReason(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID:  "P-1001",
		ActualQty:  decimal.NewFromInt(95),
		DiffReason: model.DiffReason("被偷了"),
	})
	if !errors.Is(err, service.ErrInvalidDiffReason) {
		t.Fatalf("未知 diff_reason 应被拒绝, got %v", err)
	}
}

func TestService_AddLine_RejectsOverlongDiffReason(t *testing.T) {
	// 这条以前会走成 500:字符串原样落库 → 超 varchar(32) → PG 报错 →
	// "create line: %w" → mapErr default → internal_error。
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID:  "P-1001",
		ActualQty:  decimal.NewFromInt(95),
		DiffReason: model.DiffReason(strings.Repeat("x", 33)),
	})
	if !errors.Is(err, service.ErrInvalidDiffReason) {
		t.Fatalf("超长 diff_reason 应在 service 层被拒, got %v", err)
	}
}

func TestService_AddLine_AllDiffReasonsAccepted(t *testing.T) {
	// 全部合法值都要能落库,防止 Valid() 写错导致误杀。
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	for i, r := range model.AllDiffReasons() {
		_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
			ProductID:  "P-1001",
			ActualQty:  decimal.NewFromInt(int64(90 + i)),
			DiffReason: r,
		})
		if err != nil {
			t.Errorf("合法 diff_reason %q 被拒: %v", r, err)
		}
	}
}

func TestService_AddLine_EmptyDiffReasonAllowed(t *testing.T) {
	// 空 diff_reason 合法:diff=0 的行本来就没原因可填
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(100),
	}); err != nil {
		t.Fatalf("空 diff_reason 应被接受, got %v", err)
	}
}

func TestService_UpdateLine_RejectsUnknownDiffReason(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, _ := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})
	_, err := svc.UpdateLine(context.Background(), line.ID, service.UpdateLineInput{
		DiffReason: ptrDiffReason(model.DiffReason("乱填")),
	})
	if !errors.Is(err, service.ErrInvalidDiffReason) {
		t.Fatalf("改数量时未知 diff_reason 应被拒绝, got %v", err)
	}
}

// ---- method 枚举 ----

func TestService_AddLine_RejectsUnknownMethod(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
		Method: model.OpMethod("telepathy"),
	})
	if !errors.Is(err, service.ErrInvalidOpMethod) {
		t.Fatalf("未知 method 应被拒绝, got %v", err)
	}
}

func TestService_AddLine_AllMethodsAccepted(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	for i, m := range model.AllOpMethods() {
		_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
			ProductID: "P-1001",
			ActualQty: decimal.NewFromInt(int64(90 + i)),
			Method:    m,
		})
		if err != nil {
			t.Errorf("合法 method %q 被拒: %v", m, err)
		}
	}
}

// ---- cube 空记录:跨店阻断不能被静默绕过 ----

func TestService_AddLine_RejectsCubeEmptyStockRecord(t *testing.T) {
	// cube 返一条空记录(product_id 为空)时,必须按跨店 400 拦下,
	// 而不是静默把 book_qty 记成 0。
	cube := &emptyRecordCubeClient{Client: cubeclientfake.New()}
	svc := setupWithCube(t, cube)
	h, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	_, err = svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})
	if !errors.Is(err, service.ErrStockNotFound) {
		t.Fatalf("cube 空记录应按跨店拦截, got %v", err)
	}
	if n := countLines(t, svc, h.ID); n != 0 {
		t.Errorf("被拦截的录入不应落库, got %d line(s)", n)
	}
}

// ---- AddLine 上的 accumulate 绕过(2026-10-08 code review 发现的 blocker) ----

// TestService_AddLine_RejectsAccumulate 锁死"新增明细不接受 accumulate"。
//
// 背景:AddLineInput 的注释曾声称 accumulate 会转给 UpdateLine,但代码里从来没有
// 查过"该行是否已存在",而是直接新建一行。于是
// `op_type=accumulate, actual_qty=-50` 既跳过了非负校验、又能把负库存写进
// decimal(20,4) 列,还会留下一条伪造的 accumulate(PrevQty=0) 审计记录。
// 这条用例就是那个漏洞的回归锁 —— 把注释改成真话之后,行为必须对得上。
func TestService_AddLine_RejectsAccumulate(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	for _, qty := range []decimal.Decimal{
		decimal.NewFromInt(-50), // 负数:曾经的漏洞本体
		decimal.NewFromInt(5),   // 正数:同样没有"旧值"可加,语义不成立
	} {
		_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
			ProductID: "P-1001",
			ActualQty: qty,
			OpType:    model.OpAccumulate,
		})
		if !errors.Is(err, service.ErrInvalidOpType) {
			t.Errorf("accumulate(qty=%s) 应被 ErrInvalidOpType 拒绝, got %v", qty, err)
		}
	}
	if n := countLines(t, svc, h.ID); n != 0 {
		t.Errorf("被拒绝的 accumulate 不应落库, got %d line(s)", n)
	}
}

// TestService_AddLine_AcceptsCreateAndOverwrite 确认只挡 accumulate,
// 另两个合法 op_type 不受影响(防止把校验写过头)。
func TestService_AddLine_AcceptsCreateAndOverwrite(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	for _, op := range []model.LineOpType{model.OpCreate, model.OpOverwrite} {
		if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
			ProductID: "P-1001", ActualQty: decimal.NewFromInt(10), OpType: op,
		}); err != nil {
			t.Errorf("合法 op_type %q 被拒: %v", op, err)
		}
	}
}

// ---- decimal(20,4) 边界与尾随 0 ----

func TestService_AddLine_QtyShapeBoundaries(t *testing.T) {
	svc, _ := setupTestService(t)
	h, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"4 位小数", "95.1234", false},
		// 尾随 0 不该被算成"5 位小数":95.12340 的值就是 95.1234,
		// 完全装得进 decimal(20,4)。曾经直接用 -Exponent() 量位数会误拒。
		{"尾随 0 不算多一位", "95.12340", false},
		{"整数", "95", false},
		{"16 位整数(上限)", "1234567890123456", false},
		{"17 位整数(超限)", "12345678901234567", true},
		{"16 位整数 + 4 位小数(正好 20 位)", "1234567890123456.1234", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hh, _ := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
				BranchID: "S001", OperatorID: "u-1",
			})
			_, err := svc.AddLine(context.Background(), hh.ID, service.AddLineInput{
				ProductID: "P-1001",
				ActualQty: decimal.RequireFromString(c.raw),
			})
			if c.wantErr && !errors.Is(err, service.ErrInvalidQty) {
				t.Errorf("%s: 应被拒, got %v", c.raw, err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("%s: 应被接受, got %v", c.raw, err)
			}
		})
	}
	_ = h
}