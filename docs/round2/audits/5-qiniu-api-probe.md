# 七牛接口实测（2026-10-29，宿主 curl，--noproxy）

## llmapi.qiniu.io
- 根路径是 “XHub API - Swagger UI”，`/openapi.json` 标题 XHub API、版本 1.95.32，共 533 个路由，错误格式与 LiteLLM 代理一致。
- 相关路由：`GET /v1/models`（需 API key）、`POST /v1/chat/completions`、`POST /v1/responses`（及 compact、cancel、input_items）、`POST /v1/messages`、`POST /v1/messages/count_tokens`、`/anthropic/{endpoint}`、`GET /key/info`、`GET /public/model_hub`（无需鉴权，返回 `[]`）。
- 不带 key：`/v1/models`、`/models` 返回 401 `auth_error`（No api key passed in）。
- 带钥匙串里的比赛 key（sbx-builderday-key）：401 `token_not_found_in_db`。比赛 key 在 llmapi 上无效，需要单独的七牛 key。

## xapi.qiniu.io（比赛接口）
- 根路径标题 “XHub 控制台”，`/openapi.json` 404。
- 带比赛 key：`GET /v1/models` 200，OpenAI 格式 `{"object":"list","data":[{"id","object","created","owned_by"}]}`，5 个模型：gpt-6-astra、gpt-6.1-sol、gpt-6-sol、gpt-5.6-terra、gpt-5.6-sol。CLAUDE.md 里记的 gpt-5.5 已不在列表中，说明写死的模型名会过期。
- `GET /v1/model/info` 403，`GET /key/info` 400，暂时拿不到单价和 key 额度。

## 推论
- Codex：基址 `https://llmapi.qiniu.io/v1`，Responses API。
- Claude Code：`ANTHROPIC_BASE_URL=https://llmapi.qiniu.io`（客户端自己拼 `/v1/messages`），需用真实七牛 key 验证模型名与工具调用。
- dsh：按其协议选 `/v1/chat/completions` 或 Anthropic，需实测。
- 模型列表：宿主侧 `GET {base}/models`，Bearer 认证，解析 `data[].id`。
