package bridge

import (
	"context"
	"time"

	"im-agent/internal/store"
)

func (bridge *Bridge) syncRequestOrigins(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		candidates, err := bridge.store.PendingRequestOrigins(queryCtx)
		cancel()
		if err == nil {
			for _, candidate := range candidates {
				if ctx.Err() != nil {
					return
				}
				bridge.observeRequestOrigin(ctx, candidate)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (bridge *Bridge) observeRequestOrigin(parent context.Context, candidate store.OriginCandidate) {
	session, err := bridge.store.ByThread(candidate.ThreadID)
	if err != nil || session.Status != "active" || store.Hash(session.Address.Key()) != candidate.AddressKey {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	messages, next, err := bridge.codex.ReadLiveMessages(ctx, candidate.ThreadID, candidate.Path, candidate.Offset)
	failed := err != nil
	if failed {
		next = candidate.Offset
	} else {
		for _, message := range messages {
			if message.Issue != nil || message.Role != "user" {
				continue
			}
			fingerprint := store.Hash(message.Text)
			if fingerprint != candidate.OriginalHash && fingerprint != candidate.RenderedHash {
				continue
			}
			entry := store.MirrorEntry{ItemID: message.ID, Role: "user", Offset: message.Offset, Fingerprint: fingerprint}
			_, err = bridge.store.MatchRequestOrigin(ctx, session, candidate, entry)
			failed = err != nil
			if failed {
				next = candidate.Offset
			}
			break
		}
	}
	writeCtx, stop := context.WithTimeout(parent, time.Second)
	defer stop()
	_ = bridge.store.SaveOriginProgress(writeCtx, candidate, next, failed)
}
