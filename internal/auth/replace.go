package auth

import (
	"fmt"
	"strings"
)

// ReplaceFrom 在不更换 Auth 对象地址的前提下原子更新凭证内容。
//
// Pool 会把 *Auth 发布给长生命周期的 Account；热续期/reload 若直接替换
// Account.Auth 指针，会与正在读取 acct.Auth.Token()/Remaining() 的请求形成 race。
// 因此已发布的 Auth 指针保持稳定，只更新其 mutex 保护的数据。
func (a *Auth) ReplaceFrom(next *Auth) error {
	if a == nil || next == nil {
		return fmt.Errorf("auth: nil credential replacement")
	}
	if a == next {
		return nil
	}

	next.mu.Lock()
	uid := strings.TrimSpace(next.UID)
	userName := next.UserName
	token := strings.TrimSpace(next.AccessToken)
	updatedAt := next.UpdatedAt
	path := next.path
	next.mu.Unlock()

	if uid == "" {
		return fmt.Errorf("auth: replacement has empty uid")
	}
	if token == "" {
		return fmt.Errorf("auth: replacement has empty access token")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if currentUID := strings.TrimSpace(a.UID); currentUID != "" && currentUID != uid {
		return fmt.Errorf("auth: replacement uid mismatch: current=%q next=%q", currentUID, uid)
	}
	a.UID = uid
	a.UserName = userName
	a.AccessToken = token
	if updatedAt != 0 {
		a.UpdatedAt = updatedAt
	}
	if path != "" {
		a.path = path
	}
	return nil
}
