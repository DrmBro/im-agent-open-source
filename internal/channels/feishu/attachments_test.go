package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type rewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme = r.target.Scheme
	u.Host = r.target.Host
	clone.URL = &u
	return r.base.RoundTrip(clone)
}
func TestAttachmentDecode(t *testing.T) {
	for _, tc := range []struct {
		kind, body string
		ok         bool
	}{{"image", `{"image_key":"img_1"}`, true}, {"file", `{"file_key":"file_1","file_name":"报告.pdf"}`, true}, {"file", `{"file_key":"file_1","file_name":"../../secret"}`, false}, {"image", `{"image_key":"../secret"}`, false}, {"file", `{}`, false}, {"audio", `{}`, false}} {
		_, a, e := decodeAttachmentMessage(tc.kind, tc.body)
		if (e == nil) != tc.ok {
			t.Fatalf("%s %v", tc.kind, e)
		}
		if tc.ok && len(a) != 1 {
			t.Fatal("missing attachment")
		}
	}
}
func TestFeishuFileRoundTripProtocol(t *testing.T) {
	uploaded, sent, downloaded := false, false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/open-apis/im/v1/messages/msg/resources/file_key":
			downloaded = true
			if r.URL.Query().Get("type") != "file" {
				t.Error("wrong resource type")
			}
			w.Write([]byte("download content"))
		case r.Method == "POST" && r.URL.Path == "/open-apis/im/v1/files":
			uploaded = true
			if e := r.ParseMultipartForm(1 << 20); e != nil {
				t.Error(e)
			}
			if r.FormValue("file_type") != "stream" || r.FormValue("file_name") != "result.txt" {
				t.Error("wrong metadata")
			}
			f, _, e := r.FormFile("file")
			if e != nil {
				t.Error(e)
				return
			}
			defer f.Close()
			data, _ := io.ReadAll(f)
			if string(data) != "result" {
				t.Error("wrong payload")
			}
			io.WriteString(w, `{"code":0,"data":{"file_key":"uploaded_key"}}`)
		case r.Method == "POST" && r.URL.Path == "/open-apis/im/v1/messages":
			sent = true
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["msg_type"] != "file" || body["receive_id"] != "chat" || !strings.Contains(body["content"].(string), "uploaded_key") {
				t.Error("wrong message")
			}
			io.WriteString(w, `{"code":0,"data":{"message_id":"reply"}}`)
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c := &Client{cfg: config.Connection{ID: "im"}, token: "test-token", tokenUntil: time.Now().Add(time.Hour), http: &http.Client{Transport: rewriteTransport{http.DefaultTransport, u}, Timeout: time.Second * 2}}
	stream, e := c.DownloadAttachment(context.Background(), "msg", im.Attachment{Kind: "file", Key: "file_key"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(stream)
	stream.Close()
	if string(b) != "download content" {
		t.Fatal("bad download")
	}
	id, e := c.SendAttachment(context.Background(), im.Address{Channel: "feishu", Connection: "im", ChatID: "chat"}, "result.txt", "file", bytes.NewBufferString("result"), "key")
	if e != nil {
		t.Fatal(e)
	}
	if id != "reply" || !uploaded || !sent || !downloaded {
		t.Fatal("incomplete roundtrip")
	}
	if _, e = c.SendAttachment(context.Background(), im.Address{}, "bad.png", "image", bytes.NewBufferString("not an image"), "bad"); e == nil {
		t.Fatal("image validation bypass")
	}
}

func TestPostTextAndImages(t *testing.T) {
	text, files, e := decodeAttachmentMessage("post", `{"title":"分析截图","content":[[{"tag":"text","text":"看看这个报错"},{"tag":"img","image_key":"img_a"}],[{"tag":"a","text":"说明","href":"https://example.invalid"},{"tag":"img","image_key":"img_b"}]]}`)
	if e != nil {
		t.Fatal(e)
	}
	if len(files) != 2 || files[0].Key != "img_a" || files[1].Key != "img_b" || !strings.Contains(text, "看看这个报错") {
		t.Fatal("post split incorrectly", text, files)
	}
	text, files, e = decodeAttachmentMessage("post", `{"zh_cn":{"title":"","content":[[{"tag":"img","image_key":"img_a"}]]}}`)
	if e != nil || text != "" || len(files) != 1 {
		t.Fatal("image-only post must wait for caption", text, e)
	}
	if _, _, e = decodeAttachmentMessage("post", `{"content":[[{"tag":"video","file_key":"x"}]]}`); e == nil {
		t.Fatal("unsupported node silently lost")
	}
}

func TestPostValidationDoesNotSilentlyDropImages(t *testing.T) {
	for _, raw := range []string{`{"content":[[{"tag":"img","image_key":"../bad"}]]}`, `{"zh_cn":null}`, `{"title":"empty"}`, `{"content":[]}`, `{"content":[[{"tag":"file","file_key":"file"}]]}`} {
		if _, _, e := decodeAttachmentMessage("post", raw); e == nil {
			t.Fatal("invalid post accepted", raw)
		}
	}
	text, files, e := decodeAttachmentMessage("post", `{"en_us":{"title":"Question","content":[[{"tag":"text","text":"Help"},{"tag":"img","image_key":"image"}]]}}`)
	if e != nil || len(files) != 1 || !strings.Contains(text, "Help") {
		t.Fatal("localized post failed", e)
	}
}
