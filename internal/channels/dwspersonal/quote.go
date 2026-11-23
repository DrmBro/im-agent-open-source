package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"im-agent/internal/im"
)

// 只有平台在匹配的消息记录上明确标记撤回，才允许省略引用。
func (c *Client) CheckGroupQuote(ctx context.Context, address im.Address, mid string) (im.GroupQuote, error) {
	var result im.GroupQuote
	wire, err := address.DeliveryAddress()
	if err != nil {
		return result, err
	}
	group, ok := strings.CutPrefix(wire.ChatID, c.accountPrefix()+"group:")
	if !ok || !validID(group) || !validID(mid) || wire.Channel != channel || wire.Connection != c.cfg.ID || wire.IsTopic() {
		return result, errors.New("缺少可核对的钉钉原群或消息 ID")
	}
	result = im.GroupQuote{Profile: c.profile, Group: group, MessageID: mid}
	if c.recallLedger != nil {
		found, err := c.recallLedger.HasRecall(ctx, wire, mid)
		if err != nil {
			return result, err
		}
		if found {
			result.Recalled = true
			return result, nil
		}
	}
	raw, err := c.run(ctx, "", "chat", "message", "list-by-ids", "--msg-ids", mid)
	if err != nil {
		return result, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return result, err
	}
	var page struct {
		Success bool
		Result  struct {
			Messages []struct {
				MessageID, OpenMessageID, ConversationID, OpenConversationID string
				IsRecalled, Recalled                                         bool
			}
		}
	}
	if json.Unmarshal(raw, &page) != nil || !page.Success || len(page.Result.Messages) != 1 {
		return result, errors.New("钉钉未返回可核对的消息，无法确认是否撤回，尚未发送")
	}
	m := page.Result.Messages[0]
	if (m.MessageID != mid && m.OpenMessageID != mid) || (m.OpenConversationID != group && m.ConversationID != group) || (m.OpenConversationID != "" && m.OpenConversationID != group) {
		return result, errors.New("钉钉返回的原消息或群不匹配，尚未发送")
	}
	return im.GroupQuote{Profile: c.profile, Group: group, MessageID: mid, Recalled: m.IsRecalled || m.Recalled}, nil
}

func (c *Client) SendGroupReply(ctx context.Context, a im.Address, text, key, mid string) (string, error) {
	quote, err := c.CheckGroupQuote(ctx, a, mid)
	if err != nil {
		return "", err
	}
	text, err = cleanPersonalMarkdown(text)
	if err != nil {
		return "", err
	}
	return c.sendPersonalBody(ctx, a, text, key, "--text", &quote)
}

const recallEvent = "user_im_message_recall_group"

func (c *Client) SetRecallLedger(ledger im.RecallLedger) { c.recallLedger = ledger }

func (c *Client) watchQuoteRecalls(ctx context.Context) {
	// 复用同一 profile 的 DWS 消费器，只记录撤回消息 ID，不订阅群正文。
	workers := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	defer func() {
		for _, stop := range workers {
			stop()
		}
		wg.Wait()
	}()
	for ctx.Err() == nil {
		groups, err := c.recallLedger.RecentQuoteGroups(ctx, c.cfg.ID, c.profile)
		if err == nil {
			wanted := map[string]bool{}
			for _, g := range groups {
				if !validID(g) {
					continue
				}
				wanted[g] = true
				if workers[g] != nil {
					continue
				}
				run, stop := context.WithCancel(ctx)
				workers[g] = stop
				wg.Add(1)
				go func(group string) {
					defer wg.Done()
					for run.Err() == nil {
						_ = c.listen(run, subscription{event: recallEvent, group: group}, 0, nil)
						if !pause(run, time.Minute) {
							return
						}
					}
				}(g)
			}
			for group, stop := range workers {
				if !wanted[group] {
					stop()
					delete(workers, group)
				}
			}
		}
		if !pause(ctx, 30*time.Second) {
			return
		}
	}
}
