package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 门店级「盘点计划推送群」配置。
//
// 用户口径(2026-10-22):盘点计划的消息发到**指定的微信企业群**,
// 而不是发给固定的几个人;群在 Flutter 管理后台 →「仓库-设置」里指定。
//
// 为什么放在 settlement-auditor 而不是 stocktake:
// **发消息的是本服务**(stepStocktake 发 taskcard 命令),
// 配置跟着发送方走,少一次跨服务往返;
// stocktake 只负责建盘点单,不关心通知发给谁。

// ErrNotifyGroupNotFound —— 该门店还没设置推送群(不是错误,是空态)。
var ErrNotifyGroupNotFound = errors.New("service: 该门店尚未设置盘点计划推送群")

// GetStocktakeNotifyGroup 读某门店的推送群配置。
//
// 未配置 ⇒ 返回 (nil, ErrNotifyGroupNotFound):调用方要能区分
// "没配"(静默跳过推送)与"读失败"(流水线报错)。
func (s *Service) GetStocktakeNotifyGroup(ctx context.Context, branchID string) (*model.StocktakeNotifyGroup, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var row model.StocktakeNotifyGroup
	err := s.db.WithContext(ctx).First(&row, "branch_id = ?", branchID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotifyGroupNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load stocktake notify group: %w", err)
	}
	return &row, nil
}

// SetStocktakeNotifyGroupInput 是设置推送群的入参。
type SetStocktakeNotifyGroupInput struct {
	// ChatID / ChatName 是目标群。
	ChatID   string
	ChatName string
	// AppID / AppName 是该群所属的自建应用(企微要求 chatid 与 token 同应用)。
	AppID   string
	AppName string
}

// SetStocktakeNotifyGroup upsert 某门店的推送群配置。
//
// ⚠️ **必须带 app_id**:企微的群必须由"持有当前 Secret 的那个自建应用"创建,
// 用别的应用的 token 发会被拒(301002 一族)⇒ 只存 chat_id 的配置发不出去。
func (s *Service) SetStocktakeNotifyGroup(ctx context.Context, branchID string, in SetStocktakeNotifyGroupInput, operatorID string) (*model.StocktakeNotifyGroup, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	chatID := strings.TrimSpace(in.ChatID)
	appID := strings.TrimSpace(in.AppID)
	if chatID == "" || appID == "" {
		return nil, fmt.Errorf("%w: chat_id 与 app_id 必填(群必须指明所属自建应用)", ErrInvalidInput)
	}
	row := &model.StocktakeNotifyGroup{
		BranchID: branchID,
		ChatID:   chatID,
		ChatName: strings.TrimSpace(in.ChatName),
		AppID:    appID,
		AppName:  strings.TrimSpace(in.AppName),
		UpdatedBy: strings.TrimSpace(operatorID),
		UpdatedAt: s.now(),
	}
	// upsert:主键冲突时整行覆盖。
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		return nil, fmt.Errorf("save stocktake notify group: %w", err)
	}
	return row, nil
}

// DeleteStocktakeNotifyGroup 清掉某门店的推送群配置(回到"不推送"）。
func (s *Service) DeleteStocktakeNotifyGroup(ctx context.Context, branchID string) error {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Delete(&model.StocktakeNotifyGroup{}).Error; err != nil {
		return fmt.Errorf("delete stocktake notify group: %w", err)
	}
	return nil
}

// notifyGroupOf 是 stepStocktake 用的便捷读取:拿不到配置时返回 nil(不推送,不报错)。
func (s *Service) notifyGroupOf(ctx context.Context, branchID string) *model.StocktakeNotifyGroup {
	row, err := s.GetStocktakeNotifyGroup(ctx, branchID)
	if err == nil {
		return row
	}
	if !errors.Is(err, ErrNotifyGroupNotFound) {
		s.logger.Warn("settlement-auditor: 读盘点计划推送群失败,跳过群推送",
			"branch_id", branchID, "err", err)
	}
	return nil
}

var _ = time.Now
