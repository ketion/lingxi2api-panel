package upstream

import (
	"encoding/json"
	"testing"
)

// 真实抓包（2026-11-07，账号 1304453904）。这条 fixture 里有三个关键差异，
// 每一条都曾让面板整页流水变空：
//  1. 获得类记录的 type 是 "topup"，不是 "income"；
//  2. 消耗类记录的 id 是**数字**（28376339），获得类记录没有 id；
//  3. source 是**中文**（"灵犀客户端"），且获得类没有 source。
const ledgerFixture = `{"data":{"list":[
 {"type":"topup","amount":100,"task_name":"任务奖励","time":"2026-11-07T07:20:15+08:00",
  "remark":"智点赠送","vas_status":"effected","sku_key":"ai_credits_grant",
  "expire_time":"2026-12-08T07:20:15+08:00"},
 {"type":"expense","amount":-1,"task_name":"你都有什么功能呢","source":"灵犀客户端",
  "time":"2026-11-06T23:23:23+08:00","id":28376339,"session_id":"12092489608265934",
  "group_id":"12092663029825755","task_consumed":1,"actual_deducted":1},
 {"type":"daily_reset","amount":0,"time":"2026-11-06T00:00:00+08:00"}
],"income_cursor":1793975991,"expense_cursor":28376339,"has_more":false},"result":"ok","v7code":0}`

func TestCreditTxnUnmarshalRealPayload(t *testing.T) {
	var env struct {
		Data struct {
			List          []CreditTxn `json:"list"`
			IncomeCursor  int64       `json:"income_cursor"`
			ExpenseCursor int64       `json:"expense_cursor"`
			HasMore       bool        `json:"has_more"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(ledgerFixture), &env); err != nil {
		t.Fatalf("解析失败（数字 id 应该被 FlexID 兜住）: %v", err)
	}
	items := env.Data.List
	if len(items) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(items))
	}

	// 1) 获得类：type=topup → Kind()=income
	if k := items[0].Kind(); k != "income" {
		t.Fatalf("topup 应归一成 income，实际 %q", k)
	}
	if items[0].ID != "" {
		t.Fatalf("topup 记录没有 id，实际 %q", items[0].ID)
	}
	if items[0].SourceLabel() != "智点赠送" {
		t.Fatalf("获得类来源应回落备注，实际 %q", items[0].SourceLabel())
	}

	// 2) 消耗类：数字 id 要能读出来，来源是中文原样
	if k := items[1].Kind(); k != "expense" {
		t.Fatalf("expense 应归一成 expense，实际 %q", k)
	}
	if string(items[1].ID) != "28376339" {
		t.Fatalf("数字 id 应转成 28376339，实际 %q", items[1].ID)
	}
	if items[1].SourceLabel() != "灵犀客户端" {
		t.Fatalf("来源应原样透传，实际 %q", items[1].SourceLabel())
	}
	if items[1].ActualDeducted != 1 {
		t.Fatalf("actual_deducted 应为 1，实际 %v", items[1].ActualDeducted)
	}

	// 3) daily_reset 必须被识别出来（面板会过滤，不然统计会串）
	if k := items[2].Kind(); k != "reset" {
		t.Fatalf("daily_reset 应归一成 reset，实际 %q", k)
	}
	if env.Data.ExpenseCursor != 28376339 || env.Data.IncomeCursor != 1793975991 {
		t.Fatalf("游标解析错误: %+v", env.Data)
	}
}
