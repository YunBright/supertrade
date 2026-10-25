// Package service / surrogate_id.go —— 代理主键的**确定性**算法。
//
// # 为什么需要它
//
// core/purge 的清理是"按旁表 testrun_rows 记下的**主键值**删":
//
//	DELETE FROM <业务表> WHERE CAST(<主键> AS text) IN (
//	    SELECT row_id FROM testrun_rows WHERE run_id = ? AND table_name = ?)
//
// ⇒ 业务表必须有**单列主键**才能进 PurgeRegistry(复合主键 ⇒ 报"模型没有主键")。
// ⇒ 有几张表因此改成了"单列代理主键 + 业务唯一索引"。
//
// # ⚠️ 为什么代理主键必须**确定性**
//
// 溯源记账(gorm 回调)记的是"**本次 INSERT 语句里**的主键值"。
// 而 ON CONFLICT 的 upsert 在冲突时**保留旧行的主键** —— 也就是说:
//
//	第 1 次 upsert:插入 id = 算出来的 A   ⇒ 记账记 A   ✔(就是那一行的主键)
//	第 2 次 upsert:冲突 ⇒ 旧行的主键仍是 A,但本次 INSERT 带的也是 A ✔
//
// ⇒ 只要主键是**由业务键算出来的**,两次算出来的一样,记账记的就永远是那一行的真主键。
//
// 若主键是随机的(比如 UUID/时间戳):
//
//	第 1 次:插入 id = 随机 R1 ⇒ 记账记 R1
//	第 2 次:冲突 ⇒ 旧行主键还是 R1,但本次 INSERT 带的是 R2 ⇒ 记账记 R2
//	         ⇒ R2 **不存在** ⇒ purge 拿着 R2 去删,删不到任何行
//	         ⇒ 残留永远清不掉,而且**没有任何报错**。
//
// 判据:凡是"代理主键 + upsert + 旁表记账"的组合,主键必须确定性。

package service

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
)

// surrogateID 由业务键算出**确定性**的代理主键。
//
// parts 用 \x00 连接(业务键里不会出现 NUL)⇒ 不同切分不会撞。
func surrogateID(prefix string, parts ...string) string {
	sum := md5.Sum([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])
}

// StockPositionID 位置表(sku_stock_positions)的代理主键。
func StockPositionID(branchID, cubeProductID, location string) string {
	return surrogateID("SSP", branchID, cubeProductID, location)
}

// SkuCostStateID 成本状态表(sku_cost_states)的代理主键。
func SkuCostStateID(branchID, cubeProductID string) string {
	return surrogateID("SCS", branchID, cubeProductID)
}

// StocktakeID 盘点单(pork_cuts_stocktakes)的**确定性**代理主键。
//
// bizDate 是**归属日**("YYYY-MM-DD",营业日界时区)。
//
// ⇒ 同一 (门店, 归属日) 永远是同一行 ⇒ 重复提交同一天的盘点 = **覆盖**
// (而不是多出一张 ⇒ 报表重复计数、"当天盘了几次"说不清)。
func StocktakeID(branchID, bizDate string) string {
	return surrogateID("SK", branchID, bizDate)
}

// hashKey 由若干维度算出**定长**的短键(16 位十六进制)。
//
// # 为什么不用"把 id 拼起来"
//
// 拼起来的键会**随维度个数线性变长** ⇒ 迟早撞上列宽上限
// (实测:`line_sales_by_pigs.pos_line_id` 是 varchar(64),
// "BULK-" + 结算单19 + 产出19 + 猪18 + 明细序号 ≈ 65 ⇒ PG 报
// "value too long for type character varying(64)")。
//
// 而且它报错的地方很偏:不在 INSERT 上,而在**溯源记账**里
// (旁表 row_id 也要塞这个值)⇒ 症状是"结算莫名 500"。
//
// ⇒ 固定 16 位十六进制 = 64 位哈希,定长、永不超宽。
func hashKey(parts ...string) string {
	sum := md5.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}
