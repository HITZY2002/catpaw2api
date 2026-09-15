// Package scheduler 提供余额看门狗 + 自动申请额度 + token 自动续期。
//
// 核对结论（逆向 2026.0729.1956 桌面端 + credit.catpaw.meituan.com）：
// CatPaw 没有每日签到接口；免费额度来自「注册奖励」活动：
// POST /api/gateway/credit/register（registerChannel=CATPAW_PC）→
// data.registrationBonus（新用户一次性发放，活动截止 2026-08-27）。
// token 无 refresh_token，有效期 72h，到期只能重新走 OAuth 登录。
// 本调度器：
//  1. 启动时对每个账号调用 register 领取注册奖励（幂等，老用户 newUser=false）
//  2. 周期性查余额，低于阈值时按配置自动申请（register / campaign / none）
//  3. token 即将过期时自动发起 OAuth session，轮询 poll-token 等待新 token
//     （需一个有美团登录态的浏览器打开 auth_url 完成静默续期）
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"sort"
	"sync"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/pool"
	"catpaw2api/internal/upstream"
)

// Config 调度器配置。
type Config struct {
	Pool            *pool.Pool
	Enabled         bool
	PollInterval    time.Duration // 余额轮询周期，默认 10m
	ApplyThreshold  int64         // 低于该值触发申请，默认 50
	ApplyMethod     string        // register | campaign | none
	ApplyCooldown   time.Duration // 两次申请最小间隔，默认 6h
	RegisterOnStart bool          // 启动时领取注册奖励
	// AutoRenew token 自动续期：剩余低于 RenewThreshold 时发起 OAuth。
	AutoRenew      bool          // 是否启用自动续期（与额度 watchdog 独立）
	RenewThreshold time.Duration // 触发续期的剩余时间阈值，默认 6h
	RenewWaitMax   time.Duration // 单次续期轮询最长等待，默认 15m
	AuthDir        string        // auths 目录（续期成功后落盘）
}

// Scheduler 定时任务。
type Scheduler struct {
	cfg       Config
	mu        sync.Mutex
	lastApply map[string]time.Time

	// renewMu 保护 renewing 映射
	renewMu     sync.Mutex
	renewing    map[string]bool         // account → 是否正在续期
	renewStatus map[string]*RenewStatus // account → 续期状态
}

// RenewStatus 单个账号的续期状态（供 WebUI 展示）。
type RenewStatus struct {
	Account   string    `json:"account"`
	UID       string    `json:"uid"`
	AuthURL   string    `json:"auth_url,omitempty"`
	StartedAt time.Time `json:"started_at"`
	Done      bool      `json:"done"`
	Error     string    `json:"error,omitempty"`
}

// New 构造调度器。
func New(cfg Config) *Scheduler {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 10 * time.Minute
	}
	if cfg.ApplyThreshold <= 0 {
		cfg.ApplyThreshold = 50
	}
	if cfg.ApplyMethod == "" {
		cfg.ApplyMethod = "register"
	}
	if cfg.ApplyCooldown <= 0 {
		cfg.ApplyCooldown = 6 * time.Hour
	}
	if cfg.RenewThreshold <= 0 {
		cfg.RenewThreshold = 6 * time.Hour
	}
	if cfg.RenewWaitMax <= 0 {
		cfg.RenewWaitMax = 15 * time.Minute
	}
	return &Scheduler{
		cfg:         cfg,
		lastApply:   map[string]time.Time{},
		renewing:    map[string]bool{},
		renewStatus: map[string]*RenewStatus{},
	}
}

// Run 启动定时循环。额度 watchdog 与 AutoRenew 独立：关闭额度轮询不应顺带关闭 token 续期。
func (s *Scheduler) Run(ctx context.Context) {
	if s.cfg.Pool == nil {
		log.Printf("scheduler disabled: nil pool")
		return
	}
	if s.cfg.Enabled && s.cfg.RegisterOnStart {
		s.RegisterAll(ctx)
	}
	if !s.cfg.Enabled && !s.cfg.AutoRenew {
		log.Printf("quota watchdog and auto-renew disabled")
		return
	}
	log.Printf("scheduler started: quota_enabled=%v poll=%s threshold=%d method=%s auto_renew=%v renew_threshold=%s",
		s.cfg.Enabled, s.cfg.PollInterval, s.cfg.ApplyThreshold, s.cfg.ApplyMethod,
		s.cfg.AutoRenew, s.cfg.RenewThreshold)
	// 启动时立即巡检续期/过期状态，不等第一个 ticker。
	s.renewSweep(ctx)
	s.expirySweep()
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 遍历全部账号：先做 token 续期/到期巡检；额度 watchdog 开启时再查余额与申请。
func (s *Scheduler) Tick(ctx context.Context) {
	s.renewSweep(ctx)
	s.expirySweep()
	if !s.cfg.Enabled {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		n, err := s.refreshBalance(ctx, acct)
		if err != nil {
			var ae *upstream.ApiError
			if ok := asAPI(err, &ae); ok && (ae.Status == 401 || ae.Code == 401) {
				s.cfg.Pool.Disable(acct.Name, "401 on credit balance")
			}
			log.Printf("quota balance account=%s error: %v", acct.Name, err)
			continue
		}
		switch {
		case n <= 0:
			// 余额归零：先试申请，再重查；仍为零则冷却一个轮询周期，避免空转打满错误计数。
			s.maybeApply(ctx, acct)
			if n2, err2 := s.refreshBalance(ctx, acct); err2 == nil && n2 > 0 {
				s.cfg.Pool.Unfreeze(acct.Name)
			} else {
				s.cfg.Pool.Cooldown(acct.Name, pool.CoolPlan, s.cfg.PollInterval, "balance exhausted")
			}
		case n < s.cfg.ApplyThreshold:
			s.maybeApply(ctx, acct)
			s.cfg.Pool.Unfreeze(acct.Name)
		default:
			s.cfg.Pool.Unfreeze(acct.Name)
		}
	}
}

// refreshBalance 拉一次余额并写回池缓存。
func (s *Scheduler) refreshBalance(ctx context.Context, acct *pool.Account) (int64, error) {
	balance, err := acct.Client.CreditBalance(ctx, acct.Auth.Token())
	if err != nil {
		return 0, err
	}
	n := upstream.ParseCredits(balance)
	s.cfg.Pool.SetBalance(acct.Name, n)
	log.Printf("quota balance account=%s credits=%d", acct.Name, n)
	return n, nil
}

// expirySweep 按 72h 推算 token 寿命：已过期直接禁用（不等 401），
// 24h 内将过期打告警日志提醒重登。
func (s *Scheduler) expirySweep() {
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct == nil || acct.Auth == nil {
			continue
		}
		switch {
		case acct.Auth.Expired():
			if !s.cfg.Pool.IsDisabled(acct.Name) {
				s.cfg.Pool.Disable(acct.Name, "token expired (72h), re-login via catpaw2api-login")
			}
		case acct.Auth.ExpiringSoon(24 * time.Hour):
			log.Printf("quota token account=%s expiring soon (remaining=%s), run catpaw2api-login to refresh",
				acct.Name, acct.Auth.Remaining().Round(time.Minute))
		}
	}
}

// renewSweep 检测 token 即将过期（< RenewThreshold）的账号，自动发起 OAuth 续期。
func (s *Scheduler) renewSweep(ctx context.Context) {
	if !s.cfg.AutoRenew || s.cfg.Pool == nil {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		if acct == nil || acct.Auth == nil {
			continue
		}
		remaining := acct.Auth.Remaining()
		if remaining <= 0 || remaining > s.cfg.RenewThreshold {
			continue
		}
		// 已在续期中则跳过。
		s.renewMu.Lock()
		if s.renewing[acct.Name] {
			s.renewMu.Unlock()
			continue
		}
		s.renewing[acct.Name] = true
		s.renewMu.Unlock()

		log.Printf("renew account=%s token expiring soon (remaining=%s), starting auto-renew",
			acct.Name, remaining.Round(time.Minute))
		go s.renewAccount(ctx, acct)
	}
}

// renewAccount 执行单个账号的 OAuth 续期（后台 goroutine）。
func (s *Scheduler) renewAccount(ctx context.Context, acct *pool.Account) {
	defer func() {
		s.renewMu.Lock()
		delete(s.renewing, acct.Name)
		s.renewMu.Unlock()
	}()

	client := upstream.New(30 * time.Second)

	// 1. login-config
	loginEntry, err := client.LoginConfig(ctx)
	if err != nil {
		s.setRenewStatus(acct, "", false, "login-config 失败: "+err.Error())
		log.Printf("renew account=%s login-config error: %v", acct.Name, err)
		return
	}

	// 2. 拼 auth_url
	sid := randHex(16)
	state := randHex(16)
	u, err := url.Parse(loginEntry)
	if err != nil {
		s.setRenewStatus(acct, "", false, "解析 loginEntryUrl 失败: "+err.Error())
		return
	}
	q := u.Query()
	q.Set("state", state)
	q.Set("redirect", "http://127.0.0.1:37890/callback")
	q.Set("sid", sid)
	u.RawQuery = q.Encode()
	authURL := u.String()

	s.setRenewStatus(acct, authURL, false, "")
	// authURL 含一次性 sid/state，不写入普通日志；WebUI 通过 RenewStatus 获取即可。
	log.Printf("renew account=%s waiting for browser authorization", acct.Name)

	// 3. 轮询 poll-token（最长等待 RenewWaitMax）
	renewCtx, cancel := context.WithTimeout(ctx, s.cfg.RenewWaitMax)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-renewCtx.Done():
			s.setRenewStatus(acct, authURL, false, "续期超时（未在期限内完成浏览器登录）")
			log.Printf("renew account=%s timeout (no browser login in %s)", acct.Name, s.cfg.RenewWaitMax)
			return
		case <-ticker.C:
			tok, err := client.PollToken(renewCtx, sid)
			if err != nil || tok == "" {
				continue
			}

			// 必须确认浏览器完成授权的账号就是正在续期的账号。
			user, err := client.CurrentUser(renewCtx, tok)
			if err != nil {
				s.setRenewStatus(acct, authURL, false, "续期 token 身份校验失败: "+err.Error())
				log.Printf("renew account=%s identity check error: %v", acct.Name, err)
				return
			}
			if err := validateRenewedIdentity(acct, user); err != nil {
				s.setRenewStatus(acct, authURL, false, err.Error())
				log.Printf("renew account=%s identity mismatch", acct.Name)
				return
			}

			// 4. 拿到匹配账号的新 token：更新 pool + 落盘。
			userName := acct.UserName
			if user != nil && user.UserName != "" {
				userName = user.UserName
			}
			newAuth := auth.New(acct.UID, userName, tok)
			if s.cfg.AuthDir != "" {
				if err := auth.SaveNew(s.cfg.AuthDir, newAuth); err != nil {
					s.setRenewStatus(acct, authURL, false, "落盘失败: "+err.Error())
					log.Printf("renew account=%s save error: %v", acct.Name, err)
					return
				}
			}
			s.installRenewedAuth(newAuth)
			s.setRenewStatus(acct, authURL, true, "")
			log.Printf("renew account=%s SUCCESS new token remaining=72h", acct.Name)
			return
		}
	}
}

func validateRenewedIdentity(acct *pool.Account, user *upstream.UserInfo) error {
	if acct == nil || user == nil || user.UserID == "" {
		return fmt.Errorf("续期身份校验失败：缺少账号身份")
	}
	if user.UserID != acct.UID {
		return fmt.Errorf("续期账号不匹配：浏览器登录的账号不是目标账号，请切换到正确账号后重新授权")
	}
	return nil
}

// installRenewedAuth 替换 token 并清理冷却/错误状态，但保留真实余额缓存。
// 旧实现误把 token 剩余分钟数写入 balance，导致余额与账号排序被污染。
func (s *Scheduler) installRenewedAuth(newAuth *auth.Auth) *pool.Account {
	if s.cfg.Pool == nil || newAuth == nil {
		return nil
	}
	updated := s.cfg.Pool.AddAccount(newAuth)
	if updated != nil {
		s.cfg.Pool.Unfreeze(updated.Name)
	}
	return updated
}

// setRenewStatus 更新续期状态（供 WebUI 查询）。
func (s *Scheduler) setRenewStatus(acct *pool.Account, authURL string, done bool, errMsg string) {
	if acct == nil {
		return
	}
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	startedAt := time.Now()
	if old := s.renewStatus[acct.Name]; old != nil && !old.StartedAt.IsZero() {
		startedAt = old.StartedAt
	}
	s.renewStatus[acct.Name] = &RenewStatus{
		Account:   acct.Name,
		UID:       acct.UID,
		AuthURL:   authURL,
		StartedAt: startedAt,
		Done:      done,
		Error:     errMsg,
	}
}

// RenewStatuses 返回所有账号的续期状态快照（供 WebUI 展示），按账号稳定排序。
func (s *Scheduler) RenewStatuses() []RenewStatus {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	out := make([]RenewStatus, 0, len(s.renewStatus))
	for _, st := range s.renewStatus {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

func randHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

// RegisterAll 启动时为每个账号领取注册奖励（幂等）。
func (s *Scheduler) RegisterAll(ctx context.Context) {
	if s.cfg.Pool == nil {
		return
	}
	for _, acct := range s.cfg.Pool.Accounts() {
		res, err := acct.Client.Register(ctx, acct.Auth.Token())
		if err != nil {
			log.Printf("quota register account=%s error: %v", acct.Name, err)
			continue
		}
		if res.NewUser {
			log.Printf("quota register account=%s NEW_USER bonus=%d expire_days=%d valid_until=%s",
				acct.Name, res.RegistrationBonus, res.RewardExpireDays, res.RewardLastValidDate)
		} else {
			log.Printf("quota register account=%s already registered (bonus=%d)", acct.Name, res.RegistrationBonus)
		}
	}
}

// maybeApply 按冷却与配置执行申请动作。
func (s *Scheduler) maybeApply(ctx context.Context, acct *pool.Account) {
	s.mu.Lock()
	last, seen := s.lastApply[acct.Name]
	if seen && time.Since(last) < s.cfg.ApplyCooldown {
		s.mu.Unlock()
		log.Printf("quota apply account=%s skipped (cooldown until %s)", acct.Name, last.Add(s.cfg.ApplyCooldown).Format(time.RFC3339))
		return
	}
	s.lastApply[acct.Name] = time.Now()
	s.mu.Unlock()

	switch s.cfg.ApplyMethod {
	case "register":
		res, err := acct.Client.Register(ctx, acct.Auth.Token())
		if err != nil {
			log.Printf("quota apply account=%s register failed: %v", acct.Name, err)
			return
		}
		log.Printf("quota apply account=%s register done newUser=%v bonus=%d", acct.Name, res.NewUser, res.RegistrationBonus)
	case "campaign":
		out, err := acct.Client.CampaignInit(ctx, acct.Auth.Token())
		if err != nil {
			log.Printf("quota apply account=%s campaign failed: %v", acct.Name, err)
			return
		}
		log.Printf("quota apply account=%s campaign done data=%v", acct.Name, out)
	case "none":
		log.Printf("quota apply account=%s below threshold but apply_method=none (manual action required)", acct.Name)
	default:
		log.Printf("quota apply account=%s unknown apply_method=%q", acct.Name, s.cfg.ApplyMethod)
	}
}

func asAPI(err error, target **upstream.ApiError) bool {
	if ae, ok := err.(*upstream.ApiError); ok {
		*target = ae
		return true
	}
	return false
}
