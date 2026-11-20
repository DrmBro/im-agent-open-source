package store

import (
	"context"
	"testing"

	"im-agent/internal/im"
)

func TestAssistantTriageSplitsQueuesAndRecordsFeedback(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const day, start = "2026-11-19", int64(1_000_000)
	msgs := []im.PulledMessage{
		{ConversationID: "cidG", MessageID: "m1", SenderID: "a", SenderName: "甲", Text: "报表数据不对，帮看下", CreatedMs: start + 10},
		{ConversationID: "cidG", MessageID: "m2", SenderID: "b", SenderName: "乙", Text: "下午会议改几点？", CreatedMs: start + 20},
		{ConversationID: "cidG", MessageID: "m3", SenderID: "c", SenderName: "丙", Text: "哈哈", CreatedMs: start + 30},
	}
	if n, err := db.PutAssistantMessages("group", "", "项目群", msgs); err != nil || n != 3 {
		t.Fatalf("put=%d err=%v", n, err)
	}
	if n, _ := db.PutAssistantMessages("group", "", "项目群", msgs); n != 0 {
		t.Fatal("重复拉取不能重复入库")
	}
	convs, err := db.PendingAssistantConversations(start)
	if err != nil || len(convs) != 1 {
		t.Fatalf("convs=%v err=%v", convs, err)
	}
	report := AssistantItem{ID: "i1", Day: day, Conversation: "cidG", Kind: "group", ChatName: "项目群", Title: "报表数据异常", Category: "need_me", Complex: true, State: "open"}
	meeting := AssistantItem{ID: "i2", Day: day, Conversation: "cidG", Kind: "group", ChatName: "项目群", Title: "会议时间", Category: "auto_reply", Reply: "改到三点", State: "open"}
	err = db.ApplyAssistantTriage([]AssistantAssignment{{Item: report, New: true, Messages: []string{"m1"}}, {Item: meeting, New: true, Messages: []string{"m2"}}}, nil, []string{"m3"})
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.PendingAssistantMessages("cidG", start, 10); len(pending) != 0 {
		t.Fatalf("all messages should be assigned or ignored, left %d", len(pending))
	}
	need, _ := db.AssistantItemsNeedingSession(day)
	if len(need) != 1 || need[0].ID != "i1" {
		t.Fatalf("complex need_me item should need a session: %+v", need)
	}
	if err = db.LinkAssistantSession("i1", "thread-1", ""); err != nil {
		t.Fatal(err)
	}
	// 已派发会话的事项：新消息进队列，不提交给会话。
	follow := []im.PulledMessage{{ConversationID: "cidG", MessageID: "m4", SenderID: "a", SenderName: "甲", Text: "补充：是昨天那张", CreatedMs: start + 40}}
	if _, err = db.PutAssistantMessages("group", "", "项目群", follow); err != nil {
		t.Fatal(err)
	}
	open, _ := db.OpenAssistantItems("cidG", day)
	var inSession AssistantItem
	for _, x := range open {
		if x.ID == "i1" {
			inSession = x
		}
	}
	if inSession.ThreadID != "thread-1" {
		t.Fatalf("session item missing thread: %+v", inSession)
	}
	if err = db.ApplyAssistantTriage([]AssistantAssignment{{Item: inSession, Messages: []string{"m4"}, Queue: true}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	q, err := db.AssistantQueue(context.Background(), "thread-1", false)
	if err != nil || len(q) != 1 || q[0].Text != "补充：是昨天那张" || q[0].Title != "报表数据异常" {
		t.Fatalf("queue=%+v err=%v", q, err)
	}
	if q, _ = db.AssistantQueue(context.Background(), "thread-1", true); len(q) != 1 {
		t.Fatal("consume should return the batch once")
	}
	if q, _ = db.AssistantQueue(context.Background(), "thread-1", false); len(q) != 0 {
		t.Fatal("consumed messages must not be returned again")
	}
	// 一条消息可同时属于两个事项。
	both := []im.PulledMessage{{ConversationID: "cidG", MessageID: "m5", SenderID: "b", SenderName: "乙", Text: "会议定三点，报表也一起说", CreatedMs: start + 50}}
	_, _ = db.PutAssistantMessages("group", "", "项目群", both)
	meeting.Summary = "会议三点"
	if err = db.ApplyAssistantTriage([]AssistantAssignment{{Item: meeting, Messages: []string{"m5"}}, {Item: inSession, Messages: []string{"m5"}, Queue: true}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	x, _ := db.AssistantItem("i2")
	if len(x.Messages) != 2 {
		t.Fatalf("meeting item should hold m2 and m5, got %d", len(x.Messages))
	}
	// 已发送的回复不能重复领取。
	if _, err = db.ClaimAssistantReply("i2", "三点见", false); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimAssistantReply("i2", "三点见", false); err == nil {
		t.Fatal("reply in flight must not be claimed twice")
	}
	_ = db.FinishAssistantReply("i2", "sent", "", false)
	if x, _ = db.AssistantItem("i2"); x.State != "done" || x.ReplyState != "sent" {
		t.Fatalf("sent reply should close the item: %+v", x)
	}
	if err = db.RecategorizeAssistantItem("i1", "no_reply"); err != nil {
		t.Fatal(err)
	}
	fb, _ := db.AssistantFeedback(5)
	if len(fb) != 1 || fb[0].From != "need_me" || fb[0].To != "no_reply" {
		t.Fatalf("feedback=%+v", fb)
	}
}

func TestAssistantCursorStartsAtToday(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.AssistantCursor("cid", 5000); got != 5000 {
		t.Fatalf("new cursor should start at day start, got %d", got)
	}
	_ = db.SaveAssistantCursor("cid", "group", "", "群", 4000, "")
	if got := db.AssistantCursor("cid", 5000); got != 5000 {
		t.Fatalf("yesterday's cursor must not read before today, got %d", got)
	}
	_ = db.SaveAssistantCursor("cid", "group", "", "", 6000, "")
	_ = db.SaveAssistantCursor("cid", "group", "", "", 5500, "")
	if got := db.AssistantCursor("cid", 5000); got != 6000 {
		t.Fatalf("cursor must not move backwards, got %d", got)
	}
}

func TestAssistantSettingsValidation(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, _ := db.AssistantSettings()
	if s.IntervalSeconds != 120 || !s.Mentions || !s.AutoSession || s.AutoSend {
		t.Fatalf("defaults=%+v", s)
	}
	s.IntervalSeconds = 5
	if db.SaveAssistantSettings(s) == nil {
		t.Fatal("too-short interval must be rejected")
	}
	s.IntervalSeconds = 60
	s.Model = "deepseek-v4-flash"
	s.People = []AssistantTarget{{ID: "u1", Name: "甲"}, {ID: "u1", Name: "甲"}}
	if db.SaveAssistantSettings(s) == nil {
		t.Fatal("duplicate targets must be rejected")
	}
	s.People = s.People[:1]
	if err = db.SaveAssistantSettings(s); err != nil {
		t.Fatal(err)
	}
	got, _ := db.AssistantSettings()
	if got.Model != "deepseek-v4-flash" || len(got.People) != 1 {
		t.Fatalf("saved=%+v", got)
	}
}
