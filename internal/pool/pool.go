// Package pool 管理 CatPaw 账号池：token 校验、余额、冷却、禁用与轮转。
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/upstream"
)

// CoolKind 冷却原因。
type CoolKind int

const (
	CoolSoft CoolKind = iota
	CoolErr
	CoolPlan
)

// Account 一个上游账号。
type Account struct {
	Name         string `json:"name"` // 默认 = UID
	UID          string `json:"uid"`
	UserName     string `json:"user_name"`
	DefaultModel string `json:"default_model"` // auto / 模型名

	Auth   *auth.Auth       `json:"-"`
	Client *upstream.Client `json:"-"`

	mu             sync.Mutex
	balance        int64
	lastBalanceAt  time.Time
	lastValidated  time.Time
	errCount       int
	coolUntil      time.Time
	coolKind       CoolKind
	disabled       bool
	disabledReason string
	lastErr        string
}

// Config 池配置。
type Config struct {
	ErrThreshold int
	ErrCooldown  time.Duration
	SoftCooldown time.Duration
	// UpstreamTimeout 上游 JSON 总超时 / 流式首字节超时，默认 120s。
	UpstreamTimeout time.Duration
}

// Pool 账号池。
type Pool struct {
	cfg      Config
	state    string
	mu       sync.Mutex
	accounts []*Account
}

// New 构建池：每个 auth 对应一个账号。
func New(auths []*auth.Auth, cfg Config, stateFile string) (*Pool, error) {
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = 120 * time.Second
	}
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range auths {
		if a == nil || a.UID == "" || a.Token() == "" {
			continue
		}
		p.accounts = append(p.accounts, newAccount(a, cfg.UpstreamTimeout))
	}
	p.loadState()
	return p, nil
}

func newAccount(a *auth.Auth, timeout time.Duration) *Account {
	return &Account{
		Name:     a.UID,
		UID:      a.UID,
		UserName: a.UserName,
		Auth:     a,
		Client:   upstream.New(timeout),
	}
}

// Accounts 返回账号指针的切片快照；调用方不能通过修改 slice 本身破坏 pool 内部结构。
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Get 按名字取账号（显式 conversation 路由用），找不到返回 nil。
func (p *Pool) Get(name string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// List 状态快照（脱敏；字段对齐 WorkBuddy 面板：nickname/credits/disabled/cooling）。
func (p *Pool) List() []map[string]any {
	accounts := p.Accounts()
	out := make([]map[string]any, 0, len(accounts))
	now := time.Now()
	for _, a := range accounts {
		a.mu.Lock()
		cooling := !a.disabled && now.Before(a.coolUntil)
		until := ""
		if cooling {
			until = a.coolUntil.Format(time.RFC3339)
		}
		remaining := time.Duration(0)
		tokenAge := time.Duration(0)
		if a.Auth != nil {
			remaining = a.Auth.Remaining()
			tokenAge = a.Auth.Age()
		}
		reason := a.disabledReason
		if reason == "" {
			reason = a.lastErr
		}
		status := "healthy"
		if a.disabled {
			status = "disabled"
		} else if cooling {
			status = "cooling"
		}
		out = append(out, map[string]any{
			"name":            a.Name,
			"uid":             a.UID,
			"nickname":        a.UserName,
			"user_name":       a.UserName,
			"default_model":   a.DefaultModel,
			"credits":         a.balance,
			"balance":         a.balance,
			"disabled":        a.disabled,
			"cooling":         cooling,
			"status":          status,
			"until":           until,
			"err_count":       a.errCount,
			"reason":          reason,
			"last_error":      a.lastErr,
			"token_age":       tokenAge.Round(time.Minute).String(),
			"token_remaining": remaining.Round(time.Minute).String(),
			"token_expiring":  remaining > 0 && remaining <= 24*time.Hour,
		})
		a.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["uid"].(string) < out[j]["uid"].(string) })
	return out
}

// AddAccount 动态加入账号；同 UID 替换 token，同时清掉旧 token 遗留的禁用/冷却/错误状态，保留真实余额。
func (p *Pool) AddAccount(a *auth.Auth) *Account {
	if a == nil || a.UID == "" || a.Token() == "" {
		return nil
	}
	p.mu.Lock()
	for _, existing := range p.accounts {
		if existing.UID == a.UID {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.disabled = false
			existing.disabledReason = ""
			existing.lastErr = ""
			existing.errCount = 0
			existing.coolUntil = time.Time{}
			existing.coolKind = CoolSoft
			existing.lastValidated = time.Time{}
			existing.mu.Unlock()
			p.mu.Unlock()
			p.saveState()
			return existing
		}
	}
	acct := newAccount(a, p.cfg.UpstreamTimeout)
	p.accounts = append(p.accounts, acct)
	p.mu.Unlock()
	p.saveState()
	log.Printf("pool account added uid=%s name=%s", a.UID, a.UserName)
	return acct
}

// SyncToDir 用磁盘 auths 全量对齐内存池（重载）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	byUID := map[string]*Account{}
	for _, a := range p.accounts {
		byUID[a.UID] = a
	}
	next := make([]*Account, 0, len(auths))
	for _, a := range auths {
		if a == nil || a.UID == "" || a.Token() == "" {
			continue
		}
		if existing, ok := byUID[a.UID]; ok {
			existing.mu.Lock()
			existing.Auth = a
			existing.UserName = a.UserName
			existing.lastValidated = time.Time{}
			existing.mu.Unlock()
			next = append(next, existing)
			delete(byUID, a.UID)
		} else {
			next = append(next, newAccount(a, p.cfg.UpstreamTimeout))
		}
	}
	p.accounts = next
	p.mu.Unlock()
	p.saveState()
}

// PickExcluding 挑一个健康账号：余额降序，余额相同按 UID 字典序保证稳定。
func (p *Pool) PickExcluding(tried map[string]bool) *Account {
	accounts := p.Accounts()
	now := time.Now()
	type cand struct {
		a       *Account
		balance int64
	}
	var cands []cand
	for _, a := range accounts {
		if tried != nil && tried[a.Name] {
			continue
		}
		a.mu.Lock()
		healthy := !a.disabled && now.After(a.coolUntil)
		b := a.balance
		a.mu.Unlock()
		if healthy {
			cands = append(cands, cand{a, b})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].balance != cands[j].balance {
			return cands[i].balance > cands[j].balance
		}
		return cands[i].a.UID < cands[j].a.UID
	})
	return cands[0].a
}

// Validate 校验 token（带 5 分钟缓存）。只有明确 401/403 才禁用；网络错误/5xx 不得永久打死账号。
func (p *Pool) Validate(a *Account) (bool, error) {
	if a == nil || a.Auth == nil || a.Client == nil {
		return false, fmt.Errorf("invalid account")
	}
	a.mu.Lock()
	if !a.lastValidated.IsZero() && time.Since(a.lastValidated) < 5*time.Minute {
		ok := !a.disabled
		a.mu.Unlock()
		return ok, nil
	}
	a.mu.Unlock()

	ok, err := a.Client.Ping(context.Background(), a.Auth.Token())
	if err != nil {
		// Validation endpoint 暂时不可达时采用 best-effort：不改变持久状态，由真实请求继续判定。
		return false, err
	}
	if !ok {
		a.mu.Lock()
		a.disabled = true
		a.disabledReason = "token invalid (401/403)"
		a.lastErr = "token invalid"
		a.lastValidated = time.Now()
		a.mu.Unlock()
		p.saveState()
		return false, nil
	}

	a.mu.Lock()
	a.lastValidated = time.Now()
	if a.lastErr == "token invalid" {
		a.lastErr = ""
	}
	a.mu.Unlock()
	return true, nil
}

// Cooldown 冷却账号。
func (p *Pool) Cooldown(name string, kind CoolKind, dur time.Duration, reason string) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.coolUntil = time.Now().Add(dur)
		a.coolKind = kind
		a.lastErr = reason
		a.mu.Unlock()
		changed = true
		log.Printf("pool cooldown account=%s kind=%d dur=%s reason=%s", name, kind, dur, reason)
	}
	if changed {
		p.saveState()
	}
}

// NoteError 累计错误，达到阈值后进入临时冷却而不是永久禁用。
func (p *Pool) NoteError(name string, threshold int, cooldown time.Duration) {
	if threshold <= 0 {
		threshold = p.cfg.ErrThreshold
	}
	if cooldown <= 0 {
		cooldown = p.cfg.ErrCooldown
	}
	changed := false
	for _, a := range p.Accounts() {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		a.errCount++
		if a.errCount >= threshold {
			a.coolUntil = time.Now().Add(cooldown)
			a.coolKind = CoolErr
			a.lastErr = "consecutive errors"
		}
		a.mu.Unlock()
		changed = true
	}
	if changed {
		p.saveState()
	}
}

// NoteSuccess 清零错误计数，并持久化恢复状态；不主动清仍有效的 cooldown。
func (p *Pool) NoteSuccess(name string) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			if a.errCount != 0 {
				a.errCount = 0
				changed = true
			}
			a.mu.Unlock()
		}
	}
	if changed {
		p.saveState()
	}
}

// Unfreeze 解冻账号：清除冷却与错误计数（不动 disabled 状态）。
func (p *Pool) Unfreeze(name string) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name != name {
			continue
		}
		a.mu.Lock()
		if !a.coolUntil.IsZero() || a.errCount > 0 || (a.lastErr != "" && !a.disabled) {
			a.coolUntil = time.Time{}
			a.errCount = 0
			a.lastErr = ""
			changed = true
			log.Printf("pool unfreeze account=%s", name)
		}
		a.mu.Unlock()
	}
	if changed {
		p.saveState()
	}
}

// Healthy 查询账号当前可用（未禁用且不在冷却中）。
func (p *Pool) Healthy(name string) bool {
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			healthy := !a.disabled && !time.Now().Before(a.coolUntil)
			a.mu.Unlock()
			return healthy
		}
	}
	return false
}

// IsDisabled 查询账号是否已禁用。
func (p *Pool) IsDisabled(name string) bool {
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			d := a.disabled
			a.mu.Unlock()
			return d
		}
	}
	return false
}

// Disable 禁用账号。
func (p *Pool) Disable(name, reason string) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = true
			a.disabledReason = reason
			a.lastErr = reason
			a.mu.Unlock()
			changed = true
			log.Printf("pool disable account=%s reason=%s", name, reason)
		}
	}
	if changed {
		p.saveState()
	}
}

// Enable 显式启用账号并可设置余额（admin 操作用；token 续期不应借此写入伪余额）。
func (p *Pool) Enable(name string, balance int64) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			a.disabled = false
			a.disabledReason = ""
			a.lastErr = ""
			a.errCount = 0
			a.coolUntil = time.Time{}
			a.balance = balance
			a.lastBalanceAt = time.Now()
			a.mu.Unlock()
			changed = true
			log.Printf("pool enable account=%s", name)
		}
	}
	if changed {
		p.saveState()
	}
}

// ClearCooldown 清冷却。
func (p *Pool) ClearCooldown(name string) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			a.coolUntil = time.Time{}
			a.coolKind = CoolSoft
			a.mu.Unlock()
			changed = true
			log.Printf("pool clear cooldown account=%s", name)
		}
	}
	if changed {
		p.saveState()
	}
}

// Stats 汇总账号状态。
func (p *Pool) Stats() (total, healthy, disabled, cooling int, credits int64) {
	now := time.Now()
	for _, a := range p.Accounts() {
		a.mu.Lock()
		total++
		credits += a.balance
		switch {
		case a.disabled:
			disabled++
		case a.coolUntil.After(now):
			cooling++
		default:
			healthy++
		}
		a.mu.Unlock()
	}
	return
}

// SetBalance 更新余额缓存并立即持久化。
func (p *Pool) SetBalance(name string, balance int64) {
	changed := false
	for _, a := range p.Accounts() {
		if a.Name == name {
			a.mu.Lock()
			a.balance = balance
			a.lastBalanceAt = time.Now()
			a.mu.Unlock()
			changed = true
		}
	}
	if changed {
		p.saveState()
	}
}

type stateEntry struct {
	Name      string    `json:"name"`
	ErrCount  int       `json:"err_count"`
	CoolUntil time.Time `json:"cool_until"`
	Disabled  bool      `json:"disabled"`
	Reason    string    `json:"reason"`
	LastErr   string    `json:"last_error"`
	Balance   int64     `json:"balance"`
	BalanceAt time.Time `json:"balance_at"`
}

// loadState / saveState 持久化冷却与余额。
func (p *Pool) loadState() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var data []stateEntry
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("pool load state: %v", err)
		return
	}
	byName := make(map[string]stateEntry, len(data))
	for _, s := range data {
		byName[s.Name] = s
	}
	for _, a := range p.Accounts() {
		s, ok := byName[a.Name]
		if !ok {
			continue
		}
		a.mu.Lock()
		a.errCount = s.ErrCount
		a.coolUntil = s.CoolUntil
		a.disabled = s.Disabled
		a.disabledReason = s.Reason
		a.lastErr = s.LastErr
		a.balance = s.Balance
		a.lastBalanceAt = s.BalanceAt
		a.mu.Unlock()
	}
}

func (p *Pool) saveState() {
	if p.state == "" {
		return
	}
	accounts := p.Accounts()
	out := make([]stateEntry, 0, len(accounts))
	for _, a := range accounts {
		a.mu.Lock()
		out = append(out, stateEntry{
			Name:      a.Name,
			ErrCount:  a.errCount,
			CoolUntil: a.coolUntil,
			Disabled:  a.disabled,
			Reason:    a.disabledReason,
			LastErr:   a.lastErr,
			Balance:   a.balance,
			BalanceAt: a.lastBalanceAt,
		})
		a.mu.Unlock()
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Printf("pool marshal state: %v", err)
		return
	}
	dir := filepath.Dir(p.state)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("pool create state dir: %v", err)
			return
		}
	}
	tmp := p.state + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("pool write state: %v", err)
		return
	}
	if err := os.Rename(tmp, p.state); err != nil {
		_ = os.Remove(tmp)
		log.Printf("pool rename state: %v", err)
	}
}
