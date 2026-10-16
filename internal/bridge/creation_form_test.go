package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"im-agent/internal/store"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type formAdapter struct {
	fixtureFiles
	offered im.Card
	created int
}

func (f *formAdapter) Capabilities() im.Capabilities {
	return im.Capabilities{InteractiveCards: true, CreateConversation: true}
}
func (f *formAdapter) SendCard(_ context.Context, _ im.Address, c im.Card, _ string) (string, error) {
	f.offered = c
	return "form-card", nil
}
func (f *formAdapter) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	f.created++
	return im.Address{Channel: "fake", Connection: "one", ChatID: "new-group"}, nil
}
func TestCreationFormOwnerAndMessageBinding(t *testing.T) {
	f := store.CreationForm{State: "pending", ExpiresAt: time.Now().Add(time.Minute).Unix(), UserID: "owner", MessageID: "card", Address: im.Address{Connection: "one", Channel: "fake", ChatID: "group"}}
	good := im.Interaction{UserID: "owner", MessageID: "card", Address: f.Address}
	if e := validateCreationInteraction(f, good); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []im.Interaction{{UserID: "other", MessageID: "card", Address: f.Address}, {UserID: "owner", MessageID: "forwarded", Address: f.Address}, {UserID: "owner", MessageID: "card", Address: im.Address{Connection: "one", Channel: "fake", ChatID: "other"}}} {
		if e := validateCreationInteraction(f, bad); e == nil {
			t.Fatal("forged form accepted")
		}
	}
}
func TestCreationFormCreatesOneGroupAndExecutesFirstQuestionOnce(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	expectedWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "turns.jsonl")
	calls := filepath.Join(dir, "calls.jsonl")
	t.Setenv("IM_AGENT_TEST_CODEX_STUB", "1")
	t.Setenv("IM_AGENT_TEST_WORKDIR", work)
	t.Setenv("IM_AGENT_TEST_CAPTURE", capture)
	t.Setenv("IM_AGENT_TEST_CAPTURE_CALLS", calls)
	cfg := config.Defaults()
	cfg.StateDir = dir
	cfg.Codex.Home = filepath.Join(dir, "codex-home")
	cfg.Codex.Binary = os.Args[0]
	cfg.Codex.SubmissionMode = "direct"
	cfg.Connections = []config.Connection{{ID: "one", AllowedUsers: []string{"owner"}, AdminUsers: []string{"owner"}, AutoCreateChat: true, ConversationMode: "group"}}
	db, e := store.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	cx, e := codex.Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cx.Close()
	adapter := &formAdapter{}
	b := New(cfg, db, cx, map[string]im.Adapter{"one": adapter}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	qctx, qstop := context.WithCancel(ctx)
	b.jobs = queue.New(qctx, 2, 10)
	defer func() { qstop(); b.jobs.Wait() }()
	origin := im.Message{ID: "new-command", UserID: "owner", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "origin"}, Text: "/new"}
	if e = b.receiveMessage(ctx, origin); e != nil {
		t.Fatal(e)
	}
	if adapter.created != 0 || adapter.offered.Form == nil {
		t.Fatal("new command created resources before form confirmation")
	}
	in := im.Interaction{Address: origin.Address, UserID: origin.UserID, ActionID: adapter.offered.ActionID, MessageID: "form-card", Choice: "create", Fields: map[string]string{"project": "p1:0", "model": "test-model", "effort": "high", "prompt": "一次性提问"}}
	if e = b.receiveInteraction(ctx, in); e != nil {
		t.Fatal(e)
	}
	if e = b.receiveInteraction(ctx, in); e == nil {
		t.Fatal("duplicate submit accepted")
	}
	for {
		f, e := db.CreationForm(in.ActionID)
		if e != nil {
			t.Fatal(e)
		}
		if f.State == "failed" || f.State == "uncertain" {
			t.Fatal(f.Stage, f.Error)
		}
		if f.State == "done" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("creation did not finish")
		case <-time.After(20 * time.Millisecond):
		}
	}
	qstop()
	b.jobs.Wait()
	if adapter.created != 1 {
		t.Fatal("wrong group count", adapter.created)
	}
	data, e := os.ReadFile(capture)
	if e != nil {
		t.Fatal(e)
	}
	if len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 1 || !strings.Contains(string(data), "一次性提问") {
		t.Fatal("first question missing or duplicated")
	}
	data, _ = os.ReadFile(calls)
	verified := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r struct {
			Method string
			Params map[string]any
		}
		json.Unmarshal([]byte(line), &r)
		if r.Method == "thread/start" {
			config, _ := r.Params["config"].(map[string]any)
			verified = r.Params["cwd"] == expectedWork && r.Params["projectId"] == "p1" && r.Params["model"] == "test-model" && config["model_reasoning_effort"] == "high"
		}
	}
	if !verified {
		t.Fatalf("chosen project/model/effort were not applied: work=%q calls=%s", work, data)
	}
	s, e := db.ByThread("created-thread")
	if e != nil || s.Address.ChatID != "new-group" || s.WorkDir != expectedWork {
		t.Fatal("new session binding incorrect", e)
	}
}

func TestConversationNameStartsWithSessionTitle(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	name := conversationName("Codex", "检查最近的提交", id)
	if name != "检查最近的提交 · Codex · 00000001" {
		t.Fatal("session title must come first", name)
	}
	long := conversationName(strings.Repeat("标识", 20), strings.Repeat("长标题🙂", 40), id)
	if len([]rune(long)) > 55 || !strings.HasPrefix(long, "长标题") || !strings.HasSuffix(long, "00000001") {
		t.Fatal("length or ID retention failed", long)
	}
	blank := conversationName("", "\n\t", id)
	if !strings.HasPrefix(blank, "未命名会话 · Codex") {
		t.Fatal("empty title handling failed", blank)
	}
	clean := conversationName("Codex", "标题\n第二行", id)
	if !strings.HasPrefix(clean, "标题 第二行 ·") {
		t.Fatal("title cleaning failed", clean)
	}
}

// 隔离渠道：只记录关闭请求，不调用真实飞书或操作真实群。
type lifecycleFixture struct {
	fixtureFiles
	state              im.ConversationState
	stateErr, closeErr error
	closed             []im.Address
}

func (f *lifecycleFixture) Capabilities() im.Capabilities {
	return im.Capabilities{CloseConversation: true}
}
func (f *lifecycleFixture) SetClosedHandler(func(context.Context, im.ConversationClosed) error) {}
func (f *lifecycleFixture) ConversationState(context.Context, im.Address) (im.ConversationState, error) {
	return f.state, f.stateErr
}
func (f *lifecycleFixture) CloseConversation(_ context.Context, a im.Address) error {
	f.closed = append(f.closed, a)
	return f.closeErr
}
func TestArchivedSessionClosesOnlyManagedIndependentGroup(t *testing.T) {
	for _, tc := range []struct {
		name            string
		managed, shared bool
		platform        im.ConversationState
		wantCalls       int
		wantState       string
		wantError       bool
	}{
		{"managed", true, false, im.ConversationOpen, 1, "done", false},
		{"already-dissolved", true, false, im.ConversationGone, 0, "done", false},
		{"manual-binding", false, false, im.ConversationOpen, 0, "protected", false},
		{"shared-protected", true, true, im.ConversationOpen, 0, "pending", true},
		{"unknown-not-deleted", true, false, im.ConversationUnknown, 0, "pending", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			session := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "owner", WorkDir: t.TempDir(), Managed: tc.managed}
			if e = db.Save(session); e != nil {
				t.Fatal(e)
			}
			cfg := config.Defaults()
			cfg.Connections = []config.Connection{{ID: "one"}}
			if tc.shared {
				cfg.Connections[0].SharedChatID = "group"
			}
			a := &lifecycleFixture{state: tc.platform}
			b := New(cfg, db, nil, map[string]im.Adapter{"one": a}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if e = b.requestClose(session.ThreadID, "codex", true, false); e != nil {
				t.Fatal(e)
			}
			e = b.closeOne(context.Background(), session.ThreadID)
			if (e != nil) != tc.wantError {
				t.Fatal("unexpected closure result", e)
			}
			state, e := db.Closure(session.ThreadID)
			if e != nil || state.State != tc.wantState || len(a.closed) != tc.wantCalls {
				t.Fatal("incorrect closure effects", state, a.closed, e)
			}
			if tc.wantState == "done" {
				if e = b.requestClose(session.ThreadID, "codex", true, false); e != nil {
					t.Fatal(e)
				}
				if e = b.closeOne(context.Background(), session.ThreadID); e != nil {
					t.Fatal(e)
				}
				if len(a.closed) != tc.wantCalls {
					t.Fatal("duplicate archive deleted twice")
				}
			}
		})
	}
}
func TestArchivePermissionFailureRetriedWithoutFalseSuccess(t *testing.T) {
	b, db, _, s := imageFixture(t)
	a := &lifecycleFixture{state: im.ConversationOpen, closeErr: errors.New("permission denied")}
	b.channels[s.Address.Connection] = a
	if e := b.requestClose(s.ThreadID, "codex", true, false); e != nil {
		t.Fatal(e)
	}
	e := b.closeOne(context.Background(), s.ThreadID)
	if e == nil {
		t.Fatal("permission failure ignored")
	}
	if e = db.ClosureFailed(s.ThreadID, e.Error()); e != nil {
		t.Fatal(e)
	}
	failed, _ := db.Closure(s.ThreadID)
	if failed.ChatClosed || failed.State != "pending" || failed.Attempts != 1 {
		t.Fatal("failed deletion marked complete", failed)
	}
	a.closeErr = nil
	if e = db.RetryClosure(s.ThreadID); e != nil {
		t.Fatal(e)
	}
	if e = b.closeOne(context.Background(), s.ThreadID); e != nil {
		t.Fatal(e)
	}
	done, _ := db.Closure(s.ThreadID)
	if done.State != "done" || !done.ChatClosed || len(a.closed) != 2 {
		t.Fatal("retry did not finish", done, a.closed)
	}
}
func TestOfflineArchiveIsReconciledWithoutReexecutingThread(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	calls := filepath.Join(dir, "calls.jsonl")
	t.Setenv("IM_AGENT_TEST_CODEX_STUB", "1")
	t.Setenv("IM_AGENT_TEST_WORKDIR", work)
	t.Setenv("IM_AGENT_TEST_CAPTURE_CALLS", calls)
	t.Setenv("IM_AGENT_TEST_ARCHIVED_THREAD", "thread")
	cfg := config.Defaults()
	cfg.StateDir = dir
	cfg.Codex.Home = filepath.Join(dir, "codex-home")
	cfg.Codex.Binary = os.Args[0]
	cfg.Codex.SubmissionMode = "direct"
	cfg.Connections = []config.Connection{{ID: "one"}}
	db, e := store.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "owner", WorkDir: work, Managed: true}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cx, e := codex.Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cx.Close()
	a := &lifecycleFixture{state: im.ConversationOpen}
	b := New(cfg, db, cx, map[string]im.Adapter{"one": a}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	index := 0
	b.reconcileClosureFacts(ctx, &index)
	job, e := db.Closure(s.ThreadID)
	if e != nil || !job.CodexArchived {
		t.Fatal("offline archive missed", job, e)
	}
	if e = b.closeOne(ctx, s.ThreadID); e != nil {
		t.Fatal(e)
	}
	if len(a.closed) != 1 {
		t.Fatal("bound group not closed")
	}
	raw, e := os.ReadFile(calls)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), `"turn/start"`) || strings.Contains(string(raw), `"thread/archive"`) {
		t.Fatal("reconciliation reran or redundantly archived thread")
	}
}

type renameFixture struct {
	fixtureFiles
	names   []string
	created int
}

func (f *renameFixture) Capabilities() im.Capabilities {
	return im.Capabilities{RenameConversation: true}
}
func (f *renameFixture) RenameConversation(_ context.Context, _ im.Address, name string) error {
	f.names = append(f.names, name)
	return nil
}
func (f *renameFixture) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	f.created++
	return im.Address{}, nil
}
func TestExistingGroupGetsTitleFirstWithoutRecreation(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	t.Setenv("IM_AGENT_TEST_CODEX_STUB", "1")
	t.Setenv("IM_AGENT_TEST_WORKDIR", work)
	t.Setenv("IM_AGENT_TEST_THREAD_NAME", "原会话的新标题")
	cfg := config.Defaults()
	cfg.StateDir = dir
	cfg.Codex.Home = filepath.Join(dir, "codex-home")
	cfg.Codex.Binary = os.Args[0]
	cfg.Codex.SubmissionMode = "direct"
	cfg.Connections = []config.Connection{{ID: "one"}}
	db, e := store.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "group"}, Owner: "owner", WorkDir: work, Managed: true, Fresh: true, GroupName: "Codex - 原名称 - 12345678"}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cx, e := codex.Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cx.Close()
	a := &renameFixture{}
	b := New(cfg, db, cx, map[string]im.Adapter{"one": a}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, e := db.Presentation(s.ThreadID)
	if e != nil {
		t.Fatal(e)
	}
	beforeHistory := p.HistoryState
	b.presentOne(ctx, p)
	p, e = db.Presentation(s.ThreadID)
	if e != nil || !p.NameApplied || !strings.HasPrefix(p.Name, "原会话的新标题 · Codex ·") {
		t.Fatal("existing group was not renamed", p, e)
	}
	b.presentOne(ctx, p)
	if len(a.names) != 1 || a.created != 0 {
		t.Fatal("poll duplicated rename or recreated group", a.names, a.created)
	}
	p, _ = db.Presentation(s.ThreadID)
	if p.HistoryState != beforeHistory {
		t.Fatal("rename changed history state", p)
	}
	binding, _ := db.ByThread(s.ThreadID)
	if binding.Address.ChatID != s.Address.ChatID || binding.WorkDir != s.WorkDir {
		t.Fatal("rename changed mapping")
	}
}

// 状态卡只有展示行为，fixture没有模型客户端，任何意外执行都会使测试失败。
type readonlyStatusFixture struct {
	fixtureFiles
	sends, updates int
	sendErr        error
	cards          []im.Card
	addresses      []im.Address
}

func (f *readonlyStatusFixture) Capabilities() im.Capabilities {
	return im.Capabilities{InteractiveCards: true, UpdateCard: true}
}
func (f *readonlyStatusFixture) SendCard(_ context.Context, a im.Address, c im.Card, _ string) (string, error) {
	f.sends++
	f.cards = append(f.cards, c)
	f.addresses = append(f.addresses, a)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "single-status-card", nil
}
func (f *readonlyStatusFixture) UpdateCard(_ context.Context, a im.Address, id string, c im.Card) error {
	if id != "single-status-card" {
		return errors.New("wrong card ID")
	}
	f.updates++
	f.cards = append(f.cards, c)
	f.addresses = append(f.addresses, a)
	return nil
}
func TestReadonlyStatusReusesOneCardAndNeverReexecutes(t *testing.T) {
	b, db, _, session := imageFixture(t)
	a := &readonlyStatusFixture{}
	b.channels[session.Address.Connection] = a
	if _, e := db.Claim("original-task"); e != nil {
		t.Fatal(e)
	}
	_ = db.EventStage("original-task", session.Address.Key(), "completed", "")
	_ = db.EventDone("original-task", "done")
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, true); e != nil {
		t.Fatal(e)
	}
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, false); e != nil {
		t.Fatal(e)
	}
	if a.sends != 1 || a.updates != 0 {
		t.Fatal("unchanged terminal view repeated", a.sends, a.updates)
	}
	if _, e := db.Claim("status-command"); e != nil {
		t.Fatal(e)
	}
	_ = db.EventStage("status-command", session.Address.Key(), "command_completed", "")
	_ = db.EventDone("status-command", "done")
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, true); e != nil {
		t.Fatal(e)
	}
	if a.sends != 1 || a.updates != 1 {
		t.Fatal("refresh created duplicate card", a.sends, a.updates)
	}
	for i, c := range a.cards {
		if c.ActionID != "" || len(c.Buttons) > 0 || c.Form != nil {
			t.Fatal("status card exposed executable control", c)
		}
		if a.addresses[i].Key() != session.Address.Key() {
			t.Fatal("cross-group status delivery")
		}
	}
	view, e := db.ReadSessionStatus(session.ThreadID)
	if e != nil || view.Task.ID != "original-task" || view.Task.Status != "done" {
		t.Fatal("status view changed task", view, e)
	}
	// b.codex和b.jobs均未初始化，本次展示依然正常；说明没有恢复或执行任务。
}
func TestReadonlyStatusUnknownDeliveryIsNotResent(t *testing.T) {
	b, db, _, session := imageFixture(t)
	a := &readonlyStatusFixture{sendErr: &im.DeliveryUncertain{Cause: errors.New("fixture ambiguous delivery")}}
	b.channels[session.Address.Connection] = a
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, true); e == nil {
		t.Fatal("unknown delivery ignored")
	}
	card, e := db.StatusCard(session.ThreadID)
	if e != nil || card.State != "unknown" {
		t.Fatal("unsafe card delivery state", card, e)
	}
	a.sendErr = nil
	if e = b.refreshSessionStatus(context.Background(), session.ThreadID, true); e == nil {
		t.Fatal("uncertain delivery was retried")
	}
	if a.sends != 1 {
		t.Fatal("duplicate unknown card", a.sends)
	}
	view, e := db.ReadSessionStatus(session.ThreadID)
	if e != nil || view.Task.ID != "" {
		t.Fatal("display failure created execution", view, e)
	}
}
func TestReadonlyStatusDoesNotFloodOldHistoryOrClosedGroups(t *testing.T) {
	b, db, _, session := imageFixture(t)
	a := &readonlyStatusFixture{}
	b.channels[session.Address.Connection] = a
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, false); e != nil {
		t.Fatal(e)
	}
	if a.sends != 0 {
		t.Fatal("startup created idle cards in all historical groups")
	}
	if e := db.Archive(session.ThreadID); e != nil {
		t.Fatal(e)
	}
	if e := b.refreshSessionStatus(context.Background(), session.ThreadID, true); e != nil {
		t.Fatal(e)
	}
	if a.sends != 0 {
		t.Fatal("status update revived archived group")
	}
}
func TestReadonlyCardClearlyDistinguishesUnknownAndFinished(t *testing.T) {
	for _, tc := range []struct{ state, want string }{{"done", "已完成"}, {"uncertain", "结果未确认"}, {"interrupted", "观察已中断"}, {"failed", "失败／未完成"}} {
		c := readonlySessionCard(store.SessionStatus{Session: store.Session{WorkDir: "/work"}, Task: store.TaskRow{ID: "request", Status: tc.state, Stage: "waiting_model", ReceivedAt: 100, UpdatedAt: 200}}, time.Unix(500, 0))
		if !strings.Contains(c.Text, tc.want) {
			t.Fatal("incorrect display", tc, c.Text)
		}
		if (tc.state == "uncertain" || tc.state == "interrupted") && !strings.Contains(c.Text, "不能据此判断桌面任务已取消") {
			t.Fatal("uncertain result overstated", c.Text)
		}
		if !strings.Contains(c.Text, "1分40秒") {
			t.Fatal("terminal elapsed time keeps increasing", c.Text)
		}
	}
}

type blockedStatusFixture struct {
	fixtureFiles
	started chan struct{}
}

func (f *blockedStatusFixture) Capabilities() im.Capabilities {
	return im.Capabilities{InteractiveCards: true, UpdateCard: true}
}
func (f *blockedStatusFixture) SendCard(ctx context.Context, _ im.Address, _ im.Card, _ string) (string, error) {
	close(f.started)
	<-ctx.Done()
	return "", ctx.Err()
}
func TestReadonlyStatusQueueCannotOccupyExecutionQueue(t *testing.T) {
	b, _, _, session := imageFixture(t)
	a := &blockedStatusFixture{started: make(chan struct{})}
	b.channels[session.Address.Connection] = a
	ctx, cancel := context.WithCancel(context.Background())
	b.statusCardJobs = queue.New(ctx, 1, 2)
	b.jobs = queue.New(ctx, 1, 2)
	defer func() { cancel(); b.statusCardJobs.Wait(); b.jobs.Wait() }()
	if e := b.enqueueSessionStatus(session.ThreadID, true); e != nil {
		t.Fatal(e)
	}
	select {
	case <-a.started:
	case <-time.After(time.Second):
		t.Fatal("display job did not start")
	}
	progressed := make(chan struct{})
	if e := b.jobs.Push(session.Address.Key(), func(context.Context) { close(progressed) }); e != nil {
		t.Fatal(e)
	}
	select {
	case <-progressed:
	case <-time.After(time.Second):
		t.Fatal("slow card blocked execution queue")
	}
	if e := b.enqueueSessionStatus(session.ThreadID, true); e != nil {
		t.Fatal(e)
	} // 合并同一会话的并发刷新，不再发第二张卡。
}
