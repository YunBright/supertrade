// internal/fresh-meat/migrate.go —— AutoMigrate 之外的**一次性结构修正**。
//
// ⚠️ 为什么需要它:GORM 的 AutoMigrate **只建不删索引**。
// 2026-10-23 把 branch_cut_mappings 的唯一约束从
// UNIQUE(branch_id, cut_type) 改成
// UNIQUE(branch_id, cube_product_id) + INDEX(branch_id, cut_type),
// 旧的唯一索引 idx_branch_cut **会一直留在库里** ⇒
// 它会继续挡住"同一部位挂第二个 SKU"(真源库里两家店都是常态),
// 而新索引看起来又建好了 ⇒ 症状是"第二个 SKU 报映射已存在",
// 真因是一个**残留的旧索引**。
//
// ⇒ 启动时显式 DROP INDEX IF EXISTS idx_branch_cut。
//
// 幂等:重复执行安全(IF EXISTS);表不存在时也不报错(生产上 fresh-meat 与
// 其它服务共用同一个 PG,表由本服务自己 AutoMigrate 建)。
package freshmeat

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// legacyCutMappingIndex 是 2026-10-23 之前 branch_cut_mappings 上的唯一索引名
// (UNIQUE(branch_id, cut_type))。AutoMigrate 不会删它,必须显式 DROP。
const legacyCutMappingIndex = "idx_branch_cut"

// legacyPigCutIndex 是 2026-10-23 之前 pig_cuts 上的唯一索引名。
//
// ⚠️ 它当时是 **UNIQUE(cut_type) 单列**(PigID 上写的是普通 index,不是 uniqueIndex)
// ⇒ 整个系统**每个部位只能补录一行**(第二头猪的同部位直接被拒)。
// 意图是 UNIQUE(pig_id, cut_type)。
const legacyPigCutIndex = "idx_pig_cut"

// bulkProductionIndex 是 bulk_productions 上的唯一索引名。
//
// ⚠️ 2026-10-23:它的**列**变了 —— 从
// UNIQUE(branch_id, product_code, biz_date)
// 改成
// UNIQUE(branch_id, product_code, biz_date, source)
//
// 因为"日终盘点自动结转"(source=stocktake)与"手工兜底"(source=manual)
// 是**两条不同的行**,同一天同品项都要能存在。
//
// ⇒ GORM 按**索引名**判断存在与否,不比对列 ⇒ 名字在、列不对 ⇒ 它**不会重建**
// ⇒ 必须先 DROP 再让 AutoMigrate 建新的。
const bulkProductionIndex = "idx_bulk_prod_branch_day"

// ApplyPreMigrateFixups 必须在 **AutoMigrate 之前** 执行的结构修正。
//
// # 为什么有的要"前"有的要"后"
//
// GORM 的 AutoMigrate **按索引名**判断"这个索引存在吗",不比对列。
// 所以:
//   - **形状变了**的索引(名字在、列不对)⇒ 必须**先删**再让 AutoMigrate 重建;
//   - **名字变了**的索引(旧名字还在、新名字建了)⇒ AutoMigrate 后**再删**旧的。
//
// ApplyPreMigrateFixups 负责前者(目前只有 pig_cuts.idx_pig_cut),
// ApplySchemaFixups 负责后者(branch_cut_mappings.idx_branch_cut)。
func ApplyPreMigrateFixups(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("fresh-meat: ApplyPreMigrateFixups: db 为 nil")
	}
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	// ⚠️ **两件事必须各自独立跑完** —— 早期版本把 bulk 的修正放在 pig_cut 的
	// `return nil` **之后**,于是 pig_cut 已是正确形状时(生产上正是这样)
	// 直接 return ⇒ bulk 的修正**一次都没跑过** ⇒ 旧的 3 列唯一索引留在库里
	// ⇒ 新代码按 4 列做 ON CONFLICT 时 PG 报
	// "there is no unique or exclusion constraint matching the ON CONFLICT specification"
	// ⇒ 自动结转静默失败(台账永远是空的)。
	//
	// 教训:fixup 列表要**逐个独立**执行,不能串在一条早退链上。

	// ① 旧 idx_pig_cut 是 UNIQUE(cut_type) 单列 ⇒ 删掉,让 AutoMigrate 按
	// UNIQUE(pig_id, cut_type) 重建。
	var idxdef string
	err := db.Raw(`SELECT indexdef FROM pg_indexes WHERE indexname = ?`, legacyPigCutIndex).Scan(&idxdef).Error
	if err != nil {
		return fmt.Errorf("fresh-meat: inspect %s: %w", legacyPigCutIndex, err)
	}
	if idxdef != "" && !strings.Contains(idxdef, "(pig_id, cut_type)") {
		if err := db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %q`, legacyPigCutIndex)).Error; err != nil {
			return fmt.Errorf("fresh-meat: drop legacy %s: %w", legacyPigCutIndex, err)
		}
	}

	// ② bulk_productions 的唯一索引列变了(多了 source)⇒ 同理:先删,让 AutoMigrate 重建。
	if err := dropBulkProductionIndexIfShapeChanged(db); err != nil {
		return err
	}

	// ③ 两张表的**主键**从复合键换成单列代理主键(2026-10-23)。
	//
	// 起因:复合主键的表**进不了 purge 清单** ⇒ 契约测试写在上面的行永远清不掉
	// ⇒ 生产库里一天天攒下"0 kg 的幽灵行"。
	// ⇒ 改成"单列代理主键 + 业务唯一索引"(model 侧已改)。
	//
	// ⚠️ 必须在 AutoMigrate **之前**做:GORM 不会改主键,只会加列。
	// ⚠️ 代理主键必须与 service 侧算法**逐字一致**(service.StockPositionID /
	// service.SkuCostStateID)⇒ 用 PG 的 md5(与 Go 的 md5 同算法)算。
	for _, ddl := range surrogatePKDDL {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("fresh-meat: surrogate pk ddl failed (%s): %w", firstLine(ddl), err)
		}
	}

	// ④ bulk_settlements 的明细粒度老列改成**可空**(2026-10-23 表头/明细改造)。
	//
	// ⚠️ 为什么必须手写 DDL:GORM 的 AutoMigrate **不会把 NOT NULL 改成可空**,
	// 而新 INSERT 不再提供这些列 ⇒ NOT NULL 会让每次结算都失败
	// ("null value in column qty_kg violates not-null constraint")。
	if err := db.Exec(`
		DO $$ BEGIN
		   IF to_regclass('public.bulk_settlements') IS NULL THEN RETURN; END IF;
		   ALTER TABLE bulk_settlements ALTER COLUMN product_code    DROP NOT NULL;
		   ALTER TABLE bulk_settlements ALTER COLUMN from_date       DROP NOT NULL;
		   ALTER TABLE bulk_settlements ALTER COLUMN to_date         DROP NOT NULL;
		   ALTER TABLE bulk_settlements ALTER COLUMN qty_kg          DROP NOT NULL;
		   ALTER TABLE bulk_settlements ALTER COLUMN unit_price_yuan DROP NOT NULL;
		   ALTER TABLE bulk_settlements ALTER COLUMN amount_yuan     DROP NOT NULL;
		 END $$;`).Error; err != nil {
		return fmt.Errorf("fresh-meat: relax bulk_settlements legacy columns: %w", err)
	}

	// ⑤ 大宗品项:**彻底清掉 `active`**(2026-10-25)。
	//
	// 门店口径:「可直接删除大宗品项而不是停用」⇒ 大宗品项现在是**物理删除**
	// ⇒ 「启用/停用」这个状态**已经没有意义了**
	// ⇒ 列、部分唯一索引(`WHERE active`)、以及老口径(品项绑 POS 货号)留下的
	//    `cube_product_id` 列与它的索引,全部清掉。
	//
	// ⚠️ **顺序要紧**:先删**引用了 active / cube_product_id 的索引**,再删列
	//    (PG 不允许删一个被索引引用的列)。
	//
	// ⚠️ 唯一约束从「部分」变成「整表」:品项被物理删了 ⇒ 不再有"停用行占着部位"
	//    ⇒ 整表唯一才是门店要的语义(同一部位只能有一个品项)。
	//
	// ⚠️ 生产现状(2026-10-25 实测):两店各 1 行、active 全为 true
	//    ⇒ 删列不丢任何语义(没有"停用行"要保留)。
	if err := db.Exec(`
		DO $$ BEGIN
		   IF to_regclass('public.bulk_products') IS NULL THEN RETURN; END IF;
		   DROP INDEX IF EXISTS idx_bulk_product_branch_code;
		   DROP INDEX IF EXISTS idx_bulk_product_branch_cut;
		   DROP INDEX IF EXISTS idx_bulk_products_cube_product_id;
		   ALTER TABLE bulk_products DROP COLUMN IF EXISTS active;
		   ALTER TABLE bulk_products DROP COLUMN IF EXISTS cube_product_id;
		   CREATE UNIQUE INDEX IF NOT EXISTS idx_bulk_product_branch_code
		      ON bulk_products (branch_id, code);
		   CREATE UNIQUE INDEX IF NOT EXISTS idx_bulk_product_branch_cut
		      ON bulk_products (branch_id, cut_type);
		 END $$;`).Error; err != nil {
		return fmt.Errorf("fresh-meat: drop bulk_products.active: %w", err)
	}

	// ⑥ 结转记录表**改名** + **彻底去掉旧的批次标记**(owner 2026-11-03)。
	//
	// # 为什么改名
	//
	//	`stock_bags`("袋")名不副实:生产实测 194 行里 183 行是**盘点结转**
	//	(= 普通搬上台的货,没有批次标记)、8 行期初结转
	//	⇒ 「袋」这个字只来自 2026-11-02 已下线的旧玩法 ⇒ 改名 `carryover_records`。
	//
	// # ⚠️ **必须在这里(AutoMigrate 之前)改名**
	//
	//	AutoMigrate 只会**新建**一张空的 `carryover_records`
	//	⇒ 194 行老台账会留在旧表里 ⇒ FIFO 队列与大宗分摊凭据断链
	//	⇒ 先 RENAME(**数据原样保留**)再让 AutoMigrate 接手。
	//
	// # ⚠️ 删列 = **删数据**(owner 已知悉)
	//
	//	label 列里只有 6 行**契约测试**造的数据(TSTLBLT*/BAGTSTFM*),
	//	真实门店**一行都没有** ⇒ 删掉不丢任何业务数据。
	//
	// ⚠️ 索引引用着列 ⇒ 必须**先删索引、再删列**(PG 不允许删被索引引用的列)。
	if db.Dialector.Name() == "postgres" {
		if err := db.Exec(`
		DO $$ BEGIN
		   -- ① 表改名:旧表在 且 新表不在 才改(都存在 ⇒ 已被别的流程改过 ⇒ 不动)
		   IF to_regclass('public.stock_bags') IS NOT NULL
		      AND to_regclass('public.carryover_records') IS NULL THEN
		      ALTER TABLE stock_bags RENAME TO carryover_records;
		   END IF;
		   -- ② 大宗分摊的引用列改名(**数据保留**)
		   IF to_regclass('public.bulk_settlement_allocations') IS NOT NULL THEN
		      IF EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name = 'bulk_settlement_allocations' AND column_name = 'bag_id')
		         AND NOT EXISTS (SELECT 1 FROM information_schema.columns
		                 WHERE table_name = 'bulk_settlement_allocations' AND column_name = 'carryover_id') THEN
		         ALTER TABLE bulk_settlement_allocations RENAME COLUMN bag_id TO carryover_id;
		      END IF;
		      -- ⚠️ PG 的 RENAME COLUMN **不会**改索引名 ⇒ 旧名索引会**继续**指向新列
		      --   ⇒ AutoMigrate 又会按新列名建一个 ⇒ **两个一样的索引**。
		      --   ⇒ 这里先把旧的自动索引删掉,让 AutoMigrate 建唯一的那个。
		      DROP INDEX IF EXISTS idx_bulk_settlement_allocations_bag_id;
		   END IF;
		   -- ③ 旧的批次标记:两个索引 + 列**一起删**
		   IF to_regclass('public.carryover_records') IS NOT NULL THEN
		      DROP INDEX IF EXISTS idx_stock_bag_branch_label_active;
		      DROP INDEX IF EXISTS idx_stock_bags_label;
		      ALTER TABLE carryover_records DROP COLUMN IF EXISTS label;
		   END IF;
		   -- ④ 其余索引跟着表改名(⚠️ PG 的 RENAME TABLE **不会**改索引名)
		   --   ⇒ 不改的话 AutoMigrate 会按新表名再建一份 ⇒ **重复索引**。
		   IF to_regclass('public.carryover_records') IS NOT NULL THEN
		      ALTER INDEX IF EXISTS idx_stock_bags_branch_id      RENAME TO idx_carryover_records_branch_id;
		      ALTER INDEX IF EXISTS idx_stock_bags_cube_product_id RENAME TO idx_carryover_records_cube_product_id;
		      ALTER INDEX IF EXISTS idx_stock_bags_origin_date    RENAME TO idx_carryover_records_origin_date;
		   END IF;
		   -- ⑤ 主键约束也带着旧表名(PG 不会自动改)⇒ 一并改成新表名,
		   --   免得库里还留着 "stock_bags" 的字样(owner:彻底去掉)。
		   IF to_regclass('public.carryover_records') IS NOT NULL THEN
		      DO $rename_pk$
		      DECLARE cname text;
		      BEGIN
		         SELECT conname INTO cname FROM pg_constraint
		           WHERE conrelid = 'carryover_records'::regclass AND contype = 'p'
		           LIMIT 1;
		         IF cname IS NOT NULL AND cname <> 'carryover_records_pkey' THEN
		            EXECUTE format('ALTER TABLE carryover_records RENAME CONSTRAINT %I TO carryover_records_pkey', cname);
		         END IF;
		      END $rename_pk$;
		   END IF;
		 END $$;`).Error; err != nil {
			return fmt.Errorf("fresh-meat: rename carryover records table / drop legacy label: %w", err)
		}
	}

	return nil
}

// surrogatePKDDL 是把两张表的复合主键换成单列代理主键的 DDL。
//
// ⚠️ 幂等:全部用 IF EXISTS / IF NOT EXISTS,重复执行安全。
// ⚠️ 表不存在时(首次部署)第一条 ALTER 就会失败 ⇒ 用 to_regclass 先判存在。
var surrogatePKDDL = []string{
	// sku_stock_positions
	`DO $$ BEGIN
	   IF to_regclass('public.sku_stock_positions') IS NULL THEN RETURN; END IF;
	   ALTER TABLE sku_stock_positions ADD COLUMN IF NOT EXISTS id varchar(64);
	   UPDATE sku_stock_positions
	      SET id = 'SSP' || md5(branch_id || '|' || cube_product_id || '|' || location)
	    WHERE id IS NULL OR id = '';
	   ALTER TABLE sku_stock_positions DROP CONSTRAINT IF EXISTS sku_stock_positions_pkey;
	   ALTER TABLE sku_stock_positions ALTER COLUMN id SET NOT NULL;
	   ALTER TABLE sku_stock_positions ADD PRIMARY KEY (id);
	   CREATE UNIQUE INDEX IF NOT EXISTS idx_sku_pos_branch_sku_loc
	      ON sku_stock_positions (branch_id, cube_product_id, location);
	 END $$;`,
	// sku_cost_states
	`DO $$ BEGIN
	   IF to_regclass('public.sku_cost_states') IS NULL THEN RETURN; END IF;
	   ALTER TABLE sku_cost_states ADD COLUMN IF NOT EXISTS id varchar(64);
	   UPDATE sku_cost_states
	      SET id = 'SCS' || md5(branch_id || '|' || cube_product_id)
	    WHERE id IS NULL OR id = '';
	   ALTER TABLE sku_cost_states DROP CONSTRAINT IF EXISTS sku_cost_states_pkey;
	   ALTER TABLE sku_cost_states ALTER COLUMN id SET NOT NULL;
	   ALTER TABLE sku_cost_states ADD PRIMARY KEY (id);
	   CREATE UNIQUE INDEX IF NOT EXISTS idx_sku_cost_state_branch_sku
	      ON sku_cost_states (branch_id, cube_product_id);
	 END $$;`,
}

// firstLine 取 DDL 的第一行(给报错信息用,别把整段 SQL 打进日志)。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// dropBulkProductionIndexIfShapeChanged 在 bulk_productions 的唯一索引
// **不是** (branch_id, product_code, biz_date, source) 时把它删掉,
// 让 AutoMigrate 按新形状重建。
//
// ⚠️ 新索引比旧的**更宽松**(多一列)⇒ 删旧索引不会造成"历史数据违反新约束"。
func dropBulkProductionIndexIfShapeChanged(db *gorm.DB) error {
	var names []string
	err := db.Raw(`
SELECT indexname FROM pg_indexes
WHERE schemaname = current_schema()
  AND tablename = 'bulk_productions'
  AND indexname = ?`, bulkProductionIndex).Scan(&names).Error
	if err != nil {
		return fmt.Errorf("fresh-meat: scan bulk_productions indexes: %w", err)
	}
	if len(names) == 0 {
		return nil // 表还没建(首次部署)⇒ 交给 AutoMigrate
	}
	var cols []string
	if err := db.Raw(`
SELECT a.attname FROM pg_index i
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
WHERE i.indexrelid = to_regclass(?)
ORDER BY array_position(i.indkey, a.attnum)`, bulkProductionIndex).Scan(&cols).Error; err != nil {
		return fmt.Errorf("fresh-meat: inspect index %s: %w", bulkProductionIndex, err)
	}
	want := []string{"branch_id", "product_code", "biz_date", "source"}
	if len(cols) == len(want) {
		same := true
		for i := range want {
			if cols[i] != want[i] {
				same = false
				break
			}
		}
		if same {
			return nil // 已经是新形状
		}
	}
	if err := db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %q`, bulkProductionIndex)).Error; err != nil {
		return fmt.Errorf("fresh-meat: drop outdated index %s: %w", bulkProductionIndex, err)
	}
	return nil
}

// ApplySchemaFixups 执行 AutoMigrate 之外的结构修正。
//
// # 结转记录的旧批次标记:已**彻底删掉**(2026-11-03 owner)
//
// 语义曾是「**在用中**的结转记录,批次标记在门店内唯一」
// ⇒ 那个玩法 2026-11-02 就整套下线了,2026-11-03 owner 拍板连
//   `label` **列与索引一起删** ⇒ 这里**不再建**任何批次标记相关的索引。
//
// ⚠️ 删列/删索引在 **AutoMigrate 之前**做(见 ApplyPreMigrateFixups 的 ⑥)
//   ⇒ AutoMigrate 不会再把它建回来。
//
// ⚠️ GORM 的 `uniqueIndex` tag **表达不了** `WHERE` ⇒ 原来必须手写 DDL;
//   现在列都没了 ⇒ 手写的那段整段删掉了。
func ApplySchemaFixups(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("fresh-meat: ApplySchemaFixups: db 为 nil")
	}

	// 0) 旧的批次标记部分唯一索引:**已随旧玩法下线一起删掉**(owner 2026-11-03)。
	//
	// ⚠️ 原来这里会建 `UNIQUE(branch_id, label) WHERE status='active' AND label<>''`
	//   —— 现在 **label 列本身已经删了**(见 ApplyPreMigrateFixups 的 ⑥)
	//   ⇒ 再建这个索引会直接报错 ⇒ 整段删除。
	//
	// ⚠️ 代码也**不再读也不再写**批次标记(见 service/carryover_records.go)。

	// 0b) 2026-10-25:大宗结算的 FIFO 队列统一到**结转记录** ⇒ 两张表加列。
	//
	// ⚠️ 为什么手写 DDL:老库里 carryover_records.source / unit_cost_yuan 不存在,
	//   而 AutoMigrate 加 NOT NULL 列时,历史行会先拿到默认值 ⇒ 语义正好
	//   (历史行全是盘点来的 ⇒ 'stocktake')。这里再兜一次底,保证列一定在。
	//
	// ⚠️ bulk_settlement_allocations.carryover_id / location 是**可空**的:
	//   老数据(台账时代)扣的是 bulk_productions ⇒ 没有结转记录 ⇒ 留空即可。
	if db.Dialector.Name() == "postgres" {
		if err := db.Exec(`
		DO $$ BEGIN
		   IF to_regclass('public.carryover_records') IS NULL THEN RETURN; END IF;
		   ALTER TABLE carryover_records ADD COLUMN IF NOT EXISTS source varchar(16) NOT NULL DEFAULT 'stocktake';
		   ALTER TABLE carryover_records ADD COLUMN IF NOT EXISTS unit_cost_yuan numeric(16,4) NOT NULL DEFAULT 0;
		 END $$;
		DO $$ BEGIN
		   IF to_regclass('public.bulk_settlement_allocations') IS NULL THEN RETURN; END IF;
		   ALTER TABLE bulk_settlement_allocations ADD COLUMN IF NOT EXISTS carryover_id varchar(64);
		   ALTER TABLE bulk_settlement_allocations ADD COLUMN IF NOT EXISTS location varchar(16);
		 END $$;
		-- ⚠️ 索引名里的旧词(Bag)也一并改掉:PG 的 RENAME COLUMN **不会**改索引名
		--   ⇒ 先删旧名、再用新名建(同一列 ⇒ 等价,只是名字干净)。
		DROP INDEX IF EXISTS idx_bulk_alloc_bag;
		CREATE INDEX IF NOT EXISTS idx_bulk_alloc_carryover ON bulk_settlement_allocations (carryover_id);`).Error; err != nil {
			return fmt.Errorf("fresh-meat: bulk fifo columns: %w", err)
		}
	}
	// 1) 删旧唯一索引(两种方言都支持 DROP INDEX IF EXISTS <name>)。
	if db.Migrator().HasIndex(&branchCutMappingSchema{}, legacyCutMappingIndex) {
		if err := db.Migrator().DropIndex(&branchCutMappingSchema{}, legacyCutMappingIndex); err != nil {
			return fmt.Errorf("fresh-meat: drop legacy index %s: %w", legacyCutMappingIndex, err)
		}
	}

	// 2) 兜底:某些环境里旧索引可能被手工改过名,按"唯一且列正好是
	//    (branch_id, cut_type)"再扫一遍,命中就删。
	if err := dropResidualUniqueCutIndex(db); err != nil {
		return err
	}
	return nil
}

// branchCutMappingSchema 是给 Migrator 用的"空壳"模型 ——
// 只需要它带着正确的表名与索引名,不需要字段。
//
// ⚠️ 这里**不能**直接用 model.BranchCutMapping:它的标签已经改成新约束了,
// Migrator 看到的 index 名与库里残留的旧索引名对不上,HasIndex 永远返 false
// ⇒ 旧索引永远删不掉。
type branchCutMappingSchema struct {
	ID            string `gorm:"primaryKey;column:id;type:varchar(64)"`
	BranchID      string `gorm:"column:branch_id;type:varchar(64);not null;uniqueIndex:idx_branch_cut,priority:1"`
	CutType       string `gorm:"column:cut_type;type:varchar(32);not null;uniqueIndex:idx_branch_cut,priority:2"`
	CubeProductID string `gorm:"column:cube_product_id;type:varchar(64);not null"`
}

// TableName 表名。
func (branchCutMappingSchema) TableName() string { return "branch_cut_mappings" }

// dropResidualUniqueCutIndex 扫 PG 索引定义,删掉"唯一 + 列正好是
// (branch_id, cut_type)"的残留索引(不限名字)。
func dropResidualUniqueCutIndex(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil // SQLite 测试库里没有"残留索引"这个场景
	}
	var names []string
	err := db.Raw(`
SELECT indexname FROM pg_indexes
WHERE schemaname = current_schema()
  AND tablename = 'branch_cut_mappings'
  AND indexdef LIKE '%UNIQUE%'
  AND indexname <> 'branch_cut_mappings_pkey'
  AND indexname <> 'idx_branch_sku'
`).Scan(&names).Error
	if err != nil {
		return fmt.Errorf("fresh-meat: scan branch_cut_mappings indexes: %w", err)
	}
	for _, name := range names {
		// 只删"列正好是 (branch_id, cut_type)"的;别的唯一索引(比如 idx_branch_sku)别动。
		var cols []string
		if err := db.Raw(`
SELECT a.attname FROM pg_index i
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
WHERE i.indexrelid = to_regclass(?)
ORDER BY array_position(i.indkey, a.attnum)`, name).Scan(&cols).Error; err != nil {
			return fmt.Errorf("fresh-meat: inspect index %s: %w", name, err)
		}
		if len(cols) == 2 && cols[0] == "branch_id" && cols[1] == "cut_type" {
			if err := db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %q`, name)).Error; err != nil {
				return fmt.Errorf("fresh-meat: drop residual unique index %s: %w", name, err)
			}
		}
	}
	return nil
}
