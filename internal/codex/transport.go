package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// transport 是只监听回环地址的短生命周期端点，使用随机能力令牌；不开放远程控制。
type localTransport struct {
	endpoint, token, tokenDigest string
	ready                        chan struct{}
	mu                           sync.Mutex
	pending                      []byte
	announced                    bool
}

var announcedEndpoint = regexp.MustCompile(`ws://127\.0\.0\.1:([1-9][0-9]{0,4})`)

func prepareTransport() (*localTransport, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	sum := sha256.Sum256([]byte(token))
	return &localTransport{token: token, tokenDigest: hex.EncodeToString(sum[:]), ready: make(chan struct{})}, nil
}

// 只从自己启动的子进程读取已经绑定的实际端口，避免“预留后释放”端口竞争。
// 日志不对外输出，缓冲有界；令牌只存内存，服务端仅接收摘要。
func (t *localTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.announced {
		return len(p), nil
	}
	t.pending = append(t.pending, p...)
	for {
		end := bytes.IndexByte(t.pending, '\n')
		if end < 0 {
			break
		}
		line := t.pending[:end]
		if match := announcedEndpoint.Find(line); match != nil {
			t.endpoint = string(match)
			t.announced = true
			t.pending = nil
			close(t.ready)
			break
		}
		t.pending = t.pending[end+1:]
	}
	if len(t.pending) > 1024 {
		t.pending = append([]byte(nil), t.pending[len(t.pending)-1024:]...)
	}
	return len(p), nil
}
func (t *localTransport) dial(ctx context.Context) (*websocket.Conn, error) {
	d := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	ws, resp, e := d.DialContext(ctx, t.endpoint, http.Header{"Authorization": []string{"Bearer " + t.token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if e != nil {
		return nil, e
	}
	ws.SetReadLimit(16 << 20)
	return ws, nil
}
func (t *localTransport) wait(ctx context.Context) (*websocketStream, error) {
	select {
	case <-t.ready:
	case <-ctx.Done():
		return nil, errors.New("Codex 未报告本机监听端点，请确认 CLI 支持带认证的 WebSocket")
	}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ws, e := t.dial(ctx); e == nil {
			return &websocketStream{conn: ws}, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("Codex 本机能力令牌认证握手失败，未连接未认证端点")
		case <-ticker.C:
		}
	}
}

// 每个 WebSocket 消息转换为一行，复用已有有界 RPC 解析器；并非直接拼接消息。
type websocketStream struct {
	conn    *websocket.Conn
	reader  io.Reader
	newline bool
}

func (s *websocketStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if s.newline {
			s.newline = false
			p[0] = '\n'
			return 1, nil
		}
		if s.reader == nil {
			kind, r, e := s.conn.NextReader()
			if e != nil {
				return 0, e
			}
			if kind != websocket.TextMessage {
				return 0, errors.New("不支持的 Codex RPC 帧")
			}
			s.reader = r
		}
		n, e := s.reader.Read(p)
		if e == io.EOF {
			s.reader = nil
			s.newline = true
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, e
	}
}
func (s *websocketStream) Write(p []byte) (int, error) {
	if e := s.conn.WriteMessage(websocket.TextMessage, p); e != nil {
		return 0, e
	}
	return len(p), nil
}
func (s *websocketStream) Close() error { return s.conn.Close() }
