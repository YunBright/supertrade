// Package seed —— 排除域(本服务不盘的商品域)的**初始装配**数据。
//
// # 这份数据是怎么来的(2026-10-23)
//
// 两个生产门店的猪肉分类 id 取自各自的思迅源库(思迅家族,只读):
//
//	佳乐  (branch "01") → sixun-hbposv7-jiale → 分类 1201「佳乐超市猪肉部」
//	富百家(branch "00") → sixun-ysx-fb       → 分类 010303「猪肉」
//
// 同一份数据在 fresh-meat 域的初始装配里也出现过
// (internal/fresh-meat/seed/cut_mappings.go 的包注释)——
// 那边是**按 SKU** 列出猪肉部位,这边是**按分类**一刀切:
// 通用盘点只要看到商品落在这个分类里,就不许盘。
//
// 为什么**按分类**而不是按 SKU:
//   - 猪肉 SKU 会增减(改品、上新、淘汰),按 SKU 维护的清单迟早漏;
//   - 分类是门店给"猪肉部"这一整块打的标签,新增 SKU 自动落进来。
//
// 为什么**按门店**存(而不是一个全局列表):
//   - cube 的 category_id 是思迅 `item_clsno`,**每个门店各编各的**:
//     佳乐的猪肉是 1201,富百家的是 010303 —— 同一个 id 在两个门店
//     指的是完全不同的东西(甚至可能都不存在)。
//   - ⇒ 全局列表必然误伤另一个门店的同类商品。
//
// # 维护方式
//
// 门店改分类 id(思迅那边改编码)时,改这里的 seed 数据并重跑装配;
// 运行期也可以直接改表(这张表就是配置表,没有别的写路径)。
package seed

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/YunBright/supertrade/internal/stocktake/model"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	fmseed "github.com/YunBright/supertrade/internal/fresh-meat/seed"
)

// Scope 一条「门店 × 排除域」配置。
//
// ScopeKind 目前只有 model.ScopeKindCategory("category");
// ScopeValue = cube product.category_id(= 思迅 item_clsno)。
//
// Note 是**给操作员看的一句话**,拒绝时原样进错误消息 ⇒ 改文案不用发版。
type Scope struct {
	BranchID   string
	ScopeKind  string
	ScopeValue string
	Note       string
}

// ---- 佳乐(branch "01"):分类 1201「佳乐超市猪肉部」 ----
//
// ---- 富百家(branch "00"):分类 010303「猪肉」 ----

// AllScopes 两个门店的初始排除域配置。
//
// ⚠️ 2026-10-23 改为**按货号**排除(原按类目):
//
//	佳乐 01 的猪肉类目 `1201`「佳乐超市猪肉部」里**混着**羊肉/牛肉/鸡/预制菜
//	(实测 03415 羊肉、03005 特价牛肉30Y、03455 新鲜鸡肾、03527 五花肉串 …)
//	⇒ 按类目一刀切会把这些**非猪肉**商品也一起挡掉
//	⇒ 通用盘点就**盘不了**门店的真实库存 ⇒ 过度排除,同样是错的。
//
// 货号口径的**权威来源 = fresh-meat 的部位映射表**
// (internal/fresh-meat/seed/cut_mappings.go):那里就是
// "这些货号是猪肉部位"的唯一事实源 ⇒ 这里直接从它派生,**不维护第二份清单**。
//
// 映射在运行期被改动时(POST /branch-cut-mappings),
// fresh-meat 服务会**推送**一行 scope_kind='product' 到本表(见 fresh-meat 的 scope_push.go)
// ⇒ 清单保持实时,不需要重装。
var AllScopes = buildAllScopes()

// buildAllScopes 从 fresh-meat 的部位映射派生"猪肉货号"排除清单。
func buildAllScopes() []Scope {
	note := "猪肉请用鲜肉盘点(生鲜肉盘点)"
	out := make([]Scope, 0, 64)
	seen := map[string]bool{}
	for _, m := range fmseed.AllMappings {
		key := m.BranchID + "|" + m.CubeProductID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Scope{
			BranchID:   m.BranchID,
			ScopeKind:  model.ScopeKindProduct,
			ScopeValue: m.CubeProductID,
			Note:       note,
		})
	}
	return out
}

// retiredScopes 是**要删掉**的历史排除项。
//
// 2026-10-23 之前按**类目**排除(佳乐 1201 / 富百家 010303)⇒ 过度排除:
// 佳乐 1201「猪肉部」里混着羊肉/牛肉/鸡/预制菜(实测 03415 羊肉、
// 03005 特价牛肉30Y、03455 新鲜鸡肾、03527 五花肉串 …)
// ⇒ 通用盘点连这些**非猪肉**商品都盘不了 ⇒ 门店真实库存盘点不出来。
//
// ⇒ 改成按**货号**排除(见 AllScopes),而这两条类目行必须**删掉**
// —— 留着它们,改口径这件事就等于没做。
var retiredScopes = []Scope{
	{BranchID: "01", ScopeKind: model.ScopeKindCategory, ScopeValue: "1201"},
	{BranchID: "00", ScopeKind: model.ScopeKindCategory, ScopeValue: "010303"},
}

// Apply 把 AllScopes 幂等写进 stocktake_excluded_scopes。
//
// 幂等键 = (branch_id, scope_kind, scope_value):
//   - 不存在 → 插入;
//   - 存在且 note 不同 → 更新(note 是文案,改了要生效);
//   - 完全一致 → 跳过(计入 skipped)。
//
// 另外会**删掉** retiredScopes 里的历史行(见上面的说明)。
//
// 返回 (created, updated, skipped, err)。
func Apply(db *gorm.DB, now time.Time) (created, updated, skipped int, err error) {
	if db == nil {
		return 0, 0, 0, fmt.Errorf("seed: db 为 nil")
	}
	for _, sc := range AllScopes {
		if sc.BranchID == "" || sc.ScopeValue == "" {
			return created, updated, skipped,
				fmt.Errorf("seed: branch_id / scope_value 必填 (got %+v)", sc)
		}
		kind := sc.ScopeKind
		if kind == "" {
			kind = model.ScopeKindCategory
		}
		var existing model.StocktakeExcludedScope
		// "查了没有就是没有"不是错误 ⇒ 这条查询静音,别在服务启动日志里
		// 刷两条 record-not-found。
		q := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).
			Where("branch_id = ? AND scope_kind = ? AND scope_value = ?",
				sc.BranchID, kind, sc.ScopeValue).First(&existing)
		switch {
		case q.Error == gorm.ErrRecordNotFound:
			rec := model.StocktakeExcludedScope{
				ID:         deterministicID(now, sc.BranchID, kind, sc.ScopeValue),
				BranchID:   sc.BranchID,
				ScopeKind:  kind,
				ScopeValue: sc.ScopeValue,
				Note:       sc.Note,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if err := db.Create(&rec).Error; err != nil {
				return created, updated, skipped,
					fmt.Errorf("seed: insert %s/%s: %w", sc.BranchID, sc.ScopeValue, err)
			}
			created++
		case q.Error != nil:
			return created, updated, skipped,
				fmt.Errorf("seed: query %s/%s: %w", sc.BranchID, sc.ScopeValue, q.Error)
		default:
			if existing.Note == sc.Note {
				skipped++
				continue
			}
			if err := db.Model(&existing).Updates(map[string]any{
				"note":       sc.Note,
				"updated_at": now,
			}).Error; err != nil {
				return created, updated, skipped,
					fmt.Errorf("seed: update %s/%s: %w", sc.BranchID, sc.ScopeValue, err)
			}
			updated++
		}
	}
	// 退役历史行(按类目的猪肉排除)—— 留着它们,改口径等于没做。
	for _, rs := range retiredScopes {
		if err := db.Where("branch_id = ? AND scope_kind = ? AND scope_value = ?",
			rs.BranchID, rs.ScopeKind, rs.ScopeValue).
			Delete(&model.StocktakeExcludedScope{}).Error; err != nil {
			return created, updated, skipped,
				fmt.Errorf("seed: retire %s/%s/%s: %w", rs.BranchID, rs.ScopeKind, rs.ScopeValue, err)
		}
	}
	return created, updated, skipped, nil
}

// deterministicID 生成 `SES<yyyymmdd><8-hex>`。
//
// 后 8 位由 (branch, kind, value) 哈希派生 ⇒ **重复执行得到同一个 ID**
// (与 fresh-meat/seed 的 deterministicID 同形)。
func deterministicID(now time.Time, branchID, kind, value string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(branchID + "|" + kind + "|" + value))
	return fmt.Sprintf("SES%s%08x", now.UTC().Format("20060102"), h.Sum32())
}
