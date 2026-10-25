// Command fresh-meat-seed 装配「门店部位 ↔ cube SKU」映射。
//
// 用法:
//
//	POSTGRES_DSN=postgres://... go run ./cmd/fresh-meat-seed
//	POSTGRES_DSN=... go run ./cmd/fresh-meat-seed -dry-run
//
// 幂等:同 (branch_id, cube_product_id) 已存在就更新 cut_type / 名称,不重复插。
// 数据来源与纳入规则见 internal/fresh-meat/seed/cut_mappings.go 的包注释。
//
// ⚠️ 这是**初始装配**工具;之后由仓管通过 POST/PUT /branch-cut-mappings 维护。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/seed"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "只打印要写什么,不落库")
	flag.Parse()

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		fail("POSTGRES_DSN 必填")
	}

	db, err := freshmeat.OpenPostgres(dsn)
	if err != nil {
		fail("open db: %v", err)
	}
	// 装配工具要"查了没有就是没有",不是错误 ⇒ 关掉 GORM 的 record-not-found 噪音。
	// 真错误由本文件自己显式处理(每个查询都判 ErrRecordNotFound)。
	db.Logger = logger.Default.LogMode(logger.Silent)

	// 表/索引结构先对齐(与 fresh-meat 服务启动时同一条路径)。
	if err := db.AutoMigrate(&model.BranchCutMapping{}); err != nil {
		fail("auto migrate: %v", err)
	}
	if err := freshmeat.ApplySchemaFixups(db); err != nil {
		fail("apply schema fixups: %v", err)
	}

	if *dryRun {
		printPlan(db)
		return
	}

	now := time.Now()
	created, updated, skipped, err := seed.Apply(db, now)
	if err != nil {
		fail("seed apply: %v", err)
	}
	fmt.Printf("seed 完成: created=%d updated=%d skipped=%d\n", created, updated, skipped)

	// 特价档货号(2026-10-23):让这些货号的销售行**不再被丢弃**。
	pc, pu, ps, err := applyPromoSkus(db, now)
	if err != nil {
		fail("seed promo_skus: %v", err)
	}
	fmt.Printf("promo_skus 完成: created=%d updated=%d skipped=%d\n", pc, pu, ps)

	// 大宗品项(板油等)
	bc, bu, bs, err := applyBulkProducts(db, now)
	if err != nil {
		fail("seed bulk_products: %v", err)
	}
	fmt.Printf("bulk_products 完成: created=%d updated=%d skipped=%d\n", bc, bu, bs)

	printSummary(db)
}

// applyBulkProducts 幂等装配大宗品项(2026-10-24:大宗品项 = **一个部位**)。
//
// # ① 先删老口径的遗留行
//
// 老口径的品项带 code/name/unit/cube_product_id(如 lard / wateroil / lard00),
// 新口径只有部位(code = cut_type)⇒ 同一个部位会出现两行 ⇒ 结存与结算说不清。
//
// ⇒ 删掉"code <> cut_type"的行。
//
// ⚠️ 已产生的结算单引用的是 product_code **文本**(不是外键)
// ⇒ 品项删了,**结算记录仍在**(门店口径:「已产生的结算记录可保留」)。
func applyBulkProducts(db *gorm.DB, now time.Time) (created, updated, skipped int, err error) {
	var legacy []model.BulkProduct
	if err := db.Where("code <> cut_type").Find(&legacy).Error; err != nil {
		return created, updated, skipped, err
	}
	for _, l := range legacy {
		if err := db.Delete(&model.BulkProduct{}, "id = ?", l.ID).Error; err != nil {
			return created, updated, skipped, err
		}
		updated++
	}
	// ② 按部位 upsert(编码/名称/单位由服务端口径自动填)
	for _, e := range seed.BulkProductSeeds() {
		cut := model.CutType(e.CutType)
		if !cut.Valid() {
			return created, updated, skipped, fmt.Errorf("bulk product %s 的 cut_type %q 不在枚举内", e.BranchID, e.CutType)
		}
		code := string(cut)
		name := model.CutLabel(cut)
		var existing model.BulkProduct
		q := db.Where("branch_id = ? AND cut_type = ?", e.BranchID, cut).First(&existing)
		switch {
		case q.Error == nil:
			if existing.Code == code && existing.Name == name && existing.Unit == "kg" &&
				existing.Note == e.Note && existing.PosCubeProductID == e.PosCubeProductID {
				skipped++
				continue
			}
			if err := db.Model(&existing).Updates(map[string]any{
				"code": code, "name": name, "unit": "kg",
				"note": e.Note, "updated_at": now,
				// ★ 大宗线的 POS 货号(2026-10-25):「这条货号不走零售」的标记。
				"pos_cube_product_id": e.PosCubeProductID,
			}).Error; err != nil {
				return created, updated, skipped, err
			}
			updated++
		case q.Error == gorm.ErrRecordNotFound:
			rec := model.BulkProduct{
				ID:               fmt.Sprintf("BKP%s%s%s", now.UTC().Format("20060102"), e.BranchID, code),
				BranchID:         e.BranchID,
				Code:             code,
				Name:             name,
				CutType:          cut,
				Unit:             "kg",
				Note:             e.Note,
				PosCubeProductID: e.PosCubeProductID,
				CreatedAt:        now,
				UpdatedAt:        now,
			}
			if err := db.Create(&rec).Error; err != nil {
				return created, updated, skipped, err
			}
			created++
		default:
			return created, updated, skipped, q.Error
		}
	}
	return created, updated, skipped, nil
}
func applyPromoSkus(db *gorm.DB, now time.Time) (created, updated, skipped int, err error) {
	for _, e := range seed.PromoSkuSeeds() {
		var existing model.PromoSku
		q := db.Where("branch_id = ? AND cube_product_id = ?", e.BranchID, e.CubeProductID).First(&existing)
		switch {
		case q.Error == nil:
			updates := map[string]any{
				"product_name":    e.ProductName,
				"price_tier_text": e.PriceTierText,
				"active":          true,
				"updated_at":      now,
			}
			if existing.ProductName == e.ProductName &&
				existing.PriceTierText == e.PriceTierText && existing.Active {
				skipped++
				continue
			}
			if err := db.Model(&existing).Updates(updates).Error; err != nil {
				return created, updated, skipped, err
			}
			updated++
		case q.Error == gorm.ErrRecordNotFound:
			rec := model.PromoSku{
				ID:            fmt.Sprintf("PSK%s%s", now.UTC().Format("20060102"), e.CubeProductID),
				BranchID:      e.BranchID,
				CubeProductID: e.CubeProductID,
				ProductName:   e.ProductName,
				PriceTierText: e.PriceTierText,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := db.Create(&rec).Error; err != nil {
				return created, updated, skipped, err
			}
			created++
		default:
			return created, updated, skipped, q.Error
		}
	}
	return created, updated, skipped, nil
}

// printPlan 打印将要写入的内容(不落库),便于人工复核。
func printPlan(db *gorm.DB) {
	byBranch := map[string][]seed.Mapping{}
	for _, m := range seed.AllMappings {
		byBranch[m.BranchID] = append(byBranch[m.BranchID], m)
	}
	branches := make([]string, 0, len(byBranch))
	for b := range byBranch {
		branches = append(branches, b)
	}
	sort.Strings(branches)

	for _, b := range branches {
		ms := byBranch[b]
		sort.Slice(ms, func(i, j int) bool { return ms[i].CubeProductID < ms[j].CubeProductID })
		fmt.Printf("\n=== branch %s (%d 个 SKU)===\n", b, len(ms))
		for _, m := range ms {
			mark := " "
			if m.Daily() {
				mark = "*" // 观测窗口里每天都卖
			}
			var existing model.BranchCutMapping
			state := "insert"
			q := db.Where("branch_id = ? AND cube_product_id = ?", m.BranchID, m.CubeProductID).First(&existing)
			switch {
			case q.Error == nil:
				if existing.CutType == m.CutType {
					state = "same"
				} else {
					state = fmt.Sprintf("update(%s→%s)", existing.CutType, m.CutType)
				}
			case q.Error != gorm.ErrRecordNotFound:
				state = "err:" + q.Error.Error()
			}
			fmt.Printf("  %s %-8s %-20s %-24s %2d天 %-16s %s\n",
				mark, m.CubeProductID, m.CubeProductName, m.CutType, m.DaysSold, state, model.CutLabel(m.CutType))
		}
	}
	fmt.Printf("\n(★ = 近 14 个完整营业日里每天都卖)\n")
}

// printSummary 按门店 × 部位汇总落库结果。
func printSummary(db *gorm.DB) {
	var rows []model.BranchCutMapping
	if err := db.Order("branch_id, cut_type, cube_product_id").Find(&rows).Error; err != nil {
		fail("summary query: %v", err)
	}
	fmt.Printf("\n=== branch_cut_mappings 共 %d 条 ===\n", len(rows))
	byBranch := map[string][]model.BranchCutMapping{}
	for _, r := range rows {
		byBranch[r.BranchID] = append(byBranch[r.BranchID], r)
	}
	branches := make([]string, 0, len(byBranch))
	for b := range byBranch {
		branches = append(branches, b)
	}
	sort.Strings(branches)
	for _, b := range branches {
		ms := byBranch[b]
		cuts := map[model.CutType]int{}
		for _, m := range ms {
			cuts[m.CutType]++
		}
		fmt.Printf("\n--- branch %s: %d 条 / %d 个部位 ---\n", b, len(ms), len(cuts))
		types := make([]model.CutType, 0, len(cuts))
		for c := range cuts {
			types = append(types, c)
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		for _, c := range types {
			skus := []string{}
			for _, m := range ms {
				if m.CutType == c {
					skus = append(skus, m.CubeProductID+" "+m.CubeProductName)
				}
			}
			fmt.Printf("  %-18s %-22s %v\n", c, model.CutLabel(c), skus)
		}
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
