// CatPaw 直连云端客户端。
//
// 纯 HTTP，不依赖任何客户端进程。鉴权方式按端点区分（全部来自逆向）：
//   - 网关（catx.nocode.cn）       ：X-Auth-Token
//   - 直连（ai.catpaw.meituan.com）：X-Passport-Token + Cookie
//   - 聊天（nocode.cn /api/chat/*）：access-token + client-id
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ApiError 携带业务 code 的上游错误。
type ApiError struct {
	Code    int
	Status  int
	Message string
	Path    string
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("catpaw api code=%d http=%d path=%s msg=%s", e.Code, e.Status, e.Path, e.Message)
}

// Client 是 CatPaw 云 API 客户端。
//
// 内部拆两个 http.Client：
//   - httpJSON ：REST/JSON 请求，带总超时（默认 120s）
//   - httpStream：SSE 流式请求，无总超时（长回复不被截断），
//     仅靠 Transport.ResponseHeaderTimeout 做首字节兜底（与 traework2api 一致）
type Client struct {
	httpJSON   *http.Client
	httpStream *http.Client
}

// New 构造客户端。timeout 同时用作 JSON 总超时与流式首字节超时。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	streamTransport := http.DefaultTransport.(*http.Transport).Clone()
	streamTransport.ResponseHeaderTimeout = timeout
	return &Client{
		httpJSON:   &http.Client{Timeout: timeout},
		httpStream: &http.Client{Transport: streamTransport},
	}
}

// doJSON 发请求并解析统一信封。CatPaw 云端有两套信封：
//   - 网关 catx.nocode.cn：{code, message, data, errorCode}（code=0 视为成功）
//   - 直连 ai.catpaw.meituan.com / 聊天 nocode.cn：{unifyCode, code, msg, data, success}
//     （success=true 视为成功；失败时 msg 是错误文案）
//
// 任何 HTTP >=400 必须先被视为错误；即使 body 恰好能被解成调用方期望的结构，
// 也不能绕过 transport status。随后才按 success/code 解释 2xx/3xx 的业务信封。
func (c *Client) doJSON(ctx context.Context, method, baseURL, path string, headers map[string]string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpJSON.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read upstream response %s: %w", path, err)
	}

	// 统一信封解析：success 字段存在 → 直连/聊天信封；否则按网关 code 字段。
	var envelope struct {
		Code      int             `json:"code"`
		UnifyCode int             `json:"unifyCode"`
		Message   string          `json:"message"` // 网关错误文案
		Msg       string          `json:"msg"`     // 直连/聊天错误文案
		Success   *bool           `json:"success"` // 指针区分“未出现”与 false
		Data      json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &envelope)

	apiError := func(defaultCode int) *ApiError {
		code := envelope.Code
		if code == 0 {
			code = envelope.UnifyCode
		}
		if code == 0 {
			code = defaultCode
		}
		msg := envelope.Msg
		if msg == "" {
			msg = envelope.Message
		}
		if msg == "" {
			msg = truncateStr(string(raw), 200)
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &ApiError{Code: code, Status: resp.StatusCode, Message: msg, Path: path}
	}

	// Transport status 是不可绕过的第一层门禁。旧实现把这段放在 raw fallback 之后，
	// 导致某些 4xx/5xx JSON 在成功 Unmarshal 后直接 return nil。
	if resp.StatusCode >= http.StatusBadRequest {
		return apiError(resp.StatusCode)
	}

	// 直连/聊天信封。
	if envelope.Success != nil {
		if !*envelope.Success {
			return apiError(envelope.Code)
		}
		if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
			if err := json.Unmarshal(envelope.Data, out); err != nil {
				return fmt.Errorf("decode upstream data %s: %w", path, err)
			}
		}
		return nil
	}

	// 网关信封：非 0/200 code 是业务错误。
	if envelope.Code != 0 && envelope.Code != 200 {
		return apiError(envelope.Code)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode upstream data %s: %w", path, err)
		}
		return nil
	}

	// 某些 credit web 端点没有统一信封，直接返回目标对象。
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode upstream response %s: %w", path, err)
	}
	return nil
}

// gatewayHeaders 网关 REST 请求头。
func gatewayHeaders(token string) map[string]string {
	return map[string]string{
		"X-Auth-Token": token,
		"Accept":       "application/json",
	}
}

// passportHeaders 直连请求头（stream/model/history）。
func passportHeaders(token, uid string) map[string]string {
	h := map[string]string{
		"X-Passport-Token": token,
		"Cookie":           "X-Passport-Token=" + token,
		"Accept":           "text/event-stream",
	}
	if uid != "" {
		h["user-mis-id"] = uid
	}
	return h
}

// nocodeHeaders 聊天请求头。
func nocodeHeaders(token string) map[string]string {
	return map[string]string{
		"access-token": token,
		"client-id":    ClientID,
		"Accept":       "application/json",
	}
}

// ---------------------------------------------------------------------------
// 认证
// ---------------------------------------------------------------------------

// LoginConfig 返回 {loginEntryUrl}。
func (c *Client) LoginConfig(ctx context.Context) (string, error) {
	var out struct {
		LoginEntryURL string `json:"loginEntryUrl"`
	}
	if err := c.doJSON(ctx, http.MethodGet, GatewayHost, EpLoginConfig, nil, nil, &out); err != nil {
		return "", err
	}
	if out.LoginEntryURL == "" {
		return "", fmt.Errorf("login-config: empty loginEntryUrl")
	}
	return out.LoginEntryURL, nil
}

// PollToken 轮询换取 token。
func (c *Client) PollToken(ctx context.Context, sid string) (string, error) {
	path := EpPollToken + "?sid=" + url.QueryEscape(sid)
	var token string
	if err := c.doJSON(ctx, http.MethodGet, GatewayHost, path, nil, nil, &token); err != nil {
		return "", err
	}
	return token, nil
}

// Ping 校验 token：只有 401/403 表示凭证确定失效；5xx/网络错误属于临时故障。
func (c *Client) Ping(ctx context.Context, token string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GatewayHost+EpAuthPing, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Auth-Token", token)
	resp, err := c.httpJSON.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return false, readErr
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		return true, nil
	default:
		msg := truncateStr(string(raw), 200)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return false, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: msg, Path: EpAuthPing}
	}
}

// UserInfo 当前登录用户。
type UserInfo struct {
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
}

func (u *UserInfo) UnmarshalJSON(b []byte) error {
	type Alias UserInfo
	aux := &struct {
		UserID any `json:"userId"`
		Alias
	}{
		Alias: Alias(*u),
	}
	if err := json.Unmarshal(b, aux); err != nil {
		return err
	}
	if aux.UserID != nil {
		switch v := aux.UserID.(type) {
		case float64:
			u.UserID = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			u.UserID = v
		}
	}
	u.UserName = aux.Alias.UserName
	return nil
}

// CurrentUser 查询当前用户（登录落盘用）。
func (c *Client) CurrentUser(ctx context.Context, token string) (*UserInfo, error) {
	var out UserInfo
	if err := c.doJSON(ctx, http.MethodGet, GatewayHost, EpCurrentUser, gatewayHeaders(token), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// 积分 / 额度
// ---------------------------------------------------------------------------

// RegisterResult 注册奖励响应（data 字段）。
type RegisterResult struct {
	RegistrationBonus      int    `json:"registrationBonus"`
	RegisterBonusValueYuan string `json:"registerBonusValueYuan"`
	RewardExpireDays       int    `json:"rewardExpireDays"`
	RewardLastValidDate    string `json:"rewardLastValidDate"`
	NewUser                bool   `json:"newUser"`
}

// Register 向积分中心注册并领取注册奖励（+N 免费额度）。幂等：老用户 newUser=false。
func (c *Client) Register(ctx context.Context, token string) (*RegisterResult, error) {
	var out RegisterResult
	body := map[string]any{"registerChannel": RegisterChannel}
	if err := c.doJSON(ctx, http.MethodPost, GatewayHost, EpCreditRegister, gatewayHeaders(token), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreditBalance 查询积分余额（data 直接返回，字段以 availableCredits 为主）。
func (c *Client) CreditBalance(ctx context.Context, token string) (map[string]any, error) {
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodGet, GatewayHost, EpCreditBalance, gatewayHeaders(token), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseCredits 从 balance 响应提取可用积分。
func ParseCredits(balance map[string]any) int64 {
	if balance == nil {
		return 0
	}
	switch v := balance["availableCredits"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return int64(f)
	}
	return 0
}

// CampaignInit 触发积分中心活动初始化（可能发放活动额度）。
func (c *Client) CampaignInit(ctx context.Context, token string) (map[string]any, error) {
	var out map[string]any
	headers := map[string]string{
		"X-Passport-Token": token,
		"Cookie":           "X-Passport-Token=" + token,
		"Accept":           "application/json",
	}
	if err := c.doJSON(ctx, http.MethodPost, CreditWebHost, EpCampaignInit, headers, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 模型
// ---------------------------------------------------------------------------

// ModelInfo 模型信息（来自 /api/agent/maas/model-types 的 data 项）。
type ModelInfo struct {
	ModelTypeName   string `json:"modelTypeName"`
	CatPawModelType string `json:"catPawModelType"`
	Description     string `json:"description"`
	SupportImage    bool   `json:"supportImage"`
	ModelTypeID     int    `json:"modelTypeId"`
	ExtendedInfo    struct {
		MarketingCopy  string `json:"marketingCopy"`
		ShortName      string `json:"shortName"`
		RateMultiplier string `json:"rateMultiplier"`
		IsAuto         string `json:"isAuto"`
	} `json:"extendedInfo"`
}

// ID 返回用于 OpenAI /v1/models 与 chat 请求的模型 id（= ModelTypeName）。
func (m ModelInfo) ID() string { return m.ModelTypeName }

// FetchModels 拉取模型表 POST /api/agent/maas/model-types。
func (c *Client) FetchModels(ctx context.Context, token, uid string) ([]ModelInfo, error) {
	headers := passportHeaders(token, uid)
	headers["Accept"] = "application/json"
	headers["M-TRACEID"] = fmt.Sprintf("%d", time.Now().UnixNano())
	headers["M-APPKEY"] = "fe_com.sankuai.catpaw.external.front"
	body := map[string]any{
		"tenant": DefaultTenant,
		"scene":  DefaultScene,
		"env":    DefaultEnv,
	}
	var out []ModelInfo
	if err := c.doJSON(ctx, http.MethodPost, DirectHost, EpModelTypes, headers, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// CreateChat 创建聊天（nocode 流程第一步）。
func (c *Client) CreateChat(ctx context.Context, token, chatID, prompt string) error {
	body := map[string]any{
		"chatId":              chatID,
		"source":              "catdesk",
		"prompt":              prompt,
		"images":              []any{},
		"files":               []any{},
		"type":                "COMMON",
		"techStackTemplateId": "default",
	}
	var out any
	return c.doJSON(ctx, http.MethodPost, NocodeHost, EpChatCreate, nocodeHeaders(token), body, &out)
}

// AgentStreamResp agent-stream 响应 data。
type AgentStreamResp struct {
	ConversationID  string `json:"conversationId"`
	StreamMessageID string `json:"streamMessageId"`
}

// SendMessage 发送用户消息（nocode 流程第二步），返回 conversationId。
func (c *Client) SendMessage(ctx context.Context, token, chatID, prompt, model string) (*AgentStreamResp, error) {
	createTime := time.Now().UTC().Format(time.RFC3339Nano)
	body := map[string]any{
		"chatId": chatID,
		"messages": []any{
			map[string]any{
				"role":       "user",
				"content":    []any{map[string]any{"type": "text", "text": prompt}},
				"createTime": createTime,
			},
		},
		"files":               []any{},
		"urls":                []any{},
		"manualToolName":      "",
		"toolMessageId":       "",
		"isAgent":             true,
		"currentPath":         "/",
		"techStackTemplateId": "default",
		"sourcePlatform":      "web",
		"isImportProject":     false,
		"frontCreateTime":     time.Now().UnixMilli(),
	}
	if model != "" && model != ExactModelAuto {
		body["model"] = model
	}
	var out AgentStreamResp
	if err := c.doJSON(ctx, http.MethodPost, NocodeHost, EpChatAgentStream, nocodeHeaders(token), body, &out); err != nil {
		return nil, err
	}
	if out.ConversationID == "" {
		return nil, fmt.Errorf("agent-stream: empty conversationId")
	}
	return &out, nil
}

// ConnectStream 订阅 /api/agent/stream/connect SSE；调用方负责 Close。
func (c *Client) ConnectStream(ctx context.Context, token, conversationID string) (io.ReadCloser, error) {
	body := map[string]any{
		"timestamp":       time.Now().UnixMilli(),
		"conversationId":  conversationID,
		"messageIndex":    0,
		"incrementalOnly": true,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DirectHost+EpStreamConnect, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	h := passportHeaders(token, "")
	h["Content-Type"] = "application/json"
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := c.httpStream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode, Message: truncateStr(string(rawBody), 200), Path: EpStreamConnect}
	}
	return resp.Body, nil
}

// FetchHistory 拉取会话历史（备用/调试）。
func (c *Client) FetchHistory(ctx context.Context, token, conversationID string, size int) (map[string]any, error) {
	if size <= 0 {
		size = 50
	}
	path := EpChatHistory + "?page=1&pageSize=" + fmt.Sprint(size) + "&conversationId=" + url.QueryEscape(conversationID) + "&size=" + fmt.Sprint(size)
	headers := passportHeaders(token, "")
	headers["Accept"] = "application/json"
	var out map[string]any
	if err := c.doJSON(ctx, http.MethodGet, DirectHost, path, headers, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 轮询拿回复（free 计划 SSE 不吐数据，纯 HTTP 的唯一可靠路径）
// ---------------------------------------------------------------------------

// PollOpts 控制 PollAssistant 行为。
type PollOpts struct {
	Interval time.Duration // 轮询间隔，默认 500ms
	Timeout  time.Duration // 总超时，默认 180s
}

// AssistantResult 一轮对话的完整结果。
type AssistantResult struct {
	Content   string
	Reasoning string
	Usage     map[string]any
	Finish    string
	ErrCode   string
}

// PollAssistant 轮询 history 直到 assistant 消息出现且 finished=true，或超时/出错。
func (c *Client) PollAssistant(ctx context.Context, token, uid, conversationID string, opts PollOpts) (*AssistantResult, error) {
	if opts.Interval <= 0 {
		opts.Interval = 500 * time.Millisecond
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 180 * time.Second
	}
	deadline := time.Now().Add(opts.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	path := EpChatHistory + "?page=1&pageSize=50&conversationId=" + url.QueryEscape(conversationID) + "&size=50"
	headers := passportHeaders(token, uid)
	headers["Accept"] = "application/json"

	var attempt int
	for {
		attempt++
		var data struct {
			Items []struct {
				Type     string `json:"type"`
				Status   int    `json:"status"`
				Finished bool   `json:"finished"`
				Content  []struct {
					Type      string `json:"type"`
					Text      string `json:"text"`
					Reasoning string `json:"reasoning"`
				} `json:"content"`
				TotalUsage map[string]any `json:"totalUsage"`
			} `json:"items"`
		}
		if err := c.doJSON(ctx, http.MethodGet, DirectHost, path, headers, nil, &data); err != nil {
			return nil, err
		}
		var assistant = -1
		var userUsage map[string]any
		for i, it := range data.Items {
			if it.Type == "user" && it.TotalUsage != nil {
				userUsage = it.TotalUsage
			}
			if it.Type == "assistant" {
				assistant = i
				break
			}
		}
		if assistant >= 0 {
			it := data.Items[assistant]
			if it.Status != 0 && it.Status != 1 {
				return nil, &UpstreamError{Code: fmt.Sprintf("status_%d", it.Status), Msg: "assistant generation failed"}
			}
			var sb strings.Builder
			var rb strings.Builder
			for _, c := range it.Content {
				switch c.Type {
				case "text":
					sb.WriteString(c.Text)
				case "reasoning":
					if c.Reasoning != "" {
						rb.WriteString(c.Reasoning)
					} else {
						rb.WriteString(c.Text)
					}
				}
			}
			finish := "stop"
			if !it.Finished {
				finish = "length"
			}
			return &AssistantResult{
				Content:   sb.String(),
				Reasoning: rb.String(),
				Usage:     userUsage,
				Finish:    finish,
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("poll assistant timeout after %s (%d attempts, conv=%s)", opts.Timeout, attempt, conversationID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(opts.Interval):
		}
	}
}

// Log 打印上游日志（统一前缀便于排查）。
func Log(format string, args ...any) {
	log.Printf("upstream: "+format, args...)
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
