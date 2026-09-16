# CatPaw2API

> CatPaw（美团 CatDesk）免费额度的 OpenAI 兼容代理。**无需安装/运行 CatPaw 客户端**，
> 纯 Go 直连云端 API，多账号轮转。

## 参考项目

本项目是 [Sliverkiss](https://github.com/Sliverkiss) 同系列开源项目的延伸实现，架构与运维形态参考了以下仓库：

- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) — WorkBuddy CN OpenAI 兼容反代（账号池 / 轮转 / 签到架构）
- [traework2api](https://github.com/Sliverkiss/traework2api) — TRAE Work OpenAI 兼容反代（零依赖 Go 骨架）
- [qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) — QoderWork CN OpenAI 兼容反代（OAuth 授权流程）

感谢原作者的开源与优秀设计。

## 安全默认

- 服务 **必须配置 `CP2A_API_KEY`**；缺失或使用 `changeme` / `change-me` 等示例值时拒绝启动。
- 裸机默认监听 `127.0.0.1:7867`，不会自动暴露到局域网/公网。
- Docker 主机端默认同样只绑定 `127.0.0.1:7867`。只有确认网络边界和防火墙后，才建议显式设置 `CP2A_BIND_ADDR=0.0.0.0`。
- `auths/*.json` 包含上游 access token，程序以 `0600` 权限写入；不要提交到 Git。
- `/healthz` 不需要 API Key，仅用于存活检查；OpenAI API 和 Admin API 均要求 Bearer key。

## 快速开始（Ubuntu / Linux）

### 1. 编译

```bash
make linux        # 产物在 bin/（纯静态，无 CGO 依赖）
make test         # 跑单测
```

### 2. 登录账号

```bash
# 服务器（无浏览器）：打印登录链接，轮询等待
./login.sh -print-only
#   ① 在任意机器浏览器打开打印的链接完成登录
#   ② 浏览器跳到打不开的 127.0.0.1 属正常，token 会自动轮询下发

# 本机有浏览器：直接打开浏览器登录
./login.sh

# 凭证落盘 auths/catpaw-{uid}.json
```

本地浏览器回调会校验 OAuth `state`，不接受不属于当前登录会话的 token。

### 3. 配置 & 启动

```bash
cp config.example.json config.json
export CP2A_API_KEY="$(openssl rand -hex 24)"
./bin/catpaw2api -config config.json
```

默认地址：`127.0.0.1:7867`。

### 4. 验证 + WebUI

```bash
curl http://127.0.0.1:7867/healthz
curl http://127.0.0.1:7867/v1/models \
  -H "Authorization: Bearer $CP2A_API_KEY"

curl -X POST http://127.0.0.1:7867/v1/chat/completions \
  -H "Authorization: Bearer $CP2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}]}'
```

浏览器打开 **http://127.0.0.1:7867/** 即 WebUI 控制台：账号余额/状态、对话测试（流式/非流式）。首次使用输入 `CP2A_API_KEY` 即可（只存在当前浏览器会话）。WebUI 是纯静态单页，无外部 CDN 依赖。

多轮对话默认按账号自动续接上下文；也可用请求头 `X-Catpaw-Conversation-Id: <conversationId>` 或 body 里的 `conversation_id` 显式指定会话。

## 部署

### systemd

```bash
sudo mkdir -p /opt/catpaw2api/{auths,data}
sudo cp -r bin config.example.json deploy /opt/catpaw2api/
sudo cp /opt/catpaw2api/config.example.json /opt/catpaw2api/config.json

# 生成服务 API Key；EnvironmentFile 权限建议 0600
printf 'CP2A_API_KEY=%s\n' "$(openssl rand -hex 24)" | sudo tee /opt/catpaw2api/.env >/dev/null
sudo chmod 600 /opt/catpaw2api/.env

sudo cp deploy/catpaw2api.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now catpaw2api
sudo journalctl -u catpaw2api -f
```

如需监听非 loopback 地址，请显式设置 `CP2A_LISTEN`，并同时配置防火墙/反向代理。

### Docker Compose

```bash
cp config.example.json config.json
cp .env.example .env

# 把随机 key 写入 .env
KEY="$(openssl rand -hex 24)"
printf 'CP2A_API_KEY=%s\nCP2A_BIND_ADDR=127.0.0.1\n' "$KEY" > .env
chmod 600 .env

mkdir -p auths data
docker compose up -d --build
docker compose ps
```

默认只可从宿主机访问 `127.0.0.1:7867`。确有局域网/公网暴露需求时：

```bash
CP2A_BIND_ADDR=0.0.0.0 docker compose up -d
```

这会扩大攻击面；请确保 API Key、防火墙和反向代理配置正确。

## 配置

`CP2A_API_KEY` 只从环境变量读取，不写入 `config.json`。常用覆盖项：

```text
CP2A_LISTEN
CP2A_AUTH_DIR
CP2A_STATE_FILE
CP2A_DEFAULT_MODEL
CP2A_UPSTREAM_TIMEOUT_SECONDS
CP2A_QUOTA_ENABLED
CP2A_QUOTA_POLL_MINUTES
CP2A_QUOTA_THRESHOLD
CP2A_QUOTA_METHOD
CP2A_QUOTA_COOLDOWN_HOURS
CP2A_QUOTA_REGISTER_ON_START
CP2A_QUOTA_AUTO_RENEW
CP2A_QUOTA_RENEW_THRESHOLD_HOURS
```

环境变量格式错误会直接导致启动失败，而不是静默回退默认值。详见 `.env.example`。

`quota.enabled=false` 只关闭额度轮询/自动申请；`quota.auto_renew=true` 时 token 续期仍独立运行。自动续期拿到新 token 后会核验实际登录 UID，避免浏览器登录了另一个账号后覆盖目标账号凭证。

## 开发与 CI

GitHub Actions 会执行：

```text
gofmt check
go test ./...
go test -race ./...
go vet ./...
go build ./...
docker build
```

提交前建议本地至少运行：

```bash
gofmt -w .
go test ./...
go vet ./...
go build ./...
```

## 目录结构

```text
cmd/server/         HTTP 服务（config + main）
cmd/login/          浏览器登录 → auths/catpaw-{uid}.json
cmd/credit/         余额查询 + 手动申请额度
cmd/apply/          批量自动申请额度
internal/auth/      auth 文件读写
internal/upstream/  云端客户端（网关/直连/聊天/SSE）+ 常量
internal/pool/      账号池（token 校验/冷却/禁用）
internal/scheduler/ 余额看门狗 + token 自动续期
internal/server/    OpenAI 兼容路由
internal/webui/     内嵌 WebUI 控制台
deploy/             systemd unit 样例
docs/               逆向过程与接口清单
```

## 免责声明

仅供学习和研究使用。使用者需遵守 CatPaw 服务条款，自行承担使用风险。

## License

MIT
