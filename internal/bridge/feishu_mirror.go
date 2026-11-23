package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) ensureFeishuMirror(ctx context.Context, a im.Adapter, t codex.Thread) error {
	cc := b.conn(a.Identity().Connection)
	if _, err := b.store.FeishuMirror(t.ID, cc.ID); !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, since := assistantDay(time.Now())
	if t.UpdatedAt < since/1000 {
		return nil
	}
	if len(cc.AdminUsers) == 0 {
		return errors.New("飞书群缺少管理员")
	}
	title, err := b.store.MirrorItemTitle(t.ID)
	if err != nil {
		return err
	}
	if title != "" {
		t.Name = title
	}
	address, err := b.newSessionAddress(ctx, a, im.Message{Address: a.Identity(), UserID: cc.AdminUsers[0], ID: "watch:" + t.ID}, t)
	if err != nil {
		return err
	}
	return b.store.SaveFeishuMirror(t.ID, address, since)
}

func (b *Bridge) syncFeishuMirrors(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		mirrors, err := b.store.ActiveFeishuMirrors(b.cfg.Watch.ConnectionID)
		if err != nil {
			b.log.Warn("读取飞书同步群失败", "error", diagnostics.Error(err))
		}
		for _, m := range mirrors {
			if ctx.Err() != nil {
				return
			}
			op, cancel := context.WithTimeout(ctx, 35*time.Second)
			err := b.syncFeishuMirror(op, m)
			cancel()
			if err != nil {
				b.log.Warn("飞书副本同步待处理", "thread", m.Thread, "error", diagnostics.Error(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *Bridge) syncFeishuMirror(ctx context.Context, m store.FeishuMirror) error {
	engine := b.engineForThread(m.Thread)
	files, err := engine.LiveRolloutFiles(ctx, m.Thread)
	if err != nil {
		return err
	}
	for _, path := range files {
		header, size, err := engine.LiveRolloutInfo(ctx, m.Thread, path)
		if err != nil {
			return err
		}
		key := "feishu-mirror-cursor:" + store.Hash(m.Thread+"\x00"+m.Address.Key()+"\x00"+path)
		var cursor struct {
			Offset      int64
			Fingerprint string
		}
		raw, err := b.store.GetMeta(key)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			cursor.Offset = header
		case err != nil:
			return err
		default:
			if err := json.Unmarshal([]byte(raw), &cursor); err != nil {
				return err
			}
		}
		if cursor.Offset == size {
			continue
		}
		for batch := 0; batch < 8; batch++ {
			messages, next, fp, err := engine.ReadLiveMessagesChecked(ctx, m.Thread, path, cursor.Offset, cursor.Fingerprint)
			if err != nil {
				return err
			}
			for _, msg := range messages {
				if msg.Issue != nil {
					return fmt.Errorf("日志条目待核对：%s (%d)", msg.Issue.Kind, msg.Offset)
				}
				when, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
				if err != nil {
					return errors.New("公开消息缺少有效时间")
				}
				if when.UnixMilli() < m.Since {
					continue
				}
				if msg.Role == "user" {
					original, _ := codex.DesktopInputForms(msg.Text)
					echo, err := b.store.IsFeishuInputEcho(m, msg.ID, path, store.Hash(original), msg.Offset)
					if err != nil {
						return err
					}
					if echo {
						continue
					}
				}
				if err = b.sendFeishuMirrorEntry(ctx, m, msg); err != nil {
					return err
				}
			}
			if next == cursor.Offset {
				break
			}
			cursor.Offset, cursor.Fingerprint = next, fp
			if err := b.store.SetMeta(key, store.JSON(cursor)); err != nil {
				return err
			}
			if next >= size {
				break
			}
		}
	}
	return nil
}

func (b *Bridge) sendFeishuMirrorEntry(ctx context.Context, m store.FeishuMirror, msg codex.LiveMessage) error {
	a := b.channels[m.Address.Connection]
	if a == nil {
		return errors.New("飞书连接不可用")
	}
	md, markdown := a.(im.MarkdownAdapter)
	entry := b.mirrorEntryForThread(m.Thread, msg.HistoryMessage, msg.Offset, markdown)
	for i, body := range entry.Parts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		claimed, err := b.store.ClaimFeishuMirrorPart(m, msg.ID, i)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		key := store.Hash(fmt.Sprintf("%s:%s:%s:%d", m.Address.Key(), m.Thread, msg.ID, i))
		var receipt string
		if markdown && msg.Role == "assistant" {
			receipt, err = md.SendMarkdown(ctx, m.Address, body, key)
		} else {
			receipt, err = a.SendText(ctx, m.Address, body, key)
		}
		var unknown *im.DeliveryUncertain
		uncertain := errors.As(err, &unknown) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (err == nil && receipt == "")
		if e := b.store.FinishFeishuMirrorPart(m, msg.ID, i, receipt, uncertain); e != nil {
			return e
		}
		if err != nil {
			return err
		}
		if uncertain {
			return errors.New("飞书未确认送达，请核对回执")
		}
	}
	return nil
}
