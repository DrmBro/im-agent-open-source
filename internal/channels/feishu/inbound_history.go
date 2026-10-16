package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"im-agent/internal/config"
	"im-agent/internal/im"
)

// 历史与长连接共用内容转换；仅移除平台产生的提及占位符。
func decodeInbound(a im.Address, id, user, kind, content string, mentionKeys []string, direct bool) (im.Message, string) {
	text, files, err := decodeAttachmentMessage(kind, content)
	if err != nil {
		return im.Message{}, "unsupported_content"
	}
	for _, key := range mentionKeys {
		if key != "" {
			text = strings.ReplaceAll(text, key, "")
		}
	}
	text = strings.TrimSpace(text)
	if a.ChatID == "" || id == "" || user == "" || (text == "" && len(files) == 0) {
		return im.Message{}, "empty_or_invalid"
	}
	return im.Message{Address: a, ID: id, UserID: user, Text: text, Attachments: files, IsDirect: direct}, ""
}

func (c *Client) ReadInboundHistory(ctx context.Context, target im.Address, fromMS, toMS int64, token string) (im.InboundHistoryPage, error) {
	var out im.InboundHistoryPage
	if target.Channel != "feishu" || target.Connection != c.cfg.ID || target.ChatID == "" || fromMS < 0 || toMS <= fromMS {
		return out, errors.New("历史收件对账目标或时间窗无效")
	}
	q := url.Values{"container_id_type": {"chat"}, "container_id": {target.ChatID},
		"start_time": {strconv.FormatInt(fromMS/1000, 10)}, "end_time": {strconv.FormatInt(toMS/1000, 10)},
		"sort_type": {"ByCreateTimeAsc"}, "page_size": {"50"}}
	if token != "" {
		q.Set("page_token", token)
	}
	raw, err := c.request(ctx, "GET", "/im/v1/messages?"+q.Encode(), nil)
	if err != nil {
		return out, err
	}
	var data struct {
		Items []*larkim.Message `json:"items"`
		More  *bool             `json:"has_more"`
		Token *string           `json:"page_token"`
	}
	if err = json.Unmarshal(raw, &data); err != nil || data.More == nil {
		return out, errors.New("历史消息分页响应不完整，未推进收件游标")
	}
	out.HasMore, out.NextToken = *data.More, str(data.Token)
	if out.HasMore && (out.NextToken == "" || out.NextToken == token) {
		return out, errors.New("历史消息分页标记无进展，未推进收件游标")
	}
	for _, item := range data.Items {
		if item == nil || item.Sender == nil || str(item.Sender.SenderType) != "user" {
			continue // 机器人输出不能作为新模型输入。
		}
		ms, parseErr := strconv.ParseInt(str(item.CreateTime), 10, 64)
		if parseErr != nil || ms <= 0 || str(item.MessageId) == "" {
			return out, errors.New("历史用户消息身份或时间无效，保留分页位置")
		}
		if ms < fromMS || ms >= toMS {
			continue // API 使用秒边界；本地再次按固定窗口核验。
		}
		a := c.Identity()
		a.ChatID = str(item.ChatId)
		if a.ChatID == "" {
			a.ChatID = target.ChatID // 来源是指定群的官方历史查询。
		}
		if a.ChatID != target.ChatID {
			return out, errors.New("历史响应群身份与查询目标冲突")
		}
		a.TopicID = str(item.ThreadId)
		if a.IsTopic() {
			a.RootMessageID = str(item.RootId)
			if a.RootMessageID == "" {
				a.RootMessageID = str(item.MessageId)
			}
		}
		record := im.InboundHistoryRecord{ID: str(item.MessageId), CreatedMS: ms, Address: a}
		switch {
		case item.Deleted != nil && *item.Deleted:
			record.SkipReason = "deleted"
		case str(item.Sender.IdType) != "open_id":
			return out, errors.New("历史用户身份不是本应用 open_id，未猜测白名单")
		case !config.Contains(c.cfg.AllowedUsers, str(item.Sender.Id)):
			record.SkipReason = "not_allowed"
		default:
			content := ""
			if item.Body != nil {
				content = str(item.Body.Content)
			}
			var mentions []string
			for _, m := range item.Mentions {
				if m != nil {
					mentions = append(mentions, str(m.Key))
				}
			}
			message, reason := decodeInbound(a, record.ID, str(item.Sender.Id), str(item.MsgType), content, mentions, false)
			record.SkipReason = reason
			if reason == "" {
				record.Message = &message
			}
		}
		out.Records = append(out.Records, record)
	}
	sort.SliceStable(out.Records, func(i, j int) bool { return out.Records[i].CreatedMS < out.Records[j].CreatedMS })
	return out, nil
}
