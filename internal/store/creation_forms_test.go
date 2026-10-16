package store

import (
	"im-agent/internal/im"
	"testing"
	"time"
)

func TestCreationFormSingleUseAndRestart(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	f := CreationForm{ID: "new:1", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}, UserID: "owner", Catalog: "{}", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	if e = db.PutCreationForm(f); e != nil {
		t.Fatal(e)
	}
	if ok, _ := db.ClaimCreation(f.ID, "{}"); ok {
		t.Fatal("unacknowledged card submitted")
	}
	db.CreationMessage(f.ID, "card")
	if ok, e := db.ClaimCreation(f.ID, "{}"); !ok || e != nil {
		t.Fatal("first submit rejected", e)
	}
	if ok, _ := db.ClaimCreation(f.ID, "{}"); ok {
		t.Fatal("double submit accepted")
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	saved, e := db.CreationForm(f.ID)
	if e != nil || saved.State != "interrupted" {
		t.Fatal("crash should not replay creation", saved, e)
	}
	if ok, _ := db.ClaimCreation(f.ID, "{}"); ok {
		t.Fatal("interrupted form replayed")
	}
}
