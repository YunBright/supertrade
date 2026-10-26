package service_test

// 回归锁:**分割建议必须活过客户端的断开**(2026-10-26 门店反馈)。
//
// 门店反馈：「点『提交录入并生成分割建议』报网络错误;刷新后猪录进去了,
// 但**没有分割建议**。」
//
// 生产现场(店 00 / 2026-10-26 19:32):
//
//	19:32:18  whole_pigs 落库(pig WP20261026ef79e859,110.2kg)
//	19:32:47  WARN predictFn failed … "dapr converse: rpc error: code = Canceled desc = context canceled"
//	19:32:47  WARN update llm_advice_json failed  err="context canceled"
//	19:32:48  客户端弹「网络连接失败」
//
// ⇒ 完整链条:
//
//	录猪接口**同步**等 LLM
//	  → 客户端 dio receiveTimeout(30s)与服务端 LLM 超时(30s)撞车
//	  → 客户端先超时并断开 ⇒ **请求 ctx 被取消**
//	  → LLM 调用被取消 ⇒ history_avg 回落也用同一 ctx ⇒ 也被取消
//	  → 写回 llm_advice_json 也被取消
//	  ⇒ 猪在库里(事实成立),建议**永久丢失**,而且没有补偿机制。
//
// ⇒ 修法:
//
//	① 录猪**立即返回**,不等 LLM;
//	② 建议在后台用**独立 ctx**(context.WithoutCancel)生成;
//	③ 给「重新生成建议」入口兜底。
//
// 本文件钉住 ①②③ 三条。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// waitAdviceReady 轮询等建议就绪(异步模式下用)。
//
// ⚠️ 超时给 3s(单测里 predictFn 是 fake,毫秒级返回)⇒ 不会拖慢测试。
func waitAdviceReady(t *testing.T, svc *service.Service, branchID, pigID string) *model.WholePig {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		pig, err := svc.GetWholePig(context.Background(), branchID, pigID)
		if err == nil && pig.AdviceStatus == model.AdviceStatusReady {
			return pig
		}
		if time.Now().After(deadline) {
			t.Fatalf("等建议就绪超时: %+v", pig)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRecordWholePig_AdviceSurvivesClientDisconnect ★ 本轮最核心的一条。
//
// 模拟生产现场:录猪请求的 ctx 被**取消**(客户端超时/断网)
// ⇒ 建议**仍然必须**写进库。
//
// ⚠️ 这条用例用的是**默认异步**执行器(不注入同步 runner)
//   —— 否则"取消"发生在建议生成之后,测不出任何东西。
func TestRecordWholePig_AdviceSurvivesClientDisconnect(t *testing.T) {
	svc, _ := setupTestServiceAsync(t)
	fake := &fakePredictFn{fn: func(ctx context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		// 模拟 LLM:如果 ctx 被取消,这里就会收到取消信号(生产就是这样炸的)。
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
			return defaultLLMResult(), nil
		}
	}}
	svc.SetPredictFn(fake.Predict)

	ctx, cancel := context.WithCancel(context.Background())
	pig, err := svc.RecordWholePig(ctx, "S001",
		model.RecordWholePigInput{
			EarTag: "EB-DISC", GrossWeightKg: d("110.2"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 录猪**立即返回**(不等 LLM)⇒ 此刻建议还没好。
	if pig.AdviceStatus != model.AdviceStatusGenerating {
		t.Errorf("★ 录猪返回时 advice_status 应是 generating,got %q", pig.AdviceStatus)
	}

	// 模拟客户端断开:取消请求 ctx。
	cancel()

	// ⭐ 建议**必须**照样落库(后台用的是 WithoutCancel 的独立 ctx)。
	row := waitAdviceReady(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceLLM {
		t.Errorf("★ 客户端断开不该影响建议: data_source = %q, want llm", row.DataSource)
	}
	if len(row.LLMAdviceJSON) == 0 {
		t.Errorf("★ 客户端断开不该影响建议: llm_advice_json 应非空")
	}
}

// TestRecordWholePig_AdviceDoesNotBlockResponse 锁"录猪不等 LLM"。
//
// ⚠️ 这是客户端超时的根因:服务端同步等 LLM 30s,而客户端 dio receiveTimeout
// 也是 30s ⇒ 客户端必然先超时 ⇒ 必然报"网络连接失败"。
//
// ⇒ 断言:录猪接口的耗时**不含** LLM 的耗时。
func TestRecordWholePig_AdviceDoesNotBlockResponse(t *testing.T) {
	svc, _ := setupTestServiceAsync(t)
	release := make(chan struct{})
	fake := &fakePredictFn{fn: func(ctx context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		<-release // 卡住,模拟慢 LLM
		return defaultLLMResult(), nil
	}}
	svc.SetPredictFn(fake.Predict)

	start := time.Now()
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-FAST", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("15"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	elapsed := time.Since(start)
	mustNoErr(t, err, "RecordWholePig")
	close(release) // 放行后台任务

	if elapsed > 2*time.Second {
		t.Errorf("★ 录猪不该等 LLM:耗时 %s", elapsed)
	}
	if pig.AdviceStatus != model.AdviceStatusGenerating {
		t.Errorf("★ 录猪返回时 advice_status 应是 generating,got %q", pig.AdviceStatus)
	}
}

// TestRecordWholePig_AdviceFailedStatus 锁"LLM 挂了 ⇒ 回落 history_avg,状态 ready"。
//
// ⚠️ 落 history_avg 时它会写一个**空 cuts 的占位 JSON**(不是 NULL)
//   ⇒ 状态是 ready、data_source=history_avg
//   ⇒ 前端能看到"有建议但没有具体部位"(历史样本不足时就是这个形状)。
//
// ⚠️ advice_status=failed 只在**写回失败**(DB 出问题)时出现 ——
//   LLM 失败**不会**导致 failed(有 history_avg 兜底)。
func TestRecordWholePig_AdviceFailedStatus(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{fn: func(_ context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return nil, errors.New("LLM 挂了")
	}}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-FAIL", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("15"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig(建议失败不该让录猪失败)")

	row := reloadPig(t, svc, "S001", pig.ID)
	if row.DataSource != model.DataSourceHistoryAvg {
		t.Errorf("★ LLM 失败应回落 history_avg,got %q", row.DataSource)
	}
	if row.AdviceStatus != model.AdviceStatusReady {
		t.Errorf("★ 回落成功 ⇒ advice_status 应是 ready,got %q", row.AdviceStatus)
	}
	if len(row.LLMAdviceJSON) == 0 {
		t.Errorf("★ history_avg 兜底也该写占位 JSON(前端要能区分'有但为空')")
	}
}

// TestRegeneratePigAdvice_Regenerates 锁"手动重生成"这条补救路径。
//
// ⚠️ 它是 2026-10-26 那头猪(110.2kg,建议丢了)的**唯一**补救手段:
//   历史上没有建议的猪,靠它补跑一次 ⇒ 成本链才不缺那一环。
func TestRegeneratePigAdvice_Regenerates(t *testing.T) {
	svc, _ := setupTestService(t)
	fake := &fakePredictFn{fn: func(_ context.Context, _ *model.WholePig, _ []model.WholePig) (*service.LLMResult, error) {
		return defaultLLMResult(), nil
	}}
	svc.SetPredictFn(fake.Predict)

	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-REGEN", GrossWeightKg: d("110.2"), PurchaseUnitPriceYuan: d("14.6"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 手动重生成(同步执行器 ⇒ 跑完就绪)。
	out, err := svc.RegeneratePigAdvice(context.Background(), "S001", pig.ID)
	mustNoErr(t, err, "RegeneratePigAdvice")
	if out.AdviceStatus != model.AdviceStatusGenerating {
		t.Errorf("重生成返回时 advice_status 应是 generating,got %q", out.AdviceStatus)
	}

	row := reloadPig(t, svc, "S001", pig.ID)
	if row.AdviceStatus != model.AdviceStatusReady {
		t.Errorf("★ 重生成后 advice_status 应是 ready,got %q", row.AdviceStatus)
	}
	if row.DataSource != model.DataSourceLLM {
		t.Errorf("重生成后 data_source 应是 llm,got %q", row.DataSource)
	}
	if len(row.LLMAdviceJSON) == 0 {
		t.Errorf("重生成后 llm_advice_json 应非空")
	}
}

// TestRegeneratePigAdvice_CrossBranchRejected 锁"不能给别的店重生成"。
func TestRegeneratePigAdvice_CrossBranchRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-X", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("15"),
			SupplierID: "SUP-1", ArrivedAt: time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC),
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	if _, err := svc.RegeneratePigAdvice(context.Background(), "S002", pig.ID); err == nil {
		t.Errorf("★ 跨店重生成应被拒")
	}
}

// TestRegeneratePigAdvice_NotFound 锁"不存在的猪 ⇒ ErrWholePigNotFound"。
func TestRegeneratePigAdvice_NotFound(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.RegeneratePigAdvice(context.Background(), "S001", "WP-not-exist")
	if !errors.Is(err, service.ErrWholePigNotFound) {
		t.Errorf("★ 应返 ErrWholePigNotFound,got %v", err)
	}
}
