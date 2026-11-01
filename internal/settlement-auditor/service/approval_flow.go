package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 审批流程节点类型(键 = 提交体 flow_selections 的键 / 配置里的 type)。
//
// 命名对齐**企微的节点语义**,不是本系统的业务叫法:
//   - approver = 审批人(企微 node type 1):同意/驳回,决定单据是否通过;
//   - handler  = 办理人(企微 node type 3):**只能确认办理、不能驳回**
//     (企业微信帮助中心「如何设置审批办理人」:办理人角色适用于"报销、付款"
//     等场景,需要出纳收到单据后前往打款);
//   - cc       = 抄送人(企微 node type 2):只抄送,不参与审批。
const (
	FlowNodeApprover = "approver"
	FlowNodeHandler  = "handler"
	FlowNodeCC       = "cc"
)

// 企微 applyevent 的 process.node_list[].type(文档 91853)。
const (
	wecomNodeTypeApprover = 1
	wecomNodeTypeCC       = 2
	wecomNodeTypeHandler  = 3
)

// 多人办理方式(企微 process.node_list[].apv_rel)。
const (
	ApvRelAll    = 1 // 会签:全部同意才算通过
	ApvRelAny    = 2 // 或签:任一同意即通过
	ApvRelSerial = 3 // 依次审批:按顺序一个个来
)

// 选人方式(企微办理人/审批人节点的「单选/多选」)。
const (
	FlowSelectSingle = "single"
	FlowSelectMulti  = "multi"
)

// ApprovalFlowNode 是审批流程里的一个节点。
//
// # 这份声明对应企业微信管理后台里「付款审批」模板的**流程设置**
//
// 节点类型、顺序、选人方式都必须与模板一致 —— 提交时 app 生成的
// process.node_list 才是模板期望的形状(节点类型对不上，企微那边就不对)。
type ApprovalFlowNode struct {
	// Type 是节点类型:approver / handler / cc。
	Type string `json:"type"`
	// Label 是 UI 上的行标题(审批人 / 办理人 / 抄送人)。
	Label string `json:"label"`
	// Select 是选人方式:single(单选) / multi(多选)。
	Select string `json:"select"`
	// ApvRel 是多人办理方式:1会签 2或签 3依次审批(抄送人不适用,填 0)。
	ApvRel int `json:"apv_rel"`
	// Required 表示该节点必填(缺人时提交直接被拒,不提一个空节点给企微)。
	Required bool `json:"required"`
}

// NodeType 返回该节点在企微 applyevent 里的 node type。
func (n ApprovalFlowNode) NodeType() int {
	switch n.Type {
	case FlowNodeHandler:
		return wecomNodeTypeHandler
	case FlowNodeCC:
		return wecomNodeTypeCC
	default:
		return wecomNodeTypeApprover
	}
}

// DefaultApprovalFlow 是默认流程 —— 对齐 2026-10-23 企微管理后台里
// 「付款审批」模板的实际配置:
//
//	岑溪市富百家超市(个体工商户)
//	  ├ 审批人  申请人自选 不限范围 多选 **或签**
//	  ├ 办理人  申请人自选 不限范围 单选
//	  └ 抄送人  申请人自选
//
// ⚠️ 这几个节点**全是「申请人自选」** —— 成员由发起人在提交时选,
// 而不是模板里写死。所以提交必须走 mode=0(app 自带 process.node_list),
// 且 app 必须把**每一个**节点的成员都填上,包括 type=3 的办理人。
//
// ⚠️ **多人审批方式也必须跟着后台改**:走 mode=0 时企微按 app 给的
// node_list 走 ⇒ `apv_rel` 由这里决定,**后台模板里改「或签」不会生效**。
// 2026-11-01 用户口径:审批人节点在管理后台已从「依次审批」改成「或签」
// (一名成员同意即可) ⇒ 这里必须同步成 [ApvRelAny]。
//
// ⇒ 早先"写死 mode=0 + 只给审批人/抄送人两个节点"的做法,会让模板里
// 新加的办理人节点**静默消失** —— 企微按 app 给的 node_list 走,模板流程
// 里多出来的节点根本没有对应的人。
func DefaultApprovalFlow() []ApprovalFlowNode {
	return []ApprovalFlowNode{
		{Type: FlowNodeApprover, Label: "审批人", Select: FlowSelectMulti, ApvRel: ApvRelAny, Required: true},
		{Type: FlowNodeHandler, Label: "办理人", Select: FlowSelectSingle, ApvRel: ApvRelAny, Required: true},
		{Type: FlowNodeCC, Label: "抄送人", Select: FlowSelectMulti, ApvRel: 0, Required: false},
	}
}

// ParseApprovalFlow 解析审批流程声明(JSON 数组)。
//
// 空串 ⇒ 返回默认流程(见 DefaultApprovalFlow)。
//
// 格式:
//
//	[{"type":"approver","label":"审批人","select":"multi","apv_rel":2,"required":true},
//	 {"type":"handler","label":"办理人","select":"single","apv_rel":2,"required":true},
//	 {"type":"cc","label":"抄送人","select":"multi","required":false}]
func ParseApprovalFlow(raw string) ([]ApprovalFlowNode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultApprovalFlow(), nil
	}
	var nodes []ApprovalFlowNode
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, fmt.Errorf("解析审批流程配置: %w", err)
	}
	if err := ValidateApprovalFlow(nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// ValidateApprovalFlow 校验流程声明。
//
// 校验**在启动期**做(不是提交时) —— 配置错了要立刻炸，不能等到店长在
// 企微里发现"这单没人办"。
func ValidateApprovalFlow(nodes []ApprovalFlowNode) error {
	if len(nodes) == 0 {
		return fmt.Errorf("审批流程配置为空:至少要有一个节点")
	}
	seen := map[string]bool{}
	for i, n := range nodes {
		switch n.Type {
		case FlowNodeApprover, FlowNodeHandler, FlowNodeCC:
		default:
			return fmt.Errorf("第 %d 个节点 type=%q 非法(只能是 approver/handler/cc)", i+1, n.Type)
		}
		if seen[n.Type] {
			return fmt.Errorf("第 %d 个节点 type=%q 重复(每种节点只能有一个)", i+1, n.Type)
		}
		seen[n.Type] = true
		switch n.Select {
		case FlowSelectSingle, FlowSelectMulti:
		default:
			return fmt.Errorf("第 %d 个节点 select=%q 非法(只能是 single/multi)", i+1, n.Select)
		}
		if n.Type != FlowNodeCC {
			switch n.ApvRel {
			case ApvRelAll, ApvRelAny, ApvRelSerial:
			default:
				return fmt.Errorf("第 %d 个节点 apv_rel=%d 非法(1会签/2或签/3依次审批)", i+1, n.ApvRel)
			}
		}
		if n.Label == "" {
			return fmt.Errorf("第 %d 个节点缺 label(UI 要用它当行标题)", i+1)
		}
	}
	if !seen[FlowNodeApprover] {
		return fmt.Errorf("审批流程配置里没有审批人节点(approver)")
	}
	return nil
}

// FlowSelections 是按**节点类型**分桶的人员选择。
//
// 键 = 节点 type(approver/handler/cc),值 = 企微 userid 列表。
//
// # 为什么按桶而不是三个固定字段
//
// 节点集合由**模板的流程配置**决定(Config.ApprovalFlow)—— 模板加一个节点
// (比如以后加个"财务总监"),协议与 UI 都不用改:多一个桶而已。
type FlowSelections map[string][]string

// Merge 把 legacy 的 approver/cc 字段并进分桶。
//
// 兼容旧客户端(只发 approver_userids / cc_userids)与已上线的契约测试:
// 它们不知道"办理人"这回事 ⇒ 合并后仍按旧口径走(缺办理人节点 ⇒
// 后端按配置的 required 判定并给出可读报错)。
func (f FlowSelections) Merge(approvers, handlers, cc []string) FlowSelections {
	out := make(FlowSelections, len(f)+3)
	for k, v := range f {
		out[k] = v
	}
	if len(approvers) > 0 {
		out[FlowNodeApprover] = append(out[FlowNodeApprover], approvers...)
	}
	if len(handlers) > 0 {
		out[FlowNodeHandler] = append(out[FlowNodeHandler], handlers...)
	}
	if len(cc) > 0 {
		out[FlowNodeCC] = append(out[FlowNodeCC], cc...)
	}
	return out
}

// ApprovalFlowNodes 返回当前生效的流程节点声明(给 UI 渲染用)。
//
// 永远返回**非空**列表:配置没给时用默认流程(见 DefaultApprovalFlow)。
// UI 拿到空列表就没法渲染任何一行 ⇒ 发起人无从选人 ⇒ 提交必然失败。
func (s *Service) ApprovalFlowNodes() []ApprovalFlowNode {
	if len(s.cfg.ApprovalFlow) == 0 {
		return DefaultApprovalFlow()
	}
	return s.cfg.ApprovalFlow
}

// resolveFlowSelections 按流程配置校验并归一每个节点的人员。
//
// 校验在这里做(而不是提交到企微时) —— 缺人要在**发起页当场**报错，
// 不能等审批单发出去才发现"这单没人办"。
func (s *Service) resolveFlowSelections(sel FlowSelections) (FlowSelections, error) {
	nodes := s.cfg.ApprovalFlow
	if len(nodes) == 0 {
		nodes = DefaultApprovalFlow()
	}
	out := make(FlowSelections, len(nodes))
	for _, nd := range nodes {
		ids := dedupeStrings(sel[nd.Type])
		if len(ids) > maxApproversPerApproval {
			return nil, fmt.Errorf("%w: %s 最多 %d 人", ErrInvalidInput, nd.Label, maxApproversPerApproval)
		}
		if nd.Select == FlowSelectSingle && len(ids) > 1 {
			return nil, fmt.Errorf("%w: %s 只能选 1 人(模板配的是单选)", ErrInvalidInput, nd.Label)
		}
		if len(ids) == 0 {
			if nd.Required {
				return nil, fmt.Errorf("%w: 请选择%s", ErrInvalidInput, nd.Label)
			}
			continue
		}
		out[nd.Type] = ids
	}
	// 兜底:审批人节点必须有人(无论配置里 required 是什么)——
	// 没有审批人的付款单在企微里不会有人批。
	if len(out[FlowNodeApprover]) == 0 {
		return nil, fmt.Errorf("%w: 请选择审批人", ErrInvalidInput)
	}
	return out, nil
}

// buildProcess 按流程配置把各节点人员组装成企微 applyevent 的 process.node_list。
//
// 节点**顺序** = 配置里的顺序(必须与企微模板的流程图一致:
// 申请人 → 审批人 → 办理人 → 抄送人)。
//
// ⚠️ 提交时**必须**用 mode=0(自带 process):这个模板的节点全是「申请人自选」,
// mode=1 时企微会忽略 app 给的成员,节点成员为空 ⇒ 没人办。
func (s *Service) buildProcess(sel FlowSelections) (*ProcessConfig, error) {
	nodes := s.cfg.ApprovalFlow
	if len(nodes) == 0 {
		nodes = DefaultApprovalFlow()
	}
	out := &ProcessConfig{NodeList: []ProcessNode{}}
	for _, nd := range nodes {
		ids := dedupeStrings(sel[nd.Type])
		if len(ids) == 0 {
			continue // 可选节点没人 ⇒ 不给企微一个空节点
		}
		out.NodeList = append(out.NodeList, ProcessNode{
			Type:   nd.NodeType(),
			ApvRel: nd.ApvRel,
			UserID: ids,
		})
	}
	if len(out.NodeList) == 0 {
		return nil, fmt.Errorf("审批流为空:没有任何节点成员")
	}
	return out, nil
}
