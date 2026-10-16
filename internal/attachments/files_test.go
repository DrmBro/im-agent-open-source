package attachments

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInboxAndSizeLimit(t *testing.T) {
	dir := t.TempDir()
	if _, e := Save(dir, "one", "../../secret", bytes.NewBufferString("x"), 10); e == nil {
		t.Fatal("unsafe name accepted")
	}
	if _, e := Save(dir, "two", "a.txt", bytes.NewBufferString("too large"), 2); e == nil {
		t.Fatal("oversized input accepted")
	}
	p, e := Save(dir, "three", "note.txt", bytes.NewBufferString("hello"), 10)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "hello" {
		t.Fatal("corrupted")
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatal("public attachment")
	}
}
func TestOutboundPathProtection(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600)
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt"))
	for _, p := range []string{"../secret.txt", "escape.txt", filepath.Join(outside, "secret.txt")} {
		if f, _, e := OpenExport(root, p, 100); e == nil {
			f.Close()
			t.Fatalf("accepted %s", p)
		}
	}
	os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0600)
	f, _, e := OpenExport(root, "ok.txt", 100)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
}

func TestInboxDuplicateDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	p, e := Save(dir, "key", "data.txt", bytes.NewBufferString("first"), 100)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Save(dir, "key", "data.txt", bytes.NewBufferString("second"), 100); e == nil {
		t.Fatal("duplicate overwrote attachment")
	}
	data, _ := os.ReadFile(p)
	if string(data) != "first" {
		t.Fatal("original changed")
	}
}
