# wbproxy Zeabur 部署参数（本文件含敏感值，勿提交/勿外发）

## 环境变量（Zeabur 服务设置里填）
- AUTH_JSON: <auth.json 的完整内容，单行>
- GATE_KEY: sk-wb-work-2026（newapi 渠道里填这个）

## 模型名
- 对外: deepseek-v4.1-flash-work
- wbproxy 收到后映射为 deepseek-v4.1-flash（由 newapi 渠道做映射）
