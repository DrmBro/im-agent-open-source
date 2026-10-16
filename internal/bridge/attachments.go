package bridge

import (
	"context"
	"errors"
	"fmt"
	"im-agent/internal/attachments"
	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SendFileRequest struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
}

func (b *Bridge) receiveAttachments(ctx context.Context, adapter im.Adapter, s store.Session, msg im.Message) (string, error) {
	if len(msg.Attachments) == 0 {
		return msg.Text, nil
	}
	if len(msg.Attachments) > 5 {
		return "", diagnostics.UserError("单次任务合计最多5个附件")
	}
	files, ok := adapter.(im.FileAdapter)
	if !ok {
		return "", diagnostics.UserError("当前IM连接不支持文件资源下载")
	}
	var inputs []codex.AttachmentInputFile
	display := msg.Text
	for i, a := range msg.Attachments {
		source, sourceIndex := msg.ID, i
		if a.SourceMessageID != "" {
			source, sourceIndex = a.SourceMessageID, a.SourceIndex
		}
		if a.Kind != "image" && a.Kind != "file" {
			return "", diagnostics.UserError("附件类型不支持，未提交模型")
		}
		if e := attachments.SafeName(a.Name); e != nil {
			return "", diagnostics.UserError("附件名称包含无效字符或路径，未保存")
		}
		id := store.Hash(msg.Address.Key() + "\x00" + source + fmt.Sprint(":", sourceIndex))
		identity := store.Transfer{ID: id, ThreadID: s.ThreadID, Direction: "in", Kind: a.Kind, Name: a.Name}
		old, download, e := b.store.BeginInboundTransfer(identity)
		if e != nil {
			return "", diagnostics.UserError("附件来源账本核对失败，未重复下载或提交")
		}
		dir := filepath.Join(b.cfg.StateDir, "attachments", "inbox", store.Hash(s.ThreadID)[:16])
		path, proof, size, inspectErr := attachments.InspectIncoming(dir, id, a.Name)
		if inspectErr == nil {
			if old.Path != "" && old.Path != path || old.ContentSHA != "" && old.ContentSHA != proof || old.SizeBytes > 0 && old.SizeBytes != size {
				return "", diagnostics.UserError("本机已收到附件的路径或内容发生变化，未覆盖或提交")
			}
		} else if !os.IsNotExist(inspectErr) || !download {
			return "", diagnostics.UserError("原附件本机文件不可安全读取，未把缺失或异常文件伪装为已收到")
		} else {
			op, cancel := context.WithTimeout(ctx, 90*time.Second)
			var readErr error
			for attempt := 0; attempt < 3; attempt++ {
				stream, downloadErr := files.DownloadAttachment(op, source, a)
				readErr = downloadErr
				if readErr == nil {
					path, readErr = attachments.Save(dir, id, a.Name, stream, attachments.MaxBytes)
					if readErr != nil {
						var streamError net.Error
						switch {
						case errors.Is(readErr, attachments.ErrTooLarge):
							readErr = diagnostics.UserError("附件流实际超过20 MiB上限，未保存或提交")
						case errors.Is(readErr, attachments.ErrEmpty):
							readErr = diagnostics.UserError("飞书附件资源为空，未保存或提交")
						case errors.As(readErr, &streamError), errors.Is(readErr, io.ErrUnexpectedEOF): // 网络读取仍可有限重试。
						default:
							readErr = diagnostics.UserError("本机附件保存失败，请核对私有目录权限、可用磁盘空间或同名已有文件；未覆盖或提交")
						}
					}
					closeErr := stream.Close()
					if readErr == nil {
						readErr = closeErr
					}
				}
				if readErr == nil {
					break
				}
				// 写入已原子完成但Close发生错误时，核对同一源对象，而不是覆盖重下。
				if savedPath, sha, n, checkErr := attachments.InspectIncoming(dir, id, a.Name); checkErr == nil {
					path, proof, size = savedPath, sha, n
					readErr = nil
					break
				}
				var resource *im.AttachmentReadError
				var network net.Error
				retry := errors.As(readErr, &resource) && resource.Retryable || errors.As(readErr, &network) || errors.Is(readErr, io.ErrUnexpectedEOF)
				if !retry || attempt == 2 || op.Err() != nil {
					break
				}
				select {
				case <-op.Done():
					readErr = op.Err()
				case <-time.After(time.Duration(attempt+1) * time.Second):
				}
				if op.Err() != nil {
					break
				}
			}
			cancel()
			if readErr != nil {
				_ = b.store.FinishTransfer(id, "failed", "", "", diagnostics.Error(readErr))
				return "", diagnostics.UserError("附件读取或保存未完成：" + diagnostics.Error(readErr) + "；任务未提交，保留原资源身份以供恢复")
			}
			path, proof, size, inspectErr = attachments.InspectIncoming(dir, id, a.Name)
			if inspectErr != nil {
				return "", diagnostics.UserError("附件下载后完整性核对失败，未提交模型")
			}
		}
		if e = b.store.FinishInboundTransfer(identity, path, proof, size); e != nil {
			return "", diagnostics.UserError("附件完整性账本保存未确认，未提交模型")
		}
		inputs = append(inputs, codex.AttachmentInputFile{Kind: a.Kind, Name: a.Name, Path: path})
		display += fmt.Sprintf("\n[附件 %d：%s]", i+1, a.Name)
	}
	token, e := b.store.EnsureAttachmentContext(s.ThreadID, msg.ID, display)
	if e != nil {
		return "", diagnostics.UserError("附件上下文保存失败，未提交模型")
	}
	return codex.BuildAttachmentInput(msg.Text, codex.AttachmentInputContext{RequestID: msg.ID, Token: token, Files: inputs})
}
func (b *Bridge) SendFile(ctx context.Context, r SendFileRequest) error {
	if r.ID == "" || len(r.ID) > 128 || r.ThreadID == "" || (r.Kind != "image" && r.Kind != "file") {
		return diagnostics.UserError("文件发送参数无效")
	}
	lock := b.liveLock(r.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	s, e := b.store.ByThread(r.ThreadID)
	if e != nil {
		return e
	}
	if s.Status != "active" {
		return diagnostics.UserError("归档会话不可发送文件")
	}
	adapter, ok := b.channels[s.Address.Connection].(im.FileAdapter)
	if !ok {
		return diagnostics.UserError("该连接不支持发送附件")
	}
	file, name, e := attachments.OpenExport(s.WorkDir, r.Path, attachments.MaxBytes)
	if e != nil {
		return diagnostics.UserError(e.Error())
	}
	defer file.Close()
	// 同一用户动作 ID 只允许发一次；未知结果不自动回退或重发。
	id := "export:" + r.ID
	claimed, e := b.store.BeginTransfer(store.Transfer{ID: id, ThreadID: s.ThreadID, Direction: "out", Kind: r.Kind, Name: name})
	if e != nil {
		return e
	}
	if !claimed {
		return diagnostics.UserError("这次文件发送已经处理过，请核对附件记录，未重复发送")
	}
	platform, e := adapter.SendAttachment(ctx, s.Address, name, r.Kind, file, store.Hash(id + s.Address.Key())[:32])
	if e != nil || platform == "" {
		var uncertain *im.DeliveryUncertain
		state := "failed"
		if errors.As(e, &uncertain) || e == nil || errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			state = "unknown"
		}
		_ = b.store.FinishTransfer(id, state, "", "", diagnostics.Error(e))
		if state == "unknown" {
			return diagnostics.UserError("文件送达结果未确认，请先查看群内消息，不要直接重发")
		}
		return diagnostics.UserError("附件上传或发送被拒绝，请查看附件记录")
	}
	return b.store.FinishTransfer(id, "sent", "", platform, "")
}
func (b *Bridge) fileCommand(ctx context.Context, msg im.Message) error {
	s, e := b.store.ByChat(msg.Address)
	if e != nil {
		return e
	}
	if !b.allowed(s.Address.Connection, msg.UserID) {
		return errors.New("无权发送文件")
	}
	path := strings.TrimSpace(strings.TrimPrefix(msg.Text, "/sendfile"))
	if path == "" {
		return diagnostics.UserError("用法：/sendfile 会话工作目录内的相对文件路径")
	}
	return b.SendFile(ctx, SendFileRequest{ID: msg.ID, ThreadID: s.ThreadID, Path: path, Kind: "file"})
}
