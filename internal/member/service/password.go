// Package service / password.go —— 会员口令哈希(pbkdf2-sha256)。
//
// # 为什么用标准库的 crypto/pbkdf2
//
// 会员口令是**弱口令**(4~6 位数字居多),必须用**慢哈希 + 随机盐**。
//
// ⚠️ 不用 bcrypt:仓里没有 golang.org/x/crypto/bcrypt 的直接依赖,
//
//	加一个依赖要动 go.mod(而 go.mod 是**全仓共享文件**,并行开发时冲突成本高)。
//	⇒ 用 Go 1.24+ 标准库的 `crypto/pbkdf2`(HMAC-SHA256 变体,RFC 8018)。
//
// # 存储格式
//
//	pbkdf2-sha256$<iterations>$<base64 salt>$<base64 derived key>
//
// ⚠️ 带 iterations 与盐 ⇒ 以后调��迭代次数**不需要**迁移老哈希
//
//	(老哈希自带它当时的迭代次数,校验时按它自己带的值算)。
//
// ⚠️ 校验用**恒定时间比较**(subtle.ConstantTimeCompare)——
//
//	普通 `==` 会在第一个不同的字节处短路 ⇒ 逐字节爆破。
package service

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// passwordAlgorithm 是算法标识(哈希串的第 0 段)。
	passwordAlgorithm = "pbkdf2-sha256"
	// passwordIterations 是迭代次数。
	//
	// ⚠️ 210000 是 OWASP 对 PBKDF2-HMAC-SHA256 的推荐下限(2023)。
	passwordIterations = 210000
	// passwordSaltLen 是盐长度(字节)。
	passwordSaltLen = 16
	// passwordKeyLen 是派生密钥长度(字节)。
	passwordKeyLen = 32
)

// ErrPasswordFormat 是哈希串格式不对。
var ErrPasswordFormat = errors.New("member: 口令哈希格式非法")

// HashPassword 生成会员口令哈希。
func HashPassword(password string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("%w: 口令不能为空", ErrPasswordFormat)
	}
	salt := make([]byte, passwordSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("member: 生成盐失败: %w", err)
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyLen)
	if err != nil {
		return "", fmt.Errorf("member: 派生密钥失败: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s",
		passwordAlgorithm,
		passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword 校验口令。
//
// ⚠️ 哈希为空(还没设过口令)⇒ 返回 false,**不是** error:
//
//	"没设口令" 与 "口令错" 对调用方是同一件事(都要先设置)。
func VerifyPassword(password, hash string) (bool, error) {
	if strings.TrimSpace(password) == "" || strings.TrimSpace(hash) == "" {
		return false, nil
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != passwordAlgorithm {
		return false, nil
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false, nil
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, nil
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, nil
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, nil
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
