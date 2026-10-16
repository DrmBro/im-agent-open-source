package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

// 仅查询已绑定托管会话，绝不自动扫描用户其它群或回放启用前历史。
func (b *Bridge) syncInboundHistory(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var recovered int
	for {
		if ctx.Err() != nil {
			return
		}
		sessions, err := b.store.LiveMirrorSessions()
		failed, checked := 0, 0
		if err != nil {
			failed++
			b.log.Warn("读取历史收件对账会话失败")
		}
		for _, s := range sessions {
			if ctx.Err() != nil {
				return
			}
			adapter, ok := b.channels[s.Address.Connection].(im.InboundHistoryAdapter)
			if !ok {
				continue
			}
			op, cancel := context.WithTimeout(ctx, 25*time.Second)
			count, e := b.reconcileInboundHistory(op, s, adapter)
			cancel()
			recovered += count
			if e != nil {
				failed++
				b.log.Warn("群收件历史对账失败，保留游标", "error", diagnostics.Error(e))
			} else {
				checked++
			}
		}
		b.diagMu.Lock()
		b.diag.IntakeRecovered = recovered
		b.diag.IntakeAt = time.Now().Unix()
		if failed > 0 {
			b.diag.IntakePhase = "degraded"
			b.diag.Intake = fmt.Sprintf("%d 个群已核对，%d 个群对账失败；长连接正常不代表收件无遗漏，后续继续核对", checked, failed)
		} else {
			b.diag.IntakePhase = "running"
			b.diag.Intake = fmt.Sprintf("%d 个群历史收件对账运行中；本次运行补收 %d 条，按原消息ID避免重复执行", checked, recovered)
		}
		b.diagMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) reconcileInboundHistory(ctx context.Context, s store.Session, adapter im.InboundHistoryAdapter) (recovered int, err error) {
	cursor, err := b.store.InboundHistoryCursor(s)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = b.store.RecordInboundHistoryFailure(cursor, diagnostics.Error(err))
		}
	}()
	allowed, err := b.store.AuthorizedInboundBackfills(cursor)
	if err != nil {
		return 0, err
	}
	approved := map[string]int64{}
	for _, x := range allowed {
		approved[x.EventID] = x.CreatedMS
	}
	if cursor.ToMS == 0 {
		next := cursor
		next.FromMS = cursor.ThroughMS - 120000
		if next.FromMS < cursor.BaselineMS {
			next.FromMS = cursor.BaselineMS
		}
		for _, x := range allowed {
			if x.CreatedMS < next.FromMS {
				next.FromMS = x.CreatedMS
			}
		}
		next.FromMS = next.FromMS / 1000 * 1000
		next.ToMS = (time.Now().Unix() - 5) * 1000 // 留出事件写入平台历史的短时延。
		if next.ToMS <= next.FromMS {
			return 0, nil
		}
		if err = b.store.SaveInboundHistoryWindow(cursor, next); err != nil {
			return 0, err
		}
		cursor = next
	}
	page, err := adapter.ReadInboundHistory(ctx, s.Address, cursor.FromMS, cursor.ToMS, cursor.Token)
	if err != nil {
		return 0, err
	}
	for _, record := range page.Records {
		if ctx.Err() != nil {
			return recovered, ctx.Err()
		}
		// 指定群、话题、时间与批准清单必须同时相符。
		if record.Address.Key() != s.Address.Key() {
			continue
		}
		if record.CreatedMS < cursor.BaselineMS && approved[record.ID] != record.CreatedMS {
			continue
		}
		current, e := b.store.ByThread(s.ThreadID)
		if e != nil {
			return recovered, e
		}
		if current.Status != "active" || current.Address.Key() != s.Address.Key() {
			return recovered, errors.New("历史收件过程中会话绑定改变，未跨群提交")
		}
		handled, e := b.store.InboundHistoryHandled(s.ThreadID, record.ID)
		if e != nil {
			return recovered, e
		}
		if handled {
			continue
		}
		if record.Message == nil {
			if record.SkipReason == "" {
				return recovered, errors.New("历史消息缺少明确解析状态")
			}
			if e = b.store.RecordInboundHistoryReceipt(cursor, record.ID, record.SkipReason, record.CreatedMS); e != nil {
				return recovered, e
			}
			if record.SkipReason == "unsupported_content" || record.SkipReason == "empty_or_invalid" {
				b.log.Warn("历史收件存在不可执行消息，已留痕", "category", record.SkipReason)
			}
			continue
		}
		e = b.receiveMessage(ctx, *record.Message)
		// 业务处理可能明确拒绝；只要原事件已经落盘，就不再次触发模型执行。
		handled, checkErr := b.store.InboundHistoryHandled(s.ThreadID, record.ID)
		if checkErr != nil {
			return recovered, checkErr
		}
		if !handled {
			if e != nil {
				return recovered, e
			}
			// 接收入口的明确拒绝（超长输入/旧迁移入口）不得堵住其它消息。
			if e = b.store.RecordInboundHistoryReceipt(cursor, record.ID, "rejected_before_claim", record.CreatedMS); e != nil {
				return recovered, e
			}
			b.log.Warn("历史补收消息被入口保护拒绝，未执行模型")
			continue
		}
		if e = b.store.RecordInboundHistoryReceipt(cursor, record.ID, "received", record.CreatedMS); e != nil {
			return recovered, e
		}
		recovered++
	}
	next := cursor
	if page.HasMore {
		next.Token = page.NextToken
	} else {
		if next.ToMS > next.ThroughMS {
			next.ThroughMS = next.ToMS
		}
		next.FromMS, next.ToMS, next.Token = 0, 0, ""
	}
	if err = b.store.SaveInboundHistoryWindow(cursor, next); err != nil {
		return recovered, err
	}
	return recovered, nil
}
