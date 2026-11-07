package panel

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
	"github.com/lingxi2api/lingxi2api-panel/internal/upstream"
)

// 智点明细（获取 / 消耗流水）。
//
// 为什么需要它：对话接口的响应里没有计费结果，余额又是**延迟归账**（实测对话结束后
// 立刻查余额与 consume/list 都是 0），所以按 token×倍率估算出来的扣费永远对不上账单。
// 上游真正的账在 credits/transaction/list 里，每条记录带 amount（负数为消耗）、
// task_name、source、time、expire_time —— 面板直接展示它，才是「准确」的来源。

// ledgerTTL 流水快照缓存时长：面板刷新频繁，但流水是延迟归账的，10 秒内重复拉没有意义。
const ledgerTTL = 20 * time.Second

// ledgerMaxPages 单次最多翻几页（每页 50 条）。取 3 页 = 150 条，
// 足够覆盖「今天」并且不会把面板拖慢。
const ledgerMaxPages = 4

// ledgerPageSize 每次取多少条。type=all 时 limit 同时约束两侧，
// 实测 limit=20 就能把普通账号的全部记录一次拿完。
const ledgerPageSize = 30

// ledgerRow 面板展示用的一行流水。
type ledgerRow struct {
	ID      string  `json:"id"`
	Type    string  `json:"type"` // income / expense
	Title   string  `json:"title"`
	Source  string  `json:"source"`
	Time    string  `json:"time"`    // 2026-11-07 07:20
	Delta   float64 `json:"delta"`   // 正=获得，负=消耗
	Expire  string  `json:"expire,omitempty"`
	Session string  `json:"session_id,omitempty"`
	Model   string  `json:"model,omitempty"`
}

// ledgerSnapshot 一个账号的流水快照 + 汇总。
type ledgerSnapshot struct {
	At       time.Time   `json:"at"`
	UID      string      `json:"uid"`
	Rows     []ledgerRow `json:"rows"`
	Earned   float64     `json:"earned"`   // 近期获得合计（拉到的页内）
	Spent    float64     `json:"spent"`    // 近期消耗合计
	Today    float64     `json:"today"`    // 今日消耗
	Income   int         `json:"income_n"` // 获得条数
	Expense  int         `json:"expense_n"`
	Truncated bool       `json:"truncated"` // 是否还有更早的流水没拉完
	Error    string      `json:"error,omitempty"`
}

type ledgerCacheEntry struct {
	at   time.Time
	snap *ledgerSnapshot
}

// ledgerCache 按账号缓存流水快照。
type ledgerCache struct {
	mu sync.Mutex
	m  map[string]*ledgerCacheEntry
}

func (c *ledgerCache) get(uid string, force bool) (*ledgerSnapshot, bool) {
	if c.m == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[uid]
	if e == nil || e.snap == nil {
		return nil, false
	}
	if !force && time.Since(e.at) < ledgerTTL {
		return e.snap, true
	}
	return nil, false
}

func (c *ledgerCache) put(uid string, snap *ledgerSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*ledgerCacheEntry{}
	}
	c.m[uid] = &ledgerCacheEntry{at: time.Now(), snap: snap}
}

// fetchLedger 拉一个账号的流水并汇总。force=false 时优先用缓存。
func (p *Panel) fetchLedger(a *auth.Auth, force bool) *ledgerSnapshot {
	if snap, ok := p.ledger.get(a.UID, force); ok {
		return snap
	}
	snap := p.pullLedger(a)
	p.ledger.put(a.UID, snap)
	return snap
}

// pullLedger 真正打上游。
//
// 分页必须用 type=all + 双向游标：实测 type=income / type=expense 单独查时
// 回的游标恒为 0、has_more 恒为 false（翻不了页），只有 type=all 会给出
// 下一对 income_cursor / expense_cursor。客户端也是这么调的。
func (p *Panel) pullLedger(a *auth.Auth) *ledgerSnapshot {
	snap := &ledgerSnapshot{At: time.Now(), UID: a.UID, Rows: []ledgerRow{}}
	today := time.Now().Format("2006-01-02")

	var incomeCursor, expenseCursor int64
	for page := 0; page < ledgerMaxPages; page++ {
		res, err := p.cfg.Upstream.CreditLedger(a, "all", incomeCursor, expenseCursor, ledgerPageSize)
		if err != nil {
			snap.Error = err.Error()
			break
		}
		for _, it := range res.Items {
			row, kind := toLedgerRow(it)
			switch kind {
			case "income":
				snap.Earned += row.Delta
				snap.Income++
			case "expense":
				snap.Spent += -row.Delta
				snap.Expense++
				if len(row.Time) >= 10 && row.Time[:10] == today {
					snap.Today += -row.Delta
				}
			default: // daily_reset 等：客户端也会过滤掉
				continue
			}
			snap.Rows = append(snap.Rows, row)
		}
		if !res.HasMore {
			break
		}
		if res.IncomeCursor == incomeCursor && res.ExpenseCursor == expenseCursor {
			break // 游标没前进，防止死循环
		}
		if page == ledgerMaxPages-1 {
			snap.Truncated = true
			break
		}
		incomeCursor, expenseCursor = res.IncomeCursor, res.ExpenseCursor
	}

	sort.SliceStable(snap.Rows, func(i, j int) bool { return snap.Rows[i].Time > snap.Rows[j].Time })
	return snap
}

// toLedgerRow 上游流水 → 展示行。第二个返回值是归一后的类型（income/expense/其他）。
func toLedgerRow(t upstream.CreditTxn) (ledgerRow, string) {
	kind := t.Kind()
	row := ledgerRow{
		ID:      string(t.ID),
		Type:    kind,
		Title:   t.Title(),
		Source:  t.SourceLabel(),
		Delta:   t.Amount,
		Session: t.SessionID,
	}
	// 时间统一成 "2026-11-07 07:20"，与客户端智点明细一致
	stamp := t.Time
	if stamp == "" {
		stamp = t.CreateTime
	}
	if len(stamp) >= 16 {
		row.Time = stamp[:10] + " " + stamp[11:16]
	} else {
		row.Time = stamp
	}
	if len(t.ExpireTime) >= 10 {
		row.Expire = t.ExpireTime[:10]
	}
	return row, kind
}

// accountLedger 单账号流水：GET /panel/api/accounts/{uid}/ledger?refresh=1&limit=30
func (p *Panel) accountLedger(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	snap := p.fetchLedger(a, force)
	limit := atoiDefault(r.URL.Query().Get("limit"), 0)
	rows := snap.Rows
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"uid":       snap.UID,
		"at":        snap.At.Format(time.RFC3339),
		"rows":      rows,
		"earned":    snap.Earned,
		"spent":     snap.Spent,
		"today":     snap.Today,
		"income_n":  snap.Income,
		"expense_n": snap.Expense,
		"truncated": snap.Truncated,
		"error":     snap.Error,
	})
}

// ledgerAll 全账号流水（合并按时间倒序）：GET /panel/api/ledger?refresh=1&limit=60
func (p *Panel) ledgerAll(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	limit := atoiDefault(r.URL.Query().Get("limit"), 60)

	type accSummary struct {
		UID     string  `json:"uid"`
		Nick    string  `json:"nickname"`
		Earned  float64 `json:"earned"`
		Spent   float64 `json:"spent"`
		Today   float64 `json:"today"`
		Error   string  `json:"error,omitempty"`
	}
	summaries := make([]accSummary, 0, 4)
	rows := make([]map[string]any, 0, 64)

	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		snap := p.fetchLedger(a, force)
		summaries = append(summaries, accSummary{
			UID: a.UID, Nick: st.Nickname,
			Earned: snap.Earned, Spent: snap.Spent, Today: snap.Today, Error: snap.Error,
		})
		for _, row := range snap.Rows {
			rows = append(rows, map[string]any{
				"id": row.ID, "type": row.Type, "title": row.Title, "source": row.Source,
				"time": row.Time, "delta": row.Delta, "expire": row.Expire,
				"session_id": row.Session, "uid": a.UID, "nickname": st.Nickname,
			})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["time"]) > fmt.Sprint(rows[j]["time"])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"rows":      rows,
		"accounts":  summaries,
		"fetched_at": time.Now().Format(time.RFC3339),
	})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
