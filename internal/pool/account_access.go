package pool

import "time"

// Token 返回当前账号 token 的并发安全快照。
// Account.Auth 会在 OAuth 续期或 auths reload 时被替换，因此任何运行时读取
// 都必须先在 Account.mu 下取得指针快照，不能直接读 acct.Auth。
func (a *Account) Token() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	ref := a.Auth
	a.mu.Unlock()
	if ref == nil {
		return ""
	}
	return ref.Token()
}

// TokenAge 返回当前 token 已使用时长。
func (a *Account) TokenAge() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	ref := a.Auth
	a.mu.Unlock()
	if ref == nil {
		return 0
	}
	return ref.Age()
}

// TokenRemaining 返回当前 token 预计剩余有效期。
func (a *Account) TokenRemaining() time.Duration {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	ref := a.Auth
	a.mu.Unlock()
	if ref == nil {
		return 0
	}
	return ref.Remaining()
}

// TokenExpired 报告当前 token 是否已过期。
func (a *Account) TokenExpired() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	ref := a.Auth
	a.mu.Unlock()
	return ref == nil || ref.Expired()
}

// TokenExpiringSoon 报告当前 token 是否将在 within 内过期。
func (a *Account) TokenExpiringSoon(within time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	ref := a.Auth
	a.mu.Unlock()
	return ref != nil && ref.ExpiringSoon(within)
}

// UserNameSnapshot 返回可并发读取的显示名。
func (a *Account) UserNameSnapshot() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.UserName
}
