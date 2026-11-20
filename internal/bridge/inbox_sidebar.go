package bridge

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (b *Bridge) syncInboxSidebar(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		thread, _, title, err := b.store.NextInboxPlacement(ctx)
		if err == nil {
			op, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = b.engineForThread(thread).PlaceInboxThread(op, thread, title)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				b.log.Warn("收件会话侧边栏归组待重试", "thread", thread)
			}
			err = b.store.FinishInboxPlacement(ctx, thread, err == nil)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			b.log.Warn("侧边栏归组记录暂不可用")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
