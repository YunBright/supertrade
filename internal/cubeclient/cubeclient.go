// Package cubeclient 提供本系统 → cube sixun 的客户端接口。
//
// 生产实现只有 DaprCubeClient 一个(经 dapr/go-sdk 的 dapr.Client 调用 cube)。
// 没有 mock / 内存实现,也没有运行期开关 ——
// 曾存在的 CUBE_CLIENT_MODE=memory 在 2026-10-08 移除,原因见
// internal/cubeclient/cubeclientfake 包注释(那三个"生产读假数据"的服务)。
//
// 测试请用 internal/cubeclient/cubeclientfake。
//
// DTO 定义见本包 dto.go。
package cubeclient

import "context"

// Client 是 cube sixun 的客户端抽象。
//
// 生产唯一实现是 DaprCubeClient。
type Client interface {
	GetProduct(ctx context.Context, productID string) (*ProductDTO, error)
	GetStock(ctx context.Context, branchID, productID string) (*StockSnapshotDTO, error)

	// SearchProductsByBarcode 按 barcode 查商品(合并该门店 stock),
	// 用于扫商品接口(REQUIREMENTS §2.1.4.1)。
	//
	// barcode 长度策略:
	//   - <5 位:返回空(防全表扫)
	//   - 5~12 位:后缀匹配(逐条 endsWith)
	//   - ≥13 位:精确匹配
	//
	// limit 在 5~12 位时尊重用户;≥13 位自动取 1。
	// 返回的 ProductWithStock.Stock 可能 nil(该门店无 stock 数据)。
	SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) ([]ProductWithStock, error)

	// SearchSuppliers 按 name 模糊查供应商(用于品类 / 供应商下拉)。
	SearchSuppliers(ctx context.Context, query string, limit int) ([]SupplierDTO, error)

	// GetActivePromotion 查某商品**当前生效**的促销(取最低促销价那一条)。
	//
	// itemNo 用商品条码 / product id(cube product.id = 思迅 item_no,见
	// SearchProductsByBarcode 的领域事实)。onDate 是判定基准日,格式
	// yyyy-mm-dd;留空表示今天。
	//
	// **无生效促销返回 (nil, nil)** —— 这是绝大多数商品的正常状态,不是错误。
	// 调用方因此不能靠 err != nil 判断"有没有促销"。
	GetActivePromotion(ctx context.Context, itemNo, onDate string) (*PromotionDTO, error)

	// BatchCurrentPrices **批量**查一批商品的当前售价(零售价 + 促销价)。
	//
	// 一次给一批(几十个 SKU)而不是逐个查 ⇒ 两次 cube 查询(product + promotion)
	// 就够,不会把审计拖成几十秒。
	//
	// 返回 map[product_id]ProductPriceQuote:
	//   - **没维护售价**的商品不在返回里(调用方要能区分"没维护价"与"0 元");
	//   - 没有生效促销 ⇒ PromoPriceYuan = nil。
	//
	// onDate 是判定基准日("yyyy-mm-dd");留空 = 今天。
	BatchCurrentPrices(ctx context.Context, productIDs []string, onDate string) (map[string]ProductPriceQuote, error)

	// ---- 商品选择控件(ProductPicker)用的只读查询 ----
	//
	// 这几个方法是一组:每个方法发**固定条数**的 cube 查询,分类名 / 供应商名
	// 由调用方在 Go 侧 join(cube 的 join 引擎只支持 live 模型之间的 join,
	// product / category / supplier 都是 duck 模型,跨 model join 直接 400)。
	//
	// 每个方法实际发出去的 cube 查询见 catalog.go 里同名方法的注释。

	// ListProducts 分页查商品(不过滤关键词)。
	//
	// 返回的 Total 是"过滤后的总条数"(来自 cube 响应顶层的 total,
	// 需要查询带 total:true),不是本页条数。
	ListProducts(ctx context.Context, q ProductQuery) (*ProductPageDTO, error)

	// SearchProducts 按关键词查商品(命中 product.id 或 product.name)。
	//
	// 关键词为空时等价于 ListProducts。
	//
	// ⚠️ 商品**编码**与**名称**的匹配都是**大小写无关**的
	// (cube 的 containsCI 算子:列侧 LOWER、模式侧编译期 lower,值仍走参数绑定)。
	// cube 的 contains 本身大小写敏感(`RTRIM(col) LIKE '%v%'`),
	// 所以这里不能退回用 contains —— 用户打出小写 "face" 就搜不到 "洁柔Face"。
	SearchProducts(ctx context.Context, q ProductQuery) (*ProductPageDTO, error)

	// ListCategories 取分类字典(全量:id / name / parent_id)。
	//
	// parent_id 是分类**树**的边(思迅 t_bd_item_cls.cls_parent),
	// 上层(supertrade catalog)用它把平铺的分类拼成树。
	ListCategories(ctx context.Context) (*CategoryListDTO, error)

	// ListSuppliers 取供应商字典(全量:id / name / type / cust_flag / status / supply_way)。
	//
	// 本期不过滤 cust_flag / type(picker 回答的是"这家店在卖什么")。
	ListSuppliers(ctx context.Context) (*SupplierListDTO, error)

	// ListSaleWays 取「销售方式」字典(思迅 t_bd_base_code,type_no='SW':id / name)。
	//
	// 供应商两级结构的**第一层**。cube 侧取数时已按 type_no='SW' 过滤,
	// 所以拿到的就是销售方式那几行(A 购销 / B 代销 / C 联营 / …)。
	//
	// ⚠️ 字典查不到**不算失败**:那只是"没有类别名"这一层信息缺失,
	// 供应商本身还在(上层降级成"全部进未分类")。
	ListSaleWays(ctx context.Context) (*SaleWayListDTO, error)

	// CountProductsByCategory 返回「每个分类下的商品数」。
	//
	// 与 ListProducts(category_id=X) 的 Total 必然相等(同一个 product 模型、
	// 同一套过滤口径),这是"侧栏 count == 列表 total"这条契约的基础。
	CountProductsByCategory(ctx context.Context) (map[string]int, error)

	// CountProductsBySupplier 返回「每个供应商下的商品数」。
	//
	// 与 ListProducts(supplier_id=X) 的 Total 必然相等。
	CountProductsBySupplier(ctx context.Context) (map[string]int, error)
}
