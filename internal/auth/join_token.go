// Package auth 节点注册令牌（JOIN_TOKEN）校验。
// 凭据只从环境变量或密钥服务读取；令牌支持有效期与一次性使用（内存态撤销表）。
package auth

import (
	"crypto/subtle"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNoToken   = errors.New("join token not configured")
	ErrBadToken  = errors.New("invalid join token")
	ErrExpired   = errors.New("join token expired")
	ErrSingleUse = errors.New("join token already used")
)

// JoinTokenConfig 主控侧注册令牌配置。
type JoinTokenConfig struct {
	Token     string     // 主令牌（来自 CONTROL_JOIN_TOKEN 环境变量）
	ExpiresAt *time.Time // 可选有效期
	SingleUse bool       // 是否一次性（首个节点注册后失效）
}

// LoadFromEnv 从环境变量加载：CONTROL_JOIN_TOKEN（必填）、CONTROL_JOIN_TOKEN_TTL_HOURS（可选）、CONTROL_JOIN_TOKEN_SINGLE_USE（可选）。
func LoadFromEnv() (*JoinTokenConfig, error) {
	tok := os.Getenv("CONTROL_JOIN_TOKEN")
	if tok == "" {
		return nil, ErrNoToken
	}
	cfg := &JoinTokenConfig{Token: tok}
	if v := os.Getenv("CONTROL_JOIN_TOKEN_TTL_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			t := time.Now().Add(time.Duration(h) * time.Hour)
			cfg.ExpiresAt = &t
		}
	}
	cfg.SingleUse = strings.EqualFold(os.Getenv("CONTROL_JOIN_TOKEN_SINGLE_USE"), "true")
	return cfg, nil
}

// Validator 并发安全的令牌校验器。
type Validator struct {
	mu   sync.Mutex
	cfg  *JoinTokenConfig
	used bool
}

// NewValidator 构造。
func NewValidator(cfg *JoinTokenConfig) *Validator { return &Validator{cfg: cfg} }

// Validate 常量时间比较 + 有效期 + 一次性检查。
func (v *Validator) Validate(token string) error {
	if v.cfg == nil || v.cfg.Token == "" {
		return ErrNoToken
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(v.cfg.Token)) != 1 {
		return ErrBadToken
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cfg.ExpiresAt != nil && time.Now().After(*v.cfg.ExpiresAt) {
		return ErrExpired
	}
	if v.cfg.SingleUse && v.used {
		return ErrSingleUse
	}
	v.used = true
	return nil
}
