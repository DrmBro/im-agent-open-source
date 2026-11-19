package codex

import (
	"context"
	"encoding/json"
	"errors"
)

// PlaceInboxThread uses the server-owned sidebar order shared with Codex Desktop.
func (c *Client) PlaceInboxThread(ctx context.Context, thread, title string) error {
	t, err := c.ReadMetadata(ctx, thread)
	if err != nil {
		return err
	}
	if t.Section != nil {
		return nil
	}
	id, err := c.inboxSection(ctx)
	if err != nil {
		return err
	}
	if t.Name == "" && title != "" {
		if _, err = c.Call(ctx, "thread/name/set", map[string]string{"threadId": thread, "name": title}); err != nil {
			return err
		}
	}
	_, err = c.Call(ctx, "thread/section/move", map[string]string{"threadId": thread, "sectionId": id})
	return err
}

func (c *Client) inboxSection(ctx context.Context) (string, error) {
	const name = "IM Agent"
	params := map[string]any{"limit": 100}
	for {
		raw, err := c.Call(ctx, "threadSection/list", params)
		if err != nil {
			return "", err
		}
		var page struct {
			Data       []struct{ ID, Name string }
			NextCursor string
		}
		if err = json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		for _, s := range page.Data {
			if s.Name == name && s.ID != "" {
				return s.ID, nil
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == params["cursor"] {
			return "", errors.New("侧边栏分组游标重复")
		}
		params["cursor"] = page.NextCursor
	}
	raw, err := c.Call(ctx, "threadSection/create", map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	var result struct{ Section struct{ ID string } }
	if json.Unmarshal(raw, &result) != nil || result.Section.ID == "" {
		return "", errors.New("Codex 没有返回侧边栏分组 ID")
	}
	return result.Section.ID, nil
}
