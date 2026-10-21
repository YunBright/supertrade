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
// db 用于**每日播种**(数当天已有多少单);播种之后取号不再碰 DB。
func (g *saleIDGen) next(ctx context.Context, db *gorm.DB, now time.Time) (string, error) {
	biz := now.In(bizZone)
	day := biz.Format("20060102")

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.day != day {
		n, err := countSalesOfDay(ctx, db, biz)
		if err != nil {
			return "", err
		}
		g.day = day
		g.seq = n
	}
	g.seq++
	return fmt.Sprintf("SO%s%06d", day, g.seq), nil
}

// countSalesOfDay 数出某个业务日已有多少张销售单(跨门店,单号是全局的)。
func countSalesOfDay(ctx context.Context, db *gorm.DB, biz time.Time) (int64, error) {
	start := time.Date(biz.Year(), biz.Month(), biz.Day(), 0, 0, 0, 0, bizZone)
	end := start.AddDate(0, 0, 1)
	var n int64
	err := db.WithContext(ctx).
		Model(&model.SalesOrder{}).
		Where("sold_at >= ? AND sold_at < ?", start, end).
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("pos: 统计当天单量失败: %w", err)
	}
	return n, nil
}
