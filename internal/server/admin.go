package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/upstream"
)

// adminOverview 面板总览（对齐 WorkBuddy 字段）。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	total, healthy, disabled, cooling, credits := h.cfg.Pool.Stats()
	models := h.modelList()
	ids := make([]map[string]any, 0, len(models))
	for _, m := range models {
		ids = append(ids, map[string]any{"id": m["id"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "catpaw2api",
		"region":  "cn",
		"stats": map[string]any{
			"total":    total,
			"healthy":  healthy,
			"disabled": disabled,
			"cooling":  cooling,
			"credits":  credits,
		},
		"accounts": h.cfg.Pool.List(),
		"models":   ids,
		"schedule": map[string]any{
			"quota": h.cfg.QuotaInfo,
		},
		"quota": h.cfg.QuotaInfo,
	})
}

type uidBody struct {
	UID     string `json:"uid"`
	Account string `json:"account"`
	Reason  string `json:"reason"`
}

func readUIDBody(r *http.Request) (uidBody, error) {
	var b uidBody
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.UID == "" {
		b.UID = b.Account
	}
	return b, nil
}

type actionResult struct {
	UID     string `json:"uid"`
	OK      bool   `json:"ok"`
	Credits int64  `json:"credits,omitempty"`
	Message string `json:"message,omitempty"`
}

// adminCredits 刷新积分余额。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	log.Printf("adminCredits body.UID=%q", body.UID)
	targets := h.pickTargets(body.UID)
	log.Printf("adminCredits targets=%v", targets)
	results := make([]actionResult, 0, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for _, uid := range targets {
		uid := uid
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := h.refreshOneCredits(uid)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      allOK(results),
		"message": summaryMsg("积分刷新", results),
		"results": results,
	})
}

// adminApply 申请额度（面板「申请额度」→ /admin/api/checkin）。
func (h *Handler) adminApply(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "bad json: " + err.Error()})
		return
	}
	method := "register"
	if h.cfg.QuotaInfo != nil {
		if m, ok := h.cfg.QuotaInfo["apply_method"].(string); ok && m != "" {
			method = m
		}
	}
	targets := h.pickTargets(body.UID)
	results := make([]actionResult, 0, len(targets))
	for _, name := range targets {
		acct := h.cfg.Pool.Get(name)
		if acct == nil {
			results = append(results, actionResult{UID: name, OK: false, Message: "no account"})
			continue
		}
		res := actionResult{UID: name}
		switch method {
		case "register":
			_, err := acct.Client.Register(r.Context(), acct.Auth.Token())
			if err != nil {
				res.Message = err.Error()
			} else {
				res.OK = true
				res.Message = "register ok"
			}
		case "campaign":
			_, err := acct.Client.CampaignInit(r.Context(), acct.Auth.Token())
			if err != nil {
				res.Message = err.Error()
			} else {
				res.OK = true
				res.Message = "campaign ok"
			}
		case "none":
			res.OK = true
			res.Message = "apply_method=none"
		default:
			res.Message = "unknown method " + method
		}
		if res.OK {
			if bal, err := acct.Client.CreditBalance(r.Context(), acct.Auth.Token()); err == nil {
				credits := upstream.ParseCredits(bal)
				h.cfg.Pool.SetBalance(name, credits)
				res.Credits = credits
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      allOK(results),
		"message": summaryMsg("申请额度", results),
		"results": results,
	})
}

// adminKeepalive CatPaw 个人版 token 无 refresh，按钮复用为刷新余额。
func (h *Handler) adminKeepalive(w http.ResponseWriter, r *http.Request) {
	h.adminCredits(w, r)
}

// adminReload 从 auths/ 热加载。
func (h *Handler) adminReload(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	auths, err := auth.LoadDir(h.cfg.AuthDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.cfg.Pool.SyncToDir(auths)
	total, healthy, _, _, _ := h.cfg.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已重载 auths", "loaded": len(auths), "total": total, "healthy": healthy,
	})
}

func (h *Handler) adminEnable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	balance, ok := h.cachedBalance(body.UID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	// 启用账号只改变可用状态，不能把真实积分余额重置为 0。
	h.cfg.Pool.Enable(body.UID, balance)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已启用 " + body.UID})
}

func (h *Handler) adminDisable(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	reason := body.Reason
	if reason == "" {
		reason = "manual disable"
	}
	h.cfg.Pool.Disable(body.UID, reason)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已禁用 " + body.UID})
}

func (h *Handler) adminClearCooldown(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	h.cfg.Pool.ClearCooldown(body.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清冷却 " + body.UID})
}

func (h *Handler) adminUnfreeze(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	h.cfg.Pool.Unfreeze(body.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已强制解冻 " + body.UID})
}

func (h *Handler) cachedBalance(uid string) (int64, bool) {
	for _, st := range h.cfg.Pool.List() {
		u, _ := st["uid"].(string)
		if u != uid {
			continue
		}
		switch v := st["balance"].(type) {
		case int64:
			return v, true
		case int:
			return int64(v), true
		case float64:
			return int64(v), true
		default:
			return 0, true
		}
	}
	return 0, false
}

func (h *Handler) pickTargets(uid string) []string {
	if uid != "" {
		if h.cfg.Pool.Get(uid) == nil {
			return nil
		}
		return []string{uid}
	}
	out := make([]string, 0)
	for _, st := range h.cfg.Pool.List() {
		if disabled, _ := st["disabled"].(bool); disabled {
			continue
		}
		if u, ok := st["uid"].(string); ok && u != "" {
			out = append(out, u)
		}
	}
	return out
}

func (h *Handler) refreshOneCredits(uid string) actionResult {
	acct := h.cfg.Pool.Get(uid)
	if acct == nil {
		return actionResult{UID: uid, OK: false, Message: "no auth"}
	}
	bal, err := acct.Client.CreditBalance(context.Background(), acct.Auth.Token())
	if err != nil {
		log.Printf("refreshOneCredits uid=%s CreditBalance error: %v", uid, err)
		return actionResult{UID: uid, OK: false, Message: err.Error()}
	}
	log.Printf("refreshOneCredits uid=%s bal=%v", uid, bal)
	credits := upstream.ParseCredits(bal)
	log.Printf("refreshOneCredits uid=%s credits=%d", uid, credits)
	h.cfg.Pool.SetBalance(uid, credits)
	return actionResult{UID: uid, OK: true, Credits: credits, Message: "ok"}
}

func allOK(results []actionResult) bool {
	if len(results) == 0 {
		return true
	}
	for _, r := range results {
		if !r.OK {
			return false
		}
	}
	return true
}

func summaryMsg(action string, results []actionResult) string {
	if len(results) == 0 {
		return action + ": 无目标账号"
	}
	ok, fail := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			fail++
		}
	}
	if fail == 0 {
		return action + "完成: " + itoa(ok) + " 成功"
	}
	return action + "完成: " + itoa(ok) + " 成功 / " + itoa(fail) + " 失败"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// adminRenewStatus 返回 token 自动续期状态（哪些账号正在续期、auth_url 等）。
func (h *Handler) adminRenewStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RenewStatuses == nil {
		writeJSON(w, http.StatusOK, map[string]any{"renewals": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"renewals": h.cfg.RenewStatuses()})
}
