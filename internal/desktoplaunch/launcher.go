// Package desktoplaunch只管理用户显式触发的本机桌面启动尝试。
// 不发起模型请求，不执行页面脚本，不退出或修补已有应用。
package desktoplaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const bundleID = "com.openai.codex"
const signingTeam = "2DC432GLL2"

var launchMu sync.Mutex

type Application struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Build      string `json:"build"`
	executable string
}
type Status struct {
	Applications []Application `json:"applications"`
	Selected     string        `json:"selected"`
	Phase        string        `json:"phase"`
	Message      string        `json:"message"`
	Running      bool          `json:"running"`
	Ready        bool          `json:"ready"`
	Port         int           `json:"port"`
}

func initial() Status { return Status{Applications: []Application{}, Phase: "unknown"} }
func commandOutput(parent context.Context, seconds time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, seconds)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}
func application(ctx context.Context, path string) (Application, error) {
	var a Application
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return a, e
	}
	raw, e := commandOutput(ctx, 2*time.Second, "/usr/bin/plutil", "-convert", "json", "-o", "-", filepath.Join(resolved, "Contents/Info.plist"))
	if e != nil {
		return a, e
	}
	var p struct {
		ID         string `json:"CFBundleIdentifier"`
		Executable string `json:"CFBundleExecutable"`
		Version    string `json:"CFBundleShortVersionString"`
		Build      string `json:"CFBundleVersion"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ID != bundleID || p.Executable == "" || filepath.Base(p.Executable) != p.Executable {
		return a, errors.New("桌面应用身份不匹配")
	}
	binary := filepath.Join(resolved, "Contents/MacOS", p.Executable)
	realBinary, e := filepath.EvalSymlinks(binary)
	if e != nil {
		return a, e
	}
	rel, e := filepath.Rel(filepath.Join(resolved, "Contents/MacOS"), realBinary)
	if e != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return a, errors.New("桌面可执行文件不在应用包内")
	}
	info, e := os.Stat(realBinary)
	if e != nil || !info.Mode().IsRegular() {
		return a, errors.New("桌面应用可执行文件无效")
	}
	return Application{Path: resolved, Name: strings.TrimSuffix(filepath.Base(resolved), ".app"), Version: p.Version, Build: p.Build, executable: realBinary}, nil
}
func installed(ctx context.Context) ([]Application, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return nil, e
	}
	var apps []Application
	seen := map[string]bool{}
	// 目录枚举不依赖显示名称，兼容用户把Codex.app改名为ChatGPT.app。
	for _, dir := range []string{"/Applications", filepath.Join(home, "Applications")} {
		entries, e := os.ReadDir(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !strings.HasSuffix(entry.Name(), ".app") {
				continue
			}
			a, e := application(ctx, filepath.Join(dir, entry.Name()))
			if e != nil {
				continue
			}
			if !seen[a.Path] {
				apps = append(apps, a)
				seen[a.Path] = true
			}
		}
	}
	return apps, nil
}
func verified(ctx context.Context, a Application) bool {
	// codesign的-R参数以“=”开头才是内联规则，否则整段文本会被当成文件路径。
	requirement := fmt.Sprintf(`=identifier "%s" and anchor apple generic and certificate leaf[subject.OU] = "%s"`, bundleID, signingTeam)
	_, e := commandOutput(ctx, 8*time.Second, "/usr/bin/codesign", "--verify", "--strict", "-R", requirement, a.Path)
	return e == nil
}
func processIDs(ctx context.Context, a Application) ([]int, error) {
	raw, e := commandOutput(ctx, 3*time.Second, "/bin/ps", "-ww", "-axo", "pid=,stat=,comm=")
	if e != nil {
		return nil, e
	}
	var ids []int
	for _, line := range strings.Split(string(raw), "\n") {
		pidText, rest := processField(line)
		state, path := processField(rest)
		if path != a.executable || strings.Contains(state, "Z") {
			continue
		}
		id, e := strconv.Atoi(pidText)
		if e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, nil
}
func processField(value string) (string, string) {
	value = strings.TrimLeftFunc(value, unicode.IsSpace)
	end := strings.IndexFunc(value, unicode.IsSpace)
	if end < 0 {
		return value, ""
	}
	return value[:end], strings.TrimLeftFunc(value[end:], unicode.IsSpace)
}

func candidatePorts(preferred int) []int {
	ports := []int{}
	if preferred >= 1024 && preferred <= 65535 {
		ports = append(ports, preferred)
	}
	for p := 9222; p <= 9235; p++ {
		if p != preferred {
			ports = append(ports, p)
		}
	}
	return ports
}

// ownedListeners拒绝通配和局域网地址；不把其他进程的调试端口当成桌面入口。
func ownedListeners(ctx context.Context, pid int) (map[int]bool, error) {
	raw, e := commandOutput(ctx, 3*time.Second, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-Fn")
	var exit *exec.ExitError
	if e != nil && !(errors.As(e, &exit) && exit.ExitCode() == 1 && len(raw) == 0) {
		return nil, e
	}
	result := map[int]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		address := strings.TrimPrefix(line, "n")
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			if strings.HasPrefix(address, "*:") {
				host = "*"
				port = strings.TrimPrefix(address, "*:")
			} else {
				return nil, errors.New("无法核对桌面监听地址")
			}
		}
		n, e := strconv.Atoi(port)
		if e != nil {
			return nil, e
		}
		ip := net.ParseIP(host)
		loopback := ip != nil && ip.IsLoopback()
		if previous, ok := result[n]; ok {
			result[n] = previous && loopback
		} else {
			result[n] = loopback
		}
	}
	return result, nil
}
func endpointReady(ctx context.Context, port int) bool {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 1500 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/json", nil)
	if e != nil {
		return false
	}
	response, e := client.Do(req)
	if e != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if e != nil || len(raw) > 256*1024 {
		return false
	}
	var targets []struct {
		URL       string `json:"url"`
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(raw, &targets) != nil {
		return false
	}
	for _, t := range targets {
		if t.URL != "app://-/index.html" && !strings.HasPrefix(t.URL, "app://-/") {
			continue
		}
		u, e := url.Parse(t.WebSocket)
		if e != nil || u.Scheme != "ws" || u.User != nil || u.Port() != strconv.Itoa(port) {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || !strings.HasPrefix(u.Path, "/devtools/") {
			continue
		}
		return true
	}
	return false
}
func inspectApp(ctx context.Context, apps []Application, a Application, preferred int) Status {
	out := Status{Applications: apps, Selected: a.Path, Phase: "unknown"}
	if !verified(ctx, a) {
		out.Phase = "untrusted"
		out.Message = "应用签名或身份未通过校验，未尝试连接或启动。"
		return out
	}
	ids, e := processIDs(ctx, a)
	if e != nil {
		out.Message = "无法确认桌面进程状态，未启动或连接。"
		return out
	}
	if len(ids) == 0 {
		out.Phase = "stopped"
		out.Message = "桌面尚未运行。可显式尝试以本机连接模式启动，兼容性以启动后检查为准。"
		return out
	}
	out.Running = true
	if len(ids) != 1 {
		out.Phase = "ambiguous"
		out.Message = "检测到多个桌面主进程，未选择或启动第二实例。"
		return out
	}
	listeners, e := ownedListeners(ctx, ids[0])
	if e != nil {
		out.Message = "无法确认桌面端口归属，未连接或启动。"
		return out
	}
	for _, port := range candidatePorts(preferred) {
		local, found := listeners[port]
		if !found {
			continue
		}
		if !local {
			out.Phase = "unsafe_listener"
			out.Message = "桌面连接端口含非回环监听地址，未将其标记为可用。请手动关闭该入口并检查启动方式。"
			return out
		}
	}
	for _, port := range candidatePorts(preferred) {
		if listeners[port] && endpointReady(ctx, port) {
			out.Phase = "ready"
			out.Ready = true
			out.Port = port
			out.Message = "桌面入口已核对为本机可用；会话和输入区仍需在每次提交前校验。这不代表模型接口已验收。"
			return out
		}
	}
	out.Phase = "running_without_endpoint"
	out.Message = "桌面已运行，但没有可确认的本机提交入口。请先保存草稿并自行退出桌面，再点击启动；不会自动退出或重启现有应用。"
	return out
}
func Inspect(ctx context.Context, path string, preferred int) (Status, error) {
	out := initial()
	if runtime.GOOS != "darwin" {
		out.Phase = "unsupported"
		out.Message = "此启动入口目前仅实现macOS。"
		return out, nil
	}
	apps, e := installed(ctx)
	if e != nil {
		return out, errors.New("读取已安装桌面应用失败")
	}
	out.Applications = apps
	if len(apps) == 0 {
		out.Phase = "missing"
		out.Message = "标准Applications目录中没有发现身份匹配的桌面应用。"
		return out, nil
	}
	if path == "" && len(apps) == 1 {
		path = apps[0].Path
	}
	for _, a := range apps {
		if a.Path == path {
			return inspectApp(ctx, apps, a, preferred), nil
		}
	}
	out.Phase = "select_application"
	out.Message = "请选择本次要检查或启动的桌面应用；不会按名称猜测。"
	return out, nil
}

// Launch必须由用户确认触发。桌面已运行时只检查，绝不结束进程或另开实例。
func Launch(ctx context.Context, path string, preferred int, confirmed bool) (Status, error) {
	if !confirmed {
		return initial(), errors.New("请先确认启动本机桌面连接模式")
	}
	launchMu.Lock()
	defer launchMu.Unlock()
	status, e := Inspect(ctx, path, preferred)
	if e != nil {
		return status, e
	}
	if status.Phase != "stopped" {
		return status, nil
	}
	var chosen Application
	for _, a := range status.Applications {
		if a.Path == status.Selected {
			chosen = a
		}
	}
	if chosen.Path == "" {
		return status, errors.New("桌面应用选择已失效")
	}
	// 同一身份的其他安装副本正在运行时，不启动第二份或把新参数发给错误实例。
	for _, app := range status.Applications {
		ids, probeErr := processIDs(ctx, app)
		if probeErr != nil {
			return status, errors.New("无法再次核对桌面进程，未启动")
		}
		if len(ids) > 0 {
			status.Phase = "running_without_endpoint"
			status.Running = true
			status.Message = "检测到桌面应用已在运行，请选择正在使用的安装副本检查；不会启动第二实例。"
			return status, nil
		}
	}
	port := 0
	for _, p := range candidatePorts(preferred) {
		listener, e := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if e != nil {
			continue
		}
		listener.Close()
		port = p
		break
	}
	if port == 0 {
		return status, errors.New("没有空闲的本机连接端口，未启动")
	}
	// 不使用-n，不拷贝用户数据目录，不绕过macOS应用单实例机制。
	_, e = commandOutput(ctx, 8*time.Second, "/usr/bin/open", "-a", chosen.Path, "--args", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port="+strconv.Itoa(port))
	if e != nil {
		return status, errors.New("桌面启动命令未确认成功，请检查桌面；没有自动重试")
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// 成功只由进程归属、回环监听和本机目标校验共同决定。
		status = inspectApp(deadline, status.Applications, chosen, port)
		if status.Ready || status.Phase == "unsafe_listener" || status.Phase == "ambiguous" || status.Phase == "untrusted" {
			return status, nil
		}
		select {
		case <-deadline.Done():
			status.Phase = "launch_unconfirmed"
			status.Ready = false
			status.Message = "已尝试启动，但未在等待时间内确认连接入口。当前版本是否支持该启动方式仍待核对；未再次启动、退出桌面或重放任何问题。"
			return status, nil
		case <-ticker.C:
		}
	}
}
