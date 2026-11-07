package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// accountsPaste 批量添加账号：一行一个 wps_sid。
//
// 灵犀没有 OAuth，唯一凭证是 Cookie 里的 wps_sid。粘贴框接受三种写法：
//   1) 裸 sid                   V02S...
//   2) wps_sid=xxx              单键
//   3) 整段 Cookie              wps_sid=xxx; wpsua=yyy; uid=zzz
// 整段写法会把原始 cookie 一并存下来（部分接口除 wps_sid 还看 wpsua，掉线率更低）。
func (p *Panel) accountsPaste(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text     string `json:"text"`
		Nickname string `json:"nickname"`
		Checkin  bool   `json:"checkin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	type lineResult struct {
		Index    int    `json:"index"`
		Input    string `json:"input"` // 脱敏后的输入摘要
		UID      string `json:"uid,omitempty"`
		Nickname string `json:"nickname,omitempty"`
		OK       bool   `json:"ok"`
		Credits  int64  `json:"credits,omitempty"`
		Total    int64  `json:"credits_total,omitempty"`
		Message  string `json:"message"`
	}

	results := make([]lineResult, 0)
	added := 0
	idx := 0
	for _, raw := range strings.Split(body.Text, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx++
		res := lineResult{Index: idx, Input: maskSecret(line)}
		sid, uid := auth.ParseCookie(line)
		if sid == "" {
			res.Message = "没能从这一行里找到 wps_sid"
			results = append(results, res)
			continue
		}
		if len(sid) < 16 {
			res.Message = "wps_sid 太短，像是没粘全"
			results = append(results, res)
			continue
		}
		if uid == "" {
			tail := sid
			if len(tail) > 8 {
				tail = tail[len(tail)-8:]
			}
			uid = "sid-" + tail
			// 整段 cookie 里没有再补 uid，Parse 的回落规则与这里保持一致
		}
		name := body.Nickname
		if name == "" {
			t := sid
			if len(t) > 4 {
				t = t[len(t)-4:]
			}
			name = "灵犀 " + t
		}

		rec := map[string]any{"wps_sid": sid, "uid": uid, "nickname": name}
		if strings.Contains(line, "wpsua=") || strings.Contains(line, "csrf=") {
			rec["cookie"] = strings.TrimPrefix(line, "Cookie:")
		}

		// **先校验再入库**：早期是先写文件、入池、再验余额，校验失败只提示一句
		// "已保存" —— 结果是池里留下一堆 401 的死号，面板还显示"可用"。
		acc, err := auth.Parse(mustJSON(rec))
		if err != nil {
			res.Message = "解析失败: " + err.Error()
			results = append(results, res)
			continue
		}
		acc.FilePath = filepath.Join(p.cfg.AuthDir, uid+".json")
		remain, total, dead, rerr := p.verifyAccount(acc)
		res.UID, res.Nickname = uid, name
		if dead {
			// 登录态无效：不落盘、不入池。让用户当场面，而不是留下一个假"可用"号。
			res.Message = "登录态无效（401 用户无权限），未加入账号池，请重新登录后取新的 wps_sid"
			// 常见坑：以为"重新登录了"，其实复制到的还是旧 cookie（同一串字符）。
			// 这里直接指出"与池里哪个账号是同一条 sid"，比让用户反复试更有用。
			if dup := p.sameSIDOwner(sid, uid); dup != "" {
				res.Message += "；注意：这条 sid 与账号「" + dup + "」现有的完全一致，说明拿到的不是新登录态"
			}
			results = append(results, res)
			continue
		}
		if err := writeAuthFile(p.cfg.AuthDir, uid, rec); err != nil {
			res.Message = "写文件失败: " + err.Error()
			results = append(results, res)
			continue
		}
		p.cfg.Pool.Add(acc)
		p.cfg.Pool.Revive(uid)
		if rerr != nil {
			// 非鉴权类错误（超时/抖动）：先收进池，等下次余额刷新再说
			res.Message = "已保存，但余额校验失败：" + rerr.Error()
			results = append(results, res)
			continue
		}
		res.OK = true
		res.Credits, res.Total = remain, total
		res.Message = fmt.Sprintf("添加成功，余额 %d", remain)
		if body.Checkin {
			if err := p.cfg.Upstream.DailyCheckin(acc); err != nil {
				res.Message += "；签到失败: " + err.Error()
			} else {
				res.Message += "；已签到"
				if rm, _, e := p.cfg.Upstream.UserResource(acc); e == nil {
					res.Credits = rm
				}
			}
		}
		added++
		results = append(results, res)
	}

	if added > 0 {
		log.Printf("panel: 批量粘贴添加账号 成功 %d / 共 %d 行", added, idx)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"added":   added,
		"total":   idx,
		"results": results,
	})
}

func writeAuthFile(dir, uid string, rec map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, uid+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// maskSecret 日志/回显用的脱敏：只留头尾各 6 位。
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	// 整段 cookie 只显示键名
	if strings.Contains(s, ";") {
		var keys []string
		for _, part := range strings.Split(s, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				keys = append(keys, kv[0]+"=…")
			}
		}
		return strings.Join(keys, "; ")
	}
	if len(s) <= 16 {
		return s
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// sameSIDOwner 返回池中与给定 sid 完全相同的账号昵称/uid（含同 uid 的旧记录）。
// 用来识别"以为重新登录了、其实复制的还是旧 cookie"这种情况 —— 同一条 sid
// 再贴一次，恰恰说明登录态没变。
func (p *Panel) sameSIDOwner(sid, selfUID string) string {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return ""
	}
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if a.AccessTokenValue() == sid {
			if st.Nickname != "" {
				return st.Nickname
			}
			return st.UID
		}
	}
	return ""
}
