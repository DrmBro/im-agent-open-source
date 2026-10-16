package codex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSendPromptRollbackOnlyBeforeEnter(t *testing.T) {
	for _, mode := range []string{"success", "ready_rejected", "enter_uncertain"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer ws.Close()
				for {
					var req struct {
						ID     uint64         `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if ws.ReadJSON(&req) != nil {
						return
					}
					name := req.Method
					if name == "Runtime.evaluate" {
						expr, _ := req.Params["expression"].(string)
						for _, a := range []string{"prepare", "ready", "receipt", "rollback", "cleanup"} {
							if strings.Contains(expr, "(...[\""+a+"\",") {
								name += ":" + a
								break
							}
						}
					}
					if req.Method == "Input.dispatchKeyEvent" {
						name += ":" + req.Params["type"].(string)
					}
					mu.Lock()
					calls = append(calls, name)
					mu.Unlock()
					result := any(map[string]any{})
					if req.Method == "Runtime.evaluate" {
						value := map[string]any{"ok": true}
						if mode == "ready_rejected" && strings.HasSuffix(name, ":ready") {
							value = map[string]any{"ok": false, "reason": "composer_content_mismatch"}
						}
						result = map[string]any{"result": map[string]any{"value": value}}
					}
					response := map[string]any{"id": req.ID, "result": result}
					if mode == "enter_uncertain" && name == "Input.dispatchKeyEvent:keyDown" {
						delete(response, "result")
						response["error"] = map[string]string{"message": "fixture failure"}
					}
					if e = ws.WriteJSON(response); e != nil {
						return
					}
				}
			}))
			defer server.Close()
			ws, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if e != nil {
				t.Fatal(e)
			}
			c := &cdpClient{ws: ws}
			defer c.close()
			ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			e = c.sendPrompt(ctx, "00000000-0000-4000-8000-000000000001", "附件\n\n路径", desktopSendOptions{})
			mu.Lock()
			recorded := strings.Join(calls, ",")
			mu.Unlock()
			if mode == "success" && e != nil {
				t.Fatal(e)
			}
			if mode == "ready_rejected" {
				if e == nil || !strings.Contains(recorded, "Runtime.evaluate:rollback") || strings.Contains(recorded, "Input.dispatchKeyEvent") {
					t.Fatal("invalid pre-submit recovery:", e, recorded)
				}
			}
			if mode == "enter_uncertain" {
				var unknown *DesktopOutcomeUnconfirmed
				if !errors.As(e, &unknown) || strings.Contains(recorded, "Runtime.evaluate:rollback") {
					t.Fatal("must not erase uncertain submission:", e, recorded)
				}
			}
			if !strings.HasSuffix(recorded, "Runtime.evaluate:cleanup") {
				t.Fatal("transaction not cleaned up:", recorded)
			}
		})
	}
}

// 把脚本嵌入表达式的形态保留为可序列化纯值，不插入用户内容拼接代码。
func TestGuardArgumentsRemainJSON(t *testing.T) {
	b, e := json.Marshal([]string{"prepare", "token", "thread", `"; throw new Error('injection') //`})
	if e != nil || !json.Valid(b) {
		t.Fatal("unsafe args")
	}
}

func TestRestoredBaselineHasNoMidnightInputClassification(t *testing.T) {
	if !strings.Contains(desktopGuardScript, `querySelectorAll('textarea,[contenteditable="true"]')`) {
		t.Fatal("baseline selector not restored")
	}
	for _, unexpected := range []string{"waiting_editor", "data-codex-composer-root", "editorKind", "inputProblem", "hasModal", "innerHeight", "composer_is_search"} {
		if strings.Contains(desktopGuardScript, unexpected) {
			t.Fatal("midnight classification still active", unexpected)
		}
	}
	for _, required := range []string{"currentThread() !== expected", "inputs.length !== 1", "textOf(el)", "matchesPrompt", "state.valid", "state.cleanup"} {
		if !strings.Contains(desktopGuardScript, required) {
			t.Fatal("baseline guard lost", required)
		}
	}
}
