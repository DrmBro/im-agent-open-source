package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"im-agent/internal/attachments"
	"im-agent/internal/im"
)

func validResource(a im.Attachment) error {
	if a.Key == "" || len(a.Key) > 1024 || strings.ContainsAny(a.Key, "/\\\x00\r\n") {
		return errors.New("附件资源 ID 无效")
	}
	return attachments.SafeName(a.Name)
}
func decodePost(raw string) (string, []im.Attachment, error) {
	var obj map[string]json.RawMessage
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &obj) != nil {
		return "", nil, errors.New("图文内容格式或大小无效")
	}
	body := []byte(raw)
	if _, ok := obj["content"]; !ok {
		if v, ok := obj["zh_cn"]; ok {
			body = v
		} else if v, ok := obj["en_us"]; ok {
			body = v
		} else {
			return "", nil, errors.New("图文消息缺少支持的内容结构")
		}
	}
	var post struct {
		Title   string `json:"title"`
		Content [][]struct {
			Tag      string `json:"tag"`
			Text     string `json:"text"`
			Href     string `json:"href"`
			ImageKey string `json:"image_key"`
			UserName string `json:"user_name"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &post) != nil || post.Content == nil {
		return "", nil, errors.New("图文消息内容无效")
	}
	var files []im.Attachment
	lines := []string{}
	hasCaption := strings.TrimSpace(post.Title) != ""
	if hasCaption {
		lines = append(lines, post.Title)
	}
	nodes := 0
	for _, row := range post.Content {
		var line strings.Builder
		for _, n := range row {
			nodes++
			if nodes > 1000 {
				return "", nil, errors.New("图文节点过多")
			}
			switch n.Tag {
			case "text", "md", "code_block":
				line.WriteString(n.Text)
				hasCaption = hasCaption || strings.TrimSpace(n.Text) != ""
			case "a":
				line.WriteString(n.Text)
				if n.Href != "" {
					line.WriteString("（" + n.Href + "）")
				}
				hasCaption = hasCaption || strings.TrimSpace(n.Text+n.Href) != ""
			case "at":
				line.WriteString("[提及 " + n.UserName + "]")
			case "img":
				if len(files) >= 5 {
					return "", nil, errors.New("一组图片最多 5 张")
				}
				a := im.Attachment{Kind: "image", Key: n.ImageKey, Name: fmt.Sprintf("image-%d.png", len(files)+1)}
				if e := validResource(a); e != nil {
					return "", nil, e
				}
				files = append(files, a)
				line.WriteString(fmt.Sprintf("[本条图文图片 %d]", len(files)))
			default:
				return "", nil, errors.New("图文含不支持的节点，请使用文字和图片；未截断后继续执行")
			}
		}
		lines = append(lines, line.String())
	}
	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if !hasCaption && len(files) > 0 {
		text = ""
	} // 纯图仍先等待说明，不能用占位符触发模型。
	if text == "" && len(files) == 0 {
		return "", nil, errors.New("图文消息为空")
	}
	return text, files, nil
}
