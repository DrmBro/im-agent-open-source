package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"im-agent/internal/config"
	"strings"
)

type ProjectChoice struct {
	Value string `json:"value"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
}
type ModelChoice struct {
	Model         string   `json:"model"`
	Name          string   `json:"name"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
}
type CreationCatalog struct {
	Projects []ProjectChoice `json:"projects"`
	Models   []ModelChoice   `json:"models"`
}
type CreationSelection struct {
	Project string `json:"project"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
	Prompt  string `json:"prompt"`
}

func (c *Client) CreationCatalog(ctx context.Context) (CreationCatalog, error) {
	out := CreationCatalog{Projects: []ProjectChoice{{Value: "automatic", Name: "无项目（自动分配目录）"}}, Models: []ModelChoice{}}
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, e := c.Call(ctx, "project/list", params)
		if e != nil {
			return out, e
		}
		var data struct {
			Data []struct {
				ID, Name string
				Roots    []struct{ Path string }
			}
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &data) != nil || data.Data == nil {
			return out, errors.New("Codex 项目目录响应无效")
		}
		for _, p := range data.Data {
			for i, r := range p.Roots {
				path, e := config.ExistingDirectory(r.Path)
				if e != nil {
					continue
				}
				if len(out.Projects) >= 100 {
					return out, errors.New("可选项目目录超过表单上限，请先缩小桌面项目范围")
				}
				out.Projects = append(out.Projects, ProjectChoice{Value: fmt.Sprintf("%s:%d", p.ID, i), ID: p.ID, Name: p.Name, Path: path})
			}
		}
		if data.NextCursor == nil || *data.NextCursor == "" {
			break
		}
		if *data.NextCursor == cursor || page == 9 {
			return out, errors.New("项目目录分页异常")
		}
		cursor = *data.NextCursor
	}
	cursor = ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, e := c.Call(ctx, "model/list", params)
		if e != nil {
			return out, e
		}
		var data struct {
			Data []struct {
				Model       string
				DisplayName string `json:"displayName"`
				Hidden      bool
				Default     string `json:"defaultReasoningEffort"`
				Efforts     []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			}
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &data) != nil || data.Data == nil {
			return out, errors.New("Codex 模型目录响应无效")
		}
		for _, m := range data.Data {
			if m.Hidden || m.Model == "" {
				continue
			}
			choice := ModelChoice{Model: m.Model, Name: m.DisplayName, DefaultEffort: m.Default}
			for _, eff := range m.Efforts {
				choice.Efforts = append(choice.Efforts, eff.Effort)
			}
			if len(choice.Efforts) == 0 {
				continue
			}
			out.Models = append(out.Models, choice)
			if len(out.Models) > 100 {
				return out, errors.New("模型目录超过表单上限")
			}
		}
		if data.NextCursor == nil || *data.NextCursor == "" {
			break
		}
		if *data.NextCursor == cursor || page == 9 {
			return out, errors.New("模型目录分页异常")
		}
		cursor = *data.NextCursor
	}
	if len(out.Models) == 0 {
		return out, errors.New("Codex 未返回可选模型能力，未编造选项")
	}
	return out, nil
}
func (c CreationCatalog) Validate(s CreationSelection) (ProjectChoice, error) {
	var project ProjectChoice
	found := false
	for _, p := range c.Projects {
		if p.Value == s.Project {
			project = p
			found = true
			break
		}
	}
	if !found {
		return project, errors.New("请选择表单中的有效项目")
	}
	for _, m := range c.Models {
		if m.Model == s.Model {
			for _, e := range m.Efforts {
				if e == s.Effort {
					return project, nil
				}
			}
			return project, errors.New("所选模型不支持此思考程度，请重新选择")
		}
	}
	return project, errors.New("所选模型不在本次同步的目录中")
}
func (c *Client) StartConfiguredThread(ctx context.Context, cfg config.Config, p ProjectChoice, s CreationSelection) (Thread, error) {
	call := c.Call
	if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
		// 直接由桌面宿主的 App Server 创建：避免辅助进程持有写锁，
		// 并让所选模型/思考程度属于实际执行的桌面会话，不切换客户端身份。
		desktop, err := discoverDesktopCDP(ctx, c.runtimeConfig.Codex.CdpPort)
		if err != nil {
			return Thread{}, err
		}
		defer desktop.close()
		call = desktop.threadRequest
	}
	return c.startConfiguredThread(ctx, cfg, p, s, call)
}

func (c *Client) startConfiguredThread(ctx context.Context, cfg config.Config, p ProjectChoice, s CreationSelection, call func(context.Context, string, any) (json.RawMessage, error)) (Thread, error) {
	work, e := NewSessionDirectory(cfg.StateDir, p.Path)
	if e != nil {
		return Thread{}, e
	}
	if p.Path != "" && work != p.Path {
		return Thread{}, errors.New("项目目录在选择后发生变化，未创建会话")
	}
	cfg.WorkDir = work
	params := Options(cfg)
	params["ephemeral"] = false
	params["model"] = s.Model
	params["config"] = map[string]any{"model_reasoning_effort": s.Effort}
	params["allowProviderModelFallback"] = false
	if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
		// 桌面桥接按 rollout 关联首问与回复，空分页线程尚无 rollout。
		// 使用协议支持的 legacy 历史，使首次输入前即可建立可靠的消费基线。
		params["historyMode"] = "legacy"
	}
	if p.ID != "" {
		params["projectId"] = p.ID
	}
	raw, e := call(ctx, "thread/start", params)
	if e != nil {
		return Thread{}, e
	}
	var result struct {
		Thread Thread `json:"thread"`
		Model  string `json:"model"`
		Effort string `json:"reasoningEffort"`
		Cwd    string `json:"cwd"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" {
		if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
			return Thread{}, &DesktopCreationUnconfirmed{}
		}
		return Thread{}, errors.New("新会话响应缺少 ID，创建结果未确认")
	}
	t := result.Thread
	if result.Model != s.Model || result.Effort != s.Effort {
		return t, errors.New("新会话实际模型或思考程度与所选值不一致，未提交首问")
	}
	actual, e := config.ExistingDirectory(t.Cwd)
	if e != nil || actual != work {
		return t, errors.New("新会话实际工作目录与选择不一致，未提交首问")
	}
	if p.ID != "" && t.ProjectID != p.ID {
		return t, errors.New("Codex 未确认所选项目归属，未提交首问")
	}
	t.Cwd = actual
	c.freshThreads.Store(t.ID, t)
	if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
		// 空线程首轮前未必落盘。由桌面自己命名保存，让辅助只读进程
		// 能建立消费基线；不写 Codex 数据库、不伪造首轮。
		title := strings.TrimSpace(strings.SplitN(s.Prompt, "\n", 2)[0])
		if title == "" {
			title = "IM 新会话"
		}
		if chars := []rune(title); len(chars) > 64 {
			title = string(chars[:64])
		}
		if _, e = call(ctx, "thread/name/set", map[string]any{"threadId": t.ID, "name": title}); e != nil {
			return t, fmt.Errorf("新会话尚未保存为桌面可读取的会话，未提交首问：%w", e)
		}
		if _, _, e = c.LiveBaseline(ctx, t.ID); e != nil {
			return t, fmt.Errorf("新会话落盘尚未确认，未提交首问：%w", e)
		}
		t.Name = title
		c.freshThreads.Store(t.ID, t)
	}
	return t, nil
}
