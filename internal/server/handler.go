// Package server 暴露 OpenAI 兼容接口：/v1/chat/completions、/v1/models、/status。
package server

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"catpaw2api/internal/pool"
	"catpaw2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool          *pool.Pool
	APIKey        string
	AuthDir       string // OAuth 落盘 / 重载 auths
	MaxRotate     int
	SoftCooldown  time.Duration
	ErrThreshold  int
	ErrCooldown   time.Duration
	DefaultModel  string
	ConvStateFile string         // 会话持久化（chatId/conversationId/指纹）
	QuotaInfo     map[string]any // WebUI 展示的额度调度配置
	// RenewStatuses 返回 token 自动续期状态（由 scheduler 注入，避免循环导入）。
	RenewStatuses func() []any
}

const (
	maxBodyBytes = 8 << 20
	maxConvs     = 1000 // 会话表上限，超出按最久未用淘汰
)

// errNoNewMessage 客户端重放了已吸收的历史，没有可发送的新消息。
var errNoNewMessage = errors.New("no new message to send (history already absorbed)")

// Handler 主路由。
type Handler struct {
	cfg   Config
	mux   *http.ServeMux
	oauth *oauthStore

	convMu sync.Mutex
	convs  map[string]*convState // conversationID → 不可变会话快照
	latest map[string]string     // account → 最近会话 conversationID
}

// convState 一个服务端会话的网关侧状态。
//
// CatPaw 续接上下文按 chatId 关联（conversationId 只用于订阅读流），
// 无状态客户端每次全量回发历史，因此用 Absorbed+Fingerprint 记录
// 服务端会话已吸收的消息前缀，下轮请求做指纹比对决定增量还是新会话。
// 存入 Handler.convs 后视为不可变；读取/更新都通过 copy-on-write 快照完成。
type convState struct {
	ChatID         string `json:"chat_id"`
	ConversationID string `json:"conversation_id"`
	Account        string `json:"account"`
	Absorbed       int    `json:"absorbed"`
	Fingerprint    string `json:"fingerprint"`
	UpdatedAt      int64  `json:"updated_at"`
}

// convFile 会话持久化文件格式。
type convFile struct {
	Conversations []*convState      `json:"conversations"`
	Latest        map[string]string `json:"latest"`
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "glm-5.2"
	}
	h := &Handler{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		oauth:  newOAuthStore(),
		convs:  map[string]*convState{},
		latest: map[string]string{},
	}
	h.loadConvs()
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// WebUI（WorkBuddy 风格控制台；页面不鉴权，API 走 Bearer）
	h.mux.HandleFunc("GET /{$}", h.servePanel)
	h.mux.HandleFunc("GET /admin", h.servePanel)
	h.mux.HandleFunc("GET /panel", h.servePanel)
	h.mux.HandleFunc("GET /panel/", h.servePanel)
	h.mux.HandleFunc("GET /admin/api/overview", h.withAuth(h.adminOverview))
	h.mux.HandleFunc("POST /admin/api/credits", h.withAuth(h.adminCredits))
	h.mux.HandleFunc("POST /admin/api/checkin", h.withAuth(h.adminApply)) // CatPaw：申请额度
	h.mux.HandleFunc("POST /admin/api/keepalive", h.withAuth(h.adminKeepalive))
	h.mux.HandleFunc("POST /admin/api/reload", h.withAuth(h.adminReload))
	h.mux.HandleFunc("POST /admin/api/accounts/enable", h.withAuth(h.adminEnable))
	h.mux.HandleFunc("POST /admin/api/accounts/disable", h.withAuth(h.adminDisable))
	h.mux.HandleFunc("POST /admin/api/accounts/clear-cooldown", h.withAuth(h.adminClearCooldown))
	h.mux.HandleFunc("POST /admin/api/accounts/unfreeze", h.withAuth(h.adminUnfreeze))
	h.mux.HandleFunc("POST /admin/api/oauth/start", h.withAuth(h.adminOAuthStart))
	h.mux.HandleFunc("POST /admin/api/oauth/poll", h.withAuth(h.adminOAuthPoll))
	h.mux.HandleFunc("GET /admin/api/renew-status", h.withAuth(h.adminRenewStatus))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			key := authz[len(prefix):]
			if subtle.ConstantTimeCompare([]byte(key), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": h.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

var dynamicModels struct {
	sync.RWMutex
	list     []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, m := range infos {
			entry := map[string]any{
				"id":       m.ID(),
				"object":   "model",
				"created":  1753600000,
				"owned_by": "catpaw",
			}
			if m.Description != "" {
				entry["description"] = m.Description
			}
			if m.ExtendedInfo.ShortName != "" {
				entry["short_name"] = m.ExtendedInfo.ShortName
			}
			if m.ExtendedInfo.RateMultiplier != "" {
				entry["rate_multiplier"] = m.ExtendedInfo.RateMultiplier
			}
			if m.ExtendedInfo.MarketingCopy != "" {
				entry["marketing_copy"] = m.ExtendedInfo.MarketingCopy
			}
			out = append(out, entry)
		}
		return out
	}
	// 兜底：逆向自 app.asar 的真实模型表（tenant=CatDesk,scene=CATX_APP,env=EXTERNAL 返回的 7 个）。
	// FetchModels 失败时用这份，确保 /v1/models 永远返回真实可用模型。
	names := []string{
		upstream.ExactModelAuto,
		upstream.ExactModelLongCat2,
		upstream.ExactModelDeepseekV4Flash,
		upstream.ExactModelDeepseekV4Pro,
		upstream.ExactModelGLM52,
		upstream.ExactModelMiniMaxM3,
		upstream.ExactModelKimiK3,
	}
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"id": n, "object": "model", "created": 1753600000, "owned_by": "catpaw"})
	}
	return out
}

func (h *Handler) fetchModels() []upstream.ModelInfo {
	dynamicModels.RLock()
	if len(dynamicModels.list) > 0 && time.Since(dynamicModels.fetched) < time.Hour {
		out := dynamicModels.list
		dynamicModels.RUnlock()
		return out
	}
	if !dynamicModels.lastFail.IsZero() && time.Since(dynamicModels.lastFail) < 5*time.Minute {
		dynamicModels.RUnlock()
		return nil
	}
	dynamicModels.RUnlock()

	if h.cfg.Pool == nil {
		return nil
	}
	acct := h.cfg.Pool.PickExcluding(nil)
	if acct == nil {
		return nil
	}
	infos, err := acct.Client.FetchModels(context.Background(), acct.Auth.Token(), acct.UID)
	if err != nil || len(infos) == 0 {
		dynamicModels.Lock()
		dynamicModels.lastFail = time.Now()
		dynamicModels.Unlock()
		return nil
	}
	dynamicModels.Lock()
	dynamicModels.list = infos
	dynamicModels.fetched = time.Now()
	dynamicModels.lastFail = time.Time{}
	dynamicModels.Unlock()
	return infos
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(body) > maxBodyBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ConversationID == "" {
		req.ConversationID = r.Header.Get("X-Catpaw-Conversation-Id")
	}

	toolsOn := toolsActive(req)
	toolsBlock := ""
	if toolsOn {
		toolsBlock = buildToolsPrompt(normalizeTools(req.Tools), req.ToolChoice)
	}

	// 显式 conversation_id → 锁定属主账号（会话按 chatId 续接，token 必须匹配）。
	var forced *pool.Account
	var forcedConv *convState
	if req.ConversationID != "" {
		h.convMu.Lock()
		if stored := h.convs[req.ConversationID]; stored != nil {
			snapshot := *stored
			forcedConv = &snapshot
		}
		h.convMu.Unlock()
		if forcedConv == nil {
			writeOpenAIError(w, http.StatusBadRequest, "unknown_conversation",
				"unknown conversation_id: "+req.ConversationID+"（只识别本网关创建的会话）")
			return
		}
		forced = h.cfg.Pool.Get(forcedConv.Account)
		if forced == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "conversation_orphaned",
				"conversation owner account "+forcedConv.Account+" not in pool")
			return
		}
		if !h.cfg.Pool.Healthy(forced.Name) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "conversation_owner_unavailable",
				"conversation owner account "+forced.Name+" is cooling/disabled")
			return
		}
	}

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *pool.Account
		if forced != nil {
			if i > 0 {
				break
			}
			acct = forced
		} else {
			acct = h.cfg.Pool.PickExcluding(tried)
			if acct == nil {
				break
			}
			tried[acct.Name] = true
		}

		ok, verr := h.cfg.Pool.Validate(acct)
		if verr == nil && !ok {
			lastErr = errors.New("token invalid")
			continue
		}

		model := h.mapModel(req.Model, acct.DefaultModel)

		conv, isNew, prompt, perr := h.planConversation(acct, req, forcedConv, toolsBlock)
		if perr != nil {
			// 请求级问题（如没有新消息），换号无意义。
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", perr.Error())
			return
		}

		conv, derr := h.driveConversation(r.Context(), acct, conv, isNew, prompt, model)
		if derr != nil {
			lastErr = derr
			h.handleUpstreamError(acct, derr)
			continue
		}

		// 轮询 history 拿完整回复（free 计划 SSE 通道不吐数据，纯 HTTP 走轮询）。
		result, perr := acct.Client.PollAssistant(r.Context(), acct.Auth.Token(), acct.UID, conv.ConversationID, upstream.PollOpts{
			Timeout: h.upstreamTimeout(),
		})
		if perr != nil {
			lastErr = perr
			h.handleUpstreamError(acct, perr)
			continue
		}

		w.Header().Set("X-Catpaw-Conversation-Id", conv.ConversationID)

		content, finish := result.Content, result.Finish
		usage := usageFromUpstream(result.Usage, prompt, result)
		var calls []openAIToolCall
		if toolsOn {
			if c, rest, found := extractToolCalls(result.Content); found {
				calls = c
				assignCallIDs(calls)
				content = rest
				finish = "tool_calls"
			}
		}

		h.finalizeConversation(acct, conv, req, content, calls)
		h.cfg.Pool.NoteSuccess(acct.Name)

		if req.Stream {
			h.emitSyntheticStream(w, model, result.Reasoning, content, calls, finish)
		} else {
			writeJSON(w, http.StatusOK, buildCompletion(model, result.Reasoning, content, calls, finish, conv.ConversationID, usage))
		}
		return
	}
	msg := "all accounts unavailable (disabled/cooldown)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// planConversation 决定本次请求走哪条会话、发什么提示词：
//   - 显式会话（forcedConv）或账号最近会话，指纹前缀匹配 → 增量发送尾部消息
//   - 指纹不匹配（历史被改写/新客户端）→ 新会话 + 全量折叠
func (h *Handler) planConversation(acct *pool.Account, req *chatRequest, forcedConv *convState, toolsBlock string) (conv *convState, isNew bool, prompt string, err error) {
	h.convMu.Lock()
	if forcedConv != nil {
		snapshot := *forcedConv
		conv = &snapshot
	} else if id := h.latest[acct.Name]; id != "" {
		if stored := h.convs[id]; stored != nil {
			snapshot := *stored
			conv = &snapshot
		}
	}
	h.convMu.Unlock()

	if conv != nil && conv.ChatID != "" && conv.Absorbed > 0 {
		// 指纹匹配：客户端重放了完整历史前缀 → 只发增量尾部。
		if conv.Absorbed <= len(req.Messages) && fingerprintOf(req.Messages[:conv.Absorbed]) == conv.Fingerprint {
			tail := req.Messages[conv.Absorbed:]
			if len(tail) == 0 {
				return nil, false, "", errNoNewMessage
			}
			return conv, false, renderTailPrompt(tail, toolsBlock), nil
		}
		// 指纹不匹配（客户端只发了新消息，或历史被改写）。
		// CatPaw 上下文由服务端按 chatId 维持，不需要客户端重放历史：
		// 显式 conversation_id 的客户端明确表达了「继续这个会话」的意图，
		// 只把客户端本次发的消息发给上游即可（服务端已有前文）。
		if forcedConv != nil {
			prompt = renderTailPrompt(req.Messages, toolsBlock)
			log.Printf("chat conv=%s reusing chatId (client did not replay full history)", conv.ConversationID)
			return conv, false, prompt, nil
		}
		log.Printf("chat conv=%s history diverged, starting new conversation", conv.ConversationID)
	}
	return nil, true, renderFullPrompt(req.Messages, toolsBlock), nil
}

// driveConversation 把提示词真正发给上游（新会话先 create 再 send）。
func (h *Handler) driveConversation(ctx context.Context, acct *pool.Account, conv *convState, isNew bool, prompt, model string) (*convState, error) {
	if !isNew && conv != nil {
		if _, err := acct.Client.SendMessage(ctx, acct.Auth.Token(), conv.ChatID, prompt, model); err != nil {
			return nil, fmt.Errorf("send message: %w", err)
		}
		return conv, nil
	}
	chatID := "desk-" + randHex(16)
	if err := acct.Client.CreateChat(ctx, acct.Auth.Token(), chatID, prompt); err != nil {
		return nil, fmt.Errorf("create chat: %w", err)
	}
	resp, err := acct.Client.SendMessage(ctx, acct.Auth.Token(), chatID, prompt, model)
	if err != nil {
		return nil, fmt.Errorf("send message: %w", err)
	}
	log.Printf("chat conv created account=%s chat=%s conv=%s", acct.Name, chatID, resp.ConversationID)
	return &convState{
		ChatID:         chatID,
		ConversationID: resp.ConversationID,
		Account:        acct.Name,
	}, nil
}

// finalizeConversation 把本轮吸收的消息（含助手回复）写入会话状态并落盘。
// 助手回复的 tool_calls id 由本网关生成，客户端原样回发时可逐字节复算指纹。
// 使用 copy-on-write，避免请求持有的快照被其他 goroutine 原地修改。
func (h *Handler) finalizeConversation(acct *pool.Account, conv *convState, req *chatRequest, content string, calls []openAIToolCall) {
	assistant := openAIMessage{Role: "assistant", Text: content, ToolCalls: calls}
	fp := chainFingerprint(fingerprintOf(req.Messages), assistant)

	next := *conv
	next.Absorbed = len(req.Messages) + 1
	next.Fingerprint = fp
	next.UpdatedAt = time.Now().Unix()

	h.convMu.Lock()
	defer h.convMu.Unlock()
	h.convs[next.ConversationID] = &next
	h.latest[acct.Name] = next.ConversationID
	h.evictConvsLocked()
	h.saveConvsLocked()
}

// evictConvsLocked 淘汰最久未用的会话（保留最新 maxConvs 条）。调用方须持锁。
func (h *Handler) evictConvsLocked() {
	if len(h.convs) <= maxConvs {
		return
	}
	var oldestID string
	var oldestTS int64
	for id, c := range h.convs {
		if oldestID == "" || c.UpdatedAt < oldestTS {
			oldestID, oldestTS = id, c.UpdatedAt
		}
	}
	delete(h.convs, oldestID)
	for acct, id := range h.latest {
		if id == oldestID {
			delete(h.latest, acct)
		}
	}
}

// emitSyntheticStream 聚合后合成 OpenAI SSE（工具调用场景：
// 必须先拿全文做 tool_call 解析，无法直通）。
func (h *Handler) emitSyntheticStream(w http.ResponseWriter, model, reasoning, content string, calls []openAIToolCall, finish string) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	writeChunk := func(delta map[string]any, finishReason string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finishReason != "" {
			choice["finish_reason"] = finishReason
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{choice},
		}
		raw, _ := json.Marshal(chunk)
		_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}

	if reasoning != "" {
		writeChunk(map[string]any{"reasoning_content": reasoning}, "")
	}
	if content != "" {
		// 按字符切片模拟流式增量（上游是轮询拿到的完整文本，
		// 切成小段逐帧输出，客户端能看到逐字流式效果）。
		runes := []rune(content)
		const chunkSize = 24
		for i := 0; i < len(runes); i += chunkSize {
			end := i + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			writeChunk(map[string]any{"content": string(runes[i:end])}, "")
		}
	}
	for i, c := range calls {
		// 首帧带 id+name+空 arguments，次帧带完整 arguments（OpenAI 增量格式）。
		writeChunk(map[string]any{"tool_calls": []any{
			map[string]any{
				"index": i,
				"id":    c.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": "",
				},
			},
		}}, "")
		writeChunk(map[string]any{"tool_calls": []any{
			map[string]any{
				"index": i,
				"function": map[string]any{
					"arguments": c.Arguments,
				},
			},
		}}, "")
	}
	writeChunk(map[string]any{}, finish)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// buildCompletion 组装非流式 chat.completion 响应。
func buildCompletion(model, reasoning, content string, calls []openAIToolCall, finish, conversationID string, usage map[string]int) map[string]any {
	message := map[string]any{"role": "assistant"}
	if len(calls) > 0 {
		message["tool_calls"] = toOpenAIToolCalls(calls)
		if content == "" {
			message["content"] = nil
		} else {
			message["content"] = content
		}
	} else {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finish},
		},
		"conversation_id": conversationID,
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// upsertTimeout 返回上游总超时（轮询上限）。优先用 ErrCooldown*6 兜底，默认 180s。
func (h *Handler) upstreamTimeout() time.Duration {
	// 给上游生成留够时间，轮询单次很快，总超时取 180s。
	return 180 * time.Second
}

// usageFromUpstream 优先用上游返回的 totalUsage；缺失时回退本地估算。
func usageFromUpstream(up map[string]any, prompt string, result *upstream.AssistantResult) map[string]int {
	pt, ct, tt := 0, 0, 0
	if up != nil {
		pt = toInt(up["prompt_tokens"])
		ct = toInt(up["completion_tokens"])
		tt = toInt(up["total_tokens"])
		if tt == 0 && pt+ct > 0 {
			tt = pt + ct
		}
	}
	if tt == 0 {
		pt = len([]rune(prompt))/4 + 1
		ct = (len([]rune(result.Content)) + len([]rune(result.Reasoning))) / 4
		tt = pt + ct
	}
	return map[string]int{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// promptTokensEstimate 粗略 token 估算（上游不报 usage 时的回退）。
func promptTokensEstimate(prompt string, comp *upstream.RawCompletion) map[string]int {
	pt := len([]rune(prompt))/4 + 1
	ct := (len([]rune(comp.Content))+len([]rune(comp.Reasoning)))/4 + 1
	return map[string]int{
		"prompt_tokens":     pt,
		"completion_tokens": ct,
		"total_tokens":      pt + ct,
	}
}

// handleUpstreamError 按错误类型更新池状态。
func (h *Handler) handleUpstreamError(acct *pool.Account, err error) {
	var ae *upstream.ApiError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Code == 401:
			h.cfg.Pool.Disable(acct.Name, "401 "+ae.Message)
		case ae.Status == 429:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolSoft, h.cfg.SoftCooldown, ae.Error())
		case ae.Status >= 500:
			h.cfg.Pool.Cooldown(acct.Name, pool.CoolErr, h.cfg.ErrCooldown, ae.Error())
		default:
			h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		}
		return
	}
	h.cfg.Pool.NoteError(acct.Name, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
}

// mapModel 解析 model：空/auto → 账号默认；其他 → 原样透传。
func (h *Handler) mapModel(model, accountDefault string) string {
	model = strings.TrimSpace(model)
	if model == "" || model == upstream.ExactModelAuto {
		if accountDefault != "" {
			return accountDefault
		}
		if h.cfg.DefaultModel != "" {
			return h.cfg.DefaultModel
		}
		// 自动取模型表第一个，失败则空（上游用默认）
		if infos := h.fetchModels(); len(infos) > 0 {
			return infos[0].ID()
		}
		return ""
	}
	return model
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

// randHex 生成 n 位十六进制随机串（crypto/rand）。
func randHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, (n+1)/2)
	if _, err := crand.Read(b); err != nil {
		// crypto/rand 理论上不会失败；兜底用纳秒时间戳循环填充。
		seed := fmt.Sprintf("%x", time.Now().UnixNano())
		for len(seed) < n {
			seed += seed
		}
		return seed[:n]
	}
	return hex.EncodeToString(b)[:n]
}

// ---------------------------------------------------------------------------
// 会话持久化
// ---------------------------------------------------------------------------

func (h *Handler) loadConvs() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	raw, err := os.ReadFile(h.cfg.ConvStateFile)
	if err != nil {
		return
	}
	var f convFile
	if err := json.Unmarshal(raw, &f); err == nil && f.Conversations != nil {
		for _, c := range f.Conversations {
			if c == nil || c.ConversationID == "" {
				continue
			}
			h.convs[c.ConversationID] = c
		}
		for acct, id := range f.Latest {
			if _, ok := h.convs[id]; ok {
				h.latest[acct] = id
			}
		}
		return
	}
	// 兼容旧版格式：map[account]{chat_id,conversation_id}（Absorbed=0，下一轮自动开新会话）。
	var legacy map[string]*convState
	if err := json.Unmarshal(raw, &legacy); err == nil {
		for acct, c := range legacy {
			if c == nil || c.ConversationID == "" {
				continue
			}
			c.Account = acct
			h.convs[c.ConversationID] = c
			h.latest[acct] = c.ConversationID
		}
	}
}

// saveConvsLocked 落盘会话表（tmp+rename 原子写）。调用方须持 convMu。
func (h *Handler) saveConvsLocked() {
	if h.cfg.ConvStateFile == "" {
		return
	}
	f := convFile{Latest: h.latest, Conversations: make([]*convState, 0, len(h.convs))}
	for _, c := range h.convs {
		f.Conversations = append(f.Conversations, c)
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(h.cfg.ConvStateFile+".tmp", raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(h.cfg.ConvStateFile+".tmp", h.cfg.ConvStateFile)
}
