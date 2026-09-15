// Package auth 管理 auths/ 目录下的 CatPaw 凭证文件 catpaw-{uid}.json。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenLifetime 个人版 passport token 有效时长（72h，逆向自 ACCESS_TOKEN_LIFETIME_MS）。
const TokenLifetime = 72 * time.Hour

// Auth 单个账号凭证。
type Auth struct {
	UID         string `json:"uid"`
	UserName    string `json:"user_name"`
	AccessToken string `json:"access_token"`
	UpdatedAt   int64  `json:"updated_at"` // Unix 秒

	path string
	mu   sync.Mutex
}

// New 构造 Auth（登录落盘用）。
func New(uid, userName, token string) *Auth {
	return &Auth{
		UID:         strings.TrimSpace(uid),
		UserName:    userName,
		AccessToken: strings.TrimSpace(token),
		UpdatedAt:   time.Now().Unix(),
	}
}

// FileName 返回安全的 auth 文件名。UID 永远不会直接成为路径片段。
func (a *Auth) FileName() string {
	return fmt.Sprintf("catpaw-%s.json", safeID(a.UID))
}

func safeID(uid string) string {
	uid = strings.TrimSpace(uid)
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 128 {
			break
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" || out == "." || out == ".." {
		return "unknown"
	}
	return out
}

// Token 返回 access token（并发安全快照）。
func (a *Auth) Token() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// Age 返回 token 已存在时长。
func (a *Auth) Age() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UpdatedAt == 0 {
		return 0
	}
	return time.Since(time.Unix(a.UpdatedAt, 0))
}

// ExpiresAt 返回 token 预计过期时间（UpdatedAt + 72h；UpdatedAt 缺失时返回零值）。
func (a *Auth) ExpiresAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UpdatedAt == 0 {
		return time.Time{}
	}
	return time.Unix(a.UpdatedAt, 0).Add(TokenLifetime)
}

// Remaining 返回 token 剩余有效期（<=0 表示已过期；UpdatedAt 缺失时返回 TokenLifetime）。
func (a *Auth) Remaining() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UpdatedAt == 0 {
		return TokenLifetime
	}
	return time.Until(time.Unix(a.UpdatedAt, 0).Add(TokenLifetime))
}

// Expired 报告 token 是否已过期（按 72h 推算，不等上游 401）。
func (a *Auth) Expired() bool {
	return a == nil || a.Remaining() <= 0
}

// ExpiringSoon 报告 token 是否将在 within 内过期。
func (a *Auth) ExpiringSoon(within time.Duration) bool {
	r := a.Remaining()
	return r > 0 && r <= within
}

// Save 原子写回 auth 文件（0600）。
func (a *Auth) Save() error {
	if a == nil {
		return fmt.Errorf("auth: nil credential")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validateLocked(a); err != nil {
		return err
	}
	return saveLocked(a.path, a)
}

// SetPath 设置文件路径（LoadDir 时内部调用）。
func (a *Auth) SetPath(p string) {
	if a != nil {
		a.path = p
	}
}

// LoadDir 加载 auths 目录下全部 catpaw-*.json。损坏/缺 UID 的 credential fail closed，
// 避免静默生成空账号或让同一 UID 出现两份可轮转状态。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "catpaw-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var a Auth
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		a.UID = strings.TrimSpace(a.UID)
		a.AccessToken = strings.TrimSpace(a.AccessToken)
		if a.AccessToken == "" {
			continue
		}
		if a.UID == "" {
			return nil, fmt.Errorf("parse %s: missing uid", e.Name())
		}
		if previous, ok := seen[a.UID]; ok {
			return nil, fmt.Errorf("duplicate uid %q in %s and %s", a.UID, previous, e.Name())
		}
		seen[a.UID] = e.Name()
		a.path = p
		out = append(out, &a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// SaveNew 把新账号写入 auths 目录。
func SaveNew(dir string, a *Auth) error {
	if a == nil {
		return fmt.Errorf("auth: nil credential")
	}
	a.mu.Lock()
	if err := validateLocked(a); err != nil {
		a.mu.Unlock()
		return err
	}
	filename := a.FileName()
	a.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	a.path = filepath.Join(dir, filename)
	return a.Save()
}

func validateLocked(a *Auth) error {
	if strings.TrimSpace(a.UID) == "" {
		return fmt.Errorf("auth: empty uid")
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("auth: empty access token")
	}
	return nil
}

func saveLocked(path string, v any) error {
	if path == "" {
		return fmt.Errorf("auth: empty path")
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
