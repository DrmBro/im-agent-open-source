// Package daemon 提供仅本机同用户可访问的后台控制面，不对网络公开。
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"im-agent/internal/channels"
	"im-agent/internal/channels/dingtalk"
	"im-agent/internal/channels/feishu"
	"im-agent/internal/preferences"
	"im-agent/internal/service"
)

func Authenticated(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser origin rejected", 403)
			return
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func socketPath(dir string) string { return filepath.Join(dir, "control.sock") }
func tokenPath(dir string) string  { return filepath.Join(dir, "control.token") }
func secureDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("后台数据目录不是普通目录")
	}
	return os.Chmod(dir, 0700)
}
func writePrivate(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".service-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type response struct {
	Error string `json:"error,omitempty"`
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(response{Error: err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return errors.New("只接受单个 JSON 请求")
	}
	return nil
}

// Run 独占控制锁；业务锁仍由 Store 管理。意图只记录运行/停止；钉钉仅恢复账本中未提交的队列，不重放未知执行。
func Run(parent context.Context, dir string) error {
	if err := secureDirectory(dir); err != nil {
		return err
	}
	release, err := lockDaemon(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return err
	}
	defer release()
	if info, e := os.Lstat(socketPath(dir)); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("控制端点不是 socket，拒绝覆盖")
		}
		if e = os.Remove(socketPath(dir)); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return err
	}
	token := hex.EncodeToString(secret)
	if err = writePrivate(tokenPath(dir), []byte(token), 0600); err != nil {
		return err
	}
	listener, err := net.Listen("unix", socketPath(dir))
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socketPath(dir))
	if err = os.Chmod(socketPath(dir), 0600); err != nil {
		return err
	}
	reg := channels.NewRegistry()
	if err = feishu.Register(reg); err != nil {
		return err
	}
	if err = dingtalk.Register(reg); err != nil {
		return err
	}
	prefs, err := preferences.New(dir, reg)
	if err != nil {
		return err
	}
	defer prefs.Close()
	control := service.New(reg)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer func() {
		control.Stop()
		wait, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = control.Wait(wait)
	}()
	var mu sync.Mutex
	intentPath := filepath.Join(dir, "service.intent")
	raw, _ := os.ReadFile(intentPath)
	desired := strings.TrimSpace(string(raw)) == "running"
	start := func() error {
		cfg, revision, e := prefs.Runtime()
		if e != nil {
			return e
		}
		return control.Start(ctx, cfg, revision)
	}
	var startupError string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		s := control.Status()
		if startupError != "" {
			s.Phase = "failed"
			s.Message = startupError
		}
		reply(w, s, nil)
	})
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		s := control.Status()
		if s.Phase == "running" || s.Phase == "starting" {
			reply(w, response{}, nil)
			return
		}
		if e := writePrivate(intentPath, []byte("running"), 0600); e != nil {
			reply(w, nil, e)
			return
		}
		desired = true
		e := start()
		startupError = ""
		if e != nil {
			startupError = e.Error()
		}
		reply(w, response{}, e)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		e := writePrivate(intentPath, []byte("stopped"), 0600)
		if e == nil {
			desired = false
			startupError = ""
			control.Stop()
		}
		reply(w, response{}, e)
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) { reply(w, response{}, nil); cancel() })
	mux.HandleFunc("GET /lifecycle", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]string{"warning": control.LifecycleWarning()}, nil)
	})
	mux.HandleFunc("POST /retry-presentation", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thread string `json:"thread"`
		}
		if e := decode(r, &body); e != nil {
			reply(w, nil, errors.New("请求格式无效"))
			return
		}
		reply(w, response{}, control.RetryPresentation(body.Thread))
	})
	mux.HandleFunc("POST /retry-closure", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thread string `json:"thread"`
		}
		if e := decode(r, &body); e != nil {
			reply(w, nil, errors.New("请求格式无效"))
			return
		}
		reply(w, response{}, control.RetryClosure(body.Thread))
	})
	registerOperations(mux, control, filepath.Join(dir, "runtime"))
	registerQAOperations(mux, control)
	server := &http.Server{Handler: Authenticated(token, mux), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	resume := func() {
		mu.Lock()
		defer mu.Unlock()
		s := control.Status()
		if desired && (s.Phase == "stopped" || s.Phase == "failed") {
			if e := start(); e != nil {
				startupError = e.Error()
			} else {
				startupError = ""
			}
		}
	}
	resume()
	for {
		select {
		case <-ctx.Done():
			stop, close := context.WithTimeout(context.Background(), 5*time.Second)
			e := server.Shutdown(stop)
			close()
			return e
		case e := <-done:
			if errors.Is(e, http.ErrServerClosed) {
				return nil
			}
			return e
		case <-ticker.C:
			resume()
		}
	}
}
