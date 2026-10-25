package seed_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/seed"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"gorm.io/gorm"

	fmseed "github.com/YunBright/supertrade/internal/fresh-meat/seed"
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.StocktakeExcludedScope{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestSeedDataIsSane —— 锁住排除域是「**按货号**」且与 fresh-meat 的部位映射**逐条一致**。
//
// # 为什么是按货号而不是按类目
//
// 佳乐 01 的猪肉类目 `1201`「佳乐超市猪肉部」里**混着**羊肉/牛肉/鸡/预制菜
// (实测 03415 羊肉、03005 特价牛肉30Y、03455 新鲜鸡肾、03527 五花肉串 …)
// ⇒ 按类目一刀切会把这些**非猪肉**商品也挡掉,通用盘点就盘不了门店的真实库存。
//
// # 为什么与 fresh-meat 的映射逐条一致
//
// "哪些货号是猪肉"的**唯一事实源**是 fresh-meat 的部位映射表。
// 这里直接从它派生 ⇒ **不维护第二份清单**(两份真相必然漂移)。
func TestSeedDataIsSane(t *testing.T) {
	if len(seed.AllScopes) == 0 {
		t.Fatalf("排除域配置不应为空")
	}
	// 逐条:kind 必须是 product、value 必须是非空货号、note 必须可操作。
	got := map[string]bool{}
	for _, sc := range seed.AllScopes {
		if sc.ScopeKind != model.ScopeKindProduct {
			t.Errorf("branch %s 的 scope_kind = %q, want %q",
				sc.BranchID, sc.ScopeKind, model.ScopeKindProduct)
		}
		if sc.ScopeValue == "" {
			t.Errorf("branch %s 有一条 scope_value 为空", sc.BranchID)
		}
		if sc.Note == "" {
			t.Errorf("branch %s 的 note 为空 —— 拒绝时没有可操作提示", sc.BranchID)
		}
		key := sc.BranchID + "|" + sc.ScopeValue
		if got[key] {
			t.Errorf("重复的排除项: %s", key)
		}
		got[key] = true
	}
	// 与 fresh-meat 的部位映射**逐条一致**(单一事实源)。
	want := map[string]bool{}
	for _, m := range fmseed.AllMappings {
		want[m.BranchID+"|"+m.CubeProductID] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("部位映射里的 %s 没有进排除清单 ⇒ 通用盘点会把它盘一遍", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("排除清单里的 %s 不在部位映射里", k)
		}
	}
}

// TestSeedApplyIsIdempotent —— 跑两次不重复插。
func TestSeedApplyIsIdempotent(t *testing.T) {
	db := newDB(t)
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	created, updated, skipped, err := seed.Apply(db, now)
	if err != nil {
		t.Fatalf("第一次 Apply: %v", err)
	}
	if created != len(seed.AllScopes) || updated != 0 || skipped != 0 {
		t.Errorf("第一次: created=%d updated=%d skipped=%d", created, updated, skipped)
	}

	created, updated, skipped, err = seed.Apply(db, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("第二次 Apply: %v", err)
	}
	if created != 0 || updated != 0 || skipped != len(seed.AllScopes) {
		t.Errorf("第二次(应全部跳过): created=%d updated=%d skipped=%d", created, updated, skipped)
	}

	var n int64
	if err := db.Model(&model.StocktakeExcludedScope{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != int64(len(seed.AllScopes)) {
		t.Errorf("库里应有 %d 行, got %d", len(seed.AllScopes), n)
	}
}

// TestSeedApplyUpdatesChangedNote —— note 改了要生效(文案不该要发版才能改)。
func TestSeedApplyUpdatesChangedNote(t *testing.T) {
	db := newDB(t)
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	if _, _, _, err := seed.Apply(db, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// 只改**一条**(排除项是按货号的,一个门店有很多条)。
	var target model.StocktakeExcludedScope
	if err := db.Order("id ASC").First(&target).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := db.Model(&model.StocktakeExcludedScope{}).
		Where("id = ?", target.ID).
		Update("note", "旧文案").Error; err != nil {
		t.Fatalf("改 note: %v", err)
	}
	created, updated, skipped, err := seed.Apply(db, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if created != 0 || updated != 1 || skipped != len(seed.AllScopes)-1 {
		t.Errorf("created=%d updated=%d skipped=%d", created, updated, skipped)
	}
	var got model.StocktakeExcludedScope
	if err := db.Where("id = ?", target.ID).First(&got).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.Note == "旧文案" {
		t.Errorf("note 未更新, got %q", got.Note)
	}
}
