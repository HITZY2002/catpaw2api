// 常量：CatPaw 直连云端 API 的端点与鉴权方式（来自 app.asar 逆向，勿轻易改动）。
package upstream

// Host 是 package 级变量而非常量：测试里可替换为 httptest.Server 地址。
// 生产代码不应修改它们。
var (
	// GatewayHost 个人版网关：登录、REST、积分（X-Auth-Token 鉴权）。
	GatewayHost = "https://catx.nocode.cn"
	// DirectHost 直连服务：流式对话、模型表、历史（X-Passport-Token / Cookie 鉴权）。
	DirectHost = "https://ai.catpaw.meituan.com"
	// NocodeHost 聊天创建/发送（access-token + client-id 鉴权）。
	NocodeHost = "https://nocode.cn"
	// CreditWebHost 积分中心网页 API（campaign 等）。
	CreditWebHost = "https://credit.catpaw.meituan.com"
)

const (
	// ClientID 是 product.json auth.provider.clientId（catx-passport）。
	ClientID = "2f73f89b68"

	// 端点
	EpLoginConfig     = "/api/gateway/passport/login-config"
	EpPollToken       = "/api/gateway/passport/poll-token"
	EpCurrentUser     = "/api/gateway/passport/current-user"
	EpAuthPing        = "/api/gateway/auth/ping"
	EpCreditBalance   = "/api/gateway/credit/balance"
	EpCreditRegister  = "/api/gateway/credit/register"
	EpModelTypes      = "/api/agent/maas/model-types"
	EpStreamConnect   = "/api/agent/stream/connect" // free 计划不吐数据，保留供未来/pro 账号
	EpChatHistory     = "/api/agent/conversation/history"
	EpChatCreate      = "/api/chat/create"
	EpChatAgentStream = "/api/chat/agent-stream"
	EpCampaignInit    = "/api/campaign/init-after-login"

	// RegisterChannel 桌面端注册渠道标识。
	RegisterChannel = "CATPAW_PC"

	// model-types 请求体字段。逆向自 app.asar 的 ModelTypes 模块：
	//   fetchModelTypes: A("/api/agent/maas/model-types", {tenant:"CatDesk", scene:"CATX_APP", env:"EXTERNAL"})
	// tenant/scene/env 为必填，缺失报 400 "tenant 不能为空; scene 不能为空"。
	DefaultTenant = "CatDesk"
	DefaultScene  = "CATX_APP"
	DefaultEnv    = "EXTERNAL"

	// 真实模型名（逆向自 app.asar，已在 free 计划账号上实测全部返回正常回复）。
	// 这 7 个是 /api/agent/maas/model-types 在 tenant=CatDesk,scene=CATX_APP,env=EXTERNAL 下返回的完整列表。
	ExactModelAuto            = "auto"
	ExactModelLongCat2        = "LongCat-2.0" // 限时免费
	ExactModelDeepseekV4Flash = "deepseek-v4-flash"
	ExactModelDeepseekV4Pro   = "deepseek-v4-pro"
	ExactModelGLM52           = "glm-5.2"
	ExactModelMiniMaxM3       = "MiniMax-M3"
	ExactModelKimiK3          = "kimi-k3"
)
