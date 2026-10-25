// Command stocktake-seed 装配「本盘点域**不盘**的商品域」(排除域)。
//
// 用法:
//
//	POSTGRES_DSN=postgres://... go run ./cmd/stocktake-seed
//	POSTGRES_DSN=... go run ./cmd/stocktake-seed -dry-run
//
// 幂等:同 (branch_id, scope_kind, scope_value) 已存在就更新 note,不重复插。
// 数据来源与口径见 internal/stocktake/seed/excluded_scopes.go 的包注释。
//
// ⚠️ 这张表在 **stocktake 服务启动时也会装配一次**(见 cmd/stocktake/main.go),
// 所以正常部署**不需要**跑这个命令。本命令用于:
//   - 装配前人工复核(-dry-run);
//   - 门店改分类 id 后,显式重跑一次。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/YunBright/supertrade/internal/stocktake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/seed"
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

	db, err := stocktake.OpenPostgres(dsn)
	if err != nil {
		fail("open db: %v", err)
	}
	// 装配工具要"查了没有就是没有",不是错误 ⇒ 关掉 GORM 的 record-not-found 噪音。
	db.Logger = logger.Default.LogMode(logger.Silent)

	if err := db.AutoMigrate(&model.StocktakeExcludedScope{}); err != nil {
		fail("auto migrate: %v", err)
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
	fmt.Printf("排除域配置完成: created=%d updated=%d skipped=%d\n", created, updated, skipped)
	printSummary(db)
}

// printPlan 打印将要写入的内容(不落库),便于人工复核。
func printPlan(db *gorm.DB) {
	for _, sc := range seed.AllScopes {
		state := "insert"
		var existing model.StocktakeExcludedScope
		q := db.Where("branch_id = ? AND scope_kind = ? AND scope_value = ?",
			sc.BranchID, sc.ScopeKind, sc.ScopeValue).First(&existing)
		switch {
		case q.Error == nil:
			if existing.Note == sc.Note {
				state = "same"
			} else {
				state = "update(note)"
			}
		case q.Error != gorm.ErrRecordNotFound:
			state = "err:" + q.Error.Error()
		}
		fmt.Printf("  branch=%-4s kind=%-9s value=%-8s %-12s %s\n",
			sc.BranchID, sc.ScopeKind, sc.ScopeValue, state, sc.Note)
	}
}

// printSummary 打印落库后的全量配置。
func printSummary(db *gorm.DB) {
	var rows []model.StocktakeExcludedScope
	if err := db.Order("branch_id, scope_kind, scope_value").Find(&rows).Error; err != nil {
		fail("summary query: %v", err)
	}
	fmt.Printf("\n=== stocktake_excluded_scopes 共 %d 条 ===\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  branch=%-4s kind=%-9s value=%-8s note=%s\n",
			r.BranchID, r.ScopeKind, r.ScopeValue, r.Note)
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
