// Package service / publisher.go —— pub/sub 发布与跨服务 wire 契约。
//
// Topic 清单:
//
//	发布:
//	  wecom.approval.command        → wechat-bot(命令:apply_event/notify_text/stocktake_taskcard)
//	  settlement.audit.status       → notification-gateway(状态变化,Flutter 监控)
//	  settlement.audit.log          → notification-gateway(推理日志增量,Flutter 实时监控)
//	订阅(见 handler/subscribe.go):
//	  wecom.approval.status_changed ← wechat-bot(企微回调)
//	  wecom.approval.command_result ← wechat-bot(命令执行结果)
//	  auth.user.access_changed      ← auth(scope 缓存失效)
//
// ⚠️ wecom wire 结构(Command/*Payload/CommandResult/StatusChanged)与
// wechat-bot internal/wapproval/model.go 同形定义 —— 两个 module 不能跨仓 import,
// 字段必须逐字对齐,改动需双仓同步。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	dapr "github.com/dapr/go-sdk/client"
	"github.com/google/uuid"

	"github.com/YunBright/supertrade/pkg/eventbus"
)

// ---- topic 常量 ----

const (
	// TopicWecomApprovalCommand 发布命令给 wechat-bot。
	TopicWecomApprovalCommand = "wecom.approval.command"
	// TopicAuditStatus 审批实例状态变化(自有事件,供 gateway Fanout)。
	TopicAuditStatus = "settlement.audit.status"
	// TopicAuditLog 推理日志增量(自有事件,供 gateway Fanout)。
	TopicAuditLog = "settlement.audit.log"
)

// 订阅侧 topic(handler/subscribe.go 注册)。
const (
	TopicWecomStatusChanged = "wecom.approval.status_changed"
	TopicWecomCommandResult = "wecom.approval.command_result"
	TopicAuthAccessChanged  = "auth.user.access_changed"
)

// ---- wecom 命令 wire(镜像 wechat-bot internal/wapproval/model.go) ----

// 命令 type 常量。
const (
	CmdApplyEvent = "apply_event"
	CmdGetDetail  = "get_detail"
	CmdNotifyText = "notify_text"
	CmdTaskCard   = "stocktake_taskcard"
)

// WecomCommand 是 wecom.approval.command 的载荷。
type WecomCommand struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	CorpID  string          `json:"corp_id"`
	Payload json.RawMessage `json:"payload"`
}

// ApplyEventPayload 代提交审批申请。
type ApplyEventPayload struct {
	CreatorUserID       string          `json:"creator_userid"`
	TemplateID          string          `json:"template_id"`
	UseTemplateApprover int             `json:"use_template_approver"`
	// Process 显式审批流程(UseTemplateApprover=0 时必填),对齐企微 91853
	// process.node_list:type 1=审批人 2=抄送人 3=办理人;审批/办理节点
	// 需 apv_rel(1会签 2或签 3依次审批)。镜像 wechat-bot
	// wapproval.ProcessConfig,wire 字段必须逐字对齐。
	Process     *ProcessConfig  `json:"process,omitempty"`
	NotifyType  int             `json:"notify_type,omitempty"`
	ApplyData   json.RawMessage `json:"apply_data"`
	SummaryList json.RawMessage `json:"summary_list"`
}

// ProcessConfig 是企微 applyevent 的新版流程列表。
type ProcessConfig struct {
	NodeList []ProcessNode `json:"node_list"`
}

// ProcessNode 是一个流程节点。
type ProcessNode struct {
	// Type 节点类型:1=审批人 2=抄送人 3=办理人。
	Type int `json:"type"`
	// ApvRel 多人审批方式(type=1/3 时必填):1=会签 2=或签 3=依次审批。
	ApvRel int      `json:"apv_rel,omitempty"`
	UserID []string `json:"userid"`
}

// NotifyTextPayload 发文本应用消息。
type NotifyTextPayload struct {
	ToUser  []string `json:"to_user"`
	Content string   `json:"content"`
}

// TaskCardPayload 发 taskcard 待办卡片。
type TaskCardPayload struct {
	ToUser      []string `json:"to_user"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	TaskID      string   `json:"task_id"`
}

// WecomCommandResult 是 wecom.approval.command_result 的载荷(订阅侧)。
type WecomCommandResult struct {
	CommandID string          `json:"command_id"`
	Type      string          `json:"type"`
	CorpID    string          `json:"corp_id"`
	OK        bool            `json:"ok"`
	SpNo      string          `json:"sp_no,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	Error     string          `json:"error,omitempty"`
	At        time.Time       `json:"at"`
}

// WecomStatusChanged 是 wecom.approval.status_changed 的载荷(订阅侧)。
type WecomStatusChanged struct {
	CorpID        string          `json:"corp_id"`
	CorpWWID      string          `json:"corp_wwid"`
	SpNo          string          `json:"sp_no"`
	SpName        string          `json:"sp_name"`
	SpStatus      int             `json:"sp_status"`
	TemplateID    string          `json:"template_id"`
	ApplyTime     int64           `json:"apply_time"`
	ApplyerUserID string          `json:"applyer_userid"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	RawXML        string          `json:"raw_xml"`
	ReceivedAt    time.Time       `json:"received_at"`
}

// 企微 sp_status 值。
const (
	SpStatusApproving     = 1
	SpStatusApproved      = 2
	SpStatusRejected      = 3
	SpStatusCancelled     = 4
	SpStatusCancelAfterOK = 6
	SpStatusPaid          = 10
)

// ---- 自有事件 payload(gateway Fanout 按 user_id/branch_id 精准投递) ----

// AuditStatusEvent 审批状态变化事件。
type AuditStatusEvent struct {
	ApprovalID string    `json:"approval_id"`
	BranchID   string    `json:"branch_id"`
	UserID     string    `json:"user_id"`
	Status     string    `json:"status"`
	SpNo       string    `json:"sp_no,omitempty"`
	SpStatus   int       `json:"sp_status,omitempty"`
	At         time.Time `json:"at"`
}

// AuditLogEvent 推理日志增量事件。
type AuditLogEvent struct {
	ApprovalID    string    `json:"approval_id"`
	BranchID      string    `json:"branch_id"`
	UserID        string    `json:"user_id"`
	Step          string    `json:"step"`
	OutputSummary string    `json:"output_summary"`
	At            time.Time `json:"at"`
}

// ---- Publisher ----

// Publisher 是 pub/sub 发布抽象(单测注入 fake)。
type Publisher interface {
	Publish(ctx context.Context, topic string, data any) error
}

// DaprPublisher Publisher 的 dapr-sdk 实现(仿 fresh-meat)。
type DaprPublisher struct {
	client dapr.Client
	pubsub string
}

// NewDaprPublisherFromEnv 构造 publisher。
//
//	DAPR_PUBSUB = "tradewind-pubsub"   // 默认
//
// ⚠️ 必须与订阅侧 component metadata.name 逐字相同。
func NewDaprPublisherFromEnv() (*DaprPublisher, error) {
	cli, err := dapr.NewClient()
	if err != nil {
		return nil, err
	}
	ps := os.Getenv("DAPR_PUBSUB")
	if ps == "" {
		ps = eventbus.Name
	}
	return &DaprPublisher{client: cli, pubsub: ps}, nil
}

// Publish 把 data 发到 pubsub/topic。
func (p *DaprPublisher) Publish(ctx context.Context, topic string, data any) error {
	return p.client.PublishEvent(ctx, p.pubsub, topic, data)
}

// ---- StocktakeClient(盘点计划建单,dapr invoke stocktake 内部端点) ----

// StocktakeCreatePlanInput 建盘点计划单入参。
type StocktakeCreatePlanInput struct {
	BranchID   string `json:"branch_id"`
	OperatorID string `json:"operator_id"`
	Remark     string `json:"remark"`
	// ProductIDs 计划商品清单(建单后写 stocktake_plan_items)。
	ProductIDs []string `json:"product_ids"`
}

// StocktakeHeaderDTO stocktake 返回的盘点单头。
type StocktakeHeaderDTO struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
	Type     string `json:"type"`
	Status   string `json:"status"`
}

// StocktakeClient 盘点建单抽象(单测注入 fake)。
type StocktakeClient interface {
	CreatePlanHeader(ctx context.Context, in StocktakeCreatePlanInput) (*StocktakeHeaderDTO, error)
}

// DaprStocktakeClient 经 dapr service invocation 调 stocktake **内部端点**
// (POST /internal/stocktake-plan-headers,由任务 1c 在 stocktake 侧提供;
// 内部端点不走 JWT,靠 dapr 内网 + 不挂 nginx 保护)。
type DaprStocktakeClient struct {
	client dapr.Client
	appID  string
}

// NewDaprStocktakeClient 构造 stocktake 内部调用客户端。
func NewDaprStocktakeClient(daprCli dapr.Client, appID string) *DaprStocktakeClient {
	if appID == "" {
		appID = "stocktake"
	}
	return &DaprStocktakeClient{client: daprCli, appID: appID}
}

// CreatePlanHeader 调 stocktake 内部端点建 plan 单 + 计划商品。
func (c *DaprStocktakeClient) CreatePlanHeader(ctx context.Context, in StocktakeCreatePlanInput) (*StocktakeHeaderDTO, error) {
	if c.client == nil {
		return nil, fmt.Errorf("dapr client not injected")
	}
	b, _ := json.Marshal(in)
	resp, err := c.client.InvokeMethodWithContent(ctx, c.appID,
		"internal/stocktake-plan-headers", "POST",
		&dapr.DataContent{ContentType: "application/json", Data: b})
	if err != nil {
		return nil, fmt.Errorf("invoke stocktake create plan header: %w", err)
	}
	var out StocktakeHeaderDTO
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("decode stocktake header: %w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("stocktake returned empty header id")
	}
	return &out, nil
}

// ---- 命令组装 helper ----

// newWecomCommand 组装命令(uuid 关联 id + payload 序列化)。
func newWecomCommand(cmdType, corpID string, payload any) (*WecomCommand, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", cmdType, err)
	}
	return &WecomCommand{
		ID:      uuid.NewString(),
		Type:    cmdType,
		CorpID:  corpID,
		Payload: b,
	}, nil
}

// publishCommand 组命令并发布;返回命令 id(用于 command_result 关联)。
func (s *Service) publishCommand(ctx context.Context, cmdType string, payload any) (string, error) {
	if s.publisher == nil {
		return "", fmt.Errorf("publisher 未配置")
	}
	cmd, err := newWecomCommand(cmdType, s.cfg.CorpID, payload)
	if err != nil {
		return "", err
	}
	if err := s.publisher.Publish(ctx, TopicWecomApprovalCommand, cmd); err != nil {
		return "", fmt.Errorf("publish %s: %w", cmdType, err)
	}
	return cmd.ID, nil
}

// publishStatusEvent 发布审批状态变化(gateway → Flutter)。失败仅 warn。
func (s *Service) publishStatusEvent(ctx context.Context, ev *AuditStatusEvent) {
	if s.publisher == nil || ev == nil {
		return
	}
	if err := s.publisher.Publish(ctx, TopicAuditStatus, ev); err != nil {
		s.logger.Warn("publish settlement.audit.status failed",
			"approval_id", ev.ApprovalID, "status", ev.Status, "err", err)
	}
}

// publishLogEvent 发布推理日志增量(gateway → Flutter)。失败仅 warn。
func (s *Service) publishLogEvent(ctx context.Context, ev *AuditLogEvent) {
	if s.publisher == nil || ev == nil {
		return
	}
	if err := s.publisher.Publish(ctx, TopicAuditLog, ev); err != nil {
		s.logger.Warn("publish settlement.audit.log failed",
			"approval_id", ev.ApprovalID, "step", ev.Step, "err", err)
	}
}
