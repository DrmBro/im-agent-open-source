package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAndCompleteViaResponses(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"reasoning","content":[]},{"type":"message","content":[{"type":"output_text","text":"{\"ok\":true}"}]}]}`))
	}))
	defer srv.Close()
	home := t.TempDir()
	cfg := "model = \"gpt-x\"\nmodel_provider = \"custom\"\nmodel_catalog_json = \"cat.json\"\n[model_providers.custom]\nname = \"c\"\nbase_url = \"" + srv.URL + "/v1/\"\nwire_api = \"responses\"\n"
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0600)
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-test"}`), 0600)
	_ = os.WriteFile(filepath.Join(home, "cat.json"), []byte(`{"models":[{"slug":"gpt-x"},{"slug":"deepseek-v4-flash","display_name":"DeepSeek"}]}`), 0600)
	p, err := Resolve(home)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Complete(context.Background(), "deepseek-v4-flash", "sys", "hi")
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got["model"] != "deepseek-v4-flash" || got["instructions"] != "sys" || got["stream"] != false {
		t.Fatalf("request=%v", got)
	}
	models := Models(home)
	if len(models) != 2 || models[1].ID != "deepseek-v4-flash" {
		t.Fatalf("models=%+v", models)
	}
}

func TestResolveRequiresKey(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider = \"x\"\n[model_providers.x]\nbase_url = \"http://127.0.0.1:1/v1\"\n"), 0600)
	if _, err := Resolve(home); err == nil {
		t.Fatal("missing key must fail so the caller can fall back to Codex")
	}
}

func TestJSONObject(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"a\":1}\n```":   `{"a":1}`,
		"结果如下：{\"a\":{\"b\":2}} 完毕": `{"a":{"b":2}}`,
		`{"a":1}`: `{"a":1}`,
	} {
		if got := JSONObject(in); got != want {
			t.Errorf("JSONObject(%q)=%q want %q", in, got, want)
		}
	}
}
