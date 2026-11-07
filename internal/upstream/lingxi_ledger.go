package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 智点流水接口（从客户端打包代码 desktop-renderer-dist 反查得到，参数名与原实现一致）：
//   GET /api/public/v1/credits/transaction/list
//       ?type=all|income|expense&income_cursor&expense_cursor&limit&category&include_future&session_id
//   GET /api/public/v1/credits/transaction/expense
//       ?session_id&cursor&limit
//
// 这是**唯一能拿到真实扣费**的地方：对话接口的 response 不带计费结果，
// 余额又是延迟归账，只有流水里写着这一次实际扣了多少（amount 为负数）。
const (
	LXTransactionList    = "/api/public/v1/credits/transaction/list"
	LXTransactionExpense = "/api/public/v1/credits/transaction/expense"
)

// FlexID 上游 id 的容错类型。
//
// 实抓发现：消耗类记录带数字 id（28376339），获得类记录（topup）**没有 id 字段**。
// 早期直接声明成 string，一条数字 id 就会让整页 json.Unmarshal 失败
// （面板表现为"部分账号读取失败：cannot unmarshal number into Go struct field"），
// 于是整个账号的流水都变成空。
type FlexID string

func (f *FlexID) UnmarshalJSON(b []byte) error {
	v := strings.TrimSpace(string(b))
	if v == "null" || v == "" {
		*f = ""
		return nil
	}
	*f = FlexID(strings.Trim(v, `"`))
	return nil
}

// CreditTxn 一条智点变动流水。字段与客户端渲染用的完全对应：
// 任务名 / 来源 / 时间 / 智点变化 / 到期时间。
//
// 注意 type 的取值：获得类是 "topup"（不是 income），消耗是 "expense"，
// 另有 "daily_reset" 属于日切重置、客户端也会过滤掉。
type CreditTxn struct {
	ID             FlexID  `json:"id"`
	Type           string  `json:"type"`
	Amount         float64 `json:"amount"`
	TaskName       string  `json:"task_name"`
	Remark         string  `json:"remark"`
	Source         string  `json:"source"` // 实抓是中文，如「灵犀客户端」
	SourceTag      string  `json:"source_tag"`
	Time           string  `json:"time"`
	CreateTime     string  `json:"create_time"`
	EffectTime     string  `json:"effect_time"`
	ExpireTime     string  `json:"expire_time"`
	SessionID      string  `json:"session_id"`
	GroupID        string  `json:"group_id"`
	SkuKey         string  `json:"sku_key"`
	VasStatus      string  `json:"vas_status"`
	Consumed       float64 `json:"consumed"`
	TaskConsumed   float64 `json:"task_consumed"`
	ActualDeducted float64 `json:"actual_deducted"`
	IsOveruse      bool    `json:"is_overuse"`
}

// Kind 归一成面板用的三类：income / expense / reset。
// 上游的 type 与金额都可能单边缺失，所以以金额符号为主、type 为辅。
func (t CreditTxn) Kind() string {
	switch t.Type {
	case "daily_reset":
		return "reset"
	}
	if t.Amount > 0 {
		return "income"
	}
	if t.Amount < 0 {
		return "expense"
	}
	if t.Type == "topup" || t.Type == "grant" || t.Type == "reward" {
		return "income"
	}
	return t.Type
}

// Title 展示名：优先任务名，其次备注，最后退回会话号。
func (t CreditTxn) Title() string {
	if t.TaskName != "" {
		return t.TaskName
	}
	if t.Remark != "" {
		return t.Remark
	}
	if t.SessionID != "" {
		s := t.SessionID
		if len(s) > 8 {
			s = s[len(s)-8:]
		}
		return "会话 " + s
	}
	return "—"
}

// SourceLabel 来源展示：上游 source 是中文，获得类通常没有，用备注兜底。
func (t CreditTxn) SourceLabel() string {
	if t.Source != "" && t.Source != "system" {
		return t.Source
	}
	if t.Remark != "" {
		return t.Remark
	}
	if t.SkuKey != "" {
		return t.SkuKey
	}
	return ""
}

// CreditLedgerPage 一页流水 + 双向游标。
type CreditLedgerPage struct {
	Items         []CreditTxn
	IncomeCursor  int64
	ExpenseCursor int64
	HasMore       bool
}

// lingxiLedger 取一页智点流水。
func (c *Client) lingxiLedger(ctx context.Context, a *auth.Auth, typ string, incomeCursor, expenseCursor int64, limit int) (*CreditLedgerPage, error) {
	if typ == "" {
		typ = "all"
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	q := url.Values{}
	q.Set("type", typ)
	// 上游只认与 type 匹配的游标：income 只给 income_cursor，expense 只给 expense_cursor
	if typ != "expense" {
		q.Set("income_cursor", strconv.FormatInt(incomeCursor, 10))
	}
	if typ != "income" {
		q.Set("expense_cursor", strconv.FormatInt(expenseCursor, 10))
		q.Set("limit", strconv.Itoa(limit))
	}
	env, err := c.lxDo(ctx, a, http.MethodGet, LXTransactionList+"?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		List          []CreditTxn `json:"list"`
		IncomeCursor  int64       `json:"income_cursor"`
		ExpenseCursor int64       `json:"expense_cursor"`
		HasMore       bool        `json:"has_more"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, err
	}
	return &CreditLedgerPage{Items: d.List, IncomeCursor: d.IncomeCursor, ExpenseCursor: d.ExpenseCursor, HasMore: d.HasMore}, nil
}

// lingxiSessionExpense 取某个会话的全部消耗明细（精确到每次 run）。
func (c *Client) lingxiSessionExpense(ctx context.Context, a *auth.Auth, sessionID string, cursor int64, limit int) (*CreditLedgerPage, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	q := url.Values{}
	q.Set("session_id", sessionID)
	q.Set("cursor", strconv.FormatInt(cursor, 10))
	q.Set("limit", strconv.Itoa(limit))
	env, err := c.lxDo(ctx, a, http.MethodGet, LXTransactionExpense+"?"+q.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		List    []CreditTxn `json:"list"`
		Cursor  int64       `json:"cursor"`
		HasMore bool        `json:"has_more"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, err
	}
	return &CreditLedgerPage{Items: d.List, ExpenseCursor: d.Cursor, HasMore: d.HasMore}, nil
}

// CreditLedger 是对外（panel）暴露的取流水入口。
func (c *Client) CreditLedger(a *auth.Auth, typ string, incomeCursor, expenseCursor int64, limit int) (*CreditLedgerPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.lingxiLedger(ctx, a, typ, incomeCursor, expenseCursor, limit)
}

// SessionExpense 取会话消耗明细。
func (c *Client) SessionExpense(a *auth.Auth, sessionID string, cursor int64, limit int) (*CreditLedgerPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.lingxiSessionExpense(ctx, a, sessionID, cursor, limit)
}
