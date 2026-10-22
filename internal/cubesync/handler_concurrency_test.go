// Package cubesync —— 同步并发保护的单元测试。
//
// # 为什么并发在"有水位"之后变危险了
//
//	没有水位时,两轮并发同步只是"重复劳动"(幂等 upsert,结果一样)。
//
//	有了水位之后,两轮并发的窗口**不一样**:
//
//	轮 A 读到的水位是 W_old(它启动得早),
//	轮 B 读到的水位是 W_new > W_old(B 启动得晚,因为 A/B 之间还有一轮成功了)。
//
//	⇒ A 导的是**更宽**的窗口 (W_old, …],B 导的是 (W_new, …]。
//	⇒ 若 A **失败**、B **成功**:B 把水位推进到 W_new
//	⇒ 下一轮只会导 (W_new, …] ⇒ **(W_old, W_new] 这段永远补不回来**。
//
//	症状:库存慢慢偏掉,而每一轮同步报告都写"成功" —— **全程零报错**。
//
// ⇒ 所以人工触发与定时回调**必须互斥**。
package cubesync

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// blockingExporter 会卡在导出里的假导出方(用来制造"一轮正在跑")。
type blockingExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingExporter) Export(_ context.Context, _, scope string, _, _ int, _ string) (*archivedto.ExportPage, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return &archivedto.ExportPage{
		Scope: scope,
		Rows:  []map[string]any{{"id": "P1", "name": "商品一"}},
		Count: 1, Total: 1,
	}, nil
}

// TestManualTrigger_IsRejectedWhileJobRuns 锁"人工触发不得与定时回调并发"。
//
// ⚠️ 这是**增量水位**引入的新约束(见包注释的推导)。
func TestManualTrigger_IsRejectedWhileJobRuns(t *testing.T) {
	ex := &blockingExporter{started: make(chan struct{}), release: make(chan struct{})}
	im := &fakeImporter{}
	cfg := Config{SourceBranchID: "00", TargetBranchID: "10", JobPrefix: "cube-async",
		Mode:  "overwrite",
		Scopes: []string{archivedto.ScopeProducts}, PageSize: 10}
	h := NewHandler(newTestSyncer(cfg, ex, im), testToken, nil)
	r := newTestRouter(h)

	// ① 定时回调:受理后同步在后台跑(卡在导出里)。
	w := do(r, http.MethodPost, testJobPath, testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("定时回调应 200,实际 %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-ex.started:
	case <-time.After(5 * time.Second):
		t.Fatal("后台同步没有起来")
	}

	// ② 人工触发撞上 ⇒ 必须被拒(不能并发跑)。
	w2 := do(r, http.MethodPost, RunPath, testToken, "")
	if w2.Code != http.StatusConflict {
		t.Errorf("并发时人工触发应 409,实际 %d: %s", w2.Code, w2.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body["code"] != "sync_busy" {
		t.Errorf("错误码应为 sync_busy,实际 %v", body["code"])
	}

	// ③ 放开后台同步,等它结束(避免 goroutine 泄漏到别的用例)。
	close(ex.release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(im.snapshot()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("后台同步没有完成")
}
