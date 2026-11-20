// Package llm 直接调用本机 Codex 当前激活的模型供应商（通常由 CC Switch 写入 ~/.codex），
// 用于消息分流这类无需工具的短推理。它不写入任何配置，也不缓存密钥。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Provider 是一次调用所需的端点信息；Key 只在内存中使用。
type Provider struct {
	Name    string
	BaseURL string
	Wire    string // responses 或 chat
	Model   string // config.toml 里的默认模型
	key     string
}

type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func readBounded(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 4<<20 {
		return nil, errors.New("配置文件不可读取")
	}
	return os.ReadFile(path)
}

func codexHome(home string) string {
	if home != "" {
		return home
	}
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return env
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// Resolve 读取当前激活的供应商。每次调用重新读取，CC Switch 切换后无需重启。
func Resolve(home string) (Provider, error) {
	home = codexHome(home)
	raw, err := readBounded(filepath.Join(home, "config.toml"))
	if err != nil {
		return Provider{}, errors.New("未能读取本机 Codex 配置，请在 CC Switch 激活 Codex 供应商")
	}
	var cfg struct {
		Model     string `toml:"model"`
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			Name    string `toml:"name"`
			BaseURL string `toml:"base_url"`
			Wire    string `toml:"wire_api"`
			EnvKey  string `toml:"env_key"`
			OpenAI  bool   `toml:"requires_openai_auth"`
			Bearer  string `toml:"experimental_bearer_token"`
			Headers map[string]string
		} `toml:"model_providers"`
	}
	if toml.Unmarshal(raw, &cfg) != nil {
		return Provider{}, errors.New("Codex config.toml 格式无效")
	}
	p := Provider{Model: cfg.Model, Wire: "responses"}
	name := cfg.Provider
	if name == "" || name == "openai" {
		p.Name, p.BaseURL = "openai", "https://api.openai.com/v1"
	} else {
		entry, ok := cfg.Providers[name]
		if !ok || entry.BaseURL == "" {
			return Provider{}, fmt.Errorf("Codex 供应商 %s 缺少 base_url", name)
		}
		p.Name, p.BaseURL = entry.Name, strings.TrimRight(entry.BaseURL, "/")
		if entry.Wire == "chat" {
			p.Wire = "chat"
		}
		p.key = entry.Bearer
		if p.key == "" && entry.EnvKey != "" {
			p.key = os.Getenv(entry.EnvKey)
		}
	}
	if p.key == "" {
		if auth, e := readBounded(filepath.Join(home, "auth.json")); e == nil {
			var a struct {
				Key string `json:"OPENAI_API_KEY"`
			}
			if json.Unmarshal(auth, &a) == nil {
				p.key = a.Key
			}
		}
	}
	if p.key == "" {
		return Provider{}, errors.New("未找到模型密钥；ChatGPT 登录方式不支持直接调用，请在 CC Switch 使用 API Key 供应商")
	}
	return p, nil
}

// Models 列出 CC Switch 写入的模型目录，供界面选择；目录缺失时只返回默认模型。
func Models(home string) []Model {
	home = codexHome(home)
	seen := map[string]bool{}
	var out []Model
	add := func(id, name string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		out = append(out, Model{ID: id, Name: name})
	}
	raw, _ := readBounded(filepath.Join(home, "config.toml"))
	var cfg struct {
		Model   string `toml:"model"`
		Catalog string `toml:"model_catalog_json"`
	}
	_ = toml.Unmarshal(raw, &cfg)
	add(cfg.Model, "")
	catalog := cfg.Catalog
	if catalog == "" {
		catalog = "cc-switch-model-catalog.json"
	}
	if !filepath.IsAbs(catalog) {
		catalog = filepath.Join(home, catalog)
	}
	if data, err := readBounded(catalog); err == nil {
		var c struct {
			Models []struct {
				Slug    string `json:"slug"`
				Display string `json:"display_name"`
			} `json:"models"`
		}
		if json.Unmarshal(data, &c) == nil {
			for _, m := range c.Models {
				add(m.Slug, m.Display)
			}
		}
	}
	return out
}

// Complete 发送一次无工具、无流式的请求，返回模型的文本输出。
func (p Provider) Complete(ctx context.Context, model, system, user string) (string, error) {
	if model == "" {
		model = p.Model
	}
	if model == "" {
		return "", errors.New("未指定分析模型")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var body any
	url := p.BaseURL + "/responses"
	if p.Wire == "chat" {
		url = p.BaseURL + "/chat/completions"
		body = map[string]any{"model": model, "stream": false, "temperature": 0, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}}}
	} else {
		body = map[string]any{"model": model, "stream": false, "store": false, "instructions": system, "input": user}
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", errors.New("模型接口不可达；若使用 CC Switch 本地代理，请确认代理正在运行")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("模型接口返回 HTTP %d", resp.StatusCode)
	}
	return extractText(raw, p.Wire)
}

func extractText(raw []byte, wire string) (string, error) {
	if wire == "chat" {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &r) != nil || len(r.Choices) == 0 {
			return "", errors.New("模型响应格式无效")
		}
		return r.Choices[0].Message.Content, nil
	}
	var r struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return "", errors.New("模型响应格式无效")
	}
	if r.OutputText != "" {
		return r.OutputText, nil
	}
	var b strings.Builder
	for _, o := range r.Output {
		if o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	if b.Len() == 0 {
		return "", errors.New("模型没有返回文本")
	}
	return b.String(), nil
}

// JSONObject 从模型输出中取出第一个 JSON 对象，兼容 ```json 代码块或前后说明文字。
func JSONObject(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			text = strings.TrimSpace(rest[:j])
		}
	}
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return text
	}
	return text[start : end+1]
}
