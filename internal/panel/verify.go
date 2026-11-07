package panel

import (
	"net/http"
	"strings"
	"sync"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 账号体检：逐个账号打一次 /credits/balance，确认 wps_sid 现在还能不能用。
//
// 为什么需要：池里的账号状态是"上一次操作成功"留下的，wps_sid 被上游回收
// （重新登录、换设备、风控）后，面板仍然显示「可用 / 余额 xxx」，直到真正发请求
// 才 401。用户看到的"好像又 401 了"就是这么来的。
//
// 401/403 视为登录态失效：直接 Disable（reason 标注），不再让它参与选号；
// 其余错误（超时/网络）只报错、不动状态，避免把抖动当死亡。

// isAuthExpired 判断错误是否是"登录态失效"。
func isAuthExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, kw := range []string{"401", "403", "无权限", "Unauthorized", "UserUnauthorized"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// accountsVerifyAll POST /panel/api/accounts/verify_all —— 全账号体检。
func (p *Panel) accountsVerifyAll(w http.ResponseWriter, r *http.Request) {
	type item struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		OK       bool   `json:"ok"`
		Credits  int64  `json:"credits,omitempty"`
		Total    int64  `json:"credits_total,omitempty"`
		Dead     bool   `json:"dead"` // 登录态失效（已被禁用）
		Error    string `json:"error,omitempty"`
	}

	states := p.cfg.Pool.List()
	items := make([]item, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		wg.Add(1)
		go func(i int, uid, nick string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				items[i] = item{UID: uid, Nickname: nick, Error: "账号不存在"}
				return
			}
			it := item{UID: uid, Nickname: nick}
			remain, total, err := p.cfg.Upstream.UserResource(a)
			if err != nil {
				it.Error = err.Error()
				if isAuthExpired(err) {
					it.Dead = true
					p.cfg.Pool.Disable(uid, "登录态失效（401），请重新登录后替换 wps_sid")
				}
				items[i] = it
				return
			}
			it.OK = true
			it.Credits, it.Total = remain, total
			// 之前因 401 被禁用的，现在能用了就放回池里
			p.cfg.Pool.Revive(uid)
			items[i] = it
		}(i, st.UID, st.Nickname)
	}
	wg.Wait()

	var ok, dead, failed int
	for _, it := range items {
		switch {
		case it.OK:
			ok++
		case it.Dead:
			dead++
		default:
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"items":   items,
		"healthy": ok,
		"dead":    dead,
		"failed":  failed,
		"total":   len(items),
	})
}

// verifyAccount 供批量粘贴使用：返回 (余额, 总额, 是否登录态失效, err)。
func (p *Panel) verifyAccount(a *auth.Auth) (remain, total int64, dead bool, err error) {
	remain, total, err = p.cfg.Upstream.UserResource(a)
	if err != nil {
		return 0, 0, isAuthExpired(err), err
	}
	return remain, total, false, nil
}
