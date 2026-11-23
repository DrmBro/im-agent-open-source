package dwspersonal

import (
	"context"
	"im-agent/internal/attachments"
	"im-agent/internal/im"
)

// 普通入口不隐式上传；图片上传仅由已核验的图文投递入口调用。
func (c *Client) SendMarkdown(ctx context.Context, a im.Address, text, key string) (string, error) {
	text, err := cleanPersonalMarkdown(text)
	if err != nil {
		return "", err
	}
	return c.sendPersonal(ctx, a, text, key, "--markdown")
}

func cleanPersonalMarkdown(text string) (string, error) {
	images, err := attachments.MarkdownImages(text)
	if err != nil {
		return "", err
	}
	replacements := make([]string, len(images))
	for i, image := range images {
		replacements[i] = image.Alt + "（图片未上传）"
	}
	text = attachments.ReplaceMarkdownImages(text, images, replacements)
	return attachments.WithoutLocalFileLinks(text), nil
}
func (c *Client) SendImageMarkdown(ctx context.Context, a im.Address, text, key, dir string) (string, error) {
	return c.sendPersonal(ctx, a, text, key, "--markdown", dir)
}
