package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"im-agent/internal/config"
	"im-agent/internal/drafts"
)

// 标识来自真实 CLI 的 queue/add 原始请求；绝不从文本相似度或最近轮次推断归属。
type nativeSubmission struct {
	thread, clientID, requestID, queueID, turnID string
	submitted, confirmed                         bool
	ready                                        chan string
}

func (c *Client) nativeTurn(ctx context.Context, thread, text string) (turn string, err error) {
	metadata, e := c.ReadMetadata(ctx, thread)
	if e != nil {
		return "", e
	}
	cwd, e := config.ExistingDirectory(metadata.Cwd)
	if e != nil {
		return "", e
	}
	s := &nativeSubmission{thread: thread, ready: make(chan string, 1)}
	c.nativeMu.Lock()
	if c.nativeTurns[thread] != nil {
		c.nativeMu.Unlock()
		return "", errors.New("当前会话已有原生队列提交")
	}
	c.nativeTurns[thread] = s
	c.nativeMu.Unlock()
	success := false
	defer func() {
		c.freshThreads.Delete(thread)
		if !success {
			c.cancelNativeSubmission(s)
			c.nativeMu.Lock()
			delete(c.nativeTurns, thread)
			c.nativeMu.Unlock()
		}
	}()
	endpoint, stop, e := c.queueProxy(ctx, s)
	if e != nil {
		return "", e
	}
	defer stop()
	cfg := c.runtimeConfig
	args := []string{"queue", "--remote", endpoint, "--remote-auth-token-env", "IM_AGENT_CODEX_TRANSPORT_TOKEN", "--thread", thread, "--message=" + text, "--sandbox", cfg.Codex.Sandbox, "-c", "approval_policy=" + strconv.Quote(cfg.Codex.ApprovalPolicy), "--enable", "default_mode_request_user_input"}
	if cfg.Codex.Mode == "custom" && cfg.Codex.Model != "" {
		args = append(args, "--model", cfg.Codex.Model)
	}
	submitCtx, submitCancel := context.WithTimeout(ctx, 25*time.Second)
	command := exec.CommandContext(submitCtx, cfg.Codex.Binary, args...)
	command.Env = setEnv(append([]string(nil), c.cmd.Env...), "IM_AGENT_CODEX_TRANSPORT_TOKEN", c.transport.token)
	command.Dir = cwd
	command.WaitDelay = 3 * time.Second
	commandErr := command.Run()
	submitCancel()
	// 命令输出不承担回执判定。必须取得真正 queue/add 成功响应和同 clientId 的用户条目。
	c.nativeMu.Lock()
	confirmed := s.confirmed
	c.nativeMu.Unlock()
	if !confirmed {
		if commandErr != nil {
			return "", errors.New("官方 Codex CLI 队列提交失败或回执不确定，未自动重放；请确认 CLI 支持 queue 与远程端点")
		}
		return "", errors.New("未收到原生 CLI 的队列确认，停止本轮，不回退重复提交")
	}
	select {
	case turn = <-s.ready:
		success = true
		return turn, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-c.done:
		return "", errors.New("等待原生队列启动时 Codex 已退出")
	}
}

func (c *Client) cancelNativeSubmission(s *nativeSubmission) {
	c.nativeMu.Lock()
	submitted, id, turn := s.submitted, s.queueID, s.turnID
	c.nativeMu.Unlock()
	if !submitted {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if turn != "" {
		if c.Interrupt(ctx, s.thread, turn) == nil {
			return
		}
	}
	if id != "" {
		if _, e := c.Call(ctx, "thread/queue/delete", map[string]any{"threadId": s.thread, "queuedSubmissionId": id}); e == nil {
			return
		}
	}
	// 不能证明排队/执行已取消时终止自己管理的服务，避免失败消息在后台继续执行。
	c.cancel()
}

// queueProxy 是本机透明协议转发器，只观察关联 ID。CLI 的 initialize、客户端身份、
// 请求正文和响应保持原样，未改 User-Agent/originator，也不自行伪造 queue 请求。
func (c *Client) queueProxy(parent context.Context, s *nativeSubmission) (string, func(), error) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return "", nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	var claimed atomic.Bool
	var peersMu sync.Mutex
	peers := []*websocket.Conn{}
	closePeers := func() {
		peersMu.Lock()
		defer peersMu.Unlock()
		for _, p := range peers {
			_ = p.Close()
		}
	}
	register := func(p *websocket.Conn) {
		peersMu.Lock()
		peers = append(peers, p)
		peersMu.Unlock()
		if ctx.Err() != nil {
			_ = p.Close()
		}
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + c.transport.token
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 || r.URL.Path != "/" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claimed.Swap(true) {
			http.Error(w, "one submission per endpoint", http.StatusConflict)
			return
		}
		downstream, e := upgrader.Upgrade(w, r, nil)
		if e != nil {
			return
		}
		register(downstream)
		defer downstream.Close()
		downstream.SetReadLimit(16 << 20)
		upstream, e := c.transport.dial(ctx)
		if e != nil {
			return
		}
		register(upstream)
		defer upstream.Close()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer downstream.Close()
			for {
				kind, raw, e := upstream.ReadMessage()
				if e != nil {
					return
				}
				c.captureQueueReply(s, raw)
				if e = downstream.WriteMessage(kind, raw); e != nil {
					return
				}
			}
		}()
		for {
			kind, raw, e := downstream.ReadMessage()
			if e != nil {
				break
			}
			if !c.captureQueueRequest(s, raw) {
				break
			}
			if e = upstream.WriteMessage(kind, raw); e != nil {
				break
			}
		}
		upstream.Close()
		<-done
	})
	go func() { _ = server.Serve(listener) }()
	stopped := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); _ = server.Close(); closePeers(); close(stopped) }) }
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-stopped:
		}
	}()
	return "ws://" + listener.Addr().String(), stop, nil
}
func (c *Client) captureQueueRequest(s *nativeSubmission, raw []byte) bool {
	var f Frame
	if json.Unmarshal(raw, &f) != nil {
		return false
	}
	if f.Method != "thread/queue/add" {
		return true
	}
	var p struct {
		ThreadID string `json:"threadId"`
		ClientID string `json:"clientUserMessageId"`
	}
	if json.Unmarshal(f.Params, &p) != nil || p.ThreadID != s.thread || p.ClientID == "" || len(f.ID) == 0 {
		return false
	}
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	if s.submitted {
		return false
	}
	s.submitted = true
	s.clientID = p.ClientID
	s.requestID = string(f.ID)
	return true
}
func (c *Client) captureQueueReply(s *nativeSubmission, raw []byte) {
	var f Frame
	if json.Unmarshal(raw, &f) != nil || f.Method != "" {
		return
	}
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	if !s.submitted || string(f.ID) != s.requestID || f.Error != nil {
		return
	}
	var p struct {
		Queued struct {
			ID       string `json:"id"`
			ClientID string `json:"clientUserMessageId"`
		} `json:"queuedSubmission"`
	}
	if json.Unmarshal(f.Result, &p) == nil && p.Queued.ID != "" && p.Queued.ClientID == s.clientID {
		s.queueID = p.Queued.ID
		s.confirmed = true
	}
}
func (c *Client) observeNativeNotice(thread string, f Frame) bool {
	if c.transport == nil {
		return true
	}
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	s := c.nativeTurns[thread]
	if s == nil {
		return true
	}
	var p struct {
		TurnID string `json:"turnId"`
		Turn   struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item struct {
			Type     string `json:"type"`
			ClientID string `json:"clientId"`
		} `json:"item"`
	}
	if json.Unmarshal(f.Params, &p) != nil {
		return false
	}
	if s.turnID == "" && f.Method == "item/started" && p.Item.Type == "userMessage" && p.Item.ClientID != "" && p.Item.ClientID == s.clientID && p.TurnID != "" {
		s.turnID = p.TurnID
		s.ready <- p.TurnID
	}
	if s.turnID == "" {
		return false
	}
	turn := p.TurnID
	if turn == "" {
		turn = p.Turn.ID
	}
	return turn == "" || turn == s.turnID
}
func (c *Client) allowNativeRequest(f Frame) bool {
	if c.transport == nil {
		return true
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	if json.Unmarshal(f.Params, &p) != nil || p.ThreadID == "" || p.TurnID == "" {
		return false
	}
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	s := c.nativeTurns[p.ThreadID]
	return s != nil && s.turnID != "" && s.turnID == p.TurnID
}

// desktopQueueTurn 将消息交给当前 CODEX_HOME 中已经存在的会话，由桌面 Codex 客户端负责执行。
// 提交路径不得重复发送；回传必须关联新用户输入所属的真实轮次。
func (c *Client) desktopQueueTurn(ctx context.Context, threadID, text string, hooks TurnHooks) (string, error) {
	metadata, err := c.ReadMetadata(ctx, threadID)
	if err != nil {
		return "", err
	}
	// 桌面 CDP 在已有会话输入区发送，不以本机历史 cwd 启动子进程。
	// CLI 回退才需要有效工作目录；两种路径不能共用此前的前置校验。
	rolloutPath, rolloutOffset := findDesktopRollout(c.runtimeConfig.Codex.Home, threadID)
	watch := func() string {
		turnID := "desktop-" + storeHash(threadID + text + time.Now().String())[:16]
		watchCtx, watchCancel := context.WithCancel(ctx)
		c.nativeMu.Lock()
		if old := c.desktopTurns[threadID]; old != nil {
			old()
		}
		c.desktopTurns[threadID] = watchCancel
		c.nativeMu.Unlock()
		go c.watchDesktopTurn(watchCtx, threadID, turnID, text, rolloutPath, rolloutOffset)
		return turnID
	}

	// 只要当前桌面端通过 Codex++ 暴露了 CDP，就直接把消息交给桌面端。
	// 不能同时再执行 codex queue：那会产生第二条排队消息，桌面端不会消费，
	// 并且容易造成重复执行或一直停在 waiting_model。
	endpoint, discoverErr := discoverDesktopCDP(ctx, c.runtimeConfig.Codex.CdpPort)
	if discoverErr == nil {
		defer endpoint.close()
		if err = endpoint.sendPrompt(ctx, threadID, text, desktopSendOptions{
			PreserveDraft:    c.runtimeConfig.Codex.PreserveDesktopDraft,
			AutoRestoreDraft: c.runtimeConfig.Codex.AutoRestoreDesktopDraft,
			RequestID:        hooks.RequestID,
			Drafts:           drafts.New(c.runtimeConfig.StateDir),
			OnDraftRetained:  hooks.DraftRecoveryNeeded,
			OpenThread:       openDesktopThread,
			AutoSwitch:       c.runtimeConfig.Codex.AutoSwitchThread,
			Progress:         hooks.Progress,
			BeforeSend: func() error {
				rolloutPath, rolloutOffset = findDesktopRollout(c.runtimeConfig.Codex.Home, threadID)
				if hooks.BeforeSubmit != nil {
					return hooks.BeforeSubmit(rolloutPath, rolloutOffset)
				}
				return nil
			},
		}); err != nil {
			return "", err
		}
		return watch(), nil
	}
	if c.runtimeConfig.Codex.SubmissionMode == "native_queue" || c.runtimeConfig.Codex.SubmissionMode == "desktop_cdp" || c.runtimeConfig.Codex.CdpPort > 0 {
		return "", discoverErr
	}

	cwd, err := config.ExistingDirectory(metadata.Cwd)
	if err != nil {
		return "", err
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"queue", "--thread", threadID, "--message", text}
	if c.runtimeConfig.Codex.Mode == "custom" && c.runtimeConfig.Codex.Model != "" {
		args = append(args, "--model", c.runtimeConfig.Codex.Model)
	}
	cmd := exec.CommandContext(commandCtx, c.runtimeConfig.Codex.Binary, args...)
	cmd.Dir = cwd
	cmd.Env = c.cmd.Env
	if hooks.BeforeSubmit != nil {
		if err = hooks.BeforeSubmit(rolloutPath, rolloutOffset); err != nil {
			return "", err
		}
	}
	output, queueErr := cmd.CombinedOutput()
	if queueErr != nil {
		return "", fmt.Errorf("桌面 Codex 队列提交失败：%s", safeQueueOutput(output))
	}
	queuedID := parseQueuedMessageID(output)
	if queuedID == "" {
		return "", errors.New("桌面 Codex 未返回可关联的队列消息 ID")
	}
	if e := c.startQueuedSubmission(ctx, threadID, queuedID); e != nil {
		return "", fmt.Errorf("桌面会话队列启动失败：%w", e)
	}
	return watch(), nil
}

func (c *Client) watchDesktopTurn(ctx context.Context, threadID, turnID, input string, rolloutPath string, rolloutOffset int64) {
	defer func() { c.nativeMu.Lock(); delete(c.desktopTurns, threadID); c.nativeMu.Unlock() }()
	if rolloutPath == "" {
		c.sendDesktopObservationStopped(threadID, turnID)
		return
	}
	if !readDesktopRolloutUntil(ctx, rolloutPath, rolloutOffset, turnID, input, c, threadID) && ctx.Err() != context.Canceled {
		c.sendDesktopObservationStopped(threadID, turnID)
	}
}

func parseQueuedMessageID(raw []byte) string {
	re := regexp.MustCompile(`Queued message ([0-9a-f-]{20,}) for thread`)
	m := re.FindSubmatch(raw)
	if len(m) == 2 {
		return string(m[1])
	}
	return ""
}
func (c *Client) startQueuedSubmission(ctx context.Context, threadID, clientID string) error {
	raw, e := c.Call(ctx, "thread/queue/list", map[string]any{"threadId": threadID, "limit": 50})
	if e != nil {
		return e
	}
	var out struct {
		Data []struct {
			ID       string `json:"id"`
			ClientID string `json:"clientUserMessageId"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return errors.New("桌面队列列表格式无效")
	}
	for _, item := range out.Data {
		if item.ClientID == clientID && item.ID != "" {
			_, e = c.Call(ctx, "thread/queue/start", map[string]any{"threadId": threadID, "queuedSubmissionId": item.ID})
			return e
		}
	}
	return errors.New("未找到当前提交的队列回执，未确认启动；请检查桌面排队状态")
}
func findDesktopRollout(home, threadID string) (string, int64) {
	if home == "" {
		if h, e := os.UserHomeDir(); e == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	root := filepath.Join(home, "sessions")
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") || !strings.Contains(filepath.Base(path), threadID) {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		raw, readErr := readRolloutLine(bufio.NewReader(f))
		f.Close()
		if readErr != nil {
			return nil
		}
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
			return nil
		}
		if found == "" || filepath.Base(path) > filepath.Base(found) {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", 0
	}
	info, e := os.Stat(found)
	if e != nil {
		return "", 0
	}
	return found, info.Size()
}

// 只接受提交后真实用户条目所属轮次的完成事件，不能拿提交前正在执行的轮次作为回执。
func readDesktopRolloutUntil(ctx context.Context, path string, offset int64, turnID, expected string, c *Client, threadID string) bool {
	currentTurn, matchedTurn, final, finalID := "", "", "", ""
	observedTurn := false
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		info, statErr := f.Stat()
		if statErr != nil || info.Size() < offset {
			// 桌面日志回退后旧观察身份不可再安全关联，结束本地等待但不重发。
			f.Close()
			return false
		}
		if _, err = f.Seek(offset, 0); err != nil {
			f.Close()
			return false
		}
		reader := bufio.NewReaderSize(f, 64<<10)
		for {
			raw, err := readRolloutLine(reader)
			if err == io.EOF {
				break // 尾部尚未写完，等待完整记录。
			}
			if err != nil {
				// 确定的读取错误不能在同一位置无限重试；不猜测轮次，也不重发。
				f.Close()
				c.sendDesktopObservationStopped(threadID, turnID)
				return true
			}
			offset += int64(len(raw))
			var r struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			if r.Type == "event_msg" {
				var ev struct {
					Type   string `json:"type"`
					TurnID string `json:"turn_id"`
					Last   string `json:"last_agent_message"`
				}
				if json.Unmarshal(r.Payload, &ev) != nil {
					continue
				}
				if ev.Type == "task_started" {
					currentTurn = ev.TurnID
					observedTurn = ev.TurnID != ""
				}
				if matchedTurn == "" && observedTurn && ev.TurnID == currentTurn &&
					(ev.Type == "task_complete" || ev.Type == "turn_aborted") {
					// 桌面轮次已结束却无法关联本次输入：结果未知而非继续占住本地队列。
					// 不猜测回复归属，也不重放模型输入。
					f.Close()
					c.sendDesktopObservationStopped(threadID, turnID)
					return true
				}
				if matchedTurn != "" && ev.TurnID == matchedTurn {
					if ev.Type == "turn_aborted" {
						f.Close()
						c.sendDesktopTurnFailure(threadID, turnID, "桌面本轮已中断")
						return true
					}
					if ev.Type == "task_complete" {
						if ev.Last != "" {
							if finalID != "" && final != ev.Last {
								f.Close()
								c.sendDesktopObservationStopped(threadID, turnID)
								return true
							}
							final = ev.Last
						}
						f.Close()
						if strings.TrimSpace(final) == "" {
							c.sendDesktopTurnFailure(threadID, turnID, "匹配的桌面轮次没有最终文本")
							return true
						}
						if finalID == "" {
							c.sendDesktopObservationStopped(threadID, turnID)
							return true
						}
						c.subMu.Lock()
						ch := c.subs[threadID]
						if ch != nil {
							item := map[string]any{"threadId": threadID, "turnId": turnID, "item": map[string]any{"id": finalID, "type": "agentMessage", "phase": "final_answer", "text": final}}
							complete := map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "completed"}}
							select {
							case ch <- Notice{"item/completed", mustJSON(item)}:
							default:
							}
							select {
							case ch <- Notice{"turn/completed", mustJSON(complete)}:
							default:
							}
						}
						c.subMu.Unlock()
						return true
					}
				}
			}
			if r.Type == "response_item" {
				var item struct {
					ID      string         `json:"id"`
					Type    string         `json:"type"`
					Role    string         `json:"role"`
					Phase   string         `json:"phase"`
					Content []InputContent `json:"content"`
					Meta    struct {
						TurnID string `json:"turn_id"`
					} `json:"internal_chat_message_metadata_passthrough"`
				}
				if json.Unmarshal(r.Payload, &item) != nil || item.Type != "message" {
					continue
				}
				parts := []string{}
				for _, p := range item.Content {
					if p.Type == "input_text" || p.Type == "output_text" || p.Type == "text" {
						parts = append(parts, p.Text)
					}
				}
				text := strings.Join(parts, "\n")
				if matchedTurn == "" && item.Role == "user" && desktopInputMatches(text, expected) {
					actual := item.Meta.TurnID
					if actual == "" {
						actual = currentTurn
					}
					if actual != "" {
						matchedTurn = actual
					}
				}
				if matchedTurn != "" && currentTurn == matchedTurn && item.Role == "assistant" && item.Phase == "final_answer" {
					finalID = item.ID
					final = text
				}
			}
		}
		f.Close()
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func safeQueueOutput(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len([]rune(text)) > 240 {
		text = string([]rune(text)[:240]) + "…"
	}
	return text
}
func storeHash(s string) string      { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func mustJSON(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }

func (c *Client) sendDesktopTurnFailure(threadID, turnID, message string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	ch := c.subs[threadID]
	if ch == nil {
		return
	}
	params := mustJSON(map[string]any{"threadId": threadID, "turnId": turnID, "message": message})
	select {
	case ch <- Notice{"turn/failed", params}:
	default:
	}
}

// 观察结束不同于真实 turn/failed，不向上层伪造桌面执行失败。
func (c *Client) sendDesktopObservationStopped(threadID, turnID string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if ch := c.subs[threadID]; ch != nil {
		params := mustJSON(map[string]any{"threadId": threadID, "turnId": turnID})
		select {
		case ch <- Notice{"bridge/desktopObservationStopped", params}:
		default:
		}
	}
}
