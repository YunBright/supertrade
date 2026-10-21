// Package service —— member 域的业务逻辑。
//
// # 分层
//
//	service(本包) → model(纯表结构 + 错误 sentinel)
//	              → pkg/outbox(事务性发件箱)
//
// service **不 import** http/gin:HTTP 语义(状态码 / 错误信封)在 handler 包里。
//
// # 四条贯穿全包的不变量
//
//  1. **金额/积分一律 decimal.Decimal**,⛔ 绝不用 float64。
//  2. **写操作幂等**:每条写都带 `op_id`,靠唯一索引 + `clause.OnConflict{DoNothing}`
//     去重;**重放返回首次结果,不报错**。
//  3. **事件与业务行同事务**:`outbox.Recorder.Enqueue(tx, ...)` 必须传业务事务的 tx
//     ⇒ 进程在 commit 与 publish 之间被杀,事件也不会丢。
//  4. **积分口径只有两个列**:累计获得(points_accumulated)与累计扣减(points_used),
//     剩余积分 = 两者之差(派生,不落列)。
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// actor 是系统写入的 created_by / updated_by 兜底值。
const actor = "system"

// Service 是 member 域的业务服务。
type Service struct {
	db *gorm.DB
	// recorder 是事务性发件箱的写入器(只接受调用方传进来的 tx)。
	recorder *outbox.Recorder
	// checker 判定"某用户在某门店是否拥有某 scope"(per-branch 权限矩阵)。
	checker ScopeChecker
	// verifier 校验工号+口令(图4 越权授权链路)。
	verifier EmployeeVerifier
	// now 是可注入时钟(测试用)。
	now func() time.Time
}

// New 构造 Service。
func New(db *gorm.DB) *Service {
	return &Service{
		db:       db,
		recorder: outbox.NewRecorder("member"),
		now:      func() time.Time { return time.Now() },
	}
}

// SetClock 注入时钟(测试用)。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// SetScopeChecker 注入 per-branch scope 判定器。
//
// ⚠️ 不注入 ⇒ 写端点一律 403(见 Authorize)—— fail-closed。
func (s *Service) SetScopeChecker(c ScopeChecker) { s.checker = c }

// ScopeChecker 返回当前注入的 scope 判定器(nil = 未注入)。
func (s *Service) ScopeChecker() ScopeChecker { return s.checker }

// SetEmployeeVerifier 注入工号+口令校验器(图4 越权授权链路)。
func (s *Service) SetEmployeeVerifier(v EmployeeVerifier) { s.verifier = v }

// DB 暴露底层 gorm.DB(装配侧 / 测试用)。
func (s *Service) DB() *gorm.DB { return s.db }

// ---- 内部 helpers ----

// getMember 取会员(按复合主键 id + branch_id)。
//
// ⚠️ 软删行**查不到**:`gorm.DeletedAt` 让 GORM 自动加 `deleted_at IS NULL`。
func (s *Service) getMember(ctx context.Context, branchID, memberID string) (*model.Member, error) {
	var row model.Member
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", strings.TrimSpace(memberID), strings.TrimSpace(branchID)).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberNotFound
		}
		return nil, err
	}
	fillDerived(&row)
	return &row, nil
}

// fillDerived 填充派生字段(剩余积分)。
func fillDerived(m *model.Member) {
	if m == nil {
		return
	}
	m.RemainingPoints = m.PointsAccumulated.Sub(m.PointsUsed)
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// newLedgerID 生成流水行 id。
//
// ⚠️ 带随机后缀:同一秒内两笔流水是常态(两台收银机同时消费),
// 只用时间戳会撞主键。
func newLedgerID(prefix string, t time.Time) string {
	return fmt.Sprintf("%s%s%s", prefix, t.UTC().Format("20060102150405.000000000"), randomSuffix())
}

// randomSuffix 生成 6 位随机十六进制后缀(3 字节)。
//
// ⚠️ 碰撞概率约 1/1670 万,单店规模可忽略。
func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 读不到随机源(几乎不可能)⇒ 用纳秒兜底。
		return fmt.Sprintf("%09x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// pointsRateOf 取门店的积分率(默认 1.0 = 消费 1 元得 1 积分)。
func (s *Service) pointsRateOf(ctx context.Context, branchID string) decimal.Decimal {
	var row model.MemberSettings
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", strings.TrimSpace(branchID)).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 没有配置行 ⇒ 用默认值(1.0)。⚠️ 不是错误:门店还没配会员参数时
			// 会员功能仍要能开卡(思迅源库的实测值也是 1.0)。
			return PointsRateDefault
		}
		// 读配置失败 ⇒ 保守回落默认率(宁可少算积分,也不要因为配置表读不到
		// 就把积分算错 —— 那会让每一笔都对不上)。
		slog.Warn("member: 读取门店会员配置失败,按默认积分率 1.0 处理", "err", err)
		return PointsRateDefault
	}
	if row.PointsRate.LessThanOrEqual(decimal.Zero) {
		return PointsRateDefault
	}
	return row.PointsRate
}
