// internal/stocktake/service/scope.go —— 排除域(本服务不盘的商品域)。
//
// # 要解决的问题
//
// 猪肉由 fresh-meat 域盘(按部位),通用盘点**不许**再盘一次 ——
// 同一批猪肉被两条流程各盘一次 ⇒ 两个数、没人说得清哪个对,而失败是**静默**的。
//
// # 判定口径
//
// 排除域是**按门店**配置的「cube 商品分类(cube product.category_id = 思迅 item_clsno)」。
// 分类 id 每个门店各编各的(佳乐 01 的猪肉是 1201,富百家 00 的是 010303),
// 所以查表时**必须带 branch_id** —— 否则 A 店的配置会误伤 B 店。
//
// # fail open(判定不出来就不拦)
//
// 排除域是**守卫**,不是业务主流程。cube 挂了 / 分类取不到 / 配置读不出来时,
// **一律放行**,只记日志:
// 让"守卫依赖不可用"变成"整个通用盘点不能用",是把一个小故障放大成大故障。
// 真正要拦的只有"明确查到商品落在排除域里"这一种情况。
package service

import (
	"context"
	"fmt"

	"github.com/YunBright/supertrade/internal/stocktake/model"
)

// defaultExcludedScopeHint 是配置行没写 note 时的兜底提示。
//
// 宁可给一句不够具体的提示,也不要给一句看不懂的 ——
// 拒绝的理由必须是"**你该去哪个流程盘**",而不是"内部规则不让你盘"。
const defaultExcludedScopeHint = "该商品由其它业务域盘点,请到对应流程盘点"

// excludedScopeSet 是某门店的排除域集合(载入一次,批量判定用)。
type excludedScopeSet struct {
	// products: cube product.id(= 思迅 item_no) → 提示文案(note)。
	//
	// ⚠️ 猪肉走**货号**口径:类目口径会连带挡掉猪肉类目里的
	// 羊肉/牛肉/预制菜(佳乐 1201「猪肉部」就是混的)⇒ 过度排除。
	products map[string]string
	// categories: cube category_id → 提示文案(note)。
	//
	// 保留这个机制(整类排除仍然有用,比如"整个蔬果类归 fresh-produce"),
	// 但**猪肉不用它**。
	categories map[string]string
}

// loadExcludedScopes 读某门店的排除域配置。
//
// 只取 scope_kind ∈ {product, category} 的行。
//
// branchID 为空 ⇒ 排除集为空(单店上下文缺失时不猜,也不拦)。
//
// ⚠️ **不做缓存**:这张表是配置,几行而已,而缓存会引入"改了配置不生效"的
// 陈旧窗口 —— 那正是"守卫静默失效"的一种。查询走主键/唯一索引,成本可忽略。
func (s *Service) loadExcludedScopes(ctx context.Context, branchID string) (*excludedScopeSet, error) {
	set := &excludedScopeSet{
		products:   map[string]string{},
		categories: map[string]string{},
	}
	if branchID == "" {
		return set, nil
	}
	var rows []model.StocktakeExcludedScope
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND scope_kind IN ?", branchID,
			[]string{model.ScopeKindProduct, model.ScopeKindCategory}).
		Find(&rows).Error; err != nil {
		// ⚠️ 配置读不出来 ≠ 商品被排除。这里**返回错误**(调用方按 fail open
		// 处理并记日志),但绝不把它翻译成"这个商品不许盘" ——
		// 那是凭空捏造的业务结论。
		return nil, fmt.Errorf("load excluded scopes: %w", err)
	}
	for _, r := range rows {
		if r.ScopeValue == "" {
			continue
		}
		switch r.ScopeKind {
		case model.ScopeKindProduct:
			set.products[r.ScopeValue] = r.Note
		case model.ScopeKindCategory:
			set.categories[r.ScopeValue] = r.Note
		}
	}
	return set, nil
}

// excludeError 判定商品是否落在排除域里;是则返回一个**可操作**的错误。
//
// 判定顺序:**货号优先** ——
//   - 货号在排除集里 ⇒ 排除(这是猪肉的判定口径,精确);
//   - 否则看类目(整类排除,例如整个蔬果类)。
//
// 任何一边都判不出来(取不到货号/类目)⇒ fail open,返回 nil(放行)。
//
// 错误消息里带 note(配置行里的一句人话),让操作员知道**该去哪个流程盘**,
// 而不是只看到一句"不允许"。
func (set *excludedScopeSet) excludeError(branchID, categoryID, productID string) error {
	if productID != "" {
		if note, ok := set.products[productID]; ok {
			return fmt.Errorf("%w: %s (product_id=%s category_id=%s branch_id=%s)",
				ErrProductScopeExcluded, hintOf(note), productID, categoryID, branchID)
		}
	}
	if categoryID == "" {
		return nil
	}
	note, ok := set.categories[categoryID]
	if !ok {
		return nil
	}
	return fmt.Errorf("%w: %s (product_id=%s category_id=%s branch_id=%s)",
		ErrProductScopeExcluded, hintOf(note), productID, categoryID, branchID)
}

// hintOf 取提示文案(空则用兜底)。
func hintOf(note string) string {
	if note == "" {
		return defaultExcludedScopeHint
	}
	return note
}

// checkProductExcluded 判定「某门店下的某商品分类」是否被排除(单条路径用)。
//
// DB 查询失败 ⇒ 返回错误(fail open 由调用方处理:记日志后放行)。
func (s *Service) checkProductExcluded(ctx context.Context, branchID, categoryID, productID string) error {
	set, err := s.loadExcludedScopes(ctx, branchID)
	if err != nil {
		return err
	}
	return set.excludeError(branchID, categoryID, productID)
}
