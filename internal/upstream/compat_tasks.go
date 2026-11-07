package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lingxi2api/lingxi2api-panel/internal/auth"
)

// 灵犀任务中心适配：把 /api/public/v1/tasks 映射成网关统一的 Task 结构。
// 灵犀只有 4 个任务（每日签到 + 三个一次性体验任务），没有 CodeBuddy 那套
// 「接受 → 进度 → 领取」的成长任务链，所以 AcceptTasks 是幂等空操作。

type lxTask struct {
	TaskKey     string `json:"task_key"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Reward      int64  `json:"reward_amount"`
	Status      string `json:"status"`
	Action      string `json:"action"`
}

type lxTaskList struct {
	Tasks      []lxTask `json:"tasks"`
	TotalClaim int64    `json:"total_claimed_credits"`
}

func (c *Client) lingxiTasks(ctx context.Context, a *auth.Auth) ([]Task, int64, error) {
	env, err := c.lxDo(ctx, a, http.MethodGet, LXTasks, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	var d lxTaskList
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, 0, fmt.Errorf("解析任务列表失败: %w", err)
	}
	out := make([]Task, 0, len(d.Tasks))
	for _, t := range d.Tasks {
		task := Task{
			TaskCode:    t.TaskKey,
			Title:       t.Title,
			Description: t.Description,
			Credit:      t.Reward,
			HasReward:   t.Reward > 0,
			TaskType:    t.Type,
			Status:      t.Status,
			Target:      1,
			Current:     0,
		}
		switch t.Status {
		case "claimable":
			task.Claimable = true
			task.Current = 1
		case "claimed":
			task.Claimed = true
			task.Current = 1
		}
		if t.Action != "" {
			task.JumpURL = "lingxi://" + t.Action
		} else if t.Type == "daily" {
			task.Tag = "每日"
		}
		out = append(out, task)
	}
	return out, d.TotalClaim, nil
}

// ListTasks 拉灵犀任务中心。
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tasks, _, err := c.lingxiTasks(ctx, a)
	return tasks, err
}

func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) { return c.ListTasks(a) }

// AcceptTasks 灵犀没有"接受任务"这一步，幂等返回 nil。
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error { return nil }

func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) error { return nil }

// ClaimReward 领取指定任务；返回智点奖励（灵犀无能量概念）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (int64, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := strings.TrimSpace(taskCode)
	if key == "" {
		return 0, 0, fmt.Errorf("task_code 为空")
	}
	path := fmt.Sprintf(LXClaim, key)
	env, err := c.lxDo(ctx, a, http.MethodPost, path, map[string]any{}, nil)
	if err != nil {
		return 0, 0, err
	}
	var r struct {
		Reward int64 `json:"reward_amount"`
	}
	_ = json.Unmarshal(env.Data, &r)
	return r.Reward, 0, nil
}

func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (int64, int64, error) {
	return c.ClaimReward(a, taskCode)
}
