# wbproxy — WorkBuddy AI 模型反代

把 WorkBuddy AI 桌面端的登录态转成 OpenAI 兼容 API，主用 `deepseek-v4.1-flash`。
单二进制，纯 Go 标准库，macOS / Linux 通用。

## 原理

- WorkBuddy AI（腾讯 CodeBuddy 同源）登录后，凭据存在
  `~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/workbuddy-desktop-ai.info`
- 模型推理上游是 `https://www.workbuddy.ai/v2/chat/completions`（OpenAI 协议、仅流式、首条必须 system）
- 鉴权：`Authorization: Bearer <accessToken>` + `X-User-Id` + `X-Domain`
- wbproxy 持有这套 token，对外暴露标准 `/v1/chat/completions`、`/v1/models`；
  非流式请求自动转流式并聚合成普通 completion；token 过期自动用 refreshToken 刷新并回写

## 本机运行（已验证）

```bash
cd "/Users/limin/Documents/ceshi/开开开/workbuddy-proxy"
go build -o wbproxy . && ./wbproxy -listen 127.0.0.1:8080
```

任何 OpenAI 客户端：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}'
```

流式加 `"stream": true`，与 OpenAI SSE 格式一致。

## 部署到服务器

1. 拷贝两个文件到服务器（如 `/opt/wbproxy/`）：
   - `wbproxy-linux-amd64`（或 `wbproxy-linux-arm64`，按服务器架构）
   - `auth.json`（登录态，从本机导出）

2. systemd 服务 `/etc/systemd/system/wbproxy.service`：

```ini
[Unit]
Description=WorkBuddy model reverse proxy
After=network-online.target

[Service]
ExecStart=/opt/wbproxy/wbproxy-linux-amd64 -auth /opt/wbproxy/auth.json -listen 127.0.0.1:8080 -key sk-换一个自己的密钥
Restart=always
RestartSec=5
User=www-data

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now wbproxy
```

3. 对外暴露：前面套 Nginx/Caddy 做 TLS + 域名；或本机 SSH 隧道 `ssh -L 8080:127.0.0.1:8080 server`。
   `-key` 必设后所有请求要带 `Authorization: Bearer sk-换一个自己的密钥`。

## 参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-auth` | macOS 登录态路径 | 登录态文件（auth.info 或导出的 auth.json） |
| `-upstream` | `https://www.workbuddy.ai` | 上游 |
| `-listen` | `:8080` | 监听地址 |
| `-key` | 空 | 门禁密钥；空则不鉴权（仅限本机） |

`/health` 可看 token 过期时间。

## 注意

- token 有效期到 2027-09，refreshToken 同寿命；但桌面端重新登录/登出会使旧 token 作废，届时重新导出 auth.json
- 上游有频控，高频调用可能触发限流；自用没问题
- 免费额度策略随腾讯侧调整，走的是你 WorkBuddy 账号自身的配额
