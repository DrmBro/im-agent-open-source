package attachments

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var inlineMarkdownImage = regexp.MustCompile(`!\[([^\]\n]*)\]\((<[^>\n]+>|[^()\n]+)\)`)

type MarkdownImage struct {
	Start, End  int
	Alt, Source string
	Remote      bool
}

// 复用图文识别，图片目标与普通文件链接分别处理。
func MarkdownImages(text string) ([]MarkdownImage, error) {
	out := []MarkdownImage{}
	for _, m := range inlineMarkdownImage.FindAllStringSubmatchIndex(text, -1) {
		target := strings.TrimSpace(text[m[4]:m[5]])
		if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
			target = target[1 : len(target)-1]
		}
		u, err := url.Parse(target)
		if err != nil {
			return nil, errors.New("图片链接无效")
		}
		remote := u.Scheme == "https" || u.Scheme == "http"
		if u.Scheme != "" && !remote {
			return nil, errors.New("图片只支持本地文件或 HTTP(S) 地址")
		}
		if !remote {
			target, err = url.PathUnescape(target)
			if err != nil {
				return nil, errors.New("本地图片路径无效")
			}
		}
		out = append(out, MarkdownImage{Start: m[0], End: m[1], Alt: text[m[2]:m[3]], Source: target, Remote: remote})
	}
	if len(out) != strings.Count(text, "![") {
		return nil, errors.New("图片链接格式暂不支持，请使用 ![说明](图片路径) 内联格式")
	}
	return out, nil
}
func ReplaceMarkdownImages(text string, images []MarkdownImage, replacements []string) string {
	for i := len(images) - 1; i >= 0; i-- {
		text = text[:images[i].Start] + replacements[i] + text[images[i].End:]
	}
	return text
}

var plainMarkdownLink = regexp.MustCompile(`\[([^\]]*)\]\(([^\n)]*)\)`)

// 本地文件不上传；保留公开链接，图片由调用方单独处理。
func WithoutLocalFileLinks(text string) string {
	return plainMarkdownLink.ReplaceAllStringFunc(text, func(link string) string {
		m := plainMarkdownLink.FindStringSubmatch(link)
		if strings.HasPrefix(m[2], "https://") || strings.HasPrefix(m[2], "http://") {
			return link
		}
		return m[1] + "（文件未上传）"
	})
}
