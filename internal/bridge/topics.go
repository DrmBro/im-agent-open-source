package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

type creationRecord struct {
	State   string     `json:"state"`
	Address im.Address `json:"address"`
}

// rememberedCreate 必须在 bindMu 内调用。先落盘再请求平台，避免超时/崩溃后创建重复话题。
func (b *Bridge) rememberedCreate(key string, create func() (im.Address, error)) (im.Address, error) {
	raw, err := b.store.GetMeta(key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return im.Address{}, err
	}
	if err == nil {
		var r creationRecord
		if json.Unmarshal([]byte(raw), &r) != nil {
			return im.Address{}, diagnostics.UserError("创建记录损坏，停止自动创建")
		}
		if r.State == "ready" && r.Address.ChatID != "" {
			return r.Address, nil
		}
		if r.State != "failed" {
			return im.Address{}, diagnostics.UserError("上次群/话题创建结果不确定，已停止自动重建；请核对飞书与创建记录")
		}
	}
	if err = b.store.SetMeta(key, store.JSON(creationRecord{State: "creating"})); err != nil {
		return im.Address{}, err
	}
	a, err := create()
	if err != nil {
		var uncertain *im.DeliveryUncertain
		state := "failed"
		if errors.As(err, &uncertain) || a.ChatID != "" || a.RootMessageID != "" {
			state = "unknown"
		}
		if saveErr := b.store.SetMeta(key, store.JSON(creationRecord{State: state, Address: a})); saveErr != nil {
			return im.Address{}, diagnostics.UserError("创建失败且确认状态未能落盘，停止自动重建")
		}
		return im.Address{}, err
	}
	if a.ChatID == "" {
		return im.Address{}, diagnostics.UserError("创建结果缺少地址，停止自动重建")
	}
	if err = b.store.SetMeta(key, store.JSON(creationRecord{State: "ready", Address: a})); err != nil {
		return im.Address{}, diagnostics.UserError("群/话题可能已创建，但地址保存失败，停止自动重建")
	}
	return a, nil
}

func (b *Bridge) isSharedChat(a im.Address) (bool, error) {
	cc := b.conn(a.Connection)
	// 即使暂时切回独立群模式，已登记的共享群仍永久受保护。
	if cc.SharedChatID != "" && a.ChatID == cc.SharedChatID {
		return true, nil
	}
	if _, err := b.store.GetMeta("shared-chat:" + a.BaseKey()); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return b.store.HasTopics(a)
}

func (b *Bridge) topicParent(ctx context.Context, adapter im.Adapter, owner string) (im.Address, error) {
	topics, ok := adapter.(im.TopicAdapter)
	if !ok || !adapter.Capabilities().Topics {
		return im.Address{}, diagnostics.UserError("此 IM 没有话题能力")
	}
	cc := b.conn(adapter.Identity().Connection)
	parent := adapter.Identity()
	parent.ChatID = cc.SharedChatID
	if parent.ChatID == "" {
		var err error
		parent, err = b.rememberedCreate("topic-group-v1:"+cc.ID, func() (im.Address, error) {
			members := append([]string{}, cc.AdminUsers...)
			if owner != "" && !config.Contains(members, owner) {
				members = append(members, owner)
			}
			return topics.CreateTopicGroup(ctx, im.Conversation{Name: cc.ChatNamePrefix + " 工作台", Members: members, Owner: owner, IdempotencyKey: store.Hash("topic-group:" + cc.ID)})
		})
		if err != nil {
			return im.Address{}, err
		}
	}
	bound, err := b.store.HasGroupBinding(parent)
	if err != nil {
		return im.Address{}, err
	}
	if bound {
		return im.Address{}, diagnostics.UserError("目标共享群已绑定旧独立会话，请选择新群；不会覆盖或自动迁移旧绑定")
	}
	if err = topics.ValidateTopicGroup(ctx, parent); err != nil {
		return im.Address{}, err
	}
	if err = b.store.SetMeta("shared-chat:"+parent.BaseKey(), "1"); err != nil {
		return im.Address{}, err
	}
	return parent, nil
}

func (b *Bridge) newSessionAddress(ctx context.Context, adapter im.Adapter, msg im.Message, t codex.Thread) (im.Address, error) {
	cc := b.conn(msg.Address.Connection)
	if cc.ConversationMode != "topic" {
		return b.createSessionChat(ctx, adapter, msg, b.threadName(t, cc.ID))
	}
	if !cc.AutoCreateChat {
		return im.Address{}, diagnostics.UserError("请先在客户端启用自动创建群/话题")
	}
	parent, err := b.topicParent(ctx, adapter, msg.UserID)
	if err != nil {
		return im.Address{}, err
	}
	topics := adapter.(im.TopicAdapter)
	a, err := b.rememberedCreate("topic-v1:"+cc.ID+":"+t.ID, func() (im.Address, error) {
		a, e := topics.CreateTopic(ctx, parent, im.Topic{Name: b.threadName(t, cc.ID), WorkDir: t.Cwd, SessionID: t.ID, Status: "active", IdempotencyKey: t.ID})
		if e == nil && (!a.IsTopic() || a.RootMessageID == "") {
			return a, &im.DeliveryUncertain{Cause: diagnostics.UserError("话题创建未返回完整地址")}
		}
		return a, e
	})
	if err != nil {
		return im.Address{}, err
	}
	if a.BaseKey() != parent.BaseKey() || !a.IsTopic() || a.RootMessageID == "" {
		return im.Address{}, diagnostics.UserError("已保存的话题创建结果与当前共享群不一致，请人工核对，不自动迁移或重建")
	}
	return a, nil
}

func sessionClosureHint(a im.Address) string {
	if a.IsTopic() {
		return "归档仅关闭此话题对应的 Codex 会话，并标记入口；不会解散共享群。"
	}
	return "归档此会话会解散客户端创建的对应群；解散该独立群也会同步归档 Codex。"
}
func (b *Bridge) sessionCreatedNotice(ctx context.Context, adapter im.Adapter, msg im.Message, s store.Session) {
	if msg.Address.Key() == s.Address.Key() {
		return
	}
	text := "已创建独立会话，请到新群聊继续。"
	if s.Address.IsTopic() {
		text = fmt.Sprintf("已在共享群 %s 创建话题：%s。请在对应话题内继续。", s.Address.ChatID, b.topicName(s))
	}
	_, _ = adapter.SendText(ctx, msg.Address, text, msg.ID+":created-location")
}
func (b *Bridge) topicName(s store.Session) string {
	if p, err := b.store.Presentation(s.ThreadID); err == nil && p.Name != "" {
		return p.Name
	}
	if s.GroupName != "" {
		return s.GroupName
	}
	return "Codex 会话 " + shortID(s.ThreadID)
}

// 首卡改名和归档共用锁，归档后不会被并发名称补齐覆盖成活跃状态。
func (b *Bridge) updateTopicPresentation(ctx context.Context, s store.Session, name, status string) error {
	b.topicMu.Lock()
	defer b.topicMu.Unlock()
	current, err := b.store.ByThread(s.ThreadID)
	if err != nil {
		return err
	}
	if !current.Address.IsTopic() || !current.Managed {
		return diagnostics.UserError("不是可管理的话题入口")
	}
	if status == "active" && current.Status != "active" {
		return diagnostics.UserError("会话已关闭，停止更新活跃入口")
	}
	topics, ok := b.channels[current.Address.Connection].(im.TopicAdapter)
	if !ok {
		return diagnostics.UserError("原 IM 连接未实现话题入口更新")
	}
	return topics.UpdateTopic(ctx, current.Address, im.Topic{Name: name, WorkDir: current.WorkDir, SessionID: current.ThreadID, Status: status})
}
