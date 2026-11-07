package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 灵犀桌面端三条通路。
const (
	LXSessions = "/api/aioffice/v1/cowork/sessions"
	LXRuns     = "/api/public/v1/credits/runs"
	LXChat     = "/api/public/v1/agent-runtime/chat/completions"
	LXPlans    = "/api/aioffice/v1/sessions/plans"
	LXBalance  = "/api/public/v1/credits/balance"
	LXTasks    = "/api/public/v1/tasks"
	LXClaim    = "/api/public/v1/tasks/%s/claim"

	LXClientType = "lingxi_claw"
	LXClientLang = "zh-CN"
	// LXUserAgent 桌面客户端真实 UA（实测：在内嵌浏览器里读 navigator.userAgent）。
	// 主进程会把 Electron 默认 UA 里 "Chrome/" 段前面插上 "Lingxi Claw/<版本>"：
	//   app.userAgentFallback.replace(/(Gecko\) ).+?\//, "$1Lingxi Claw/<ver>/")
	// 所以完整形态是 ... (KHTML, like Gecko) Lingxi Claw/1.3.36 Chrome/144... Electron/40... Safari/537.36
	LXUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Lingxi Claw/1.3.36 Chrome/144.0.7559.236 Electron/40.8.2 Safari/537.36"
	LXAppVersion = "1.3.36" // 与安装目录实际版本一致（x-kso-app-version）
	LXAgentVer   = 2
)

var lxBases = []string{"https://lingxi.kdocs.cn", "https://lingxi.wps.cn"}

var (
	lxDeviceOnce sync.Once
	lxDeviceID   string
)

// lxDevice 返回稳定的设备 ID。
//
// 必须持久化：上游按 device 维度管登录态，进程每次启动都换一个 id 会被判成
// 「新设备登录」，旧 wps_sid 随即失效（这正是 sid 隔一阵就 401 的原因之一）。
// 首次生成后落到 <UserConfigDir>/lingxi2api/device_id，之后一直复用。
// lxDeviceOverride 允许配置指定设备 ID（config lingxi.device_id）。
// 留空则用落盘的随机 ID。想让网关和真实客户端"同一台设备"时，就填客户端那台机器的 ID。
var lxDeviceOverride atomic.Value // string

// SetDeviceID 由配置层在启动装配时调用；空值 = 继续用落盘 ID。
func SetDeviceID(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	lxDeviceOverride.Store(id)
}

// lxDeviceFor 返回某账号的出站设备 ID。
//
// 默认**按账号派生**（sha256("lingxi2api:"+uid) 取前 32 hex）：同一账号永远同一个
// 设备，不同账号看起来是不同机器。这一点很重要 —— 一个设备 ID 挂在多个账号上，
// 在上游眼里就是"同一台客户端反复切换账号"，容易被判异常并顶掉旧会话。
// 配置 lingxi.device_id 可强制用同一个固定值（例如统一伪装成客户端那台机器）。
func lxDeviceFor(a *auth.Auth) string {
	if v, ok := lxDeviceOverride.Load().(string); ok && v != "" {
		return v
	}
	if a == nil || a.UID == "" {
		return lxDevice()
	}
	sum := sha256.Sum256([]byte("lingxi2api:" + a.UID))
	return hex.EncodeToString(sum[:16])
}

func lxDevice() string {
	if v, ok := lxDeviceOverride.Load().(string); ok && v != "" {
		return v
	}
	lxDeviceOnce.Do(func() {
		if v := readDeviceID(); v != "" {
			lxDeviceID = v
			return
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			lxDeviceID = fmt.Sprintf("%d", time.Now().UnixNano())
			return
		}
		lxDeviceID = hex.EncodeToString(b)
		writeDeviceID(lxDeviceID)
	})
	return lxDeviceID
}

// lxCookie 补齐客户端 main 进程会追加的 x-kso-* 设备键 —— 服务端靠它判断"是不是桌面端"。
func lxCookie(a *auth.Auth) string {
	// 有整段 Cookie（含 wpsua / csrf）就带上 —— 灵犀部分接口除 wps_sid 还看 wpsua，
	// 掉线时整段能提高存活率；否则回落成只拼 wps_sid。
	base := "wps_sid=" + a.AccessTokenValue()
	if a.CookieRaw != "" {
		base = a.CookieRaw
		if !strings.Contains(base, "wps_sid=") {
			base = "wps_sid=" + a.AccessTokenValue() + "; " + base
		}
	}
	parts := []string{
		base,
		"x-kso-app-name=win-lingxi-desktop",
		"x-kso-app-version=" + LXAppVersion,
		"x-kso-device-id=" + lxDeviceFor(a),
	}
	if a.UID != "" && !strings.Contains(base, "uid=") {
		parts = append(parts, "uid="+a.UID)
	}
	return strings.Join(parts, "; ")
}

func lxRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (c *Client) lxHeaders(a *auth.Auth, base string, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("Cookie", lxCookie(a))
	h.Set("Content-Type", "application/json")
	h.Set("X-Client-Type", LXClientType)
	h.Set("X-Client-Lang", LXClientLang)
	h.Set("X-Cc-Agent-Type", LXClientType)
	h.Set("X-Request-Id", lxRequestID())
	h.Set("Referer", base+"/")
	ua := strings.TrimSpace(c.UserAgent)
	if ua == "" {
		ua = LXUserAgent // 默认就用桌面客户端的真实 UA，别用裸 Chrome 冒充
	}
	h.Set("User-Agent", ua)
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// lxEnvelope 是灵犀统一的返回信封。
type lxEnvelope struct {
	Result string          `json:"result"`
	V7Code int             `json:"v7code"`
	Hint   string          `json:"hint"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

func (e *lxEnvelope) errText(status int) *Error {
	kind := Classify(status, e.Hint+e.Msg)
	msg := e.Hint
	if msg == "" {
		msg = e.Msg
	}
	if msg == "" {
		msg = fmt.Sprintf("上游返回 HTTP %d", status)
	}
	return &Error{Kind: kind, Status: status, Msg: msg}
}

func (c *Client) lxDo(ctx context.Context, a *auth.Auth, method, path string, payload any, extra map[string]string) (*lxEnvelope, error) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	var lastErr error
	for _, base := range lxBases {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header = c.lxHeaders(a, base, extra)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var env lxEnvelope
			_ = json.Unmarshal(raw, &env)
			lastErr = env.errText(resp.StatusCode)
			// 401/403/404 换域再试一次（客户端两个域都部署）
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
				continue
			}
			return nil, lastErr
		}
		var env lxEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			lastErr = fmt.Errorf("解析上游响应失败: %w", err)
			continue
		}
		if env.Result != "" && !strings.EqualFold(env.Result, "ok") && !strings.EqualFold(env.Result, "Success") {
			return nil, env.errText(resp.StatusCode)
		}
		return &env, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("上游不可达")
	}
	return nil, lastErr
}

// ---- 会话缓存（按 账号 × 模型，1 小时）+ 每次请求独立 run ----
//
// 为什么 session 缓存、run 不缓存：
//   灵犀的计费单元是 **run** —— 实测同一个 session 下，上报了
//   `execution_status running → completed` 的 run 才会入账（扣 1 点并在流水里出现
//   一条 -1），不上报的 run 永远不结算。而一个 run 在语义上就是"一轮对话"，
//   真实客户端也是每轮新建。早期实现把 run 也缓存了 1 小时，导致：
//     ① 多轮请求共用一个 run，账目无法归属；
//     ② 从不调用 execution_status，run 停在进行中 → **完全不扣费**。
//   表现为「桌面客户端有消耗、走反代的没有」。

type lxRun struct {
	SessionID string
	RunID     string
	GroupID   string
	Base      string
	At        time.Time
}

var lxCache sync.Map // key: uid|model -> sessionID(string)

func lxKey(a *auth.Auth, model string) string { return a.UID + "|" + model }

func lxDropRun(a *auth.Auth, model string) { lxCache.Delete(lxKey(a, model)) }

// lxEnsureSession 取（或新建）账号在该模型下的会话并缓存。会话只承载上下文，
// 本身不计费，所以复用是安全的，也省一次建会话请求。
func (c *Client) lxEnsureSession(ctx context.Context, a *auth.Auth, model string) (string, error) {
	key := lxKey(a, model)
	if v, ok := lxCache.Load(key); ok {
		s := v.(string)
		if s != "" {
			return s, nil
		}
	}
	env, err := c.lxDo(ctx, a, http.MethodPost, LXSessions, map[string]any{
		"session_type":  "cowork_client",
		"agent_version": LXAgentVer,
		"env": map[string]any{
			"device_id":      lxDeviceFor(a),
			"device_name":    "lingxi2api",
			"work_space_dir": "/tmp",
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("建会话失败: %w", err)
	}
	var sd struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(env.Data, &sd)
	if sd.SessionID == "" {
		return "", fmt.Errorf("建会话失败: 未返回 session_id")
	}
	lxCache.Store(key, sd.SessionID)
	return sd.SessionID, nil
}

// lxNewRun 在指定会话里领一个新 run（每轮对话一个，计费单元）。
func (c *Client) lxNewRun(ctx context.Context, a *auth.Auth, model, sessionID, text string) (*lxRun, error) {
	if text == "" {
		text = "hi"
	}
	env, err := c.lxDo(ctx, a, http.MethodPost, LXRuns, map[string]any{
		"client_request_id": fmt.Sprintf("%d", time.Now().UnixMilli()),
		"session_id":        sessionID,
		"model_provider":    "lingxi",
		"model_id":          model,
		"agent_input": map[string]any{
			"text":                   text,
			"files":                  []any{},
			"file_ids":               []any{},
			"expect_model":           model,
			"refs":                   []any{},
			"permission_mode":        "default",
			"current_mode":           "agent",
			"browser_plugin_enabled": false,
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("领 run 失败: %w", err)
	}
	var rd struct {
		RunID   string `json:"run_id"`
		GroupID string `json:"group_id"`
	}
	_ = json.Unmarshal(env.Data, &rd)
	if rd.RunID == "" {
		return nil, fmt.Errorf("领 run 失败: 未返回 run_id")
	}
	if rd.GroupID == "" {
		rd.GroupID = rd.RunID
	}
	return &lxRun{SessionID: sessionID, RunID: rd.RunID, GroupID: rd.GroupID, Base: lxBases[0], At: time.Now()}, nil
}

// lxLifecycleOn 是否上报 run 生命周期 —— 这个开关直接决定上游会不会扣费。
//
// 默认**关闭**：不上报 execution_status，run 不结算，账号不扣积分（流水里也不会
// 出现记录）。需要在「配置 → 灵犀上游 → 上报 run 生命周期」里显式勾选才启用。
//
// 启用后行为与真实桌面客户端一致：每轮一个 run，running → completed 触发结算，
// 上游按轮扣费（实测一轮 1 点起）。
var lxLifecycleOn atomic.Bool

// SetLifecycleReporting 由配置层调用（启动装配时 + 面板热改时）。
func SetLifecycleReporting(on bool) { lxLifecycleOn.Store(on) }

// LifecycleReporting 是否开启计费上报（面板与 usage 统计要据此决定是否给估算值）。
func LifecycleReporting() bool { return lxLifecycleOn.Load() }

// lxReportStatus 上报 run 执行状态。必须用独立 context：
// 客户端断开时 ctx 会被取消，而「completed」恰恰是结算的唯一触发点，丢了就不扣费。
func (c *Client) lxReportStatus(a *auth.Auth, run *lxRun, status, reason string) {
	if !lxLifecycleOn.Load() || run == nil {
		return
	}
	body := map[string]any{
		"group_id":  run.GroupID,
		"status":    status,
		"run_token": run.GroupID,
	}
	if status == "running" {
		body["lease_seconds"] = 90
	}
	if reason != "" {
		body["terminal_reason"] = reason
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := c.lxDo(ctx, a, http.MethodPut,
		"/api/public/v1/sessions/"+run.SessionID+"/execution_status", body, nil); err != nil {
		logLite("execution_status %s 上报失败: %v", status, err)
	}
}

// lastUserText 取最后一条用户消息，作为 run 的 agent_input.text。
// 真实客户端用它当会话标题，流水里的「任务」列显示的就是它。
func lastUserText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(req.Messages[i].Content, &s) == nil && s != "" {
			return truncateRunes(strings.TrimSpace(s), 40)
		}
		// 多模态：content 是 [{type:"text",text:"..."}]
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(req.Messages[i].Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					return truncateRunes(strings.TrimSpace(p.Text), 40)
				}
			}
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// logLite 轻量日志（避免 upstream 包依赖面板的日志设施）。
func logLite(format string, args ...any) { log.Printf("[灵犀] "+format, args...) }

// lxChatOnce 发一次对话，返回上游 SSE 响应。
func (c *Client) lxChatOnce(ctx context.Context, a *auth.Auth, run *lxRun, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, run.Base+LXChat, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.lxHeaders(a, run.Base, map[string]string{
		"x-cc-session-id": run.SessionID,
		"x-cc-run-id":     run.RunID,
		"Accept":          "text/event-stream",
	})
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var env lxEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.Hint != "" || env.Msg != "" {
			return nil, env.errText(resp.StatusCode)
		}
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateStr(string(raw), 200)}
	}
	return resp, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// lxRewriteBody 把 OpenAI 请求改写成灵犀 wire 格式：出站强制 stream:true。
func lxRewriteBody(body []byte) ([]byte, string, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "", fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	model, _ := in["model"].(string)
	model = lxBareModel(model)
	in["model"] = model
	in["stream"] = true
	// 灵犀把 max_tokens 放在 base_llm_arguments 里
	if mt, ok := in["max_tokens"]; ok {
		in["base_llm_arguments"] = map[string]any{"max_tokens": mt}
	}
	if eff, ok := in["reasoning_effort"].(string); ok && strings.TrimSpace(eff) != "" {
		in["llm_arguments"] = map[string]any{"reasoning_effort": eff}
	}
	out, err := json.Marshal(in)
	return out, model, err
}

func lxBareModel(m string) string {
	m = strings.TrimSpace(m)
	for _, p := range []string{"wps-lingxi/", "lingxi/", "lingxi-"} {
		m = strings.TrimPrefix(m, p)
	}
	if m == "" || m == "default" {
		return "deepseek-v4.1-flash"
	}
	return m
}

// ---- SSE 转换：灵犀帧 -> OpenAI 帧 ----

type lxFrame struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		CachedTokens     int `json:"cached_tokens"`
	} `json:"usage"`
	Choices []struct {
		Text             string `json:"text"`
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []struct {
			Index    *int   `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// translateStream 把灵犀 SSE 翻成 OpenAI SSE 写进 pw。
func translateStream(src io.Reader, pw *io.PipeWriter, model string) {
	defer pw.Close()
	w := bufio.NewWriter(pw)
	defer w.Flush()

	id := fmt.Sprintf("chatcmpl-lingxi-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	roleSent := false
	sentUsage := false

	emit := func(delta map[string]any, finish string, usage map[string]any) {
		choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		obj := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": model, "choices": []any{choice},
		}
		if usage != nil {
			obj["usage"] = usage
			obj["choices"] = []any{}
		}
		b, _ := json.Marshal(obj)
		fmt.Fprintf(w, "data: %s\n\n", b)
		w.Flush()
	}

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var f lxFrame
		if err := json.Unmarshal([]byte(payload), &f); err != nil {
			continue
		}
		if f.Code != "" && !strings.EqualFold(f.Code, "Success") {
			msg := f.Message
			if msg == "" {
				msg = "AI服务异常"
			}
			// 内容审核：灵犀命中敏感内容时不下发错误码，而是用一句兜底话术
			// （「我还在不断学习中，暂时无法回答此问题」）当正文返回。
			// 这时按 OpenAI 的 content_filter 语义透传原文，不要再套「[上游错误]」——
			// 那会让用户以为网关炸了，实际是上游拒答。
			if isAuditReply(msg, f.Code) {
				emit(map[string]any{"content": msg}, "content_filter", nil)
				break
			}
			emit(map[string]any{"content": "\n[上游错误] " + msg}, "stop", nil)
			break
		}
		var usage map[string]any
		if f.Usage != nil && f.Usage.PromptTokens+f.Usage.CompletionTokens > 0 {
			// 上游 total_tokens 是上下文大小，必须按本轮重算
			u := map[string]any{
				"prompt_tokens":     f.Usage.PromptTokens,
				"completion_tokens": f.Usage.CompletionTokens,
				"total_tokens":      f.Usage.PromptTokens + f.Usage.CompletionTokens,
			}
			// 只有在开启计费上报时才给估算值：关闭上报时上游根本不扣费，
			// 这时候还写一个「积分」出来，日志与账目会对不上（用户看到明明没扣却记了账）。
			// 关闭状态下 usage 不带 credit —— 面板那一列显示"—"才是正确表达。
			if LifecycleReporting() {
				if est := EstimateCredit(model, int64(f.Usage.PromptTokens+f.Usage.CompletionTokens)); est > 0 {
					u["credit"] = est
				}
			}
			if f.Usage.CachedTokens > 0 {
				u["prompt_tokens_details"] = map[string]any{"cached_tokens": f.Usage.CachedTokens}
			}
			usage = u
		}
		if len(f.Choices) == 0 {
			continue
		}
		ch := f.Choices[0]
		delta := map[string]any{}
		if !roleSent {
			roleSent = true
			delta["role"] = "assistant"
		}
		if ch.ReasoningContent != "" {
			delta["reasoning_content"] = ch.ReasoningContent
		}
		if ch.Text != "" {
			delta["content"] = ch.Text
		}
		if len(ch.ToolCalls) > 0 {
			var tcs []any
			for i, tc := range ch.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				item := map[string]any{"index": idx}
				if tc.ID != "" {
					item["id"] = tc.ID
				}
				if tc.Type != "" {
					item["type"] = tc.Type
				}
				fn := map[string]any{}
				if tc.Function.Name != "" {
					fn["name"] = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					fn["arguments"] = tc.Function.Arguments
				}
				item["function"] = fn
				tcs = append(tcs, item)
			}
			delta["tool_calls"] = tcs
		}
		if len(delta) > 0 || ch.FinishReason != "" {
			emit(delta, ch.FinishReason, nil)
		}
		if ch.FinishReason != "" {
			if usage != nil {
				sentUsage = true
				emit(map[string]any{}, "", usage)
			}
			break
		}
	}
	if !sentUsage {
		// 保证有收尾
		emit(map[string]any{}, "stop", nil)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	w.Flush()
}

// lingxiChatStream 是灵犀版 ChatStreamContext：三步骤 + SSE 转换。
//
// 与早期的差别（关系到计费是否发生）：
//   - run 每轮新建，不再缓存 1 小时；
//   - 建 run 后立刻上报 execution_status=running（lease 90s）；
//   - 流读完后上报 completed —— 这是上游结算扣费的唯一触发点，缺了就不扣费。
func (c *Client) lingxiChatStream(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	upBody, model, err := lxRewriteBody(body)
	if err != nil {
		return nil, 0, nil, err
	}
	text := lastUserText(body)

	run, err := c.startRun(ctx, a, model, text)
	if err != nil {
		return nil, 0, nil, err
	}
	resp, err := c.lxChatOnce(ctx, a, run, upBody)
	if err != nil {
		// 这一轮没跑起来也要收尾，否则 run 会一直挂在进行中
		c.lxReportStatus(a, run, "completed", "failed")
		if e, ok := err.(*Error); ok && (e.Status == 404 || e.Status == 400) {
			// 会话/run 可能已被上游回收，丢缓存后重建一次
			lxDropRun(a, model)
			run2, err2 := c.startRun(ctx, a, model, text)
			if err2 == nil {
				if resp2, err3 := c.lxChatOnce(ctx, a, run2, upBody); err3 == nil {
					return c.pipeWithLifecycle(resp2, a, run2, model), http.StatusOK, nil, nil
				}
				c.lxReportStatus(a, run2, "completed", "failed")
			}
		}
		return nil, 0, nil, err
	}
	return c.pipeWithLifecycle(resp, a, run, model), http.StatusOK, nil, nil
}

// startRun 取会话（缓存）+ 领一个新 run，并上报 running。
func (c *Client) startRun(ctx context.Context, a *auth.Auth, model, text string) (*lxRun, error) {
	sess, err := c.lxEnsureSession(ctx, a, model)
	if err != nil {
		return nil, err
	}
	run, err := c.lxNewRun(ctx, a, model, sess, text)
	if err != nil {
		// 会话可能已被上游回收：丢掉缓存重建一次
		lxDropRun(a, model)
		if sess2, e2 := c.lxEnsureSession(ctx, a, model); e2 == nil {
			if run, err = c.lxNewRun(ctx, a, model, sess2, text); err == nil {
				c.lxReportStatus(a, run, "running", "")
				return run, nil
			}
		}
		return nil, err
	}
	c.lxReportStatus(a, run, "running", "")
	return run, nil
}

// pipeWithLifecycle 把上游 SSE 转成下游流，并在读完后上报 completed。
func (c *Client) pipeWithLifecycle(resp *http.Response, a *auth.Auth, run *lxRun, model string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		translateStream(resp.Body, pw, model)
		// 流结束 = 本轮结束：这里必须上报，否则上游不结算、不扣费
		c.lxReportStatus(a, run, "completed", "completed")
	}()
	return pr
}

// ---- 模型目录 / 余额 / 签到 ----

// lingxiModels 从 sessions/plans 取模型目录。
func (c *Client) lingxiModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	env, err := c.lxDo(ctx, a, http.MethodGet, LXPlans, nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, err
	}
	out := parsePlansModels(d.Models)
	if len(out) == 0 {
		return nil, fmt.Errorf("模型目录为空")
	}
	return out, nil
}

// parsePlansModels 解析 sessions/plans 的 models 数组（纯函数，便于单测）。
// 灵犀字段：key / name / multiplier / max_context_tokens / max_new_tokens /
// reasoning_efforts / support_multimodal / intro
func parsePlansModels(models []map[string]any) []ModelInfo {
	var out []ModelInfo
	for _, m := range models {
		mi := ModelInfo{}
		// 灵犀模型目录字段：key / name / multiplier / max_context_tokens /
		// max_new_tokens / reasoning_efforts / support_multimodal / intro
		if v, ok := m["key"].(string); ok {
			mi.ID = v
		}
		if mi.ID == "" {
			for _, k := range []string{"model_id", "id", "model"} {
				if v, ok := m[k].(string); ok && v != "" {
					mi.ID = v
					break
				}
			}
		}
		if mi.ID == "" || strings.HasPrefix(mi.ID, "plan_") || mi.ID == "auto" {
			continue
		}
		mi.Name = mi.ID
		if v, ok := m["name"].(string); ok && v != "" {
			mi.Name = v
		}
		for _, k := range []string{"max_context_tokens", "max_input_tokens", "context_window"} {
			if v, ok := m[k].(float64); ok && v > 0 {
				mi.ContextWindow = int64(v)
				break
			}
		}
		if v, ok := m["max_new_tokens"].(float64); ok && v > 0 {
			mi.MaxTokens = int64(v)
		}
		if v, ok := m["multiplier"].(string); ok {
			mi.Credits = v
			if mv := parseMultiplier(v); mv > 0 {
				lxMultCache.Store(mi.ID, mv)
			}
		}
		if v, ok := m["intro"].(string); ok {
			mi.Description = v
		}
		if arr, ok := m["reasoning_efforts"].([]any); ok && len(arr) > 0 {
			mi.SupportsReasoning = true
			for _, it := range arr {
				if sv, ok := it.(string); ok && sv != "" {
					mi.Efforts = append(mi.Efforts, sv)
				}
			}
		}
		if v, ok := m["support_multimodal"].(bool); ok {
			mi.SupportsImages = v
		}
		mi.SupportsToolCall = true
		out = append(out, mi)
	}
	return out
}

// lingxiBalance 返回剩余智点。
func (c *Client) lingxiBalance(ctx context.Context, a *auth.Auth) (remain, total int64, err error) {
	d, err := c.lingxiBalanceDetail(ctx, a)
	if err != nil {
		return 0, 0, err
	}
	return int64(d.Total), int64(d.Total), nil
}

// lxBalanceDetail 是智点构成的完整明细：每个到期批次一条。
// 灵犀的 bonus_credits / purchased_credits 都带 balance + expire_time，
// 正好映射到面板的「积分构成」卡片。
type lxBalanceDetail struct {
	Total      float64
	Buckets    []ExpiryBucket
	// Batches 原始批次（balance/value/consumed/expire），供「积分构成」逐包展示。
	Batches    []lxCreditBatch
	EarliestAt time.Time
	// TotalConsumed 各批次 consumed 之和（上游延迟结算，仅作参考）
	TotalConsumed float64
}

func (c *Client) lingxiBalanceDetail(ctx context.Context, a *auth.Auth) (*lxBalanceDetail, error) {
	env, err := c.lxDo(ctx, a, http.MethodGet, LXBalance, nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Total     float64 `json:"total_balance"`
		Bonus     float64 `json:"total_bonus"`
		Consumed  float64 `json:"total_consumed"`
		BonusList []struct {
			Balance    float64 `json:"balance"`
			Value      float64 `json:"value"`
			Consumed   float64 `json:"consumed"`
			ExpireTime string  `json:"expire_time"`
			SkuKey     string  `json:"sku_key"`
		} `json:"bonus_credits"`
		PurchasedList []struct {
			Balance    float64 `json:"balance"`
			Value      float64 `json:"value"`
			Consumed   float64 `json:"consumed"`
			ExpireTime string  `json:"expire_time"`
			SkuKey     string  `json:"sku_key"`
		} `json:"purchased_credits"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, err
	}
	return buildBalanceDetail(d.Total, d.Consumed, d.BonusList, d.PurchasedList), nil
}

// lxCreditBatch 一个到期批次（bonus_credits / purchased_credits 同构）。
type lxCreditBatch struct {
	Balance    float64 `json:"balance"`
	Value      float64 `json:"value"` // 批次原始面额（发放时的总额，balance=value-consumed）
	Consumed   float64 `json:"consumed"`
	ExpireTime string  `json:"expire_time"`
	SkuKey     string  `json:"sku_key"`
}

// buildBalanceDetail 把批次列表转成面板要的到期桶（纯函数，便于单测）。
func buildBalanceDetail(total, consumed float64, bonus, purchased []struct {
	Balance    float64 `json:"balance"`
	Value      float64 `json:"value"`
	Consumed   float64 `json:"consumed"`
	ExpireTime string  `json:"expire_time"`
	SkuKey     string  `json:"sku_key"`
}) *lxBalanceDetail {
	out := &lxBalanceDetail{Total: total, TotalConsumed: consumed}
	seen := map[string]bool{}
	add := func(balance, value, consumed float64, expire, sku string) {
		if balance <= 0 || seen[expire+"|"+sku] {
			return
		}
		seen[expire+"|"+sku] = true
		t, err := time.Parse(time.RFC3339, expire)
		if err != nil {
			return
		}
		if value <= 0 {
			value = balance
		}
		out.Buckets = append(out.Buckets, ExpiryBucket{
			Date: t.Format("2006-01-02"), ExpireAt: t, Remain: int64(balance),
		})
		out.Batches = append(out.Batches, lxCreditBatch{
			Balance: balance, Value: value, Consumed: consumed,
			ExpireTime: expire, SkuKey: sku,
		})
		if out.EarliestAt.IsZero() || t.Before(out.EarliestAt) {
			out.EarliestAt = t
		}
	}
	for _, b := range bonus {
		add(b.Balance, b.Value, b.Consumed, b.ExpireTime, b.SkuKey)
	}
	for _, b := range purchased {
		add(b.Balance, b.Value, b.Consumed, b.ExpireTime, b.SkuKey)
	}
	return out
}

// lxMultCache 模型倍率表（解析 "～0.12x" -> 0.12），usage credit 估算用。
var lxMultCache sync.Map

func parseMultiplier(s string) float64 {
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '.' {
			return r
		}
		return -1
	}, s)
	if s == "" || s == "." {
		return 0
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return 0
	}
	return v
}

// EstimateCredit 按 token 与模型倍率估算智点消耗（上游延迟结算，此为近似值，
// 供面板「积分/token」比例使用；真实余额以 balance 刷新为准）。
func EstimateCredit(model string, totalTokens int64) float64 {
	if totalTokens <= 0 {
		return 0
	}
	v, ok := lxMultCache.Load(model)
	if !ok {
		return 0
	}
	mult, _ := v.(float64)
	if mult <= 0 {
		return 0
	}
	// 流水实测：一次 run 的 amount 是**整数**，且最少 1 点（小提示词也是 -1）。
	// 所以这里向上取整并保底 1 —— 早期按小数估算，面板会出现「0.01 智点」这种
	// 与真实账单完全对不上的数字。
	credit := float64(totalTokens) * mult / 1000
	if credit < 1 {
		return 1
	}
	return math.Ceil(credit)
}

// deviceFile 设备 ID 落盘位置（跟随用户配置目录，跨版本升级不变）。
func deviceFile() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "lingxi2api", "device_id")
}

func readDeviceID() string {
	b, err := os.ReadFile(deviceFile())
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if len(v) < 8 || len(v) > 64 {
		return ""
	}
	return v
}

func writeDeviceID(id string) {
	p := deviceFile()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(id), 0o600)
}

// lingxiCheckin 签到（= 领取 daily_check_in）。
func (c *Client) lingxiCheckin(ctx context.Context, a *auth.Auth) error {
	path := fmt.Sprintf(LXClaim, "daily_check_in")
	_, err := c.lxDo(ctx, a, http.MethodPost, path, map[string]any{}, nil)
	return err
}

// 防未使用告警
var _ = sha256.Sum256

// ---- 一次性任务自动完成（对齐真实客户端抓包） ----

// lxTaskText 任务对话文案。裸词（"hi"）会被 cowork 风控拒（AuditFailed），
// 必须是自然语言；max_tokens 给足 4096 —— 回答太短可能凑不够 1 智点而不产生
// 计费记录，任务就不会翻转。
const lxTaskText = "请用两三句话介绍一下你自己，包括你擅长的任务类型"

// LxTaskSupported 报告 task_key 是否是灵犀任务中心的可自动项。
func LxTaskSupported(key string) bool {
	switch key {
	case "try_expert_mode", "use_lingxi_client", "try_group_chat", "daily_check_in":
		return true
	}
	return false
}

// lxPutExecutionStatus 上报会话执行状态（客户端行为，不上报会话永远不 completed，
// 任务翻转 / 建群全都会卡住）：
//   run 创建后   PUT {group_id, status:"running", run_token, lease_seconds:90}
//   对话完成后   PUT {group_id, status:"completed", terminal_reason:"completed"}
func (c *Client) lxPutExecutionStatus(ctx context.Context, a *auth.Auth, run *lxRun, status string) error {
	body := map[string]any{
		"group_id":  run.GroupID,
		"status":    status,
		"run_token": run.GroupID,
	}
	if status == "running" {
		body["lease_seconds"] = 90
	}
	if status == "completed" {
		body["terminal_reason"] = "completed"
	}
	_, err := c.lxDo(ctx, a, http.MethodPut, "/api/public/v1/sessions/"+run.SessionID+"/execution_status", body, nil)
	return err
}

// lxTaskRun 建一个任务专用的独立会话与 run（不复用对话缓存的 run ——
// 任务会话的 execution_status 生命周期独立管理）。
func (c *Client) lxTaskRun(ctx context.Context, a *auth.Auth, model, sessionType string) (*lxRun, error) {
	payload := map[string]any{
		"session_type":  sessionType,
		"agent_version": LXAgentVer,
		"env": map[string]any{
			"device_id":      lxDeviceFor(a),
			"device_name":    "lingxi2api",
			"work_space_dir": "/tmp",
		},
	}
	env, err := c.lxDo(ctx, a, http.MethodPost, LXSessions, payload, nil)
	if err != nil {
		return nil, fmt.Errorf("建会话失败: %w", err)
	}
	var sd struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(env.Data, &sd)
	if sd.SessionID == "" {
		return nil, fmt.Errorf("建会话失败: 未返回 session_id")
	}
	env2, err := c.lxDo(ctx, a, http.MethodPost, LXRuns, map[string]any{
		"client_request_id": fmt.Sprintf("%d", time.Now().UnixMilli()),
		"session_id":        sd.SessionID,
		"model_provider":    "lingxi",
		"model_id":          model,
		"agent_input": map[string]any{
			"text":                   lxTaskText,
			"files":                  []any{},
			"file_ids":               []any{},
			"expect_model":           model,
			"refs":                   []any{},
			"permission_mode":        "default",
			"current_mode":           "agent",
			"browser_plugin_enabled": false,
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("领 run 失败: %w", err)
	}
	var rd struct {
		RunID   string `json:"run_id"`
		GroupID string `json:"group_id"`
	}
	_ = json.Unmarshal(env2.Data, &rd)
	if rd.RunID == "" {
		return nil, fmt.Errorf("领 run 失败: 未返回 run_id")
	}
	if rd.GroupID == "" {
		rd.GroupID = rd.RunID
	}
	return &lxRun{SessionID: sd.SessionID, RunID: rd.RunID, GroupID: rd.GroupID, Base: lxBases[0]}, nil
}

// lxTaskChat 发一次任务对话并把流读完。
func (c *Client) lxTaskChat(ctx context.Context, a *auth.Auth, run *lxRun, model string) error {
	body, _ := json.Marshal(map[string]any{
		"stream":             true,
		"model":              model,
		"messages":           []map[string]any{{"role": "user", "content": lxTaskText}},
		"base_llm_arguments": map[string]any{"max_tokens": 4096},
	})
	resp, err := c.lxChatOnce(ctx, a, run, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<20))
	return nil
}

// lxTaskModel 从目录挑第一个可用模型（auto/plan_* 不是 credits/runs 模型）。
func (c *Client) lxTaskModel(ctx context.Context, a *auth.Auth) string {
	if infos, err := c.lingxiModels(ctx, a); err == nil {
		for _, mi := range infos {
			if mi.ID != "" {
				return mi.ID
			}
		}
	}
	return "deepseek-v4.1-flash"
}

// LxCompleteTask 完成并领取一个灵犀任务。返回给人看的执行摘要。
func (c *Client) LxCompleteTask(ctx context.Context, a *auth.Auth, key string) (string, error) {
	model := c.lxTaskModel(ctx, a)
	switch key {
	case "try_expert_mode", "use_lingxi_client":
		// 专家模式 / 客户端：cowork_client 会话 + 真实对话 + execution_status 生命周期
		run, err := c.lxTaskRun(ctx, a, model, "cowork_client")
		if err != nil {
			return "", err
		}
		if err := c.lxPutExecutionStatus(ctx, a, run, "running"); err != nil {
			return "", fmt.Errorf("上报 running 失败: %w", err)
		}
		if err := c.lxTaskChat(ctx, a, run, model); err != nil {
			return "", fmt.Errorf("任务对话失败: %w", err)
		}
		if err := c.lxPutExecutionStatus(ctx, a, run, "completed"); err != nil {
			return "", fmt.Errorf("上报 completed 失败: %w", err)
		}
		return "已发起一次真实对话（cowork_client + execution_status 生命周期）", nil

	case "try_group_chat":
		// 群聊：cowork_web 会话 → 在 completions WS 上发一次 user.input →
		// POST /api/imchat/v1/channels 建群 → 解散。
		//
		// 关键：上游要求这个会话先在 **WS completions 通道** 上收到过 user.input
		// （网页端建群前的固定动作），只走 REST 对话再建群会直接 400「参数错误」；
		// 且建群的 expect_model 必须是 plan_pro（网页端固定值）。
		msg, err := c.lxGroupTask(ctx, a, lxTaskText)
		if err != nil {
			return "", err
		}
		return msg, nil
	}
	return "", fmt.Errorf("不支持的任务: %s", key)
}

// lxGroupTask 群聊任务：cowork_web 会话 + WS user.input + 建群 + 解散。
//
// 实测（对齐 magpie 与网页端）：建群前必须让该会话在
// `/api/aioffice/v1/sessions/{id}/completions/ws` 上收到过一次 user.input，
// 否则 POST /api/imchat/v1/channels 一律 400「参数错误」；expect_model 固定 plan_pro。
func (c *Client) lxGroupTask(ctx context.Context, a *auth.Auth, text string) (string, error) {
	const planModel = "plan_pro"

	env, err := c.lxDo(ctx, a, http.MethodPost, LXSessions, map[string]any{
		"session_type": "cowork_web",
	}, nil)
	if err != nil {
		return "", fmt.Errorf("建群聊会话失败: %w", err)
	}
	var sd struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(env.Data, &sd)
	if sd.SessionID == "" {
		return "", fmt.Errorf("建群聊会话失败: 未返回 session_id")
	}

	if err := c.lxGroupWS(ctx, a, sd.SessionID, planModel, text); err != nil {
		return "", err
	}

	env2, err := c.lxDo(ctx, a, http.MethodPost, "/api/imchat/v1/channels", map[string]any{
		"session_id":   sd.SessionID,
		"expect_model": planModel,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("建群失败: %w", err)
	}
	var ch struct {
		ChannelID string `json:"channel_id"`
	}
	_ = json.Unmarshal(env2.Data, &ch)
	if ch.ChannelID == "" {
		return "对话完成，但建群未返回 channel_id（任务可能已点亮）", nil
	}
	_, _ = c.lxDo(ctx, a, http.MethodDelete, "/api/imchat/v1/channels/"+ch.ChannelID, nil, nil)
	return "已建群并解散（判定完成）", nil
}

// lxGroupWS 在 completions WebSocket 上发一次 user.input 并等到 run 结束。
func (c *Client) lxGroupWS(ctx context.Context, a *auth.Auth, sessionID, model, text string) error {
	base := lxBases[0]
	wsURL := strings.Replace(base, "http", "ws", 1) +
		"/api/aioffice/v1/sessions/" + sessionID + "/completions/ws"

	h := http.Header{}
	h.Set("Cookie", lxCookie(a))
	ua := strings.TrimSpace(c.UserAgent)
	if ua == "" {
		ua = LXUserAgent
	}
	h.Set("User-Agent", ua)
	h.Set("Origin", base)

	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, h)
	if err != nil {
		return fmt.Errorf("连接群聊通道失败: %w", err)
	}
	defer conn.Close()

	payload := map[string]any{
		"event": "user.input",
		"data": map[string]any{
			"agent_input": map[string]any{
				"text":                   text,
				"files":                  []any{},
				"refs":                   []any{},
				"expect_model":           model,
				"reasoning_effort":       "low",
				"permission_mode":        "default",
				"current_mode":           "agent",
				"browser_plugin_enabled": false,
			},
			"headers": map[string]any{
				"client_type":  "lingxi_claw",
				"open_position": "clawapp",
			},
		},
	}
	if err := conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("发送群聊消息失败: %w", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("群聊通道未收到结束事件: %w", err)
		}
		var ev struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Event {
		case "run.end", "stream.end", "done":
			return nil
		case "error", "force.stop":
			return fmt.Errorf("群聊对话被上游中断（%s）", ev.Event)
		}
	}
}

// auditMarkers 上游审核拦截的兜底话术特征。
var auditMarkers = []string{
	"不断学习中", "暂时无法回答", "无法回答", "不能回答", "换个话题", "重新提问",
	"auditfailed", "audit failed", "inappropriate", "内容不合规", "不符合规范",
}

// isAuditReply 判断上游这一帧是不是内容审核拦截。
// 灵犀命中审核时不返回错误码，而是把兜底话术当正文下发（HTTP 200），
// 所以既要看 code 关键字，也要看正文特征。
func isAuditReply(msg, code string) bool {
	lc := strings.ToLower(code)
	if strings.Contains(lc, "audit") || strings.Contains(lc, "moderation") || strings.Contains(lc, "sensitive") {
		return true
	}
	lower := strings.ToLower(msg)
	for _, m := range auditMarkers {
		if strings.Contains(lower, m) || strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// 供单测使用（同包可见的最小钩子）。
func lxTestHeaders(c *Client, a *auth.Auth) http.Header { return c.lxHeaders(a, "https://lingxi.kdocs.cn", nil) }
