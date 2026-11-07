package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 浏览器登录：真正把 wps_sid 抓回来。
//
// 之前的实现只是在浏览器里打开 lingxi.kdocs.cn，面板拿不到任何东西 —— 因为
// wps_sid 是 HttpOnly，页面 JS 读不到，面板也跨不了域。这里改成「自己拉起浏览器」：
//
//	1. 用**独立 profile**（不碰用户日常浏览器数据）启动 Chrome/Edge，并开调试端口；
//	2. 用户在这个窗口里正常登录灵犀；
//	3. 面板通过 CDP 的 Network.getAllCookies 读 wps_sid / wpsua / csrf；
//	4. 落盘成账号、验余额、关掉浏览器。
//
// Chrome 136+ 只允许「非默认 user-data-dir + --remote-debugging-port」，独立 profile
// 正好满足；--remote-allow-origins=* 用于放行本地 CDP 的 WebSocket 握手。

const browserLoginTTL = 10 * time.Minute

// browserLogin 进行中的浏览器登录会话。
type browserLogin struct {
	mu      sync.Mutex
	running bool
	port    int
	exe     string
	name    string
	profile string
	cmd     *exec.Cmd
	started time.Time
	lastErr string

	// procDone 启动进程已退出（Windows 上 Chrome 可能 fork 后父进程立即退出，
	// 此时浏览器仍在跑，所以不能据此判定失败，只能作为 poll 的辅助信号）。
	procDone bool
	// miss 连续探测不到 CDP 的次数：进程已退出 + 连续 3 次探测不到 = 确实没起来。
	miss int
}

// browserLoginStart 拉起浏览器并返回状态。
func (p *Panel) browserLoginStart(w http.ResponseWriter, r *http.Request) {
	exe, name, err := findBrowser()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	port, err := freePort()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "没有可用端口: "+err.Error())
		return
	}
	profile := p.browserProfileDir()
	if abs, err := filepath.Abs(profile); err == nil {
		profile = abs
	}
	_ = os.MkdirAll(profile, 0o755)

	p.bl.mu.Lock()
	// 已经有一个在跑：先收掉，避免两个实例抢同一个 profile
	if p.bl.cmd != nil && p.bl.running {
		killTree(p.bl.cmd)
	}
	p.bl.running = true
	p.bl.port = port
	p.bl.exe = exe
	p.bl.name = name
	p.bl.profile = profile
	p.bl.started = time.Now()
	p.bl.lastErr = ""
	p.bl.procDone = false
	p.bl.miss = 0
	cmd := exec.Command(exe,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-allow-origins=*",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-features=Translate",
		"https://lingxi.kdocs.cn/",
	)
	// 启动日志留档：浏览器拒绝启动时（例如 profile 被占用、策略禁用调试端口）
	// 全在 stderr 里，不落盘就只能看到一句"浏览器已关闭"。
	if lf, ferr := os.Create(filepath.Join(profile, "launch.log")); ferr == nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	if err := cmd.Start(); err != nil {
		p.bl.running = false
		p.bl.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, "启动浏览器失败: "+err.Error())
		return
	}
	p.bl.cmd = cmd
	p.bl.mu.Unlock()

	// 浏览器退出后自动把状态收回，避免 poll 永久 pending
	go func() {
		_ = cmd.Wait()
		p.bl.mu.Lock()
		if p.bl.cmd == cmd {
			p.bl.procDone = true
		}
		p.bl.mu.Unlock()
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"port":    port,
		"browser": name,
		"profile": profile,
		"hint":    "浏览器已打开，请在窗口里登录灵犀；登录成功后这里会自动识别",
	})
}

// browserLoginPoll 轮询抓取 cookie。抓到即落盘加号并关闭浏览器。
func (p *Panel) browserLoginPoll(w http.ResponseWriter, r *http.Request) {
	p.bl.mu.Lock()
	running, port, name := p.bl.running, p.bl.port, p.bl.name
	started, cmd := p.bl.started, p.bl.cmd
	p.bl.mu.Unlock()

	if !running || port == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": "idle",
			"hint":   "没有进行中的浏览器登录",
		})
		return
	}
	if time.Since(started) > browserLoginTTL {
		if cmd != nil {
			killTree(cmd)
		}
		p.bl.mu.Lock()
		p.bl.running = false
		p.bl.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": "timeout",
			"hint":   "等待超时（10 分钟），已关闭浏览器，请重试",
		})
		return
	}

	targets, err := cdpTargets(port)
	if err != nil {
		p.bl.mu.Lock()
		miss := p.bl.miss + 1
		p.bl.miss = miss
		done := p.bl.procDone
		p.bl.mu.Unlock()
		if done && miss >= 3 {
			p.bl.mu.Lock()
			p.bl.running = false
			p.bl.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":     false,
				"status": "closed",
				"hint":   "浏览器启动失败或已被关闭，详情见 " + filepath.Join(p.browserProfileDir(), "launch.log"),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": "waiting",
			"hint":   "浏览器还没就绪…",
		})
		return
	}
	p.bl.mu.Lock()
	p.bl.miss = 0
	p.bl.mu.Unlock()

	wsURL := ""
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		if strings.Contains(t.URL, "lingxi") || strings.Contains(t.URL, "wps.cn") || strings.Contains(t.URL, "kdocs") {
			wsURL = t.WebSocketDebuggerURL
			break
		}
	}
	if wsURL == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": "waiting",
			"hint":   "等待打开灵犀页面…",
		})
		return
	}

	cookies, err := cdpCookies(wsURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": "waiting",
			"hint":   "读取登录态失败，重试中：" + err.Error(),
		})
		return
	}
	raw := buildCookieHeader(cookies)
	sid, uid := auth.ParseCookie(raw)
	if sid == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": "need_login",
			"hint":   "还没检测到登录状态，请在浏览器窗口里完成登录（登录后再等 2 秒）",
		})
		return
	}

	// 抓到 wps_sid：落盘 + 入池 + 验余额
	// uid 优先用 cookie 里的真实 uid（十进制账号号，如 1304453904）。
	// CDP 偶发抓不到 uid 键时才会 fallback 到 sid 尾 8 位 hex —— 但那是「假」uid，
	// 会导致面板显示 sid-4dc06310 这种无意义串、昵称错位。所以先兜底再判断：
	if uid == "" {
		// 双保险：splitCookie 已经扫过一遍 raw，这里再显式提一次 uid 键。
		if u := extractCookieVal(raw, "uid"); u != "" {
			uid = u
		}
	}
	if uid == "" {
		tail := sid
		if len(tail) > 8 {
			tail = tail[len(tail)-8:]
		}
		uid = "sid-" + tail
	}
	nick := "灵犀 " + lastN(sid, 4)
	rec := map[string]any{"wps_sid": sid, "uid": uid, "nickname": nick, "cookie": raw}
	acc, err := auth.Parse(mustJSON(rec))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "hint": "解析登录态失败：" + err.Error()})
		return
	}
	acc.FilePath = filepath.Join(p.cfg.AuthDir, uid+".json")

	// 先验余额再落盘：登录窗口的 profile 是长期保留的，下次直接复用，
	// 里面可能还留着上一次已过期的 cookie。这种情况不能把废号写进池，
	// 而是让用户在同一个窗口重新登录。
	remain, total, rerr := p.cfg.Upstream.UserResource(acc)
	if rerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"status": "need_login",
			"hint":   "抓到的登录态已失效（" + rerr.Error() + "），请在该浏览器窗口里重新登录",
		})
		return
	}
	if err := writeAuthFile(p.cfg.AuthDir, uid, rec); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": "error", "hint": "保存失败：" + err.Error()})
		return
	}
	p.cfg.Pool.Add(acc)
	p.cfg.Pool.Revive(uid)

	msg := fmt.Sprintf("已添加账号 %s，余额 %d", uid, remain)

	// 收尾：关掉登录用的浏览器窗口（它的 profile 只是临时的）
	if cmd != nil {
		killTree(cmd)
	}
	p.bl.mu.Lock()
	p.bl.running = false
	p.bl.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      rerr == nil,
		"status":  "done",
		"uid":     uid,
		"nickname": nick,
		"credits": remain,
		"total":   total,
		"browser": name,
		"hint":    msg,
	})
}

// browserLoginCancel 取消进行中的浏览器登录并关掉浏览器。
func (p *Panel) browserLoginCancel(w http.ResponseWriter, r *http.Request) {
	p.bl.mu.Lock()
	cmd := p.bl.cmd
	p.bl.running = false
	p.bl.mu.Unlock()
	if cmd != nil {
		killTree(cmd)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// browserProfileDir 登录专用 profile 目录（与账号目录同级，便于清理）。
func (p *Panel) browserProfileDir() string {
	base := p.cfg.AuthDir
	if base == "" {
		base = "."
	}
	return filepath.Join(filepath.Dir(base), "browser-profile")
}

// ---- CDP ----

type cdpTarget struct {
	Type                string `json:"type"`
	URL                 string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func cdpTargets(port int) ([]cdpTarget, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/json/list")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

type cdpCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

// cdpCookies 通过 CDP 读浏览器全部 cookie（这里能拿到 HttpOnly 的 wps_sid，
// 页面里的 document.cookie 是拿不到的）。
func cdpCookies(wsURL string) ([]cdpCookie, error) {
	d := websocket.Dialer{HandshakeTimeout: 8 * time.Second}
	conn, _, err := d.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := map[string]any{"id": 1, "method": "Network.getAllCookies"}
	if err := conn.WriteJSON(req); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		var env struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) != nil || env.ID != 1 {
			continue // 事件帧，跳过
		}
		if env.Error != nil {
			return nil, errors.New(env.Error.Message)
		}
		var r struct {
			Cookies []cdpCookie `json:"cookies"`
		}
		if err := json.Unmarshal(env.Result, &r); err != nil {
			return nil, err
		}
		return r.Cookies, nil
	}
}

// cookieKeep 需要带上的 cookie 键：wps_sid 是身份，wpsua 与 csrf 影响接口放行，
// uid 便于日志定位。其余（埋点、灰度、语言等）不带，减少出站体积。
var cookieKeep = map[string]bool{
	"wps_sid": true,
	"wpsua":   true,
	"csrf":    true,
	"uid":     true,
}

// buildCookieHeader 把 CDP cookie 拼回 Cookie 头（只取灵犀/金山域）。
func buildCookieHeader(cookies []cdpCookie) string {
	seen := map[string]bool{}
	var parts []string
	for _, c := range cookies {
		if !cookieKeep[c.Name] || c.Value == "" {
			continue
		}
		if !strings.Contains(c.Domain, "kdocs.cn") && !strings.Contains(c.Domain, "wps.cn") && !strings.Contains(c.Domain, "wps.com") {
			continue
		}
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ---- 工具 ----

// findBrowser 找本机可用的 Chromium 浏览器。支持环境变量覆盖。
func findBrowser() (path, name string, err error) {
	if v := os.Getenv("LINGXI_BROWSER"); v != "" {
		if _, e := os.Stat(v); e == nil {
			return v, "自定义浏览器", nil
		}
	}
	if runtime.GOOS != "windows" {
		for _, c := range []string{"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/microsoft-edge"} {
			if _, e := os.Stat(c); e == nil {
				return c, filepath.Base(c), nil
			}
		}
		return "", "", errors.New("没有找到 Chromium 系浏览器")
	}
	local := os.Getenv("LOCALAPPDATA")
	pf := os.Getenv("ProgramFiles")
	pf86 := os.Getenv("ProgramFiles(x86)")
	cands := []struct{ p, n string }{
		{filepath.Join(local, `Google\Chrome\Application\chrome.exe`), "Chrome"},
		{filepath.Join(pf, `Google\Chrome\Application\chrome.exe`), "Chrome"},
		{filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`), "Chrome"},
		{filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`), "Edge"},
		{filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`), "Edge"},
		{filepath.Join(local, `Microsoft\Edge\Application\msedge.exe`), "Edge"},
		{filepath.Join(local, `BraveSoftware\Brave-Browser\Application\brave.exe`), "Brave"},
	}
	for _, c := range cands {
		if c.p == "" {
			continue
		}
		if _, e := os.Stat(c.p); e == nil {
			return c.p, c.n, nil
		}
	}
	return "", "", errors.New("没有找到 Chrome / Edge，请先安装其一，或用 LINGXI_BROWSER 指定浏览器路径")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// killTree 连同子进程一起结束（浏览器会派生一堆渲染进程）。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return
	}
	_ = cmd.Process.Kill()
}

// extractCookieVal 从拼好的 Cookie 头里按名取值（兜底 uid 用）。
// splitCookie 已扫过一遍，这里再显式取一次，防止 CDP 抓取时某键缺失导致 uid 落到
// "sid-xxxx" 这种假值（表现为面板显示无意义 uid、昵称错位）。
func extractCookieVal(raw, name string) string {
	for _, part := range strings.Split(raw, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && kv[0] == name {
			return strings.TrimSpace(kv[1])
		}
	}
	return ""
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
