package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
	"github.com/lingxi2api/lingxi2api-panel/internal/upstream"
)

// 灵犀任务中心。
//
// 灵犀只有 4 个任务（每日签到 + 专家模式 / 群聊 / 客户端三个一次性体验任务），
// 与 CodeBuddy 那套「接受 → 进度 → 领取」的成长任务链完全不同：
//   - 没有 accept 步骤（Claim 前只要求"真的做过动作"，由上游判定 status）；
//   - status 三态：incomplete（没做）/ claimable（做了没领）/ claimed（领过）；
//   - 自动化不能靠"点一下"，必须产生真实行为（真实对话 + execution_status）；
//   - 群聊任务额外要把会话升级成群、判定后再解散。
//
// 因此这里给面板单独一块看板：按账号列出 4 个任务的状态与奖励，支持
// 「领取全部可领」与「执行全部待办（未完成的真跑一遍再领）」。

// lxBoardTask 看板上的一行任务。
type lxBoardTask struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	Reward    int64  `json:"reward"`
	Status    string `json:"status"` // incomplete / claimable / claimed
	Claimable bool   `json:"claimable"`
	Claimed   bool   `json:"claimed"`
	Auto      bool   `json:"auto"`      // 是否支持自动完成
	Hint      string `json:"hint,omitempty"` // 动作说明（不支持自动时给人工指引）
}

// lxBoardAccount 一个账号的看板数据。
type lxBoardAccount struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname"`
	Credits  int64         `json:"credits,omitempty"`
	Tasks    []lxBoardTask `json:"tasks"`
	Error    string        `json:"error,omitempty"`
}

// lxTaskHint 不支持自动完成的任务给人工指引（当前 4 个都支持，留作扩展）。
func lxTaskHint(code string) string {
	switch code {
	case "daily_check_in":
		return "每天可领一次，点「领取」或等定时签到"
	case "try_expert_mode", "use_lingxi_client":
		return "需真实发起一次对话（网关会代跑）"
	case "try_group_chat":
		return "需创建一个群聊（网关会代建并解散）"
	}
	return ""
}

// lxTasksBoard GET /panel/api/tasks/lingxi —— 全账号任务看板（只读）。
func (p *Panel) lxTasksBoard(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	out := make([]lxBoardAccount, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string, credits int64) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			acc := lxBoardAccount{UID: uid, Nickname: a.Nickname, Credits: credits}
			tasks, err := p.cfg.Upstream.ListTasks(a)
			if err != nil {
				acc.Error = err.Error()
				out[i] = acc
				return
			}
			for _, t := range tasks {
				acc.Tasks = append(acc.Tasks, lxBoardTask{
					Code:      t.TaskCode,
					Title:     t.Title,
					Reward:    t.Credit,
					Status:    t.Status,
					Claimable: t.Claimable,
					Claimed:   t.Claimed,
					Auto:      upstream.LxTaskSupported(t.TaskCode),
					Hint:      lxTaskHint(t.TaskCode),
				})
			}
			// 顺序固定：签到 → 专家 → 群聊 → 客户端，避免每次刷新顺序跳动
			order := map[string]int{"daily_check_in": 0, "try_expert_mode": 1, "try_group_chat": 2, "use_lingxi_client": 3}
			sort.SliceStable(acc.Tasks, func(x, y int) bool { return order[acc.Tasks[x].Code] < order[acc.Tasks[y].Code] })
			out[i] = acc
		}(i, st.UID, st.Credits)
	}
	wg.Wait()

	// 汇总：待办（未完成）/ 可领 / 已领 / 待领智点
	var incomplete, claimable, claimed int
	var pending, claimedCredit int64
	list := make([]lxBoardAccount, 0, len(out))
	for _, acc := range out {
		if acc.UID == "" {
			continue
		}
		list = append(list, acc)
		for _, t := range acc.Tasks {
			switch {
			case t.Claimed:
				claimed++
				claimedCredit += t.Reward
			case t.Claimable:
				claimable++
				pending += t.Reward
			default:
				incomplete++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": list,
		"summary": map[string]any{
			"incomplete":     incomplete,
			"claimable":      claimable,
			"claimed":        claimed,
			"pending_credit": pending,
			"claimed_credit": claimedCredit,
			"accounts":       len(list),
		},
	})
}

// lxRunRequest 执行看板动作的请求体。
type lxRunRequest struct {
	UIDs        []string `json:"uids"`        // 空 = 全部账号
	Concurrency int      `json:"concurrency"` // 账号间并发，默认 1
	Mode        string   `json:"mode"`        // claim = 只领可领；auto = 未完成的先跑再领
}

// lxTasksRun POST /panel/api/tasks/lingxi/run —— 批量领取 / 批量执行。
func (p *Panel) lxTasksRun(w http.ResponseWriter, r *http.Request) {
	var body lxRunRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	if body.Mode == "" {
		body.Mode = "auto"
	}
	conc := body.Concurrency
	if conc <= 0 {
		conc = 1
	}
	if conc > 3 {
		conc = 3
	}

	// 选定账号
	want := map[string]bool{}
	for _, u := range body.UIDs {
		want[u] = true
	}
	var accounts []*auth.Auth
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if len(want) > 0 && !want[st.UID] {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": []any{}, "message": "没有可用账号"})
		return
	}

	type accResult struct {
		UID      string           `json:"uid"`
		Nickname string           `json:"nickname"`
		Items    []map[string]any `json:"items"`
		Error    string           `json:"error,omitempty"`
	}
	results := make([]accResult, len(accounts))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := accResult{UID: a.UID, Nickname: a.Nickname}
			// 与「一键完成」共用 per-account 锁：同一账号的任务动作不并发
			if !p.tryLockAccount(a.UID) {
				res.Error = "该账号正在执行其他任务，已跳过"
				results[i] = res
				return
			}
			defer p.unlockAccount(a.UID)
			items, _, err := p.lxAccountTasks(a, body.Mode)
			res.Items = items
			if err != nil {
				res.Error = err.Error()
			}
			results[i] = res
		}(i, a)
	}
	wg.Wait()

	// 汇总
	var done, failed int
	for _, r := range results {
		for _, it := range r.Items {
			switch fmt.Sprint(it["status"]) {
			case "done":
				done++
			case "error":
				failed++
			}
		}
		if r.Error != "" {
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"results": results,
		"done":    done,
		"failed":  failed,
		"mode":    body.Mode,
	})
}

// lxAccountTasks 单账号任务动作。mode=claim 只领可领；mode=auto 未完成的先跑真实动作再领。
//
// lxOnly 表示该账号的任务表**全部**是灵犀任务 —— 调用方据此判断要不要再走
// CodeBuddy 的成长任务动作表（灵犀账号走了只会刷一堆"无此任务"）。
func (p *Panel) lxAccountTasks(a *auth.Auth, mode string) (items []map[string]any, lxOnly bool, err error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, false, err
	}
	lxOnly = len(tasks) > 0
	for _, t := range tasks {
		if !upstream.LxTaskSupported(t.TaskCode) {
			lxOnly = false
			break
		}
	}
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		if !upstream.LxTaskSupported(t.TaskCode) {
			continue
		}
		item := map[string]any{"task_code": t.TaskCode, "title": t.Title, "credit": t.Credit}
		if t.Claimed {
			item["status"] = "skipped"
			item["message"] = "已完成（已领取）"
			out = append(out, item)
			continue
		}
		if t.Claimable {
			credit, _, cerr := p.cfg.Upstream.ClaimReward(a, t.TaskCode)
			if cerr != nil {
				item["status"] = "error"
				item["message"] = "领取失败: " + cerr.Error()
			} else {
				item["status"] = "done"
				item["claimed"] = true
				item["credit"] = credit
				item["message"] = fmt.Sprintf("已领取 +%d 智点", credit)
			}
			out = append(out, item)
			continue
		}
		if mode == "claim" {
			item["status"] = "skipped"
			item["message"] = "未完成（需真实操作，勾选「执行全部待办」才会代跑）"
			out = append(out, item)
			continue
		}
		// 未完成：跑真实动作（对话 / 建群），再回读领取
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		msg, cerr := p.cfg.Upstream.LxCompleteTask(ctx, a, t.TaskCode)
		cancel()
		if cerr != nil {
			item["status"] = "error"
			item["message"] = cerr.Error()
			out = append(out, item)
			continue
		}
		item["status"] = "done"
		item["message"] = msg
		time.Sleep(2 * time.Second)
		if after, aerr := p.taskByCode(a, t.TaskCode); aerr == nil && after != nil && after.Claimable {
			if credit, _, cerr := p.cfg.Upstream.ClaimReward(a, t.TaskCode); cerr == nil && credit > 0 {
				item["claimed"] = true
				item["credit"] = credit
				item["message"] = msg + fmt.Sprintf("；已自动领取 +%d 智点", credit)
			}
		}
		out = append(out, item)
	}
	return out, lxOnly, nil
}
