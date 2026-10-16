package bridge

import (
	"bytes"
	"context"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureFiles struct {
	uploads   int
	downloads int
	body      string
}

func (f *fixtureFiles) Identity() im.Address          { return im.Address{Channel: "fake", Connection: "one"} }
func (f *fixtureFiles) Capabilities() im.Capabilities { return im.Capabilities{} }
func (f *fixtureFiles) Start(context.Context, func(context.Context, im.Message) error, func(context.Context, im.Interaction) error) error {
	return nil
}
func (f *fixtureFiles) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	return im.Address{}, nil
}
func (f *fixtureFiles) SendText(context.Context, im.Address, string, string) (string, error) {
	return "text", nil
}
func (f *fixtureFiles) SendCard(context.Context, im.Address, im.Card, string) (string, error) {
	return "card", nil
}
func (f *fixtureFiles) UpdateCard(context.Context, im.Address, string, im.Card) error { return nil }
func (f *fixtureFiles) DownloadAttachment(context.Context, string, im.Attachment) (io.ReadCloser, error) {
	f.downloads++
	return io.NopCloser(strings.NewReader("untrusted attachment content")), nil
}
func (f *fixtureFiles) SendAttachment(_ context.Context, _ im.Address, _, _ string, r io.Reader, _ string) (string, error) {
	f.uploads++
	b, _ := io.ReadAll(r)
	f.body = string(b)
	return "file-receipt", nil
}
func TestAttachmentBridgeDoesNotReplayOrLeakPaths(t *testing.T) {
	dir := t.TempDir()
	db, e := store.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	work := t.TempDir()
	s := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}, WorkDir: work, Owner: "user", Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	cfg := config.Defaults()
	cfg.StateDir = dir
	cfg.Connections = []config.Connection{{ID: "one", AllowedUsers: []string{"user"}}}
	adapter := &fixtureFiles{}
	b := New(cfg, db, nil, map[string]im.Adapter{"one": adapter}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 非白名单用户在下载前返回，不触发任何附件网络操作。
	denied := im.Message{ID: "denied", UserID: "outsider", Address: s.Address, Attachments: []im.Attachment{{Kind: "file", Key: "key", Name: "log.txt"}}}
	if e = b.receiveMessage(context.Background(), denied); e != nil {
		t.Fatal(e)
	}
	if adapter.downloads != 0 {
		t.Fatal("unauthorized download")
	}
	incoming := im.Message{ID: "incoming", Text: "分析日志", UserID: "user", Address: s.Address, Attachments: denied.Attachments}
	text, e := b.receiveAttachments(context.Background(), adapter, s, incoming)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(text, "不可信数据") || !strings.Contains(text, "log.txt") {
		t.Fatal("missing attachment prompt")
	}
	if _, e = b.receiveAttachments(context.Background(), adapter, s, incoming); e == nil {
		t.Fatal("duplicate inbound reprocessed")
	}
	path := filepath.Join(work, "result.txt")
	os.WriteFile(path, []byte("final result"), 0600)
	r := SendFileRequest{ID: "user-action", ThreadID: s.ThreadID, Path: path, Kind: "file"}
	if e = b.SendFile(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	if e = b.SendFile(context.Background(), r); e == nil {
		t.Fatal("duplicate outbound sent")
	}
	if adapter.uploads != 1 || adapter.body != "final result" {
		t.Fatal("wrong export")
	}
	snapshot, e := store.TaskCenterSnapshot(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(snapshot.Attachments) != 2 {
		t.Fatal("transfer records missing")
	}
	var buf bytes.Buffer
	for _, x := range snapshot.Attachments {
		buf.WriteString(x.Path)
	}
	if buf.Len() != 0 {
		t.Fatal("snapshot exposes local paths")
	}
}
