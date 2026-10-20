// Package codex 实现真实 Codex CLI app-server 的 JSON-RPC 通道。
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"im-agent/internal/config"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Method  string `json:"-"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type Notice struct {
	Method string
	Params json.RawMessage
}
type Handler func(context.Context, string, json.RawMessage) (any, error)
type Client struct {
	cmd               *exec.Cmd
	in                io.WriteCloser
	ctx               context.Context
	cancel            context.CancelFunc
	done              chan struct{}
	write             sync.Mutex
	mu                sync.Mutex
	pending           map[string]chan Frame
	subMu             sync.Mutex
	subs              map[string]chan Notice
	next              atomic.Int64
	handler           Handler
	handlerMu         sync.RWMutex
	requests          chan struct{}
	wg                sync.WaitGroup
	archived          chan string
	pendingReads      map[string]bool
	rolloutHistories  sync.Map
	freshThreads      sync.Map
	transport         *localTransport
	runtimeConfig     config.Config
	qaDisabledServers map[string]bool
	nativeMu          sync.Mutex
	nativeTurns       map[string]*nativeSubmission
	desktopTurns      map[string]context.CancelFunc
}

func Start(parent context.Context, c config.Config) (*Client, error) {
	return start(parent, c, false)
}

// StartQA 复用本机模型与认证，独立业务进程关闭主机及连接器工具。
func StartQA(parent context.Context, c config.Config) (*Client, error) {
	c.Codex.SubmissionMode = "direct"
	client, err := start(parent, c, true)
	if err != nil {
		return nil, err
	}
	check, stop := context.WithTimeout(parent, 20*time.Second)
	err = client.assertQAToolIsolation(check)
	stop()
	if err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func start(parent context.Context, c config.Config, inheritPolicy bool) (*Client, error) {
	ctx, cancel := context.WithCancel(parent)
	nativeMode := false
	var transport *localTransport
	if nativeMode {
		var e error
		transport, e = prepareTransport()
		if e != nil {
			cancel()
			return nil, e
		}
	}
	args := []string{"app-server"}
	if !inheritPolicy {
		args = append(args, "-c", "approval_policy="+strconv.Quote(c.Codex.ApprovalPolicy), "-c", "sandbox_mode="+strconv.Quote(c.Codex.Sandbox))
	} else {
		overrides, err := qaRuntimeOverrides(c)
		if err != nil {
			cancel()
			return nil, err
		}
		args = append(args, overrides...)
	}
	if nativeMode {
		args = append(args, "--listen", "ws://127.0.0.1:0", "--ws-auth", "capability-token", "--ws-token-sha256", transport.tokenDigest, "--enable", "default_mode_request_user_input")
	} else {
		args = append(args, "--stdio")
	}
	cmd := exec.CommandContext(ctx, c.Codex.Binary, args...)
	cmd.WaitDelay = 3 * time.Second
	hostDir, e := HostDirectory(c.StateDir)
	if e != nil {
		cancel()
		return nil, e
	}
	cmd.Dir = hostDir
	// IM 凭证来自内存快照，不写入环境；同时剔除遗留飞书变量，防止进入 Codex 子进程。
	env := os.Environ()
	// 独立桥接不得继承启动它的另一个 Codex 客户端身份或会话/权限上下文。
	for _, name := range []string{"CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_PERMISSION_PROFILE"} {
		env = removeEnv(env, name)
	}

	for _, entry := range append([]string(nil), env...) {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "FEISHU_") || strings.HasPrefix(name, "LARK_") || strings.HasPrefix(name, "IM_AGENT_FEISHU_") || strings.HasPrefix(name, "DINGTALK_") {
			env = removeEnv(env, name)
		}
	}
	home := c.Codex.Home
	if home == "" {
		userHome, e := os.UserHomeDir()
		if e != nil {
			cancel()
			return nil, errors.New("无法确定本机 Codex 配置目录")
		}
		home = filepath.Join(userHome, ".codex")
	}
	env = setEnv(env, "CODEX_HOME", home)
	// 通过 NVM 自动找到的 npm 启动脚本通常需要同目录下的 node。
	if filepath.IsAbs(c.Codex.Binary) {
		env = setEnv(env, "PATH", filepath.Dir(c.Codex.Binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cmd.Env = env
	var in io.WriteCloser
	var out io.Reader
	if !nativeMode {
		in, e = cmd.StdinPipe()
		if e != nil {
			cancel()
			return nil, e
		}
		out, e = cmd.StdoutPipe()
		if e != nil {
			cancel()
			return nil, e
		}
	} else {
		cmd.Stdout = transport
	}
	cmd.Stderr = io.Discard
	if nativeMode {
		cmd.Stderr = transport
	}
	if e = cmd.Start(); e != nil {
		cancel()
		return nil, e
	}
	if nativeMode {
		handshake, stop := context.WithTimeout(ctx, 20*time.Second)
		stream, dialErr := transport.wait(handshake)
		stop()
		if dialErr != nil {
			cancel()
			_ = cmd.Wait()
			return nil, dialErr
		}
		in, out = stream, stream
	}
	client := &Client{transport: transport, runtimeConfig: c, nativeTurns: map[string]*nativeSubmission{}, desktopTurns: map[string]context.CancelFunc{}, cmd: cmd, in: in, ctx: ctx, cancel: cancel, done: make(chan struct{}), pending: map[string]chan Frame{}, pendingReads: map[string]bool{}, subs: map[string]chan Notice{}, requests: make(chan struct{}, 32), archived: make(chan string, 128)}
	go client.read(out)
	initCtx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	_, e = client.Call(initCtx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "im-agent", "title": "IM Agent Bridge", "version": "0.1.0"}, "capabilities": map[string]any{"experimentalApi": true}})
	if e == nil {
		e = client.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	}
	if e != nil {
		client.Close()
		return nil, fmt.Errorf("Codex App Server 初始化失败：%w", e)
	}
	return client, nil
}
func removeEnv(env []string, key string) []string {
	out := []string{}
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return out
}
func setEnv(env []string, key, val string) []string { return append(removeEnv(env, key), key+"="+val) }
func (c *Client) SetHandler(h Handler)              { c.handlerMu.Lock(); defer c.handlerMu.Unlock(); c.handler = h }
func (c *Client) Done() <-chan struct{}             { return c.done }
func (c *Client) Close()                            { c.cancel(); _ = c.in.Close(); <-c.done; c.wg.Wait() }
func (c *Client) send(v any) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	c.write.Lock()
	defer c.write.Unlock()
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	_, e = c.in.Write(append(raw, '\n'))
	return e
}
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	id := strconv.FormatInt(c.next.Add(1), 10)
	ch := make(chan Frame, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.pendingReads[id] = method == "thread/read"
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); delete(c.pendingReads, id); c.mu.Unlock() }()
	if e := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); e != nil {
		return nil, e
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return nil, &RPCError{Method: method, Code: f.Error.Code, Message: f.Error.Message}
		}
		return f.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("Codex App Server 已退出")
	}
}
func (c *Client) Subscribe(thread string) (<-chan Notice, func(), error) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if _, ok := c.subs[thread]; ok {
		return nil, nil, errors.New("同一 Codex 会话已有执行任务")
	}
	ch := make(chan Notice, 512)
	c.subs[thread] = ch
	return ch, func() {
		c.subMu.Lock()
		delete(c.subs, thread)
		c.subMu.Unlock()
		c.nativeMu.Lock()
		delete(c.nativeTurns, thread)
		c.nativeMu.Unlock()
	}, nil
}
func (c *Client) read(out io.Reader) {
	defer close(c.done)
	defer func() {
		c.subMu.Lock()
		for id, ch := range c.subs {
			close(ch)
			delete(c.subs, id)
		}
		c.subMu.Unlock()
	}()
	defer c.cancel()
	reader := bufio.NewReaderSize(out, 64<<10)
	for {
		raw, oversized, readErr := readRPCFrame(reader)
		if readErr != nil {
			break
		}
		if oversized {
			c.rejectOversizedReads()
			continue
		}
		if len(raw) == 0 {
			continue
		}
		var f Frame
		if json.Unmarshal(raw, &f) != nil {
			c.cancel()
			break
		}
		if f.Method == "" && len(f.ID) > 0 {
			var id string
			if json.Unmarshal(f.ID, &id) != nil {
				id = string(f.ID)
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
			continue
		}
		if f.Method != "" && len(f.ID) > 0 {
			select {
			case c.requests <- struct{}{}:
				c.wg.Add(1)
				go func(f Frame) { defer c.wg.Done(); defer func() { <-c.requests }(); c.respond(f) }(f)
			default:
				_ = c.send(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": map[string]any{"code": -32000, "message": "bridge busy; request denied"}})
			}
			continue
		}
		if f.Method != "" {
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(f.Params, &p)
			if f.Method == "thread/archived" && p.ThreadID != "" {
				select {
				case c.archived <- p.ThreadID:
				default: /* 归档列表轮询补偿 */
				}
			}
			if !c.observeNativeNotice(p.ThreadID, f) {
				continue
			}
			c.subMu.Lock()
			ch := c.subs[p.ThreadID]
			if ch != nil {
				select {
				case ch <- Notice{f.Method, f.Params}:
				default:
					c.cancel()
				}
			}
			c.subMu.Unlock()
		}
	}
	c.cancel()
	_ = c.in.Close()
	_ = c.cmd.Wait()
}
func (c *Client) respond(f Frame) {
	c.handlerMu.RLock()
	h := c.handler
	c.handlerMu.RUnlock()
	var result any
	var e error
	if h == nil || !c.allowNativeRequest(f) {
		e = errors.New("handler unavailable for this turn")
	} else {
		result, e = h(c.ctx, f.Method, f.Params)
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": f.ID}
	if e != nil {
		msg["error"] = map[string]any{"code": -32000, "message": "request denied or unavailable"}
	} else {
		msg["result"] = result
	}
	_ = c.send(msg)
}

func (c *Client) ArchivedEvents() <-chan string { return c.archived }

// readRPCFrame 有界缓冲并排空过大行，不让历史读取的超大响应杀掉整个 App Server。
func readRPCFrame(reader *bufio.Reader) ([]byte, bool, error) {
	const limit = 8 << 20
	var out []byte
	oversized := false
	for {
		piece, e := reader.ReadSlice('\n')
		if !oversized {
			if len(out)+len(piece) > limit {
				out = nil
				oversized = true
			} else {
				out = append(out, piece...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if e == io.EOF && (len(out) > 0 || oversized) {
			return out, oversized, nil
		}
		if e != nil {
			return nil, oversized, e
		}
		return out, oversized, nil
	}
}
func (c *Client) rejectOversizedReads() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		if c.pendingReads[id] {
			select {
			case ch <- Frame{Error: &RPCError{Code: -32001, Message: "history frame exceeds safe limit"}}:
			default:
			}
		}
	}
}

// Error 只返回方法和代码，不回显可能含敏感内容的服务端原始消息。
func (e *RPCError) Error() string {
	return fmt.Sprintf("Codex RPC %s 失败（code=%d）", e.Method, e.Code)
}
