package panel

import (
	"testing"

	"github.com/lingxi2api/lingxi2api-panel/internal/upstream"
)

// 上游流水 → 面板行：类型归一（topup→income）、时间截成 "YYYY-MM-DD HH:MM"、
// 来源为空时用备注兜底、只有获得类才带到期日。
func TestToLedgerRow(t *testing.T) {
	cases := []struct {
		in     upstream.CreditTxn
		kind   string
		title  string
		source string
		time   string
		expire string
		delta  float64
	}{
		{
			// 获得类：type 是 topup，没有 source，靠 remark 兜底
			in: upstream.CreditTxn{
				Type: "topup", Amount: 100, TaskName: "任务奖励", Remark: "智点赠送",
				Time: "2026-11-07T07:20:15+08:00", ExpireTime: "2026-12-08T07:20:15+08:00",
			},
			kind: "income", title: "任务奖励", source: "智点赠送",
			time: "2026-11-07 07:20", expire: "2026-12-08", delta: 100,
		},
		{
			// 消耗类：中文来源原样透传，数字 id 已由 FlexID 转成字符串
			in: upstream.CreditTxn{
				ID: "28376339", Type: "expense", Amount: -1, TaskName: "你都有什么功能呢",
				Source: "灵犀客户端", Time: "2026-11-06T23:23:23+08:00",
			},
			kind: "expense", title: "你都有什么功能呢", source: "灵犀客户端",
			time: "2026-11-06 23:23", delta: -1,
		},
		{
			// 没有 task_name 时退回 remark
			in: upstream.CreditTxn{
				Type: "expense", Amount: -2, Remark: "会员权益",
				Time: "2026-11-05T10:00:00+08:00", SessionID: "120926630298255755",
			},
			kind: "expense", title: "会员权益", source: "会员权益",
			time: "2026-11-05 10:00", delta: -2,
		},
		{
			// 日切重置：面板会跳过，不能计入获得/消耗
			in:   upstream.CreditTxn{Type: "daily_reset", Time: "2026-11-06T00:00:00+08:00"},
			kind: "reset", title: "—", time: "2026-11-06 00:00",
		},
	}
	for i, c := range cases {
		row, kind := toLedgerRow(c.in)
		if kind != c.kind {
			t.Fatalf("#%d kind=%q want %q", i, kind, c.kind)
		}
		if row.Title != c.title {
			t.Fatalf("#%d title=%q want %q", i, row.Title, c.title)
		}
		if row.Source != c.source {
			t.Fatalf("#%d source=%q want %q", i, row.Source, c.source)
		}
		if row.Time != c.time {
			t.Fatalf("#%d time=%q want %q", i, row.Time, c.time)
		}
		if row.Expire != c.expire {
			t.Fatalf("#%d expire=%q want %q", i, row.Expire, c.expire)
		}
		if row.Delta != c.delta {
			t.Fatalf("#%d delta=%v want %v", i, row.Delta, c.delta)
		}
	}
}

// 浏览器登录只会把灵犀/金山域的 wps_sid / wpsua / csrf / uid 带回，
// 埋点与灰度 cookie 不带（既减小出站体积，也避免把无关身份带出去）。
func TestBuildCookieHeader(t *testing.T) {
	cookies := []cdpCookie{
		{Name: "wps_sid", Value: "SID1", Domain: ".kdocs.cn"},
		{Name: "wpsua", Value: "UA", Domain: ".kdocs.cn"},
		{Name: "csrf", Value: "C1", Domain: "lingxi.kdocs.cn"},
		{Name: "uid", Value: "1304", Domain: ".wps.cn"},
		{Name: "wps_sid", Value: "SID2-dup", Domain: "lingxi.wps.cn"}, // 重复键只留第一个
		{Name: "Hm_lvt_x", Value: "track", Domain: ".kdocs.cn"},       // 埋点不要
		{Name: "wps_sid", Value: "OTHER", Domain: ".example.com"},     // 非灵犀域不要
	}
	got := buildCookieHeader(cookies)
	want := "wps_sid=SID1; wpsua=UA; csrf=C1; uid=1304"
	if got != want {
		t.Fatalf("cookie=%q want %q", got, want)
	}
}
