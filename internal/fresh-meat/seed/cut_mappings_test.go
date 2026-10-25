package seed_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/seed"
	"github.com/YunBright/supertrade/internal/fresh-meat/testdb"
	"gorm.io/gorm"
)

func setup(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BranchCutMapping{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestSeedDataIsSane 锁住 seed 数据的形状(幂等键不重复 / 部位都在枚举内 /
// 两店都有覆盖),避免手改数据时把这几件事弄坏。
func TestSeedDataIsSane(t *testing.T) {
	if len(seed.AllMappings) == 0 {
		t.Fatal("AllMappings 不应为空")
	}
	seen := map[string]bool{}
	branchCuts := map[string]map[model.CutType]int{}
	for _, m := range seed.AllMappings {
		key := m.BranchID + "|" + m.CubeProductID
		if seen[key] {
			t.Errorf("重复的 (branch, sku): %s", key)
		}
		seen[key] = true
		if !m.CutType.Valid() {
			t.Errorf("%s 的 cut_type %q 不在枚举内", key, m.CutType)
		}
		if m.CubeProductID == "" || m.CubeProductName == "" {
			t.Errorf("%s 缺 sku id/name", key)
		}
		if branchCuts[m.BranchID] == nil {
			branchCuts[m.BranchID] = map[model.CutType]int{}
		}
		branchCuts[m.BranchID][m.CutType]++
	}

	// 两个门店都要有覆盖。
	for _, b := range []string{"00", "01"} {
		if len(branchCuts[b]) == 0 {
			t.Errorf("branch %s 没有任何映射", b)
		}
	}
	// 每店至少要覆盖到这些"每天都卖"的核心部位。
	for _, b := range []string{"00", "01"} {
		for _, c := range []model.CutType{
			model.CutBelly, model.CutLegFront, model.CutLegRear,
			model.CutRib, model.CutTrotter, model.CutBoneShank,
		} {
			if branchCuts[b][c] == 0 {
				t.Errorf("branch %s 缺核心部位 %s", b, c)
			}
		}
	}
}

// TestSeedApplyIsIdempotent 锁住"重复跑不重复插、且不报错"。
func TestSeedApplyIsIdempotent(t *testing.T) {
	db := setup(t)
	now := time.Now()

	created, updated, skipped, err := seed.Apply(db, now)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if created != len(seed.AllMappings) {
		t.Errorf("created = %d, want %d", created, len(seed.AllMappings))
	}
	if updated != 0 || skipped != 0 {
		t.Errorf("first apply: updated=%d skipped=%d, want 0/0", updated, skipped)
	}

	var n int64
	if err := db.Model(&model.BranchCutMapping{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != int64(len(seed.AllMappings)) {
		t.Fatalf("rows = %d, want %d", n, len(seed.AllMappings))
	}

	// 第二次:全部跳过,行数不变。
	created, updated, skipped, err = seed.Apply(db, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if created != 0 || updated != 0 || skipped != len(seed.AllMappings) {
		t.Errorf("second apply: created=%d updated=%d skipped=%d, want 0/0/%d",
			created, updated, skipped, len(seed.AllMappings))
	}
	if err := db.Model(&model.BranchCutMapping{}).Count(&n).Error; err != nil {
		t.Fatalf("count 2: %v", err)
	}
	if n != int64(len(seed.AllMappings)) {
		t.Errorf("rows after 2nd apply = %d, want %d", n, len(seed.AllMappings))
	}
}

// TestSeedApplyUpdatesChangedCutType 锁住"改了部位归属后重跑会更新,不会插第二条"。
func TestSeedApplyUpdatesChangedCutType(t *testing.T) {
	db := setup(t)
	now := time.Now()

	// 先手工插一条 cut_type 错的。
	if err := db.Create(&model.BranchCutMapping{
		ID: "BCM-X", BranchID: "01", CutType: model.CutBelly,
		CubeProductID: "00004", CubeProductName: "错的",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}

	created, updated, skipped, err := seed.Apply(db, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if updated != 1 {
		t.Errorf("updated = %d, want 1", updated)
	}
	if created != len(seed.AllMappings)-1 {
		t.Errorf("created = %d, want %d", created, len(seed.AllMappings)-1)
	}
	_ = skipped

	var m model.BranchCutMapping
	if err := db.Where("branch_id = ? AND cube_product_id = ?", "01", "00004").First(&m).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if m.CutType != model.CutBelly {
		t.Errorf("cut_type = %s, want belly", m.CutType)
	}
	if m.CubeProductName != "腩肉(五花肉)" {
		t.Errorf("name = %q, want 腩肉(五花肉)", m.CubeProductName)
	}

	// 不该出现第二条同 sku 的记录。
	var n int64
	if err := db.Model(&model.BranchCutMapping{}).
		Where("branch_id = ? AND cube_product_id = ?", "01", "00004").Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows for (01,00004) = %d, want 1", n)
	}
}
