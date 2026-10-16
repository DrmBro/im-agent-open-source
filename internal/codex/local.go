package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"im-agent/internal/config"
)

// LocalInfo 仅展示安全的本机配置摘要，不输出完整 TOML、URL、认证内容或指纹。
type LocalInfo struct {
	Ready       bool     `json:"ready"`
	Mode        string   `json:"mode"`
	Binary      string   `json:"binary"`
	Home        string   `json:"home"`
	ConfigPath  string   `json:"config_path"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Message     string   `json:"message"`
	Warnings    []string `json:"warnings"`
	fingerprint string
}

func (i LocalInfo) Fingerprint() string { return i.fingerprint }
func InspectLocal(c config.Codex, workDir string) LocalInfo {
	info := LocalInfo{Mode: c.Mode, Warnings: []string{}}
	home, err := os.UserHomeDir()
	if err != nil {
		info.Message = "无法找到当前用户主目录"
		return info
	}
	if info.Mode == "" {
		info.Mode = "follow_local"
	}
	if info.Mode != "follow_local" && info.Mode != "custom" {
		info.Message = "未知 Codex 模式"
		return info
	}
	dir := filepath.Join(home, ".codex")
	if info.Mode == "custom" && c.Home != "" {
		dir = c.Home
		if strings.HasPrefix(dir, "~/") {
			dir = filepath.Join(home, dir[2:])
		}
		if !filepath.IsAbs(dir) {
			info.Message = "高级配置目录必须是绝对路径"
			return info
		}
	}
	info.Home = filepath.Clean(dir)
	info.ConfigPath = filepath.Join(info.Home, "config.toml")
	explicit := ""
	if info.Mode == "custom" {
		explicit = c.Binary
	}
	info.Binary, err = findBinary(explicit, home)
	if err != nil {
		info.Message = err.Error()
	}
	h := sha256.New()
	_, _ = io.WriteString(h, info.Mode+"\x00"+info.Home+"\x00"+info.Binary)
	// 文件内容只在本机做摘要比较，认证文件永不解析、复制或输出。
	raw, readErr := boundedRead(info.ConfigPath, 2<<20)
	if readErr != nil {
		info.Message = "未能读取本机 Codex 配置，请在 CC Switch 的 Codex 页签激活配置，或在高级设置选择正确目录"
		return info
	}
	_, _ = h.Write(raw)
	var parsed struct {
		Model    string `toml:"model"`
		Provider string `toml:"model_provider"`
		Profile  string `toml:"profile"`
	}
	if toml.Unmarshal(raw, &parsed) != nil {
		info.Message = "config.toml 格式无效，未展示原文；请检查本机 Codex 配置"
		return info
	}
	info.Model = safeLabel(parsed.Model)
	info.Provider = safeLabel(parsed.Provider)
	if info.Model == "" {
		info.Model = "由 Codex 默认配置决定"
	}
	if info.Provider == "" {
		info.Provider = "由 Codex 默认配置决定"
	}
	if parsed.Profile != "" {
		info.Warnings = append(info.Warnings, "本机配置含 profile，摘要不是 profile 或任务覆盖后的最终结果。")
	}
	if info.Mode == "custom" && c.Model != "" {
		info.Model = safeLabel(c.Model)
		info.Warnings = append(info.Warnings, "高级兼容模式设置了模型覆盖；若要完全跟随 CC Switch，请切回默认模式。")
	}
	if c.Sandbox != "" {
		_, _ = io.WriteString(h, "\x00sandbox:"+c.Sandbox+"\x00approval:"+c.ApprovalPolicy)
	}
	for _, path := range []string{filepath.Join(info.Home, "auth.json")} {
		data, e := boundedRead(path, 2<<20)
		if e == nil {
			_, _ = h.Write(data)
		} else if !os.IsNotExist(e) {
			info.Warnings = append(info.Warnings, "认证文件不可读取或超限；仍由 Codex 自行认证，无法监测此文件变化。")
		}
	}
	// 检测项目层配置，明确披露 Codex 的原生覆盖关系；不读取项目密钥到配置数据库。
	if workDir != "" {
		absolute, e := filepath.Abs(workDir)
		if e == nil {
			for depth := 0; depth < 64; depth++ {
				project := filepath.Join(absolute, ".codex", "config.toml")
				if project != info.ConfigPath {
					if data, e := boundedRead(project, 2<<20); e == nil {
						_, _ = io.WriteString(h, project)
						_, _ = h.Write(data)
						info.Warnings = append(info.Warnings, "存在项目层 Codex 配置："+project+"；它可能覆盖上面的用户级摘要。")
					}
				}
				parent := filepath.Dir(absolute)
				if parent == absolute {
					break
				}
				absolute = parent
			}
		}
	}
	if info.Mode == "follow_local" {
		info.Warnings = append(info.Warnings, "默认固定使用当前用户 ~/.codex，并优先桌面应用自带的 Codex CLI 以兼容分页会话；不读取 CC Switch 数据库。")
	}
	info.Warnings = append(info.Warnings, "本页是用户级配置摘要；实际路由由 Codex 决定。若 CC Switch 使用本地代理，代理必须保持运行。")
	info.fingerprint = hex.EncodeToString(h.Sum(nil))
	if err == nil {
		info.Ready = true
		info.Message = "已发现本机 Codex 和用户配置；尚未验证 CLI 版本、认证或中转可用性"
	}
	return info
}
func boundedRead(path string, max int64) ([]byte, error) {
	st, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, errors.New("文件不可读取")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(data)) > max {
		return nil, errors.New("文件超过大小限制")
	}
	return data, nil
}

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/+ -]{1,120}$`)

func safeLabel(s string) string {
	if s == "" {
		return ""
	}
	if !labelPattern.MatchString(s) || strings.HasPrefix(s, "sk-") {
		return "自定义值（已隐藏）"
	}
	return s
}
func findBinary(explicit, home string) (string, error) {
	if explicit != "" && explicit != "codex" {
		if strings.HasPrefix(explicit, "~/") {
			explicit = filepath.Join(home, explicit[2:])
		}
		if !filepath.IsAbs(explicit) {
			p, e := exec.LookPath(explicit)
			if e != nil {
				return "", errors.New("高级设置中的 Codex CLI 不可执行")
			}
			explicit = p
		}
		if executable(explicit) {
			p, _ := filepath.Abs(explicit)
			return p, nil
		}
		return "", errors.New("高级设置中的 Codex CLI 不存在或不可执行")
	}
	// 默认优先桌面应用随附的真实 CLI，避免 PATH 旧版本无法读取桌面分页会话。
	// 用户显式选择的高级路径仍由上面的分支优先处理。
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Codex.app/Contents/Resources/codex",
			"/Applications/ChatGPT.app/Contents/Resources/codex",
			"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
			filepath.Join(home, "Applications/Codex.app/Contents/Resources/codex"),
			filepath.Join(home, "Applications/ChatGPT.app/Contents/Resources/codex"),
			filepath.Join(home, "Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"),
		} {
			if executable(p) {
				return p, nil
			}
		}
	}
	if p, e := exec.LookPath("codex"); e == nil {
		if p, e = filepath.Abs(p); e == nil && executable(p) {
			return p, nil
		}
	}
	candidates := []string{filepath.Join(home, ".local/bin/codex"), "/opt/homebrew/bin/codex", "/usr/local/bin/codex", filepath.Join(home, ".npm-global/bin/codex"), filepath.Join(home, ".volta/bin/codex")}
	for _, pattern := range []string{filepath.Join(home, ".nvm/versions/node/*/bin/codex"), filepath.Join(home, ".local/share/fnm/node-versions/*/installation/bin/codex")} {
		paths, _ := filepath.Glob(pattern)
		sort.Slice(paths, func(i, j int) bool { return versionAfter(paths[i], paths[j]) })
		candidates = append(candidates, paths...)
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Applications/Codex.app/Contents/Resources/codex", "/Applications/ChatGPT.app/Contents/Resources/codex", filepath.Join(home, "Applications/Codex.app/Contents/Resources/codex"), filepath.Join(home, "Applications/ChatGPT.app/Contents/Resources/codex"))
	}
	for _, p := range candidates {
		if executable(p) {
			return p, nil
		}
	}
	return "", errors.New("未发现 Codex CLI。请先安装 Codex，或展开高级设置选择可执行文件；无需填写模型密钥")
}
func executable(path string) bool {
	st, e := os.Stat(path)
	return e == nil && st.Mode().IsRegular() && (runtime.GOOS == "windows" || st.Mode().Perm()&0111 != 0)
}

var versionPattern = regexp.MustCompile(`(?:^|/)(?:v)?([0-9]+)\.([0-9]+)\.([0-9]+)(?:/|$)`)

func versionAfter(a, b string) bool {
	x, y := versionPattern.FindStringSubmatch(filepath.ToSlash(a)), versionPattern.FindStringSubmatch(filepath.ToSlash(b))
	if len(x) == 4 && len(y) == 4 {
		for i := 1; i <= 3; i++ {
			m, _ := strconv.Atoi(x[i])
			n, _ := strconv.Atoi(y[i])
			if m != n {
				return m > n
			}
		}
	}
	return a > b
}

// ResolveLocal 将启动目标冻结为绝对路径，不执行 CLI 或修改模型认证配置。
func ResolveLocal(c config.Config) (config.Config, LocalInfo, error) {
	info := InspectLocal(c.Codex, c.WorkDir)
	if !info.Ready {
		return c, info, fmt.Errorf("%s", info.Message)
	}
	c.Codex.Binary = info.Binary
	c.Codex.Home = info.Home
	if c.Codex.Mode == "follow_local" || c.Codex.Mode == "" {
		c.Codex.Model = ""
	}
	return c, info, nil
}
