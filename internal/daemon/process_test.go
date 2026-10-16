package daemon

import (
	"context"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonProcessHelper(t *testing.T) {
	if os.Getenv("IM_AGENT_DAEMON_TEST_HELPER") != "1" {
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if e := Run(ctx, os.Getenv("IM_AGENT_DAEMON_TEST_DIR")); e != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestBackgroundSurvivesClientDisconnectAndSingleInstance(t *testing.T) {
	dir, e := os.MkdirTemp("", "ima-daemon-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonProcessHelper$")
	cmd.Env = append(os.Environ(), "IM_AGENT_DAEMON_TEST_HELPER=1", "IM_AGENT_DAEMON_TEST_DIR="+dir)
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer cmd.Process.Kill()
	client := NewClient(dir)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for {
		e = client.Ping(ctx)
		if e == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(e)
		case <-time.After(50 * time.Millisecond):
		}
	}
	for _, p := range []string{socketPath(dir), tokenPath(dir)} {
		info, e := os.Stat(p)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("insecure endpoint %s", p)
		}
	}
	// 新 client 等价 UI 断开后重新连接；服务仍在同一独立 OS 进程。
	client = nil
	second := NewClient(dir)
	if e = second.Ping(ctx); e != nil {
		t.Fatal(e)
	}
	if unlock, e := lockDaemon(filepath.Join(dir, "daemon.lock")); e == nil {
		unlock()
		t.Fatal("second daemon acquired lock")
	}
	if e = second.call(ctx, "POST", "/shutdown", struct{}{}, nil); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if e = second.Ping(ctx); e == nil {
		t.Fatal("server still answers after shutdown")
	}
}
func TestLaunchAgentEscapesPaths(t *testing.T) {
	data := launchAgentXML("/tmp/a&b<bin>", "/tmp/data & space")
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		_, e := decoder.Token()
		if e != nil {
			if e.Error() != "EOF" {
				t.Fatal(e)
			}
			break
		}
	}
	if !strings.Contains(string(data), "a&amp;b&lt;bin&gt;") {
		t.Fatal("unescaped plist path")
	}
}

func TestDaemonCrashRestartDoesNotReplayTasks(t *testing.T) {
	dir, e := os.MkdirTemp("", "ima-restart-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	start := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDaemonProcessHelper$")
		cmd.Env = append(os.Environ(), "IM_AGENT_DAEMON_TEST_HELPER=1", "IM_AGENT_DAEMON_TEST_DIR="+dir)
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		return cmd
	}
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	wait := func() {
		for {
			if e := NewClient(dir).Ping(ctx); e == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("daemon did not become ready")
			case <-time.After(40 * time.Millisecond):
			}
		}
	}
	first := start()
	defer first.Process.Kill()
	wait()
	oldToken, e := os.ReadFile(tokenPath(dir))
	if e != nil {
		t.Fatal(e)
	}
	first.Process.Kill()
	_ = first.Wait()
	second := start()
	defer second.Process.Kill()
	wait()
	newToken, e := os.ReadFile(tokenPath(dir))
	if e != nil {
		t.Fatal(e)
	}
	if string(oldToken) == string(newToken) {
		t.Fatal("capability token not rotated")
	}
	if s := NewClient(dir).Status(); s.Phase != "stopped" {
		t.Fatalf("unexpected auto task execution: %s", s.Phase)
	}
	if e = NewClient(dir).call(ctx, "POST", "/shutdown", struct{}{}, nil); e != nil {
		t.Fatal(e)
	}
	if e = second.Wait(); e != nil {
		t.Fatal(e)
	}
}

func TestUnavailableControlIsNotStoppedProof(t *testing.T) {
	dir := t.TempDir()
	unlock, e := lockDaemon(filepath.Join(dir, "daemon.lock"))
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	if e = NewClient(dir).requireStopped(context.Background()); e == nil {
		t.Fatal("maintenance allowed while live process lock held")
	}
}
