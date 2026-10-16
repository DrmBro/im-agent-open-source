package bridge

import (
	"context"
	"strconv"
	"time"

	"im-agent/internal/store"
)

// 保留动作属于后台，不由翻页/查看/客户端启动触发；仅清本地内容，不调用IM或Codex。
func (b *Bridge) syncHistoryRetention(ctx context.Context) {
	const lastKey = "history-retention-last-success"
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		raw, _ := b.store.GetMeta(lastKey)
		last, _ := strconv.ParseInt(raw, 10, 64)
		if last <= 0 || now.Unix()-last >= int64((24*time.Hour)/time.Second) {
			pruned, finished := 0, false
			// 单轮有总预算；不足下次小时轮询继续，只有完全结束才写每日成功时间。
			for batch := 0; batch < 60 && ctx.Err() == nil; batch++ {
				result, err := b.store.PruneHistory(ctx, now)
				if err != nil {
					b.log.Warn("本地历史清理未完成，保留记录并等待后续重试")
					break
				}
				summary, err := b.store.PrunePresentationHistory(ctx, now)
				if err != nil {
					b.log.Warn("本地摘要清理未完成，保留记录并等待后续重试")
					break
				}
				retired, err := b.store.PruneInboundPayloads(ctx, now)
				if err != nil {
					b.log.Warn("已结束请求的收件正文清理未完成，等待后续重试")
					break
				}
				attachmentDisplays, err := b.store.PruneAttachmentDisplay(ctx, now)
				if err != nil {
					b.log.Warn("附件展示内容保留期清理未完成，后续重试")
					break
				}
				pruned += result.Pruned + summary.Pruned + int(retired) + int(attachmentDisplays)
				if !result.More && !summary.More && retired < 100 && attachmentDisplays < 100 {
					finished = true
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			if finished && ctx.Err() == nil {
				if err := b.store.SetMeta(lastKey, strconv.FormatInt(now.Unix(), 10)); err != nil {
					b.log.Warn("历史清理完成时间未保存，后续会再核对；不会重跑模型")
				} else if pruned > 0 {
					b.log.Info("本地历史内容已按保留策略精简", "days", store.HistoryRetentionDays, "records", pruned)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
