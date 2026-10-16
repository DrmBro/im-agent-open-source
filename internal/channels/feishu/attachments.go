package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"im-agent/internal/attachments"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
)

func decodeAttachmentMessage(kind, raw string) (string, []im.Attachment, error) {
	if kind == "post" {
		return decodePost(raw)
	}
	var b struct {
		Text     string `json:"text"`
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
		ImageKey string `json:"image_key"`
	}
	if json.Unmarshal([]byte(raw), &b) != nil {
		return "", nil, errors.New("消息内容格式无效")
	}
	if kind == "text" {
		return b.Text, nil, nil
	}
	var a im.Attachment
	switch kind {
	case "image":
		a = im.Attachment{Kind: "image", Key: b.ImageKey, Name: "image.png"}
	case "file":
		a = im.Attachment{Kind: "file", Key: b.FileKey, Name: b.FileName}
	default:
		return "", nil, errors.New("暂不支持此消息类型")
	}
	if err := validResource(a); err != nil {
		return "", nil, err
	}
	if kind == "image" {
		return "", []im.Attachment{a}, nil
	}

	return "", []im.Attachment{a}, nil
}

type attachmentDownloadBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *attachmentDownloadBody) Close() error { err := r.ReadCloser.Close(); r.cancel(); return err }

func (c *Client) DownloadAttachment(ctx context.Context, message string, a im.Attachment) (io.ReadCloser, error) {
	if message == "" || a.Key == "" || (a.Kind != "image" && a.Kind != "file") {
		return nil, diagnostics.UserError("附件来源消息或资源引用无效，未下载")
	}
	token, e := c.accessToken(ctx)
	if e != nil {
		return nil, e
	}
	// 文件读取采用独立90秒上下文，不被文字接口的20秒Client超时截断。
	op, cancel := context.WithTimeout(ctx, 90*time.Second)
	complete := false
	defer func() {
		if !complete {
			cancel()
		}
	}()
	resource := "/im/v1/messages/" + url.PathEscape(message) + "/resources/" + url.PathEscape(a.Key) + "?type=" + a.Kind
	req, e := http.NewRequestWithContext(op, "GET", apiBase+resource, nil)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := *c.http
	client.Timeout = 0 // 超时由op约束；Close会释放上下文。
	resp, e := client.Do(req)
	if e != nil {
		if op.Err() != nil {
			return nil, op.Err()
		}
		return nil, &im.AttachmentReadError{Category: "network", Retryable: true, Cause: diagnostics.UserError("飞书附件网络读取失败，尚未提交模型")}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			return nil, diagnostics.UserError(fmt.Sprintf("飞书附件资源权限或授权不足 HTTP %d，未下载", resp.StatusCode))
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, diagnostics.UserError("飞书附件原消息或资源已不存在 HTTP 404，未下载")
		}
		return nil, &im.AttachmentReadError{Category: "http", Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			Cause: diagnostics.UserError(fmt.Sprintf("飞书附件资源读取失败 HTTP %d，未下载", resp.StatusCode))}
	}
	if resp.ContentLength > attachments.MaxBytes {
		resp.Body.Close()
		return nil, diagnostics.UserError("附件超过20 MiB上限，未下载或提交模型")
	}
	var body io.ReadCloser = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") && resp.Header.Get("Content-Disposition") == "" {
		// 合法JSON文件也会使用application/json，不仅凭MIME把它当错误。
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, attachments.MaxBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(data)) > attachments.MaxBytes {
			return nil, diagnostics.UserError("附件超过20 MiB上限，未保存")
		}
		var failure struct {
			Code  *int   `json:"code"`
			Msg   string `json:"msg"`
			Error struct {
				LogID string `json:"log_id"`
			} `json:"error"`
		}
		// 只有结构化错误中的log_id与本次响应请求标识一致才判为接口错误，
		// 用户JSON中的普通code/msg字段不冒充平台错误。
		logID := resp.Header.Get("X-Tt-Logid")
		if json.Unmarshal(data, &failure) == nil && failure.Code != nil && *failure.Code != 0 && failure.Msg != "" &&
			logID != "" && failure.Error.LogID == logID {
			return nil, diagnostics.UserError(fmt.Sprintf("飞书附件接口返回资源错误 code=%d，未把错误体保存成用户文件", *failure.Code))
		}
		if a.Kind == "image" {
			return nil, diagnostics.UserError("图片资源接口返回JSON而非图片数据，请核对资源类型和权限")
		}
		body = io.NopCloser(bytes.NewReader(data))
	}
	complete = true
	return &attachmentDownloadBody{ReadCloser: body, cancel: cancel}, nil
}
func (c *Client) SendAttachment(ctx context.Context, address im.Address, name, kind string, reader io.Reader, key string) (string, error) {
	return c.sendAttachment(ctx, address, name, kind, reader, key, nil)
}

func (c *Client) SendAttachmentGuarded(ctx context.Context, address im.Address, name, kind string, reader io.Reader, key string, authorize func() error) (string, error) {
	if authorize == nil {
		return "", errors.New("缺少文件发送前的授权复核")
	}
	if err := authorize(); err != nil {
		return "", err
	}
	return c.sendAttachment(ctx, address, name, kind, reader, key, authorize)
}

func (c *Client) sendAttachment(ctx context.Context, address im.Address, name, kind string, reader io.Reader, key string, authorize func() error) (string, error) {
	if e := attachments.SafeName(name); e != nil {
		return "", e
	}
	if kind != "image" && kind != "file" {
		return "", errors.New("附件种类无效")
	}
	data, e := io.ReadAll(io.LimitReader(reader, attachments.MaxBytes+1))
	if e != nil {
		return "", e
	}
	if len(data) == 0 || int64(len(data)) > attachments.MaxBytes {
		return "", errors.New("附件为空或超过 20 MiB")
	}
	if kind == "image" {
		mime := http.DetectContentType(data)
		if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" {
			return "", errors.New("图片内容必须为 PNG/JPEG/GIF；其他类型请按文件发送")
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	endpoint, field, resultKey := "/im/v1/files", "file", "file_key"
	if kind == "image" {
		endpoint, field, resultKey = "/im/v1/images", "image", "image_key"
		_ = form.WriteField("image_type", "message")
	} else {
		_ = form.WriteField("file_type", "stream")
		_ = form.WriteField("file_name", name)
	}
	part, e := form.CreateFormFile(field, name)
	if e != nil {
		return "", e
	}
	if _, e = part.Write(data); e != nil {
		return "", e
	}
	if e = form.Close(); e != nil {
		return "", e
	}
	token, e := c.accessToken(ctx)
	if e != nil {
		return "", e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", apiBase+endpoint, &body)
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, e := c.http.Do(req)
	if e != nil {
		return "", errors.New("飞书附件上传失败，未发送聊天消息")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("附件上传失败 HTTP %d", resp.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return "", e
	}
	var result struct {
		Code int               `json:"code"`
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Code != 0 || result.Data[resultKey] == "" {
		return "", errors.New("附件上传未返回有效资源 ID，未发送聊天消息")
	}
	if authorize != nil {
		if e := authorize(); e != nil {
			return "", e
		}
		if e := ctx.Err(); e != nil {
			return "", e
		}
	}
	return c.send(ctx, address, kind, marshal(map[string]string{resultKey: result.Data[resultKey]}), key)
}
