// Package service / service.go —— settlement-auditor 核心 Service。
//
// 业务域:结算付款审批的**机器一审**(PRD §FR-2):
//
//	H5 提交 → SubmitApproval(received) → 后台 runAudit 流水线
//	  → 通过:publish wecom apply_event 命令(wechat-bot 代提交企微,进人工终审)
//	  → 拒绝:publish notify_text 通知申请人 + 落 RejectReason
//	企微回调(wecom.approval.status_changed) → OnStatusChanged 更新终态
//
// 依赖注入(New 之后由 main 调 Set*):
//   - cube:cube-router 查询客户端(settlement/purchase/sale/stock)
//   - publisher:pub/sub 广播(命令 + 自有事件)
//   - verdictFn:LLM 研判函数(默认规则降级;生产注入 Dapr Conversation API)
//   - stocktake:盘点计划建单客户端(dapr invoke stocktake 内部端点,1c 提供)
//   - userInfo:per-branch effective scopes 校验(60s 缓存,同 fresh-meat)
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// Config 是 Service 的运行参数(main.go 从 env 读入)。
type Config struct {
	// CorpID 是 wcorp 友好 id(wecom 命令的 corp 路由键)。
	CorpID string
	// TemplateID 是企微"结算付款审批"模板 id(apply_event 用)。
	TemplateID string
	// AmountToleranceYuan 金额勾稽容差(默认 0.01)。
	AmountToleranceYuan float64
	// SaleWindowDays 数量信号的销售回看窗口(默认 30 天)。
	SaleWindowDays int
	// QtyDeviationFactor 采购量/销售量偏离倍数阈值(默认 3;>factor warn,>2*factor block)。
	QtyDeviationFactor float64
	// StocktakeAppID 是 stocktake 的 dapr app-id(默认 "stocktake")。
	StocktakeAppID string
	// StocktakeNotifyUsers 是盘点待办接收人(企微 userid 列表)。
	StocktakeNotifyUsers []string
	// WxH5BaseURL 拼盘点单链接(taskcard url),如 "https://example.com/wx-h5/#/pages/inventory"。
	WxH5BaseURL string
	// ApplyControlIDs 是企微模板三个控件 id(结算单号/付款金额/备注),
	// 必须与企微后台创建的模板一致(deploy 配置,默认 Text-1/Text-2/Text-3)。
	ApplyControlIDs [3]string
	// ApplyControlTypes 是三个控件的类型(与企微模板控件一致,
	// 如 Text/Textarea;deploy 配置,默认全 Text)。applyevent 按模板
	// 控件类型校验,类型对不上会被拒。
	ApplyControlTypes [3]string
	// AIConclusionControlID 是模板「AI审核结论」多行文本控件的 id(可选)。
	// 非空时 apply_data 追加该控件,写入 LLM 审核结论精简文案;
	// 空(模板未配该控件)或结论为空时跳过,不阻塞提交。
	AIConclusionControlID string
}

func (c *Config) withDefaults() {
	if c.AmountToleranceYuan <= 0 {
		c.AmountToleranceYuan = 0.01
	}
	if c.SaleWindowDays <= 0 {
		c.SaleWindowDays = 30
	}
	if c.QtyDeviationFactor <= 0 {
		c.QtyDeviationFactor = 3
	}
	if c.StocktakeAppID == "" {
		c.StocktakeAppID = "stocktake"
	}
	if c.ApplyControlIDs[0] == "" {
		c.ApplyControlIDs = [3]string{"Text-1", "Text-2", "Text-3"}
	}
	if c.ApplyControlTypes[0] == "" {
		c.ApplyControlTypes = [3]string{"Text", "Text", "Text"}
	}
}

// Service 持有 settlement-auditor 全部业务逻辑。
type Service struct {
	db        *gorm.DB
	cfg       Config
	cube      CubeQuerier     // 可 nil(流水线对 cube 不可用 → 实例 failed)
	publisher Publisher       // nil = 禁用 pub/sub(单测)
	verdictFn VerdictFn       // 默认规则降级;SetVerdictFn 注入 LLM
	stocktake StocktakeClient // 可 nil(跳过盘点建单)
	userInfo  *userinfo.Client
	logger    *slog.Logger
	now       func() time.Time

	scopeMu    sync.RWMutex
	scopeCache map[string]scopeCacheEntry
	scopeTTL   time.Duration
}

// scopeCacheEntry 缓存一条 effective scopes entry。
type scopeCacheEntry struct {
	scopes []string
	at     time.Time
}

// defaultScopeTTL 是 effective scopes 缓存的默认 TTL。
const defaultScopeTTL = 60 * time.Second

// 业务错误(handler 映射 HTTP 状态码)。
var (
	ErrApprovalNotFound    = errors.New("service: 审批实例不存在")
	ErrInvalidInput        = errors.New("service: 输入校验失败")
	ErrInvalidStatus       = errors.New("service: 当前状态不允许该操作")
	ErrCubeUnavailable     = errors.New("service: cube client 未配置")
	ErrUserInfoUnavailable = errors.New("service: userinfo client 未配置或不可用")
)

// New 构造 Service。cfg 必填项不足时以默认值兜底(见 withDefaults)。
func New(db *gorm.DB, cfg Config) *Service {
	cfg.withDefaults()
	return &Service{
		db:         db,
		cfg:        cfg,
		logger:     slog.Default(),
		now:        func() time.Time { return time.Now() },
		verdictFn:  defaultVerdictFn,
		scopeCache: make(map[string]scopeCacheEntry),
		scopeTTL:   defaultScopeTTL,
	}
}

// Config 暴露运行参数(只读)。
func (s *Service) Config() Config { return s.cfg }

// SetCube 注入 cube 查询客户端。
func (s *Service) SetCube(c CubeQuerier) {
	if c != nil {
		s.cube = c
	}
}

// SetPublisher 注入 pub/sub publisher。
func (s *Service) SetPublisher(p Publisher) {
	if p != nil {
		s.publisher = p
	}
}

// SetVerdictFn 注入 LLM 研判函数(替换默认规则降级路径)。
func (s *Service) SetVerdictFn(fn VerdictFn) {
	if fn != nil {
		s.verdictFn = fn
	}
}

// SetStocktake 注入盘点建单客户端。
func (s *Service) SetStocktake(c StocktakeClient) {
	if c != nil {
		s.stocktake = c
	}
}

// SetUserInfo 注入 userinfo 客户端。
func (s *Service) SetUserInfo(c *userinfo.Client) {
	s.userInfo = c
}

// SetClock 注入时间(测试用)。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// ---- effective scopes(同 fresh-meat 模式:60s 缓存 + auth.user.access_changed 失效) ----

// GetEffectiveScopes 拿用户在某 branch 下的 effective scopes。
func (s *Service) GetEffectiveScopes(ctx context.Context, userID, branchID string) ([]string, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 必填", ErrInvalidInput)
	}
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if s.userInfo == nil {
		return nil, ErrUserInfoUnavailable
	}

	ck := userID + "|" + branchID

	s.scopeMu.RLock()
	entry, ok := s.scopeCache[ck]
	ttl := s.scopeTTL
	s.scopeMu.RUnlock()
	if ok && time.Since(entry.at) < ttl {
		return entry.scopes, nil
	}

	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if entry, ok := s.scopeCache[ck]; ok && time.Since(entry.at) < ttl {
		return entry.scopes, nil
	}

	p, err := s.userInfo.GetBranchPermissions(ctx, userID, branchID)
	if err != nil {
		s.logger.Warn("userinfo.GetBranchPermissions failed",
			"user_id", userID, "branch_id", branchID, "err", err)
		return nil, ErrUserInfoUnavailable
	}
	var scopes []string
	if p != nil {
		for _, row := range p.Branches {
			if row.BranchID == branchID {
				scopes = row.Scopes
				break
			}
		}
	}
	scopes = append([]string(nil), scopes...)
	s.scopeCache[ck] = scopeCacheEntry{scopes: scopes, at: s.now()}
	return scopes, nil
}

// HasEffectiveScope 判断用户在该 branch 下是否拥有某 scope。
func (s *Service) HasEffectiveScope(ctx context.Context, userID, branchID, scope string) (bool, error) {
	scopes, err := s.GetEffectiveScopes(ctx, userID, branchID)
	if err != nil {
		return false, err
	}
	for _, sc := range scopes {
		if sc == scope {
			return true, nil
		}
	}
	return false, nil
}

// InvalidateScopeCache 失效某 (user, branch) 缓存(订阅 auth.user.access_changed 时调)。
func (s *Service) InvalidateScopeCache(userID, branchID string) {
	if userID == "" {
		return
	}
	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if branchID == "" {
		// 缺省清空该 user 全部缓存(同 fresh-meat 语义)。
		prefix := userID + "|"
		for k := range s.scopeCache {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				delete(s.scopeCache, k)
			}
		}
		return
	}
	delete(s.scopeCache, userID+"|"+branchID)
}

// ---- 审批人/抄送人偏好(对齐官方审批"记住上次选择"体验) ----

// ApproverPreference 是某申请人在某 branch 下最近一次使用的审批流人员。
type ApproverPreference struct {
	ApproverUserIDs []string `json:"approver_userids"`
	CcUserIDs       []string `json:"cc_userids"`
}

// GetApproverPreference 读取偏好;从未提交过时返回空切片(不报错)。
func (s *Service) GetApproverPreference(ctx context.Context, branchID, userID string) (*ApproverPreference, error) {
	pref := &ApproverPreference{ApproverUserIDs: []string{}, CcUserIDs: []string{}}
	if branchID == "" || userID == "" {
		return pref, fmt.Errorf("%w: branch_id/user_id 必填", ErrInvalidInput)
	}
	var row model.ApprovalApproverPreference
	err := s.db.WithContext(ctx).
		Where("id = ?", branchID+":"+userID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return pref, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load approver preference: %w", err)
	}
	if len(row.ApproverUserIDs) > 0 {
		_ = json.Unmarshal(row.ApproverUserIDs, &pref.ApproverUserIDs)
	}
	if len(row.CcUserIDs) > 0 {
		_ = json.Unmarshal(row.CcUserIDs, &pref.CcUserIDs)
	}
	if pref.ApproverUserIDs == nil {
		pref.ApproverUserIDs = []string{}
	}
	if pref.CcUserIDs == nil {
		pref.CcUserIDs = []string{}
	}
	return pref, nil
}

// upsertApproverPreference 用本次提交的选择覆盖偏好(提交成功即记住)。
func (s *Service) upsertApproverPreference(ctx context.Context, branchID, userID string, approvers, cc []string) error {
	ab, _ := json.Marshal(dedupeStrings(approvers))
	cb, _ := json.Marshal(dedupeStrings(cc))
	row := model.ApprovalApproverPreference{
		ID:              branchID + ":" + userID,
		BranchID:        branchID,
		ApplicantUserID: userID,
		ApproverUserIDs: ab,
		CcUserIDs:       cb,
		UpdatedAt:       s.now(),
	}
	// upsert:主键冲突时覆盖人员与时间。
	return s.db.WithContext(ctx).Save(&row).Error
}
