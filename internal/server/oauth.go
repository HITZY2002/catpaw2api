package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/upstream"
)

// CatPaw Passport OAuth（与 cmd/login 一致）：
//  1. login-config → loginEntryUrl
//  2. 拼 state/sid/redirect 打开浏览器
//  3. 服务端轮询 poll-token?sid= 拿 token（无需本地回调）
const oauthSessionTTL = 15 * time.Minute

type oauthSession struct {
	ID        string
	SID       string
	State     string
	AuthURL   string
	CreatedAt time.Time
}

type oauthStore struct {
	mu   sync.Mutex
	byID map[string]*oauthSession
}

func newOAuthStore() *oauthStore {
	return &oauthStore{byID: map[string]*oauthSession{}}
}

func (s *oauthStore) put(sess *oauthSession) {
	if sess == nil || sess.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	s.byID[sess.ID] = sess
}

func (s *oauthStore) get(id string) *oauthSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	return s.byID[id]
}

func (s *oauthStore) del(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

func (s *oauthStore) gcLocked() {
	now := time.Now()
	for id, sess := range s.byID {
		if sess == nil || now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(s.byID, id)
		}
	}
}

func newSessionID() string { return randHex(32) }

// adminOAuthStart 发起 Passport 授权，返回浏览器登录 URL。
func (h *Handler) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	client := upstream.New(30 * time.Second)
	loginEntry, err := client.LoginConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": "login-config 失败: " + err.Error()})
		return
	}
	state := randHex(32)
	sid := randHex(32)
	u, err := url.Parse(loginEntry)
	if err != nil || u.Scheme == "" || u.Host == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": "解析 loginEntryUrl 失败"})
		return
	}
	q := u.Query()
	q.Set("state", state)
	// 远端面板：回调到 127.0.0.1 打不开属正常，靠 poll-token 通道取 token。
	q.Set("redirect", "http://127.0.0.1:37890/callback")
	q.Set("sid", sid)
	u.RawQuery = q.Encode()

	id := newSessionID()
	h.oauth.put(&oauthSession{
		ID:        id,
		SID:       sid,
		State:     state,
		AuthURL:   u.String(),
		CreatedAt: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"auth_url":   u.String(),
		"expires_in": int(oauthSessionTTL.Seconds()),
		"message":    "请在浏览器打开授权链接，完成后点「我已授权」或等待自动检测",
	})
}

// adminOAuthPoll 轮询 poll-token；成功则落盘并热加载。
func (h *Handler) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "读取请求失败"})
		return
	}
	if len(body) > 1<<20 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "status": "error", "message": "请求体过大"})
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "JSON 无效"})
			return
		}
	}
	if req.SessionID == "" {
		req.SessionID = r.URL.Query().Get("session_id")
	}
	if req.SessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "status": "error", "message": "session_id required"})
		return
	}
	sess := h.oauth.get(req.SessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "status": "error", "message": "会话不存在或已过期，请重新发起授权"})
		return
	}

	client := upstream.New(15 * time.Second)
	tok, err := client.PollToken(r.Context(), sess.SID)
	if err != nil || tok == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "pending", "message": "等待浏览器完成登录…",
		})
		return
	}

	user, err := client.CurrentUser(r.Context(), tok)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "status": "error", "message": "current-user 失败: " + err.Error(),
		})
		return
	}
	if user == nil || user.UserID == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "status": "error", "message": "已拿到 token 但缺少 uid",
		})
		return
	}

	a := auth.New(user.UserID, user.UserName, tok)
	if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "message": "落盘失败: " + err.Error(),
		})
		return
	}
	if h.cfg.Pool == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "status": "error", "message": "账号池未初始化"})
		return
	}
	h.cfg.Pool.AddAccount(a)

	// 非阻塞刷余额 + 尝试 register；请求已完成后使用独立的短超时 context。
	go func(authCopy *auth.Auth) {
		acct := h.cfg.Pool.Get(authCopy.UID)
		if acct == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = acct.Client.Register(ctx, authCopy.Token())
		if bal, err := acct.Client.CreditBalance(ctx, authCopy.Token()); err == nil {
			h.cfg.Pool.SetBalance(authCopy.UID, upstream.ParseCredits(bal))
		}
	}(a)

	h.oauth.del(req.SessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"status":  "done",
		"message": fmt.Sprintf("登录成功：%s (%s)", nonempty(user.UserName, "未命名"), shortID(user.UserID)),
		"account": map[string]any{
			"uid":      user.UserID,
			"nickname": user.UserName,
		},
	})
}

func nonempty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func shortID(u string) string {
	if len(u) <= 12 {
		return u
	}
	return u[:8] + "…"
}
