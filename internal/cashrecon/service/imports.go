// Package service / imports.go —— 银行 / 渠道流水导入（FR-02）。
//
// # 幂等
//
//	dedupe_key = sha256(channel|value_date|amount|fee|external_id|raw_line)
//
// 唯一索引 `ux_bank_dedupe (branch_id, dedupe_key)` + `ON CONFLICT DO NOTHING`
// ⇒ 同一份文件重复导入只会新增 0 行（回执里 skipped 计数），**不报错**。
//
// # 为什么整批原子
//
// 半批入库最坏：出纳看到"导入成功 30 行"，实际只有 3 行进了库，
// 而剩下的 27 行永远补不回来（他不知道缺了哪几行）。⇒ 解析失败的行
// 逐行跳过并计数，**合法行全部入库、失败行全部列出**；冻结区间则整批 409。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// ImportBankRow 是一条待导入流水（JSON 与 CSV 共用）。
type ImportBankRow struct {
	Channel       string          `json:"channel"`
	ValueDate     string          `json:"value_date"`
	Amount        decimal.Decimal `json:"amount"`
	Fee           decimal.Decimal `json:"fee"`
	SettleBatchNo string          `json:"settle_batch_no"`
	ExternalID    string          `json:"external_id"`
	RawLine       string          `json:"raw_line"`
}

// ImportBankInput 是 `POST /imports/bank` 的入参。
type ImportBankInput struct {
	BranchID string
	Kind     string
	FileName string
	CSV      string
	Rows     []ImportBankRow
	Actor    string
}

// ImportBankResult 是导入回执。
type ImportBankResult struct {
	Import model.ReconImport `json:"import"`
	// Inserted 是真正新增的行数。
	Inserted int `json:"inserted"`
	// Skipped 是重复（已被唯一键挡掉）的行数。
	Skipped int `json:"skipped"`
	// Failed 是解析失败的行数（file 里有、但入库不了）。
	Failed int `json:"failed"`
	// Errors 是失败样例（最多 5 条）。
	Errors []string `json:"errors,omitempty"`
}

// bankDedupeKey 生成导入幂等键（口径见 model.ReconBankFlow 注释）。
func bankDedupeKey(channel, valueDate, amount, fee, externalID, rawLine string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		channel, valueDate, amount, fee, externalID, rawLine,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

// ImportBankFlows 导入银行 / 渠道流水。
func (s *Service) ImportBankFlows(ctx context.Context, in ImportBankInput) (ImportBankResult, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.Actor = strings.TrimSpace(in.Actor)
	in.FileName = strings.TrimSpace(in.FileName)
	if in.BranchID == "" || in.Actor == "" {
		return ImportBankResult{}, model.ErrInvalidInput
	}
	if in.Kind == "" {
		in.Kind = model.ImportKindBank
	}
	if in.Kind != model.ImportKindBank && in.Kind != model.ImportKindChannel {
		return ImportBankResult{}, fmt.Errorf("%w: kind 必须是 bank/channel", model.ErrInvalidInput)
	}
	if len(in.Rows) == 0 && strings.TrimSpace(in.CSV) == "" {
		return ImportBankResult{}, fmt.Errorf("%w: 缺少 rows 或 csv", model.ErrInvalidInput)
	}
	if len(in.FileName) > 255 {
		in.FileName = in.FileName[:255]
	}

	parsed, parseErrs, err := s.parseImportRows(in)
	if err != nil {
		return ImportBankResult{}, err
	}

	// 规整 + 校验：非法行单独计数，合法行全部入库。
	failed := len(parseErrs)
	errSamples := append([]string{}, parseErrs...)
	valid := make([]ImportBankRow, 0, len(parsed))
	dates := map[string]bool{}
	for _, r := range parsed {
		day, ferr := parseDayFlexible(r.ValueDate)
		if ferr != nil {
			failed++
			if len(errSamples) < 5 {
				errSamples = append(errSamples, fmt.Sprintf("%s: value_date=%q 非法", r.Channel, r.ValueDate))
			}
			continue
		}
		r.Channel = strings.TrimSpace(r.Channel)
		if r.Channel == "" {
			failed++
			if len(errSamples) < 5 {
				errSamples = append(errSamples, fmt.Sprintf("%s: channel 为空", day.Format("2006-01-02")))
			}
			continue
		}
		r.ValueDate = day.Format("2006-01-02")
		r.SettleBatchNo = strings.TrimSpace(r.SettleBatchNo)
		r.ExternalID = strings.TrimSpace(r.ExternalID)
		r.RawLine = strings.TrimSpace(r.RawLine)
		if len(r.RawLine) > 1024 {
			r.RawLine = r.RawLine[:1024]
		}
		dates[r.ValueDate] = true
		valid = append(valid, r)
	}

	// 冻结区间整批拒绝（不部分成功）。
	for d := range dates {
		day, _ := parseDay(d)
		if err := s.assertNotLocked(ctx, in.BranchID, day, day); err != nil {
			return ImportBankResult{}, err
		}
	}

	now := s.now()
	ledger := model.ReconImport{
		ID:        newID(),
		BranchID:  in.BranchID,
		Kind:      in.Kind,
		FileName:  in.FileName,
		RowTotal:  len(parsed) + len(parseErrs),
		RowFailed: failed,
		CreatedBy: in.Actor,
		CreatedAt: now,
	}
	if len(errSamples) > 5 {
		errSamples = errSamples[:5]
	}
	ledger.ErrorSample = truncate(strings.Join(errSamples, "; "), 1024)

	inserted := 0
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&ledger).Error; err != nil {
			return err
		}
		if len(valid) == 0 {
			return nil
		}
		flows := make([]model.ReconBankFlow, 0, len(valid))
		for _, r := range valid {
			flows = append(flows, model.ReconBankFlow{
				ID:            newID(),
				ImportID:      ledger.ID,
				BranchID:      in.BranchID,
				Channel:       r.Channel,
				ValueDate:     mustParseDay(r.ValueDate),
				Amount:        r.Amount,
				Fee:           r.Fee,
				SettleBatchNo: r.SettleBatchNo,
				ExternalID:    r.ExternalID,
				RawLine:       r.RawLine,
				DedupeKey: bankDedupeKey(r.Channel, r.ValueDate, r.Amount.String(),
					r.Fee.String(), r.ExternalID, r.RawLine),
				CreatedAt: now,
			})
		}
		res := tx.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "branch_id"}, {Name: "dedupe_key"}},
				DoNothing: true,
			}).
			CreateInBatches(flows, 500)
		if res.Error != nil {
			return res.Error
		}
		inserted = int(res.RowsAffected)
		ledger.RowOK = inserted
		return tx.WithContext(ctx).Model(&model.ReconImport{}).
			Where("id = ?", ledger.ID).
			Updates(map[string]any{"row_ok": inserted, "row_failed": failed, "row_total": ledger.RowTotal}).Error
	})
	if err != nil {
		return ImportBankResult{}, err
	}

	res := ImportBankResult{
		Import:   ledger,
		Inserted: inserted,
		Skipped:  len(valid) - inserted,
		Failed:   failed,
	}
	if len(errSamples) > 0 {
		res.Errors = errSamples
	}
	return res, nil
}

// parseImportRows 从 JSON rows 或 CSV 文本解析出流水行。
func (s *Service) parseImportRows(in ImportBankInput) ([]ImportBankRow, []string, error) {
	if len(in.Rows) > 0 {
		return in.Rows, nil, nil
	}
	return parseBankCSV(in.CSV)
}

// parseBankCSV 解析带表头的 CSV（表头支持中英文别名）。
func parseBankCSV(text string) ([]ImportBankRow, []string, error) {
	rd := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\ufeff")))
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true

	header, err := rd.Read()
	if err == io.EOF {
		return nil, nil, fmt.Errorf("%w: CSV 为空", model.ErrInvalidInput)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: CSV 表头解析失败: %v", model.ErrInvalidInput, err)
	}
	col := make(map[string]int, len(header))
	for i, h := range header {
		col[normalizeHeader(h)] = i
	}
	get := func(key string) (int, bool) {
		i, ok := col[key]
		return i, ok
	}
	idxChannel, okChannel := get("channel")
	idxDate, okDate := get("value_date")
	idxAmount, okAmount := get("amount")
	if !okChannel || !okDate || !okAmount {
		return nil, nil, fmt.Errorf("%w: CSV 表头必须含 channel/value_date/amount（支持中文别名：渠道/到账日期/金额）", model.ErrInvalidInput)
	}
	// 可选列**必须**用 -1 表示缺失：get() 缺列时返回 (0,false)，
	// 只判 ">= 0" 会把第 0 列（channel）当成 fee/external_id 等。
	optional := func(key string) int {
		if i, ok := get(key); ok {
			return i
		}
		return -1
	}
	idxFee := optional("fee")
	idxBatch := optional("settle_batch_no")
	idxExternal := optional("external_id")
	idxRaw := optional("raw_line")

	cell := func(rec []string, i int) string {
		if i < 0 || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []ImportBankRow
	var errs []string
	for lineNo := 2; ; lineNo++ {
		rec, err := rd.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 行: CSV 解析失败: %v", lineNo, err))
			continue
		}
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue // 跳过空行
		}
		amount, aerr := parseAmountCell(cell(rec, idxAmount))
		if aerr != nil {
			errs = append(errs, fmt.Sprintf("第 %d 行: amount=%q 非法", lineNo, cell(rec, idxAmount)))
			continue
		}
		fee := decimal.Zero
		if idxFee >= 0 {
			if v, e := parseAmountCell(cell(rec, idxFee)); e == nil {
				fee = v
			} else if cell(rec, idxFee) != "" {
				errs = append(errs, fmt.Sprintf("第 %d 行: fee=%q 非法", lineNo, cell(rec, idxFee)))
				continue
			}
		}
		raw := cell(rec, idxRaw)
		if raw == "" {
			raw = strings.Join(rec, ",")
		}
		rows = append(rows, ImportBankRow{
			Channel:       cell(rec, idxChannel),
			ValueDate:     cell(rec, idxDate),
			Amount:        amount,
			Fee:           fee,
			SettleBatchNo: cell(rec, idxBatch),
			ExternalID:    cell(rec, idxExternal),
			RawLine:       raw,
		})
	}
	return rows, errs, nil
}

// normalizeHeader 归一化 CSV 表头（小写、去空白/下划线/连字符）。
func normalizeHeader(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
	h = strings.ToLower(h)
	h = strings.ReplaceAll(h, " ", "")
	h = strings.ReplaceAll(h, "_", "")
	h = strings.ReplaceAll(h, "-", "")
	switch h {
	case "channel", "渠道", "渠道名称", "paychannel":
		return "channel"
	case "valuedate", "value_date", "date", "到账日期", "交易日期", "交易时间", "入账日期":
		return "value_date"
	case "amount", "金额", "到账金额", "交易金额":
		return "amount"
	case "fee", "手续费", "服务费":
		return "fee"
	case "settlebatch", "批次号", "结算批次", "结算批次号":
		return "settle_batch_no"
	case "externalid", "流水号", "交易流水号", "订单号":
		return "external_id"
	case "rawline", "备注", "摘要", "说明":
		return "raw_line"
	}
	return h
}

// parseAmountCell 解析金额单元格（容忍千分位、货币符号、空白）。
func parseAmountCell(s string) (decimal.Decimal, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, "¥", "")
	s = strings.ReplaceAll(s, "￥", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal.Zero, nil
	}
	return decimal.NewFromString(s)
}

// parseDayFlexible 解析多种日期写法（2026-10-30 / 2026/10/30 / 20261030 / 带时刻）。
func parseDayFlexible(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("空日期")
	}
	if len(v) > 10 {
		// 带时刻（"2026-10-30 12:00:00" / "2026/10/30 12:00"）⇒ 只取日期部分。
		v = strings.SplitN(v, " ", 2)[0]
	}
	v = strings.ReplaceAll(v, "/", "-")
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return dayOnly(t), nil
		}
	}
	return time.Time{}, fmt.Errorf("日期 %q 不是 YYYY-MM-DD", v)
}

// mustParseDay 是解析成功后的内部转换（调用点已校验过）。
func mustParseDay(v string) time.Time {
	t, _ := parseDay(v)
	return t
}

// truncate 截断字符串到 max 字节（按 rune 安全截断）。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && len(string(r)) > max {
		r = r[:len(r)-1]
	}
	return string(r)
}

// ListImports 返回门店的导入批次（倒序）。
func (s *Service) ListImports(ctx context.Context, branchID string, limit int) ([]model.ReconImport, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.ReconImport
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("created_at DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetImport 按 id 取导入批次（必须属于该门店）。
func (s *Service) GetImport(ctx context.Context, branchID, id string) (model.ReconImport, error) {
	if branchID == "" || id == "" {
		return model.ReconImport{}, model.ErrInvalidInput
	}
	var row model.ReconImport
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconImport{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconImport{}, err
	}
	return row, nil
}
