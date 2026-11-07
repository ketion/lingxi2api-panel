package upstream

import (
	"context"
	"io"
	"time"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 这个文件是「灵犀适配壳」：把 workbuddy2api 原有的公开方法全部指向 lingxi.go 的实现，
// 从而完整保留上层的账号池、冷却熔断、面板、调度、日志等基础设施。
// 原 CodeBuddy 实现保留为 *WB 方法，便于对照与回退。

// ChatStreamContext 走灵犀三步骤（建会话 → 领 run → 对话），并把上游 SSE 翻成 OpenAI 帧。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.lingxiChatStream(ctx, a, body, clientIP, meta)
}

// FetchModels 用 sessions/plans 的模型目录。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c.lingxiModels(ctx, a)
}

// DailyCheckin 领取每日签到。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.lingxiCheckin(ctx, a)
}

// RefreshToken 灵犀没有 OAuth 刷新：Cookie 静态有效，过期只能重新导入。
func (c *Client) RefreshToken(a *auth.Auth) error { return nil }

// UserResource 返回剩余/总智点。
func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c.lingxiBalance(ctx, a)
}

func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, err = c.UserResource(a)
	return remain, total, 0, err
}

func (c *Client) UserResourceDetailedWithExpiry(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := c.lingxiBalanceDetail(ctx, a)
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	return int64(d.Total), int64(d.Total), expiringWithin(d, soon), d.EarliestAt, 0, nil
}

func (c *Client) UserResourceSnapshot(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, buckets []ExpiryBucket, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := c.lingxiBalanceDetail(ctx, a)
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, nil, err
	}
	// soon 窗口内到期的批次里，最近一批的剩余量
	earliestRemaining = int64(0)
	if !d.EarliestAt.IsZero() && d.EarliestAt.Before(time.Now().Add(soon)) {
		for _, b := range d.Buckets {
			if !b.ExpireAt.After(d.EarliestAt) {
				earliestRemaining += b.Remain
			}
		}
	}
	return int64(d.Total), int64(d.Total), expiringWithin(d, soon), d.EarliestAt, earliestRemaining, d.Buckets, nil
}

// expiringWithin 统计 soon 窗口内到期的智点总量（面板「快过期」提示用）。
func expiringWithin(d *lxBalanceDetail, soon time.Duration) int64 {
	if soon <= 0 {
		return 0
	}
	deadline := time.Now().Add(soon)
	var sum int64
	for _, b := range d.Buckets {
		if b.ExpireAt.Before(deadline) {
			sum += b.Remain
		}
	}
	return sum
}
