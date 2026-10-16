package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"im-agent/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("IM_AGENT_TEST_CODEX_STUB") == "1" {
		stubCodex()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// 只在临时测试进程中使用 JSON-RPC 桩，不启动真实 Codex 或模型。
func stubCodex() {
	scanner := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	threadID := "thread"
	cwd := os.Getenv("IM_AGENT_TEST_WORKDIR")
	rollout := ""
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		if target := os.Getenv("IM_AGENT_TEST_CAPTURE_CALLS"); target != "" {
			f, _ := os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if f != nil {
				f.Write(append(scanner.Bytes(), '\n'))
				f.Close()
			}
		}
		result := map[string]any{}
		switch req.Method {
		case "initialize":
		case "project/list":
			result = map[string]any{"data": []any{map[string]any{"id": "p1", "name": "测试项目", "roots": []any{map[string]string{"path": cwd}}}}}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"model": "test-model", "displayName": "测试模型", "defaultReasoningEffort": "high", "supportedReasoningEfforts": []any{map[string]string{"reasoningEffort": "high"}}}}}
		case "thread/start":
			var params struct {
				Cwd       string         `json:"cwd"`
				ProjectID string         `json:"projectId"`
				Model     string         `json:"model"`
				Config    map[string]any `json:"config"`
			}
			json.Unmarshal(req.Params, &params)
			threadID = "created-thread"
			cwd = params.Cwd
			home := filepath.Join(os.Getenv("CODEX_HOME"), "sessions")
			os.MkdirAll(home, 0700)
			rollout = filepath.Join(home, "rollout-created-thread.jsonl")
			meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": threadID}})
			os.WriteFile(rollout, append(meta, '\n'), 0600)
			result = map[string]any{"thread": map[string]any{"id": threadID, "cwd": cwd, "projectId": params.ProjectID}, "model": params.Model, "reasoningEffort": params.Config["model_reasoning_effort"]}
		case "thread/list":
			result = map[string]any{"data": []any{}, "nextCursor": nil}
			var params struct {
				Archived bool `json:"archived"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if archived := os.Getenv("IM_AGENT_TEST_ARCHIVED_THREAD"); params.Archived && archived != "" {
				result = map[string]any{"data": []any{map[string]string{"id": archived, "cwd": cwd}}, "nextCursor": nil}
			}

		case "thread/read", "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": threadID, "name": os.Getenv("IM_AGENT_TEST_THREAD_NAME"), "cwd": cwd, "turns": []any{}}}
		case "turn/start":
			f, e := os.OpenFile(os.Getenv("IM_AGENT_TEST_CAPTURE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				os.Exit(2)
			}
			f.Write(append(req.Params, '\n'))
			f.Close()
			if rollout != "" {
				var params struct {
					Input []map[string]any `json:"input"`
				}
				json.Unmarshal(req.Params, &params)
				f, _ := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0600)
				enc := json.NewEncoder(f)
				enc.Encode(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "id": "user1", "role": "user", "content": params.Input}})
				enc.Encode(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "id": "reply1", "role": "assistant", "phase": "final_answer", "content": []any{map[string]string{"type": "output_text", "text": "合并任务完成"}}}})
				f.Close()
			}
			result = map[string]any{"turn": map[string]any{"id": "turn1"}}
		default:
			writer.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "not in test protocol"}})
			continue
		}
		writer.Encode(map[string]any{"id": req.ID, "result": result})
		if req.Method == "turn/start" {
			writer.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": threadID, "turnId": "turn1", "item": map[string]any{"id": "reply1", "type": "agentMessage", "phase": "final_answer", "text": "合并任务完成"}}})
			writer.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": "turn1", "status": "completed"}}})
		}
	}
}
func TestImagesThenCaptionSubmitOneTurn(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	capture := filepath.Join(dir, "model-inputs.jsonl")
	t.Setenv("IM_AGENT_TEST_CODEX_STUB", "1")
	t.Setenv("IM_AGENT_TEST_WORKDIR", work)
	t.Setenv("IM_AGENT_TEST_CAPTURE", capture)
	cfg := config.Defaults()
	cfg.StateDir = dir
	cfg.Codex.Home = filepath.Join(dir, "codex-home")
	cfg.Codex.Binary = os.Args[0]
	cfg.Codex.SubmissionMode = "direct"
	cfg.Connections = []config.Connection{{ID: "one", AllowedUsers: []string{"alice"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, e := store.New(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s := store.Session{ThreadID: "thread", Address: im.Address{Channel: "fake", Connection: "one", ChatID: "chat"}, Owner: "alice", WorkDir: work}
	if e = db.Save(s); e != nil {
		t.Fatal(e)
	}
	cx, e := codex.Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer cx.Close()
	adapter := &draftAdapter{}
	b := New(cfg, db, cx, map[string]im.Adapter{"one": adapter}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	qctx, qstop := context.WithCancel(ctx)
	b.jobs = queue.New(qctx, 1, 10)
	defer func() { qstop(); b.jobs.Wait() }()
	for i := 1; i <= 2; i++ {
		msg := im.Message{ID: fmt.Sprint("photo", i), UserID: "alice", Address: s.Address, Attachments: []im.Attachment{{Kind: "image", Key: fmt.Sprint("img", i), Name: fmt.Sprintf("image-%d.png", i)}}}
		if e = b.receiveMessage(ctx, msg); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = os.Stat(capture); !os.IsNotExist(e) {
		t.Fatal("images started model before caption")
	}
	caption := im.Message{ID: "caption", UserID: "alice", Address: s.Address, Text: "比较两张图片的报错"}
	if e = b.receiveMessage(ctx, caption); e != nil {
		t.Fatal(e)
	}
	for {
		view, e := store.TaskCenterSnapshot(dir)
		if e != nil {
			t.Fatal(e)
		}
		finished := false
		for _, row := range view.Tasks {
			if row.ID == caption.ID {
				if row.Status == "failed" {
					t.Fatal(row.Error)
				}
				finished = row.Status == "done"
			}
		}
		if finished {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("combined turn did not complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	qstop()
	b.jobs.Wait()
	data, e := os.ReadFile(capture)
	if e != nil {
		t.Fatal(e)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 turn, got %d", len(lines))
	}
	var input struct {
		Input []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	if e = json.Unmarshal([]byte(lines[0]), &input); e != nil {
		t.Fatal(e)
	}
	if len(input.Input) != 1 || !strings.Contains(input.Input[0].Text, caption.Text) || !strings.Contains(input.Input[0].Text, "image-1.png") || !strings.Contains(input.Input[0].Text, "image-2.png") {
		t.Fatal("combined model input missing caption/images")
	}
	if len(adapter.sources) != 2 || adapter.sources[0] != "photo1" || adapter.sources[1] != "photo2" {
		t.Fatal("wrong download sources", adapter.sources)
	}
	answers := 0
	for _, notice := range adapter.notices {
		if strings.Contains(notice, "合并任务完成") {
			answers++
		}
	}
	if answers != 1 {
		t.Fatal("duplicate/missing reply", answers)
	}
}

func TestMobileQueueOrderingAcrossGroups(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := queue.New(ctx, 2, 8)
	defer func() { cancel(); q.Wait() }()
	started := make(chan string, 3)
	releaseA := make(chan struct{})
	if e := q.Push("group-a", func(context.Context) { started <- "a1"; <-releaseA }); e != nil {
		t.Fatal(e)
	}
	if e := q.Push("group-a", func(context.Context) { started <- "a2" }); e != nil {
		t.Fatal(e)
	}
	if e := q.Push("group-b", func(context.Context) { started <- "b1" }); e != nil {
		t.Fatal(e)
	}
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case v := <-started:
			got[v] = true
		case <-time.After(time.Second):
			close(releaseA)
			t.Fatal("another group blocked")
		}
	}
	if !got["a1"] || !got["b1"] || got["a2"] {
		close(releaseA)
		t.Fatal("queue ordering/isolation failed", got)
	}
	close(releaseA)
	select {
	case v := <-started:
		if v != "a2" {
			t.Fatal(v)
		}
	case <-time.After(time.Second):
		t.Fatal("second message not released")
	}
}
