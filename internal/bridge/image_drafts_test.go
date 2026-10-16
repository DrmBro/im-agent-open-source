package bridge

import (
	"context"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type draftAdapter struct {
	fixtureFiles
	notices []string
	sources []string
}

func (f *draftAdapter) SendText(_ context.Context, _ im.Address, text, _ string) (string, error) {
	f.notices = append(f.notices, text)
	return "notice", nil
}
func (f *draftAdapter) DownloadAttachment(_ context.Context, source string, _ im.Attachment) (io.ReadCloser, error) {
	f.sources = append(f.sources, source)
	return io.NopCloser(strings.NewReader("image bytes")), nil
}
func imageFixture(t *testing.T) (*Bridge, *store.Store, *draftAdapter, store.Session) {
	t.Helper()
	db, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	s := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}, Owner: "alice", WorkDir: t.TempDir(), Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	cfg.Connections = []config.Connection{{ID: "one", AllowedUsers: []string{"alice", "bob"}}}
	f := &draftAdapter{}
	b := New(cfg, db, nil, map[string]im.Adapter{"one": f}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return b, db, f, s
}
func TestImageIntakeWaitsWithoutCodexAndCombinesCaption(t *testing.T) {
	b, db, f, s := imageFixture(t)
	ctx := context.Background()
	photo := im.Message{ID: "image1", Address: s.Address, UserID: "alice", Attachments: []im.Attachment{{Kind: "image", Key: "img1", Name: "one.png"}}}
	// 没有 Codex client/任务队列也应该可以暂存；若误提交会触发测试失败。
	if e := b.receiveMessage(ctx, photo); e != nil {
		t.Fatal(e)
	}
	if len(f.notices) != 1 || len(f.sources) != 0 {
		t.Fatal("photo executed instead of staged")
	}
	if e := b.receiveMessage(ctx, photo); e != nil {
		t.Fatal(e)
	}
	if len(f.notices) != 1 {
		t.Fatal("duplicate image event replayed")
	}
	photo.ID = "image2"
	photo.Attachments[0].Key = "img2"
	if e := b.receiveMessage(ctx, photo); e != nil {
		t.Fatal(e)
	}
	other, handled, e := b.prepareImageBatch(ctx, f, im.Message{ID: "bob-text", UserID: "bob", Address: s.Address, Text: "我的问题"})
	if e != nil || handled || len(other.Attachments) != 0 {
		t.Fatal("cross-user images leaked")
	}
	caption := im.Message{ID: "caption", UserID: "alice", Address: s.Address, Text: "分析这两张图的区别"}
	db.Claim(caption.ID)
	merged, handled, e := b.prepareImageBatch(ctx, f, caption)
	if e != nil || handled || len(merged.Attachments) != 2 {
		t.Fatal("caption merge failed", e)
	}
	if merged.Attachments[0].SourceMessageID != "image1" || merged.Attachments[1].SourceMessageID != "image2" {
		t.Fatal("image source overwritten by caption")
	}
	prompt, e := b.receiveAttachments(ctx, f, s, merged)
	if e != nil {
		t.Fatal(e)
	}
	if len(f.sources) != 2 || f.sources[0] != "image1" || f.sources[1] != "image2" || !strings.Contains(prompt, caption.Text) {
		t.Fatal("image download used wrong source", f.sources)
	}
	empty, handled, e := b.prepareImageBatch(ctx, f, im.Message{ID: "later", UserID: "alice", Address: s.Address, Text: "新问题"})
	if e != nil || handled || len(empty.Attachments) != 0 {
		t.Fatal("previous images reused")
	}
}
func TestImageSendCancelCommandsAndArchivedGroup(t *testing.T) {
	b, db, f, s := imageFixture(t)
	ctx := context.Background()
	photo := im.Message{ID: "photo", UserID: "alice", Address: s.Address, Attachments: []im.Attachment{{Kind: "image", Key: "img", Name: "image.png"}}}
	if e := b.receiveMessage(ctx, photo); e != nil {
		t.Fatal(e)
	}
	command := im.Message{ID: "status", UserID: "alice", Address: s.Address, Text: "/status"}
	if _, handled, e := b.prepareImageBatch(ctx, f, command); e != nil || handled {
		t.Fatal("status unexpectedly consumes draft")
	}
	command.ID = "send"
	command.Text = "/send"
	db.Claim(command.ID)
	merged, handled, e := b.prepareImageBatch(ctx, f, command)
	if e != nil || handled || len(merged.Attachments) != 1 || merged.Text != "请分析这些图片。" {
		t.Fatal("send failed", e)
	}
	photo.ID = "next"
	if e = b.receiveMessage(ctx, photo); e != nil {
		t.Fatal(e)
	}
	command.ID = "cancel"
	command.Text = "/cancel"
	db.Claim(command.ID)
	if _, handled, e = b.prepareImageBatch(ctx, f, command); e != nil || !handled {
		t.Fatal("cancel failed")
	}
	if _, ok, e := db.ConsumeImages(s.Address, "alice", "late", 0, time.Now()); e != nil || ok {
		t.Fatal("cancelled draft submitted")
	}
	if e = db.Archive(s.ThreadID); e != nil {
		t.Fatal(e)
	}
	photo.ID = "archived"
	if e = b.receiveMessage(ctx, photo); e == nil {
		t.Fatal("archived group staged images")
	}
}

func TestRollbackRejectsRetiredPhoneControlCards(t *testing.T) {
	b, _, _, s := imageFixture(t)
	for _, id := range []string{"task:old-task", "mobile:old-confirmation"} {
		e := b.receiveInteraction(context.Background(), im.Interaction{ActionID: id, Choice: "confirm", Address: s.Address, MessageID: "old-card", UserID: "alice"})
		if e == nil || !strings.Contains(e.Error(), "回退到基础版本") {
			t.Fatal("retired action was not rejected before execution", id, e)
		}
	}
}
