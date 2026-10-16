package store

import (
	"im-agent/internal/im"
	"testing"
	"time"
)

func TestImageDraftScopeClaimExpiry(t *testing.T) {
	db, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	address := im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}
	now := time.Unix(1000, 0)
	photo := im.Attachment{Kind: "image", Key: "img", Name: "image.png", SourceMessageID: "photo", SourceIndex: 0}
	db.Claim("photo")
	draft, e := db.StageImages(address, "alice", "photo", []im.Attachment{photo}, now)
	if e != nil {
		t.Fatal(e)
	}
	if len(draft.Images) != 1 {
		t.Fatal("image missing")
	}
	if _, ok, e := db.ConsumeImages(address, "bob", "caption-bob", 0, now); e != nil || ok {
		t.Fatal("cross-user draft consumed")
	}
	other := address
	other.ChatID = "other"
	if _, ok, e := db.ConsumeImages(other, "alice", "caption-other", 0, now); e != nil || ok {
		t.Fatal("cross-chat draft consumed")
	}
	got, ok, e := db.ConsumeImages(address, "alice", "caption", 0, now)
	if e != nil || !ok || got.Images[0].SourceMessageID != "photo" {
		t.Fatal("source message lost", e)
	}
	if _, ok, e = db.ConsumeImages(address, "alice", "again", 0, now); e != nil || ok {
		t.Fatal("draft consumed twice")
	}
	db.Claim("photo2")
	photo.SourceMessageID = "photo2"
	if _, e = db.StageImages(address, "alice", "photo2", []im.Attachment{photo}, now); e != nil {
		t.Fatal(e)
	}
	expired, e := db.ExpireImageDrafts(now.Add(11 * time.Minute))
	if e != nil || len(expired) != 1 {
		t.Fatal("expiry failed", e)
	}
	if _, ok, e = db.ConsumeImages(address, "alice", "late", 0, now.Add(11*time.Minute)); e != nil || ok {
		t.Fatal("expired draft submitted")
	}
}

func TestImageDraftDuplicateLimitAndRestart(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	a := im.Address{Channel: "fake", Connection: "one", ChatID: "chat", TopicID: "topic1"}
	now := time.Now()
	images := []im.Attachment{}
	for i := 0; i < 5; i++ {
		images = append(images, im.Attachment{Kind: "image", Key: "img", Name: "image.png", SourceMessageID: "photo", SourceIndex: i})
	}
	db.Claim("photo")
	first, e := db.StageImages(a, "alice", "photo", images, now)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.StageImages(a, "alice", "duplicate", images[:1], now); e == nil {
		t.Fatal("limit bypassed")
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	other := a
	other.TopicID = "topic2"
	if _, ok, e := db.ConsumeImages(other, "alice", "wrongtopic", 0, now); e != nil || ok {
		t.Fatal("topic isolation broken")
	}
	got, ok, e := db.ConsumeImages(a, "alice", "caption", 0, now)
	if e != nil || !ok || got.ID != first.ID || len(got.Images) != 5 {
		t.Fatal("pending state lost on restart")
	}
	if _, ok, e = db.ConsumeImages(a, "alice", "caption2", 0, now); e != nil || ok {
		t.Fatal("duplicate consumption")
	}
}

func TestSubmittedImageDraftNotRestoredToPending(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	a := im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}
	now := time.Now()
	photo := im.Attachment{Kind: "image", Key: "img", Name: "image.png", SourceMessageID: "photo"}
	db.Claim("photo")
	if _, e = db.StageImages(a, "alice", "photo", []im.Attachment{photo}, now); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := db.ConsumeImages(a, "alice", "caption", 0, now); e != nil || !ok {
		t.Fatal("initial submit", e)
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, ok, e := db.ConsumeImages(a, "alice", "after-restart", 0, now); e != nil || ok {
		t.Fatal("restarted draft was replayed")
	}
}
