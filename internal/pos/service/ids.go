// Package service —— pos 域的业务逻辑(开单 / 查单 / 列表 / 退货 / 改价)。
//
// # 本文件:销售单号生成
//
// 单号格式:**SO<yyyymmdd><6 位当天序号>**,例 `SO20261021000001`。
//
// # 为什么不用"秒级时间戳"
//
// 收银是**并发常态**:一个门店多个收银台、同一个收银员一秒内连开两单。
// 只用时间戳 ⇒ 撞主键 ⇒ 现场看到的是"开单按钮点了报错",
// 而真因(两台收银机同时开单)完全看不出来。
// (仓里班次 id 就踩过这个坑,见 internal/masterdata/service/service.go 的
// NewShiftID 注释。)
//
// # 本实现:进程内计数器 + 每日 DB 播种 + 唯一索引兜底
//
//	① 进程内 sync.Mutex 计数器:同进程内并发取号**天然单调且不撞**
//	   (取号本身不碰 DB ⇒ 并发开单不会因为取号而互相等待)。
//	② 每天第一次取号时,从 DB 数出**当天已有多少张单**做播种
//	   ⇒ 进程重启 / 隔天 / 多实例之后,序号接着往后排,不会回到 1。
//	③ 跨进程仍可能撞(两个实例同时播种到同一个数)⇒
//	   **DB 主键唯一索引 + 冲突重试**兜底(见 sales.go 的 insertOrder)。
//
// ⇒ 三层叠加:进程内不撞、跨进程靠唯一索引重试、序号当天单调递增。
package service

import (
	"database/sql"
	"context"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/pos/model"
)

// bizZone 是业务时区(中国标准时间,固定 +08:00,不随服务器 TZ 变)。
//
// ⚠️ **固定成 +08:00 而不是 time.Local** ——
//
//	业务日(几点算"今天")必须是**门店所在时区**的口径,
//	不能用服务器的本地时区:同一台机器换个 TZ 或容器 TZ 变了,
//	"今天的单号前缀"就变了 ⇒ 当天序号会重新从 1 开始 ⇒ 与昨天的单号视觉上冲突。
var bizZone = time.FixedZone("CST", 8*60*60)

// saleIDGen 是销售单号生成器(进程内单例,挂在 SalesService 上)。
type saleIDGen struct {
	mu sync.Mutex
	// day 是当前计数器的业务日(yyyymmdd)。
	day string
	// seq 是当天的下一个序号。
	seq int64
}

// next 取下一个销售单号。
//
// db 用于**每日播种**(取当天已发出的最大序号);播种之后取号不再碰 DB。
func (g *saleIDGen) next(ctx context.Context, db *gorm.DB, now time.Time) (string, error) {
	biz := now.In(bizZone)
	day := biz.Format("20060102")

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.day != day {
		n, err := maxSaleSeqOfDay(ctx, db, biz)
		if err != nil {
			return "", err
		}
		g.day = day
		g.seq = n
	}
	g.seq++
	return fmt.Sprintf("SO%s%06d", day, g.seq), nil
}

// maxSaleSeqOfDay 取某个业务日**已发出的最大单号序号**。
//
// # ⚠️ 为什么不能用"当天有多少张单"(2026-10-22 修的真实缺陷)
//
//	原来的播种是 `countSalesOfDay` = **当天已有多少行**,
//	然后 `seq = n` ⇒ 下一个号 = n+1。
//
//	**这在"号段从 1 开始连续"时才成立**,而现实里不成立:
//
//	① **进程在当天中途重启**(每次部署都重启)⇒ 重新播种时
//	   count = 当时已开的行数,而**当时发出的号可能已经到 100**
//	   (号段起点不是 1)⇒ 播种算出的 60 ⇒ 下一个号 000061
//	   **已存在** ⇒ 撞主键 ⇒ 重试 62/63… 也全撞 ⇒ 20 次用尽
//	   ⇒ **开单 500**(2026-10-22 部署后实测复现)。
//	② **回放台的行被软删**(`deleted_at` 非空)⇒ count 更小 ⇒ 更偏。
//
//	⇒ 正确口径是**已发出的最大序号**:`max(right(id, 6))`。
//
// # ⚠️ 必须**含软删行**(`Unscoped`)
//
//	软删掉的号**仍然被用过** ⇒ 下一个号不能落在它上面
//	⇒ 取号要扫**全部**(含软删)当天行。
//
//	⚠️ 含软删 ⇒ 播种值 ≥ 真实最大号 ⇒ 第一个新号一定 **> 任何已用过的号**
//	⇒ 不会撞 ⇒ 重试只是兜底(跨进程并发)。
func maxSaleSeqOfDay(ctx context.Context, db *gorm.DB, biz time.Time) (int64, error) {
	prefix := "SO" + biz.Format("20060102")
	var maxSeq sql.NullInt64
	err := db.WithContext(ctx).Unscoped().Model(&model.SalesOrder{}).
		Where("id LIKE ?", prefix+"%").
		// ⚠️ 表达式必须**PG 与 SQLite 都认**:
		//   - `::bigint` —— SQLite 不认(SQL logic error: unrecognized token: ":")
		//   - `right(id, 6)` —— SQLite **没有这个函数**(no such function: right)
		//   ⇒ 用 `substr(id, <起始位>, 6)` + `CAST(... AS INTEGER)`(两边都有)。
		//     起始位 = len(prefix)+1(prefix = "SO"+yyyymmdd,序号紧跟其后)。
		Select(fmt.Sprintf("COALESCE(MAX(CAST(substr(id, %d, 6) AS INTEGER)), 0)", len(prefix)+1)).
		Scan(&maxSeq).Error
	if err != nil {
		return 0, fmt.Errorf("pos: 统计当天最大单号序号失败: %w", err)
	}
	return maxSeq.Int64, nil
}

