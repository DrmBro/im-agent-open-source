package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

func TestAssistantTriageConversationSplitsAndQueues(t *testing.T) {
	db, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := &Bridge{store: db}
	day, start := "2026-11-19", int64(1_000_000)
	_, _ = db.PutAssistantMessages("group", "", "运营群", []im.PulledMessage{
		{ConversationID: "cidG", MessageID: "m1", SenderID: "a", SenderName: "甲", Text: "昨天 GMV 怎么掉了一半？@我 看下", CreatedMs: start + 1},
		{ConversationID: "cidG", MessageID: "m2", SenderID: "b", SenderName: "乙", Text: "周会还是四点吗", CreatedMs: start + 2},
		{ConversationID: "cidG", MessageID: "m3", SenderID: "c", SenderName: "丙", Text: "👍", CreatedMs: start + 3},
	})
	var system, input string
	call := func(_ context.Context, s, u string) (string, error) {
		system, input = s, u
		return "```json\n" + `{"items":[
{"existing":0,"title":"GMV 下跌排查","category":"need_me","complex":true,"reason":"需要查数据","summary":"甲问昨天 GMV 下跌","messages":[1]},
{"existing":0,"title":"周会时间","category":"auto_reply","complex":false,"reason":"简单确认","summary":"乙确认周会时间","reply":"是的，还是四点。","messages":[2]},
{"existing":0,"title":"坏编号","category":"need_me","messages":[99]}
],"resolved":[]}` + "\n```", nil
	}
	if err = b.assistantTriageConversation(context.Background(), call, "SYS", "cidG", day, start); err != nil {
		t.Fatal(err)
	}
	if system != "SYS" || !strings.Contains(input, "GMV") || !strings.Contains(input, `"chat":"运营群"`) {
		t.Fatalf("model input=%s", input)
	}
	items, _ := db.AssistantItems(day)
	if len(items) != 2 {
		t.Fatalf("expected 2 items (invalid refs dropped), got %d", len(items))
	}
	var gmv, meeting store.AssistantItem
	for _, x := range items {
		switch x.Title {
		case "GMV 下跌排查":
			gmv = x
		case "周会时间":
			meeting = x
		}
	}
	if !gmv.Complex || gmv.Category != "need_me" || meeting.Reply != "是的，还是四点。" || meeting.Category != "auto_reply" {
		t.Fatalf("gmv=%+v meeting=%+v", gmv, meeting)
	}
	if n := db.PendingAssistantCount(start); n != 0 {
		t.Fatalf("emoji message should be ignored, pending=%d", n)
	}
	_ = db.LinkAssistantSession(gmv.ID, "thread-gmv", "")

	// 第二轮：延续已派发会话的事项 → 只进队列；auto_reply 缺回复时降级为 need_me。
	_, _ = db.PutAssistantMessages("group", "", "运营群", []im.PulledMessage{
		{ConversationID: "cidG", MessageID: "m4", SenderID: "a", SenderName: "甲", Text: "补充：只看抖音渠道", CreatedMs: start + 4},
		{ConversationID: "cidG", MessageID: "m5", SenderID: "d", SenderName: "丁", Text: "合同什么时候签", CreatedMs: start + 5},
	})
	call = func(_ context.Context, _, u string) (string, error) {
		input = u
		var in struct {
			Existing []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
			} `json:"existing"`
		}
		_ = json.Unmarshal([]byte(u), &in)
		ids := map[string]int{}
		for _, x := range in.Existing {
			ids[x.Title] = x.ID
		}
		return fmt.Sprintf(`{"items":[{"existing":%d,"title":"GMV 下跌排查","category":"need_me","complex":true,"summary":"限定抖音渠道","messages":[1]},{"existing":0,"title":"合同签署","category":"auto_reply","reply":"","messages":[2]}],"resolved":[%d]}`, ids["GMV 下跌排查"], ids["周会时间"]), nil
	}
	if err = b.assistantTriageConversation(context.Background(), call, "SYS", "cidG", day, start); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(input, `"in_session":true`) {
		t.Fatalf("existing items should tell the model which are in a session: %s", input)
	}
	q, _ := db.AssistantQueue(context.Background(), "thread-gmv", false)
	if len(q) != 1 || q[0].MessageID != "m4" {
		t.Fatalf("follow-up should be queued for the session, got %+v", q)
	}
	items, _ = db.AssistantItems(day)
	var contract store.AssistantItem
	for _, x := range items {
		if x.Title == "合同签署" {
			contract = x
		}
		if x.ID == meeting.ID && x.State != "done" {
			t.Fatalf("resolved existing item should be done: %+v", x)
		}
	}
	if contract.Category != "need_me" || contract.Reply != "" {
		t.Fatalf("auto_reply without a reply must fall back to need_me: %+v", contract)
	}
}

func TestAssistantTriageDefersOnBadModelOutput(t *testing.T) {
	db, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := &Bridge{store: db}
	_, _ = db.PutAssistantMessages("direct", "u1", "张三", []im.PulledMessage{{ConversationID: "cidD", MessageID: "m1", SenderID: "u1", SenderName: "张三", Text: "在吗", CreatedMs: 10}})
	call := func(context.Context, string, string) (string, error) { return "抱歉我无法回答", nil }
	if err = b.assistantTriageConversation(context.Background(), call, "", "cidD", "d", 0); err == nil {
		t.Fatal("non-JSON output must be an error")
	}
	if convs, _ := db.PendingAssistantConversations(0); len(convs) != 0 {
		t.Fatal("failed messages should back off before retry")
	}
	if n := db.PendingAssistantCount(0); n != 1 {
		t.Fatal("failed messages must stay pending, not be dropped")
	}
}

func TestAssistantSessionPromptMentionsQueue(t *testing.T) {
	p := assistantSessionPrompt(store.AssistantItem{ID: "i1", Kind: "group", ChatName: "群", Title: "事项", Messages: []store.AssistantMessage{{PulledMessage: im.PulledMessage{SenderName: "甲", Text: "问题"}}}})
	for _, want := range []string{"【消息助理派发】", "pending_messages.py", "事项 ID：i1", "甲：问题"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}
