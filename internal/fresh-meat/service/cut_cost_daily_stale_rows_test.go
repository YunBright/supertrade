// 2026-10-30:锁「**快照必须 upsert + 删除不再产出的行**」。
//
// # 背景(生产实测:店 00 / 2026-10-30)
//
//	落成本日快照(`cut_cost_daily`)原来**只 upsert,不 delete**。
//	⇒ 引擎**这轮不再产出**的部位,旧行会**永远留在表里**
//	⇒ 而且会被次日"读前一日快照接链"当成**真的期初**接回去 ⇒ 负数凭空复活。
//
//	生产实测:店 00 的 10-28 有 4 行是**旧链残留**
//	(猪耳 / 猪肺 / 龙骨 / 猪皮),它们的 `updated_at` 还停在 2026-10-29 16:42
//	(那一刻算过)⇒ 10-29 读前一日快照时把猪耳 −1.279 / 猪肺 −17.64 接了回去
//	⇒ 门店看到"期初为什么有负数"。
//
// # 规则
//
//	重算某天 = "这天的快照**就是**引擎这一轮产出的那几行"
//	⇒ 产出集合之外的行 = 脏行 ⇒ **同事务**删掉(只限这一天)。
//
// # 判别力
//
//	去掉 `persistCutCostDaily` 里那段删除 ⇒ 脏行留在表里
//	⇒ 本用例立刻红(猪肺那行还在)。
package service_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// TestCutCostDaily_PersistRemovesStaleRows 锁「**本轮不再产出的行必须被删掉**」。
//
// # 场景
//
//	10-06 卖了 10kg 五花肉 ⇒ 引擎产出 **belly** 这一行。
//	表里另外还有一行**旧链残留**的**猪肺**(closing −17.64kg)
//	—— 引擎这轮**根本不产出**猪肺(没销售、没期初、没产出)。
//
// # 期望
//
//	重落 10-06 的快照后:猪肺那行**消失**,五花肉那行**还在**。
//
// # 判别力
//
//	去掉删除逻辑 ⇒ 猪肺那行还在 ⇒ 本用例立刻红
//	(而它会被次日接成真的期初 ⇒ 负数复活)。
func TestCutCostDaily_PersistRemovesStaleRows(t *testing.T) {
	day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{
		"2026-10-06": {fmCubeSalesRow("P-BELLY", "10", "180")},
	})
	// 旧链残留的脏行:猪肺。引擎这轮**不产出**它。
	fmChainSeedSnapshot(t, "S001", "2026-10-06", model.CutLung, "-17.64", "-146.51", "15.9249")

	if _, err := svc.PersistCutCostDaily(ctx, "S001", day); err != nil {
		t.Fatalf("PersistCutCostDaily: %v", err)
	}

	if _, ok := fmCutCostDaily(t, "S001", "2026-10-06", model.CutLung); ok {
		t.Error("★ 引擎这轮不再产出的部位(猪肺)必须被删掉 —— " +
			"只 upsert 不 delete ⇒ 脏行会被次日当成真的期初接回去 ⇒ 负数复活")
	}
	if _, ok := fmCutCostDaily(t, "S001", "2026-10-06", model.CutBelly); !ok {
		t.Error("★ 五花肉这一行必须还在(引擎确实产出了它)")
	}
}
