package bridge

import (
	"context"
	"im-agent/internal/attachments"
)

// 先在本机制作图片快照，网络发送只读取这批副本，不跟随随后变化的源文件。
func (b *Bridge) prepareForwardImages(ctx context.Context, thread, original, text string) (body, dir, proof string, count int, cleanup func(), err error) {
	cleanup = func() {}
	images, e := attachments.MarkdownImages(text)
	if e != nil {
		err = e
		return
	}
	if len(images) > 0 {
		repl := make([]string, len(images))
		for i, img := range images {
			repl[i] = img.Alt + "（图片未上传）"
		}
		body = attachments.ReplaceMarkdownImages(text, images, repl)
		body = attachments.WithoutLocalFileLinks(body)
		return
	}
	body = attachments.WithoutLocalFileLinks(text)
	return
}
