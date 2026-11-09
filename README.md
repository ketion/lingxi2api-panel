<p align="center">
  <b>lingxi2api</b><br>
  把 WPS 灵犀账号变成 OpenAI 兼容 API 的多账号网关 · 附 Web 管理面板
</p>

---

`lingxi2api` 是一个把 **WPS 灵犀** 账号池暴露成 **OpenAI 兼容 API** 的反代网关。
它让任意 OpenAI SDK / 客户端（Cherry Studio、ChatBox、沉浸式翻译等）只需改一个
`base_url`，就能用上灵犀的真实模型（DeepSeek V4、Qwen3.8、GLM、Kimi、MiMo 等）。

基于 [workbuddy2api-panel](https://github.com/DGZSbot/workbuddy2api-panel) 的工程架构移植而来：
**流量治理层（账号池 / 熔断冷却 / 会话粘性 / 调度 / 面板 / 日志）原样保留，
只把上游协议从「CodeBuddy 单次 POST」换成「灵犀三步骤」。**

---

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [添加账号](#添加账号)
- [OpenAI 兼容接口](#openai-兼容接口)
- [管理面板](#管理面板)
- [配置](#配置)
- [核心机制](#核心机制)
- [常见问题排查](#常见问题排查)
- [已知限制](#已知限制)
- [开发](#开发)

---

## 特性

| 能力 | 说明 |
|---|---|
| **OpenAI 兼容** | `/v1/models`、`/v1/chat/completions`（流式 / 非流式），Bearer 鉴权 |
| **多账号池** | 加权随机选号、快过期偏好、Top-5 候选、防惊群、在途租约 |
| **熔断冷却** | 429 软冷却指数退避、402 硬冷却、404 短冷却、连续失败熔断 |
| **会话粘性** | 同一会话绑定同一账号，TTL 滚动续期，失败自动解绑（多轮上下文不断） |
| **管理面板** | 账号运维、用量分析、智点明细、任务中心、在线改配置、请求日志 |
| **账号体检** | 一键「验证全部」，401 死号自动停用并标注原因 |
| **浏览器登录** | 自动拉起独立 profile 浏览器，CDP 抓取 HttpOnly 的 `wps_sid` |
| **观测** | 每请求一行日志（TTFB / token 速率 / uid）、JSONL 归档、`/healthz` |

---

## 快速开始

### 环境要求

- Go 1.21+（源码构建）；或直接使用预编译的 `lingxi2api.exe`
- Windows / Linux / macOS 均可（浏览器登录功能仅在本机部署时有意义，见下文）

### 构建并启动

```bash
go build -o lingxi2api.exe ./cmd/server
./lingxi2api.exe -config config.json
```

启动成功会打印：

```
lingxi2api listening on :6789 (api_key=true)，管理面板 http://127.0.0.1:6789/panel/
```

默认监听 `:6789`，API 密钥取 `config.json` 里的 `api_key`（示例为
`sk-lingxi-change-me`，**务必改成自己的**）。

### 添加第一个账号

打开 `http://127.0.0.1:6789/panel/`（Bearer = `config.api_key`），点「添加账号」，
任选一种方式（详见[添加账号](#添加账号)）。

### 调用

```bash
curl http://127.0.0.1:6789/v1/chat/completions \
  -H "Authorization: Bearer sk-lingxi-change-me" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

任何 OpenAI SDK 改 `base_url` 即可接入：

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:6789/v1", api_key="sk-lingxi-change-me")
client.chat.completions.create(model="deepseek-v4.1-flash", messages=[{"role":"user","content":"你好"}])
```

---

## 添加账号

灵犀**没有 OAuth**，凭证就是 Cookie 里的 `wps_sid`（`V02S` 前缀）。三种方式：

### 方式一：浏览器登录（推荐，本机部署时）

面板「添加账号 → 浏览器登录 → 打开登录页」会**在本机拉起一个独立 profile 的
Chrome/Edge**，你在里面登录灵犀，网关通过 CDP（Chrome DevTools Protocol）实时读取
Cookie，抓到就自动落盘、验余额、入池。

- `wps_sid` 是 **HttpOnly**，页面 JS 和面板都读不到，所以必须走 CDP 读 Cookie 库；
- 独立 profile（`browser-profile/`）不碰你日常浏览器数据，且满足 Chrome 136+
  「远程调试必须非默认 profile」的限制，profile 会保留、下次免登录；
- **先验余额再落盘**：profile 复用后残留的过期 cookie 不会被写进池，而是提示重新登录；
- 找不到浏览器时提示安装 Chrome/Edge，或用 `LINGXI_BROWSER=<路径>` 指定。

> **注意**：这条路径依赖「网关和浏览器在同一台机器」。若网关部署在远程服务器，
> 浏览器会弹在服务器上，你操作不到——此时改用方式二或方式三。

### 方式二：粘贴 wps_sid

面板「添加账号 → 粘贴」，一行一个，支持三种形态：

```
V02Sxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx        # 裸 sid
wps_sid=V02Sxxxxxxxxxxxxxxxxxxxxxxxx           # 单键
wps_sid=V02Sxxx...; wpsua=yyy; uid=zzz         # 整段 Cookie（存活更久）
```

空行与 `#` 开头的行自动跳过；**先校验再入池**，粘错当场报错（`401 用户无权限`），
不会把坏账号留在池里。

### 方式三：从本机客户端自动导入

灵犀桌面客户端登录后，`wps_sid` 落在客户端 Cookie 库（DPAPI 加密）。脚本自动解密导入：

```bash
python scripts/import_lingxi.py                 # 从本机客户端读（需先退出客户端）
python scripts/import_lingxi.py --sid "V02S-xxxx"   # 直接给 sid
```

> 客户端**运行时独占锁死 Cookie 库**，脚本必须先退出客户端才能读。

---

## OpenAI 兼容接口

### `GET /v1/models`

返回灵犀目录里的真实模型（`key` 即模型名，无 `cn:` 前缀）：

```
deepseek-v4.1-flash · deepseek-v4-pro-0813 · qwen3.8_flash · qwen3.8_max
mimo-v2.6-pro · GLM-5.3-Flash · GLM-5.3 · kimi-k3 · …
```

### `POST /v1/chat/completions`

- 支持 `stream`（默认 SSE）与非流式；
- 多轮上下文正常（会话粘性保证同一账号 + 上游 session 复用）；
- 流式返回顺序：`role` 首帧 → `reasoning_content` 增量 → `content` 增量 →
  `usage` 帧 → 网关补 `[DONE]`；
- `usage` 里的 `credit` 字段是**估算值**（`total_tokens × 模型倍率 / 1000`，向上取整、
  保底 1），且**仅在开启计费上报时返回**（见[计费](#计费默认不扣费可选开启)）。

---

## 管理面板

`http://127.0.0.1:6789/panel/`，Bearer = `config.api_key`。

| 视图 | 内容 |
|---|---|
| **账号池** | 账号状态 / 余额 / 成功失败 / 在途 / 用量；「验证全部」一键体检、全部签到 |
| **用量** | Token 时序图、按账号/模型明细、积分扣除（估算） |
| **积分构成** | 每账号的智点批次（发了多少 / 用了多少 / 还剩多少 / 何时到期） |
| **智点明细** | 上游真实流水（获得 / 消耗），可对账 |
| **任务中心** | 灵犀 4 个任务（签到 / 专家模式 / 群聊 / 客户端）的完成与领取 |
| **模型与档位** | 模型列表、倍率、档位、max_tokens |
| **配置** | 在线改配置，保存后热生效 |
| **运行日志** | 请求记录 + 系统日志 + 积分历史 |

---

## 配置

完整示例见 [`config.example.json`](config.example.json)。关键项：

```jsonc
{
  "listen": ":6789",
  "api_key": "sk-lingxi-change-me",   // 网关鉴权密钥，务必修改
  "auth_dir": "./auths",              // 账号文件目录

  "lingxi": {
    "report_lifecycle": false,        // 是否上报 run 生命周期（= 是否扣积分），默认关
    "device_id": ""                   // 出站 x-kso-device-id，留空用落盘的固定随机值
  },

  "schedule": {
    "checkin_enabled": true,          // 每日签到（默认 9 点）
    "checkin_hours": [9],
    "balance_refresh_enabled": true,  // 余额后台刷新
    "balance_refresh_minutes": 30     // 建议 ≥30，太频繁像机器人易被风控
  },

  "session_sticky": {
    "enabled": true,
    "ttl": "30m"                      // 会话粘性时长，多轮上下文靠它
  }
}
```

账号文件格式（`auths/<uid>.json`）：

```json
{
  "wps_sid": "V02S-xxxx",
  "uid": "1304453904",
  "nickname": "灵犀账号",
  "cookie": "csrf=...; uid=...; wps_sid=...; wpsua=..."
}
```

---

## 核心机制

### 灵犀协议：一次对话是三个请求

与 CodeBuddy 最本质的差异（安装包 1.3.31+ 与 magpie-wps-lingxi 双向取证）：

```
1. POST /api/aioffice/v1/cowork/sessions          → session_id
2. POST /api/public/v1/credits/runs               → run_id（计费单元）
3. POST /api/public/v1/agent-runtime/chat/completions  → SSE
```

必须适配的坑：

| 坑 | 现象 | 处理 |
|---|---|---|
| 正文在 `choices[].text` | 当成 `delta.content` 拿到空 | 逐帧改写 |
| 没有 `[DONE]` | 流不结束 | 靠 `finish_reason` 收尾，网关补 `[DONE]` |
| `usage.total_tokens` 是上下文大小 | 总量虚高几十倍 | 按 `prompt + completion` 重算 |
| 模型目录字段是 `key` 不是 `id` | 列表为空 | 按 `key/name/multiplier/max_context_tokens` 解析 |
| Cookie 要带 `x-kso-*` 设备键 | 401 | 补 `x-kso-app-name=win-lingxi-desktop` 等 |
| 无 OAuth | — | `wps_sid` 静态 Cookie |
| 无「接受任务」环节 | — | 任务直接 claim |

### 计费：默认不扣费，可选开启

灵犀的计费单元是 **run**，必须由客户端上报 run 的终态才会结算。对照实验（同账号、同模型、
同提示词，唯一变量是 `execution_status` 上报）：

| 实验 | 操作 | 结果 |
|---|---|---|
| A | 建 session + 领 run + 对话，**不上报** | 余额不变，流水无记录 |
| B | 同上，加 `running` / `completed` | 余额 -1，流水新增 `-1` |

`lingxi.report_lifecycle` 默认 **false**（不扣费）。开启方式：面板「配置 → 灵犀上游 →
上报 run 生命周期」勾选，保存后**热生效**。开启后每轮请求：

```
1. 取会话（账号×模型 缓存 1h，会话不计费）
2. POST /credits/runs               ← 每轮新建 run
3. PUT  execution_status {running, lease_seconds:90}
4. POST /agent-runtime/chat/completions
5. 流结束 → PUT {completed}          ← 结算触发点
```

### 出站 User-Agent：对齐桌面客户端

默认 UA 是**实测自运行中客户端**的真实形态：

```
Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)
Lingxi Claw/1.3.36 Chrome/144.0.7559.236 Electron/40.8.2 Safari/537.36
```

`x-kso-app-version` 同步为 `1.3.36`、`x-kso-app-name` 为 `win-lingxi-desktop`。
`upstream.user_agent` 留空即用此值，想覆盖在配置页填自定义。

### 设备 ID：按账号稳定派生

上游按 device 维度管登录态。设备 ID 由 `sha256("lingxi2api:" + uid)` 派生 ——
**同一账号永远同一个设备、不同账号不同设备**，避免「一台客户端反复切号」被上游顶会话。
可用 `lingxi.device_id` 强制统一成某个固定值（如客户端那台机器的 ID）。

### 任务中心：灵犀 4 个任务

面板「任务中心」按账号列出 4 个任务的真实状态（`incomplete` / `claimable` / `claimed`）：

| 任务 | 动作 | 奖励 |
|---|---|---|
| `daily_check_in` | 直接 claim | +100/天 |
| `try_expert_mode` | cowork_client 会话 + 真实对话 + execution_status | +200 |
| `use_lingxi_client` | 同上 | +800 |
| `try_group_chat` | cowork_web 会话 + WS user.input + 建群 + 解散 | +400 |

三个按钮：**刷新状态** / **领取全部可领** / **执行全部待办**（未完成的先跑真实动作再领，
账号并发 1–3）。群聊任务最容易踩坑：建群前必须让会话在 WebSocket 上收到过一次
`user.input`，且 `expect_model` 必须是 `plan_pro`。

---

## 常见问题排查

### 账号 401「用户无权限」

先分清是 **sid 被回收** 还是 **请求头不对**。用同一 sid 换多套请求头测 `/credits/balance`：

- 若**所有组合都 401** → sid 已被上游回收，只能重新登录拿新值（上游**没有续期接口**，
  sid 生命周期约半天~一天，且同账号重新登录会签发新值）；
- 若**能用的号所有组合都 200** → 说明问题只在那条 sid。

面板「账号池 → 验证全部」会逐个实测并自动停用死号、复活好号。

### 为什么走反代的请求不扣积分

不是延迟，是网关默认**不上报 run 生命周期**（`report_lifecycle=false`），上游因此不结算。
需要真实扣费（和桌面客户端一致）就勾选配置里的开关。详见[计费](#计费默认不扣费可选开启)。

### 智点明细和余额对不上

上游是**延迟归账**（对话结束后要过几分钟流水才出现）。两条口径：

| 位置 | 口径 | 用途 |
|---|---|---|
| 用量「积分扣除（估算）」 | `tokens × 倍率 / 1000`，ceil 保底 1 | 实时趋势 |
| 智点明细 | 上游流水原值 | 真实金额，可对账 |

### 智点明细整页空白

上游 `transaction/list` 有三个反直觉的字段约定，网关已兜底：

- 消耗类 `id` 是**数字**、获得类**没有 id**（`FlexID` 容错）；
- 获得类 `type` 是 **`topup`** 不是 `income`（按金额符号 + type 归一）；
- 分页必须走 `type=all`（`type=income/expense` 单独查游标恒 0、翻不了页）。

---

## 已知限制

- **无 OAuth**：凭证只有 `wps_sid`（`V02S` 前缀 Cookie），失效只能重新登录导入；
- **sid 生命周期短**：约半天~一天，且重新登录必换新值，无续期接口；
- **浏览器登录仅限本机部署**：远程服务器上浏览器会弹在服务器端，无法交互；
- **客户端独占 Cookie 库**：从客户端自动导入需先退出客户端；
- **`v20`（app-bound）加密的 Cookie 不支持**：只能手动给 `wps_sid`；
- **上游提示词超约 320KB** 直接 `400 AI服务异常`，不提示「太长」；
- **内容审核兜底**：命中敏感内容时上游返回 HTTP 200 的兜底话术，网关识别后按
  `finish_reason: content_filter` 下发，无法绕过（上游风控）。

---

## 开发

```bash
go build ./...     # 编译
go test ./...      # 全量测试（17 包）
go vet ./...       # 静态检查
```

### 代码结构

```
cmd/server            启动、装配、调度
internal/
  upstream            灵犀协议（lingxi.go）+ 任务适配 + 兼容壳
  pool                账号池 / 冷却 / 熔断 / 粘性
  panel               内嵌单页面板（Go embed + 原生 JS）
  scheduler           定时签到 / 余额刷新
  auth                wps_sid 格式解析
  server              OpenAI 兼容层
scripts/import_lingxi.py   账号导入脚本
```

### 灵犀专属改动集中点

- `internal/upstream/lingxi.go` — 灵犀实现（会话 / run / 对话 / 余额 / 流水 / 群聊）
- `internal/upstream/lingxi_ledger.go` — 智点流水（`transaction/list` 解析与分页）
- `internal/upstream/compat.go` / `compat_tasks.go` — 把原公开方法指向灵犀实现
- `internal/panel/lxtasks.go` — 灵犀任务看板
- `internal/panel/browserlogin.go` — 浏览器登录（CDP 抓 cookie）
- `internal/panel/ledger.go` — 智点明细视图
- `internal/panel/verify.go` — 账号体检

原 CodeBuddy 实现保留为 `*WB` 后缀方法，可对照回退。

### 遗留文件（WorkBuddy 残留，不影响运行）

以下文件来自 WorkBuddy 移植，灵犀已不使用，保留仅供对照：

- `cmd/login` / `cmd/credit` / `cmd/signin` — WorkBuddy OAuth 登录 / 积分 / 签到 CLI
- `login.sh` / `signin.sh` / `credit.sh` — 对应脚本
- `scripts/probe_active.py` / `scripts/task_*.py` — WorkBuddy 活跃 / 成长任务脚本

> 当前实际使用的是 `cmd/server`（主程序）和 `scripts/import_lingxi.py`（账号导入）。
