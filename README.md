# 🚀 CommandCode Proxy (Go)

**A minimal, high-performance HTTP proxy with built-in SQLite analytics for the CommandCode `alpha/generate` API.**
Designed with an **OpenAI Chat Completions–compatible interface**, making it a **drop-in replacement** for:

* LiteLLM
* OpenAI SDKs
* LangChain (`langchain.openai`)
* [9Router](https://github.com/decolua/9router)
* Any OpenAI-compatible client

⚡ **Plug it in as your `base_url` and you're ready to go.**

---

## 💡 Why Use This?

* 🔥 Unified API layer for multiple providers
* 📊 Built-in analytics (SQLite)
* ⚙️ Simple, lightweight, and easy to deploy
* 🔌 Fully compatible with existing OpenAI ecosystem tools

---

## 💰 Get Access

You can join for only **$1/month**:

[https://commandcode.ai](https://commandcode.ai/haemeto)

---

## Endpoints

| Method | Path                    | Description                                                |
| -----: | ----------------------- | ---------------------------------------------------------- |
|   POST | `/alpha/generate`       | Native passthrough to `${COMMAND_CODE_API_URL}/alpha/generate` |
|   POST | `/v1/chat/completions`  | OpenAI-compatible (supports `stream:true` SSE)             |
|    GET | `/v1/models`            | OpenAI-shaped model list (env list ∪ models seen in analytics) |
|    GET | `/analytics`            | JSON aggregate (totals, by-model, by-provider, time series)|
|    GET | `/analytics/recent`     | Last N requests (`?limit=N`, default 50, max 1000)         |
|    GET | `/dashboard`            | HTML dashboard (Chart.js) auto-refreshes every 10 s        |
|    GET | `/healthz`              | Simple liveness check                                      |

`POST` paths set `Content-Type: application/json`. CORS is permissive
by default; tighten via `CORS_ORIGINS=https://your-app.example.com`.

## Configuration

All knobs are environment variables. None are required for local boot:

| Variable                 | Default                          | Description                                                          |
| ------------------------ | -------------------------------- | -------------------------------------------------------------------- |
| `PORT`                   | `3000`                           | TCP listen port                                                      |
| `COMMAND_CODE_API_URL`   | `https://api.commandcode.ai`     | Upstream base URL                                                    |
| `COMMAND_CODE_TOKEN`     | (empty)                          | Default `Bearer` token used when client omits `Authorization`        |
| `COMMAND_CODE_VERSION`   | `0.18.10`                        | Default `X-Command-Code-Version` header                              |
| `ANALYTICS_DB_PATH`      | `./data/analytics.db`            | SQLite database file (auto-created)                                  |
| `CORS_ORIGINS`           | `*`                              | Comma-separated allow list                                           |
| `ADMIN_TOKEN`            | (empty)                          | If set, gates `/analytics*` and `/dashboard` (header or `?token=`)   |
| `OPENAI_MODELS`          | `moonshotai/Kimi-K2.5`           | Static list advertised by `GET /v1/models` (any model used through `/v1/chat/completions` is auto-added) |

## Build & run

Requires Go 1.22+ (uses `embed` + standard library only beside SQLite).

```bash
cd commandcode-proxy

# build a static binary
go build -o bin/commandcode-proxy ./cmd/proxy

# or run in place
go run ./cmd/proxy
```

By default the server listens on `:3000`. Open
`http://localhost:3100/dashboard` to view analytics.

## Usage

The proxy exposes two surfaces: the **native** CommandCode shape
(identical to `api.commandcode.ai`) at `/alpha/generate`, and an
**OpenAI-compatible** shape at `/v1/chat/completions` + `/v1/models`.
Use whichever fits your client. The OpenAI surface is what you point
 LiteLLM / OpenAI SDKs at.

> Throughout the examples below, replace
> `user_xxx_your_commandcode_token` with your real `user_…` CommandCode
> token. If you set `COMMAND_CODE_TOKEN` in the proxy's environment,
> clients can omit the `Authorization` header entirely and the proxy
> will inject it.

### 1. curl — native passthrough (`/alpha/generate`)

Identical to the upstream curl from the issue, with the host swapped to
`localhost:3100`. Returns the raw CommandCode response shape (`id`,
`content`, `usage.input_tokens`, etc.):

```bash
curl --request POST \
  --url http://localhost:3100/alpha/generate \
  --header 'authorization: Bearer user_xxx_your_commandcode_token' \
  --header 'content-type: application/json' \
  --header 'x-command-code-version: 0.18.10' \
  --data '{
    "memory": "",
    "params": {
      "provider": "command-code",
      "model": "moonshotai/Kimi-K2.5",
      "messages": [{"role": "user", "content": "Hello"}]
    },
    "config": {
      "workingDir": "",
      "date": "2026-05-05",
      "environment": "linux",
      "structure": [],
      "isGitRepo": false,
      "currentBranch": "",
      "mainBranch": "main",
      "gitStatus": "",
      "recentCommits": []
    }
  }'
```

### 2. curl — OpenAI-compatible (`/v1/chat/completions`)

Same wire format as `https://api.openai.com/v1/chat/completions`. The
proxy translates the request to CommandCode shape, forwards it, then
translates the response back. The `config` block is filled in with sane
defaults so you don't have to send it.

> **Any model is accepted.** `/v1/chat/completions` does not gate on
> `OPENAI_MODELS` — the `model` field is forwarded verbatim to upstream
> as `params.model`. Use any model identifier CommandCode supports, even
> if it's not in your env list. Models actually used will start showing
> up in `GET /v1/models` automatically (they're discovered from
> analytics history).
>
> Translation rules applied to the request (so OpenAI SDKs
> work without modification):
> - `role: "system"` is folded into the next user message (CommandCode
>   only accepts `user` / `assistant` / `tool`).
> - `content` is wrapped as `[{type:"text", text:"..."}]` (CommandCode
>   uses Anthropic-style content blocks; plain strings are rejected).
> - Multimodal `image_url` parts become `[image omitted]` placeholders.
> - Unknown roles (`developer`, `function`, …) are coerced to `user`.

**Non-stream:**

```bash
curl --request POST \
  --url http://localhost:3100/v1/chat/completions \
  --header 'Authorization: Bearer user_xxx_your_commandcode_token' \
  --header 'Content-Type: application/json' \
  --data '{
    "model": "moonshotai/Kimi-K2.5",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Returns:

```json
{
  "id": "msg_…",
  "object": "chat.completion",
  "model": "moonshotai/Kimi-K2.5",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": " Hello! How can I help you today?"},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 7280, "completion_tokens": 10, "total_tokens": 7290}
}
```

**Streaming (`stream: true` → Server-Sent Events):**

```bash
curl --request POST \
  --no-buffer \
  --url http://localhost:3100/v1/chat/completions \
  --header 'Authorization: Bearer user_xxx_your_commandcode_token' \
  --header 'Content-Type: application/json' \
  --data '{
    "model": "moonshotai/Kimi-K2.5",
    "stream": true,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Frames are emitted as
`role` chunk → `content` chunk → `finish_reason` chunk → `data: [DONE]`,
exactly matching the OpenAI streaming contract.

### 3. curl — model list (`/v1/models`)

```bash
curl http://localhost:3100/v1/models
```

Returns an OpenAI-shaped list. Sources are merged (env first, then
models the proxy has actually been used with, deduped):

```json
{
  "object": "list",
  "data": [
    {"id": "moonshotai/Kimi-K2.5", "object": "model", "owned_by": "command-code"}
  ]
}
```

### 4. OpenAI SDK — Python

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3100/v1",
    api_key="user_xxx_your_commandcode_token",
)

resp = client.chat.completions.create(
    model="moonshotai/Kimi-K2.5",
    messages=[{"role": "user", "content": "Hello"}],
)
print(resp.choices[0].message.content)

# Streaming
for chunk in client.chat.completions.create(
    model="moonshotai/Kimi-K2.5",
    messages=[{"role": "user", "content": "Hi"}],
    stream=True,
):
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

### 5. OpenAI SDK — JavaScript / Node

```js
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:3100/v1",
  apiKey: "user_xxx_your_commandcode_token",
});

const r = await client.chat.completions.create({
  model: "moonshotai/Kimi-K2.5",
  messages: [{ role: "user", content: "Hello" }],
});
console.log(r.choices[0].message.content);
```


### 6. LiteLLM

```yaml
# litellm_config.yaml
model_list:
  - model_name: kimi-k25
    litellm_params:
      model: openai/moonshotai/Kimi-K2.5  # the "openai/" prefix tells LiteLLM to use the OpenAI provider
      api_base: http://your-proxy.example.com/v1
      api_key: user_xxx_your_commandcode_token
```

```bash
litellm --config litellm_config.yaml
```

Then any client targeting LiteLLM with `model: kimi-k25` is transparently
routed through this proxy → CommandCode.

### 7. langchain_openai

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    model="moonshotai/Kimi-K2.5",
    base_url="http://localhost:3100/v1",
    api_key="user_xxx_your_commandcode_token",
)
print(llm.invoke("Hello").content)
```

### 8. 9Router

**9Router** uses the **OpenAI Chat Completions** format, so it can directly connect to your proxy (CommandCode) as an *OpenAI-compatible provider*.

---

## Setup Provider in 9Router

Go to the dashboard:

👉 `/dashboard/providers` → **Add OpenAI Compatible**

Fill in the form:

* **Name**: anything (example: `cmdcode-local`)
* **Prefix**: anything (example: `cmdcode`)
* **API Type**: `Chat Completions`
* **Base URL**:

  ```
  http://localhost:3100/v1
  ```
* **API Key**:

  ```
  user_xxx_your_commandcode_token
  ```

Click **Save**

---

## Add Connection

After creating the provider:

1. Click the provider you just created
2. Go to the **Connections** section
3. Click **Add Connection**
4. Fill in the **same values** as the provider:

   * **Base URL**:

     ```
     http://localhost:3100/v1
     ```
   * **API Key**:

     ```
     user_xxx_your_commandcode_token
     ```

Click **Save**

---

## Add Models

In the **Available Models** section, you have two options:

### Manual

* Add models manually (example):

  ```
  moonshotai/Kimi-K2.5
  ```

### Import from Models

* Use **Import from Models**
* Make sure your `.env` includes:

  ```
  OPENAI_MODELS=moonshotai/Kimi-K2.5,...
  ```

---

## Important Notes

* All requests will be routed through your proxy (`http://localhost:3100`), not directly to OpenAI or OpenRouter
* The API key used here is your **CommandCode token**
* Ensure the models you use are availa


## Analytics

Every request is recorded to SQLite with timestamp, path, status code,
duration, model, provider, input / output / cache tokens, message id,
stop reason, IP, user-agent, version header, and any error message.
Body contents are **not** stored — only metadata.

`GET /analytics` JSON example:

```json
{
  "totals": {
    "total_requests": 4,
    "success_count": 4,
    "error_count": 0,
    "total_input_tokens": 29120,
    "total_output_tokens": 40,
    ...
  },
  "by_model":    [{"model": "moonshotai/Kimi-K2.5", "count": 4, ...}],
  "by_provider": [{"provider": "command-code", "count": 4, ...}],
  "last_24h_hours": [{"bucket": "2026-05-05T01", "count": 4, ...}],
  "last_30d_days":  [{"bucket": "2026-05-05",    "count": 4, ...}]
}
```

If `ADMIN_TOKEN` is set, send it as the `X-Admin-Token` header or a
`?token=` query parameter.

### Request inspector (modal)

For every recorded request, the dashboard's recent-requests table now
shows a **View** button that opens a modal with four panels:

| Panel | What it contains |
|---|---|
| Client request | The raw body the client sent to the proxy (OpenAI / OpenRouter shape on the `/v1/...` path; CommandCode shape on `/alpha/generate`) |
| Upstream request | The body the proxy actually sent to CommandCode (after OpenAI → CommandCode translation if applicable) |
| Upstream response | The raw response CommandCode returned (or the upstream error JSON if it 4xx'd) |
| Client response | The body the proxy returned to the client (translated OpenAI shape, or the SSE frame stream for `stream:true`) |

This is the recommended starting point when debugging integrations like
OpenRouter — if the upstream returns a 400, the **Upstream response**
panel will show CommandCode's exact rejection reason, and the
**Upstream request** panel will show the body that triggered it.

Bodies are capped at **64 KB per column** (truncation marker appended)
and only readable through the admin-gated endpoint. The same data is
also available as JSON at `GET /analytics/detail/{id}` (admin token
required).

## Testing

```bash
go test ./...      # unit + e2e (uses httptest mock upstream)
go vet  ./...
go build ./...
```

The test suite uses an in-process `httptest.Server` to emulate the
CommandCode upstream with the exact response shape from the issue
(`input_tokens=7280`, `output_tokens=10`, `id=msg_…`). This way the
parser, analytics, and OpenAI translator are exercised identically to
production without burning real tokens.

## Project layout

```
commandcode-proxy/
├── cmd/proxy/main.go              # entry point, .env loader, signal handling
├── internal/
│   ├── analytics/                 # SQLite schema + queries + summary
│   ├── config/                    # env-var loader
│   ├── openai/                    # OpenAI <-> CommandCode translation + handlers
│   ├── proxy/                     # forward-to-upstream + analytics recording
│   └── server/
│       ├── server.go              # routing, CORS, admin guard, dashboard
│       └── dashboard_assets/
│           └── dashboard.html     # embedded with go:embed
├── go.mod / go.sum
├── .env.example
└── README.md
```
