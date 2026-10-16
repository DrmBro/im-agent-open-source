package store

import (
	"im-agent/internal/im"
	"testing"
)

func TestDeliveryRecoveryNeverReexecutes(t *testing.T) {
	db, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := Session{ThreadID: "thread", Address: im.Address{Channel: "test", Connection: "test", ChatID: "chat"}, WorkDir: t.TempDir(), Owner: "user", Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	if e = db.EnqueueMirrorReply(s.ThreadID, MirrorEntry{ItemID: "item", Role: "assistant", Parts: []string{"answer"}}); e != nil {
		t.Fatal(e)
	}
	parts, e := db.MirrorParts(s.ThreadID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.ClaimMirrorPart(parts[0]); e != nil {
		t.Fatal(e)
	}
	if e = db.MirrorPartFailed(parts[0], "unknown", true); e != nil {
		t.Fatal(e)
	}
	r := DeliveryResolution{ThreadID: s.ThreadID, ItemID: "item", Part: 0, Action: "retry"}
	if e = db.ResolveDelivery(r); e == nil {
		t.Fatal("unknown auto-retry must fail")
	}
	r.Action = "confirmed_missing"
	r.Confirmed = true
	if e = db.ResolveDelivery(r); e != nil {
		t.Fatal(e)
	}
	var n int
	db.db.QueryRow("SELECT count(*) FROM events").Scan(&n)
	if n != 0 {
		t.Fatal("recovery created model task")
	}
	r.Action = "confirmed_sent"
	r.Confirmed = true
	if e = db.ResolveDelivery(r); e == nil {
		t.Fatal("pending is not unknown")
	}
}

func TestTaskCenterAndTransferPersistence(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	s := Session{ThreadID: "thread", Address: im.Address{Channel: "test", Connection: "test", ChatID: "chat"}, WorkDir: t.TempDir(), Owner: "user", Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	db.Claim("event")
	db.EventStage("event", s.Address.Key(), "waiting_model", "")
	tr := Transfer{ID: "file", ThreadID: "thread", Direction: "out", Kind: "file", Name: "report.txt"}
	if ok, e := db.BeginTransfer(tr); !ok || e != nil {
		t.Fatal(e)
	}
	if ok, _ := db.BeginTransfer(tr); ok {
		t.Fatal("duplicate upload")
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	view, e := TaskCenterSnapshot(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(view.Tasks) != 1 || view.Tasks[0].Status != "interrupted" {
		t.Fatal("in-flight task was replayed or lost")
	}
	if len(view.Attachments) != 1 || view.Attachments[0].State != "unknown" {
		t.Fatal("interrupted transfer not uncertain")
	}
}

func TestConfirmedReceiptDoesNotCreateNewDelivery(t *testing.T) {
	db, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := Session{ThreadID: "thread", Address: im.Address{Channel: "test", Connection: "test", ChatID: "chat"}, WorkDir: t.TempDir(), Owner: "user", Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	reply := MirrorEntry{ItemID: "item", Role: "assistant", Parts: []string{"answer"}}
	db.EnqueueMirrorReply(s.ThreadID, reply)
	parts, _ := db.MirrorParts(s.ThreadID)
	db.ClaimMirrorPart(parts[0])
	db.MirrorPartFailed(parts[0], "unknown", true)
	if e = db.ResolveDelivery(DeliveryResolution{ThreadID: s.ThreadID, ItemID: "item", Part: 0, Action: "confirmed_sent", Confirmed: true}); e != nil {
		t.Fatal(e)
	}
	if e = db.EnqueueMirrorReply(s.ThreadID, reply); e != nil {
		t.Fatal(e)
	}
	parts, e = db.MirrorParts(s.ThreadID)
	if e != nil || len(parts) != 0 {
		t.Fatal("confirmed part requeued")
	}
	sent, e := db.MirrorReplySent(s.ThreadID, "item")
	if e != nil || !sent {
		t.Fatal("receipt not retained")
	}
}

func TestBaselineStartupPreservesLaterFeatureTables(t *testing.T) {
	dir := t.TempDir()
	s, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.db.Exec(`CREATE TABLE mobile_tasks(id TEXT PRIMARY KEY,status TEXT,request_json TEXT); INSERT INTO mobile_tasks VALUES ('retained','failed','private request'); CREATE TABLE session_health(thread_id TEXT PRIMARY KEY,error TEXT); INSERT INTO session_health VALUES ('thread','old diagnostic');`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var status, request, hint string
	if e = s.db.QueryRow("SELECT status,request_json FROM mobile_tasks WHERE id='retained'").Scan(&status, &request); e != nil || status != "failed" || request != "private request" {
		t.Fatal("rollback changed later task history", e)
	}
	if e = s.db.QueryRow("SELECT error FROM session_health WHERE thread_id='thread'").Scan(&hint); e != nil || hint != "old diagnostic" {
		t.Fatal("rollback changed diagnostic history", e)
	}
}

func TestRenamingExistingManagedSessionPreservesHistory(t *testing.T) {
	db, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	session := Session{ThreadID: "name-thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "user", WorkDir: t.TempDir(), Managed: true, GroupName: "Codex - Old - 12345678", Fresh: true}
	if e = db.Save(session); e != nil {
		t.Fatal(e)
	}
	before, e := db.Presentation(session.ThreadID)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.SetPresentationName(session.ThreadID, "New title · Codex · 12345678"); e != nil {
		t.Fatal(e)
	}
	after, e := db.Presentation(session.ThreadID)
	if e != nil || after.NameApplied || after.HistoryState != before.HistoryState || after.Payload != before.Payload || after.MessageID != before.MessageID {
		t.Fatal("rename changed history or ignored previous name", after, e)
	}
	if e = db.PresentationRenamed(session.ThreadID); e != nil {
		t.Fatal(e)
	}
	if e = db.SetPresentationName(session.ThreadID, "New title · Codex · 12345678"); e != nil {
		t.Fatal(e)
	}
	stable, _ := db.Presentation(session.ThreadID)
	if !stable.NameApplied {
		t.Fatal("same-name polling resets applied state")
	}
	if e = db.SetPresentationName(session.ThreadID, "Second title · Codex · 12345678"); e != nil {
		t.Fatal(e)
	}
	changed, _ := db.Presentation(session.ThreadID)
	if changed.NameApplied {
		t.Fatal("actual title change not queued")
	}
	if _, e = db.db.Exec("UPDATE sessions SET status='archived' WHERE thread_id=?", session.ThreadID); e != nil {
		t.Fatal(e)
	}
	if e = db.SetPresentationName(session.ThreadID, "should not change archived"); e != nil {
		t.Fatal(e)
	}
	frozen, _ := db.Presentation(session.ThreadID)
	if frozen.Name != changed.Name {
		t.Fatal("renamed an archived group")
	}
	binding, _ := db.ByThread(session.ThreadID)
	if binding.Address.ChatID != session.Address.ChatID || binding.WorkDir != session.WorkDir {
		t.Fatal("rename changed binding")
	}
}

func TestReadonlySessionStatusIgnoresCommandsAndOtherGroups(t *testing.T) {
	db, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	session := Session{ThreadID: "status-thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "alice", Managed: true, WorkDir: t.TempDir(), GroupName: "会话标题 · Codex · 12345678", Fresh: true}
	if e = db.Save(session); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"running", "queued", "command", "another-group"} {
		if _, e = db.Claim(id); e != nil {
			t.Fatal(e)
		}
	}
	_ = db.EventStage("running", session.Address.Key(), "waiting_model", "")
	_ = db.EventStage("queued", session.Address.Key(), "queued", "")
	_ = db.EventStage("command", session.Address.Key(), "command_completed", "")
	_ = db.EventDone("command", "done")
	_ = db.EventStage("another-group", im.Address{Channel: "fake", Connection: "one", ChatID: "other"}.Key(), "waiting_model", "")
	view, e := db.ReadSessionStatus(session.ThreadID)
	if e != nil {
		t.Fatal(e)
	}
	if view.Task.ID != "running" || view.Queued != 1 || view.Name != session.GroupName {
		t.Fatal("status selected wrong request", view)
	}
	_ = db.EventDone("running", "done")
	view, e = db.ReadSessionStatus(session.ThreadID)
	if e != nil || view.Task.ID != "queued" {
		t.Fatal("queued request hidden", view, e)
	}
	_ = db.EventStage("queued", session.Address.Key(), "outcome_unconfirmed", "result not confirmed")
	_ = db.EventDone("queued", "uncertain")
	view, e = db.ReadSessionStatus(session.ThreadID)
	if e != nil || view.Task.Status != "uncertain" || view.Queued != 0 {
		t.Fatal("uncertain misclassified", view, e)
	}
}
func TestReadonlyStatusCardReceiptSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	s := Session{ThreadID: "status-thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "alice", Managed: true, WorkDir: t.TempDir()}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	if ok, e := db.ReserveStatusCard(s); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if ok, e := db.ReserveStatusCard(s); e != nil || ok {
		t.Fatal("duplicate reservation", ok, e)
	}
	if e = db.StatusCardSent(s.ThreadID, "card-id", "digest", "event"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	card, e := db.StatusCard(s.ThreadID)
	if e != nil || card.MessageID != "card-id" || card.State != "sent" {
		t.Fatal("receipt lost", card, e)
	}
	var n int
	_ = db.db.QueryRow("SELECT count(*) FROM events").Scan(&n)
	if n != 0 {
		t.Fatal("display created model events")
	}
}

func TestReadonlyStatusUnacknowledgedCardNotRecreatedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	db, e := New(dir)
	if e != nil {
		t.Fatal(e)
	}
	session := Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "alice", Managed: true, WorkDir: t.TempDir()}
	if e = db.Save(session); e != nil {
		t.Fatal(e)
	}
	if ok, e := db.ReserveStatusCard(session); e != nil || !ok {
		t.Fatal(ok, e)
	}
	db.Close()
	db, e = New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	card, e := db.StatusCard(session.ThreadID)
	if e != nil || card.State != "unknown" {
		t.Fatal(card, e)
	}
	if ok, e := db.ReserveStatusCard(session); e != nil || ok {
		t.Fatal("restart replays card creation", ok, e)
	}
}
