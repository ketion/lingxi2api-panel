package upstream

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 灵犀模型目录的真实形状（2026-11-06 抓包，字段是 key 不是 id）。
const plansFixture = `{"models":[
 {"key":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","multiplier":"～0.12x",
  "max_context_tokens":1048576,"max_new_tokens":393216,"support_multimodal":true,
  "reasoning_efforts":["low","high","max"],"intro":"原生多模态"},
 {"key":"qwen3.8_flash","name":"Qwen3.8-Flash","multiplier":"～0.08x",
  "max_context_tokens":1048576,"max_new_tokens":131072,"support_multimodal":false,
  "reasoning_efforts":["low","high","max"]},
 {"key":"auto","name":"自动","multiplier":"1x"},
 {"key":"plan_pro","name":"Pro","multiplier":"1x"}
]}`

func TestParsePlansModels(t *testing.T) {
	var d struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(plansFixture), &d); err != nil {
		t.Fatal(err)
	}
	out := parsePlansModels(d.Models)
	// auto / plan_* 是调度档位，不是 credits/runs 模型，必须被过滤
	if len(out) != 2 {
		t.Fatalf("应解析出 2 个真实模型，实际 %d", len(out))
	}
	first := out[0]
	if first.ID != "deepseek-v4.1-flash" {
		t.Fatalf("ID 解析错误: %q", first.ID)
	}
	if first.Name != "DeepSeek V4.1 Flash" {
		t.Fatalf("Name 解析错误: %q", first.Name)
	}
	if first.ContextWindow != 1048576 || first.MaxTokens != 393216 {
		t.Fatalf("上下文/输出长度解析错误: %d / %d", first.ContextWindow, first.MaxTokens)
	}
	if !first.SupportsReasoning || len(first.Efforts) != 3 {
		t.Fatalf("档位解析错误: reasoning=%v efforts=%v", first.SupportsReasoning, first.Efforts)
	}
	if !first.SupportsImages {
		t.Fatal("多模态标记应透出")
	}
	if first.Credits != "～0.12x" {
		t.Fatalf("倍率原文应保留: %q", first.Credits)
	}
	if out[1].SupportsImages {
		t.Fatal("qwen3.8-flash 不支持多模态")
	}
}

func TestParseMultiplierAndEstimate(t *testing.T) {
	cases := map[string]float64{
		"～0.12x": 0.12,
		"0.40x":   0.40,
		"~0.80x":  0.80,
		"":        0,
		"x":       0,
	}
	for in, want := range cases {
		if got := parseMultiplier(in); got != want {
			t.Fatalf("parseMultiplier(%q)=%v, want %v", in, got, want)
		}
	}
	lxMultCache.Store("unit-test-model", 1.0)
	if got := EstimateCredit("unit-test-model", 1000); got != 1.0 {
		t.Fatalf("EstimateCredit 应为 1.0，实际 %v", got)
	}
	if got := EstimateCredit("unknown-model", 1000); got != 0 {
		t.Fatalf("未知模型应返回 0，实际 %v", got)
	}
}

// 余额构成：两个不同到期日的批次 -> 两个桶，最近到期时间取最小。
func TestBuildBalanceDetail(t *testing.T) {
	type batch = struct {
		Balance    float64 `json:"balance"`
		Value      float64 `json:"value"`
		Consumed   float64 `json:"consumed"`
		ExpireTime string  `json:"expire_time"`
		SkuKey     string  `json:"sku_key"`
	}
	bonus := []batch{
		{Balance: 800, Consumed: 0, ExpireTime: "2026-12-07T21:57:04+08:00", SkuKey: "ai_credits_grant"},
		{Balance: 100, Consumed: 0, ExpireTime: "2026-12-07T23:08:01+08:00", SkuKey: "ai_credits_grant"},
		{Balance: 0, ExpireTime: "2026-12-08T00:00:00+08:00", SkuKey: "ai_credits_grant"},
	}
	d := buildBalanceDetail(900, 0, bonus, nil)
	if len(d.Buckets) != 2 {
		t.Fatalf("应生成 2 个到期桶（余额为 0 的跳过），实际 %d", len(d.Buckets))
	}
	if d.Buckets[0].Remain != 800 || d.Buckets[1].Remain != 100 {
		t.Fatalf("桶余额错误: %+v", d.Buckets)
	}
	if d.EarliestAt.Format("15:04") != "21:57" {
		t.Fatalf("最近到期时间错误: %v", d.EarliestAt)
	}
	if d.Total != 900 {
		t.Fatalf("总量错误: %v", d.Total)
	}
}

// 任务状态映射：claimable / claimed / incomplete 三态。
func TestLxTaskStatusMapping(t *testing.T) {
	if !LxTaskSupported("try_group_chat") || !LxTaskSupported("daily_check_in") {
		t.Fatal("灵犀任务应被识别")
	}
	if LxTaskSupported("chat_5") {
		t.Fatal("CodeBuddy 任务不应被灵犀分支接管")
	}
}

// 审核拦截识别：上游命中敏感内容时用兜底话术当正文返回（HTTP 200），
// 不能当成网关故障，要按 content_filter 语义透传。
func TestIsAuditReply(t *testing.T) {
	cases := []struct {
		msg  string
		code string
		want bool
	}{
		{"我还在不断学习中，暂时无法回答此问题，请尝试重新提问", "Success", true},
		{"抱歉，这个我无法回答", "", true},
		{"", "AuditFailed", true},
		{"", "ModerationBlocked", true},
		{"你好，有什么可以帮你？", "Success", false},
		{"AI服务异常", "AIError", false},
	}
	for _, c := range cases {
		if got := isAuditReply(c.msg, c.code); got != c.want {
			t.Fatalf("isAuditReply(%q,%q)=%v, want %v", c.msg, c.code, got, c.want)
		}
	}
}

// 智点估算：流水里的 amount 是整数且最少 1，所以估算也必须取整到整数智点，
// 否则面板会出现「0.01 智点」这种与账单完全对不上的数字。
func TestEstimateCreditRoundsToWholePoint(t *testing.T) {
	lxMultCache.Store("test-model", 0.12)
	cases := []struct {
		tokens int64
		want   float64
	}{
		{101, 1},       // 0.0121 → 保底 1
		{8300, 1},      // 0.996 → 保底 1
		{10000, 2},     // 1.2 → 2
		{100000, 12},   // 12
		{0, 0},         // 无 token 不算钱
	}
	for _, c := range cases {
		if got := EstimateCredit("test-model", c.tokens); got != c.want {
			t.Fatalf("EstimateCredit(%d)=%v want %v", c.tokens, got, c.want)
		}
	}
	if got := EstimateCredit("unknown-model", 1000); got != 0 {
		t.Fatalf("未知模型应返回 0，实际 %v", got)
	}
}

// 设备 ID 必须落盘复用：上游按 device 维度管登录态，每次换 id 会被当成新设备登录，
// 旧 wps_sid 随即失效。这里验证读写往返 + 脏数据回退。
func TestDeviceIDPersistence(t *testing.T) {
	if os.Getenv("AppData") == "" && os.Getenv("XDG_CONFIG_HOME") == "" {
		t.Skip("当前平台拿不到用户配置目录")
	}
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	if got := readDeviceID(); got != "" {
		t.Fatalf("首次应为空，实际 %q", got)
	}
	writeDeviceID("0123456789abcdef0123456789abcdef")
	if got := readDeviceID(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("往返不一致: %q", got)
	}
	writeDeviceID("bad")
	if got := readDeviceID(); got != "" {
		t.Fatalf("脏数据应被忽略，实际 %q", got)
	}
}

// 出站 UA 必须是客户端真实形态（Lingxi Claw/…），不能用裸 Chrome 冒充。
func TestLXHeadersUserAgent(t *testing.T) {
	a := &auth.Auth{AccessToken: "sid-test", UID: "1"}
	c := &Client{}

	h := lxTestHeaders(c, a)
	got := h.Get("User-Agent")
	if !strings.Contains(got, "Lingxi Claw/") || !strings.Contains(got, "Electron/") {
		t.Fatalf("默认 UA 应是客户端真实形态，实际 %q", got)
	}
	if !strings.Contains(got, "Chrome/") || !strings.Contains(got, "Safari/537.36") {
		t.Fatalf("UA 结构不对: %q", got)
	}

	// 配置里填了自定义 UA 时完全覆盖（面板可改）
	c.UserAgent = "custom/1.0"
	if got := lxTestHeaders(c, a).Get("User-Agent"); got != "custom/1.0" {
		t.Fatalf("自定义 UA 未生效: %q", got)
	}
}

// 计费上报默认关闭：不上报 execution_status = 上游不扣费，这是产品默认行为。
func TestLifecycleReportingDefaultOff(t *testing.T) {
	SetLifecycleReporting(false)
	if LifecycleReporting() {
		t.Fatal("默认应为关闭（不扣费）")
	}
	SetLifecycleReporting(true)
	if !LifecycleReporting() {
		t.Fatal("开启后应返回 true")
	}
	SetLifecycleReporting(false)
}

// 设备 ID 必须按账号稳定：同账号永远同一个值，不同账号不同值。
// 一个设备 ID 挂在多个账号上，在上游眼里就是"同一台客户端反复切号"，容易被顶会话。
func TestLXDevicePerAccount(t *testing.T) {
	a1 := &auth.Auth{UID: "1304453904"}
	a2 := &auth.Auth{UID: "1304453904"}
	a3 := &auth.Auth{UID: "sid-72dfee01"}

	d1, d2, d3 := lxDeviceFor(a1), lxDeviceFor(a2), lxDeviceFor(a3)
	if d1 != d2 {
		t.Fatalf("同账号设备 ID 应稳定: %q vs %q", d1, d2)
	}
	if d1 == d3 {
		t.Fatalf("不同账号设备 ID 应不同: %q", d1)
	}
	if len(d1) != 32 {
		t.Fatalf("设备 ID 应为 32 位 hex，实际 %q", d1)
	}
}
