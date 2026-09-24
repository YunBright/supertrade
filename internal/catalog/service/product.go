// Package service / product.go —— 商品服务入口(本期仅占位)。
//
// 本期:products 表已建(见 model/product.go::Product),handler 暂未调用本
// service(由 cubeclient 兜底);后续 Phase 实装 CRUD 时复用本 service 即可。
//
// 当前唯一职责:暴露构造函数 NewProductService,保持 service 层完整;
// CRUD 方法预留 entry,本期不实现。
package service

import (
	"errors"

	"gorm.io/gorm"
)

// ProductService 是 catalog 的商品服务(本期 entry)。
type ProductService struct {
	db *gorm.DB
}

// NewProductService 构造 ProductService。
func NewProductService(db *gorm.DB) *ProductService {
	return &ProductService{db: db}
}

// ErrNotImplemented 表示商品 CRUD 尚未实装(handler 应返 501)。
var ErrNotImplemented = errors.New("catalog: product CRUD 暂未实装,待下一 Phase")