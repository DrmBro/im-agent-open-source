package bridge

import (
	"context"
	"encoding/json"
	"time"

	"im-agent/internal/store"
	"im-agent/internal/telemetry"
)

func (bridge *Bridge) syncRequestTelemetry(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	ready := false
	var lastWarning time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var collectErr error
		if !ready {
			collectErr = bridge.store.InitRequestTelemetry(readCtx)
			ready = collectErr == nil
		}
		var candidates []store.TelemetryCandidate
		if ready {
			candidates, collectErr = bridge.store.RequestTelemetryCandidates(readCtx)
		}
		cancel()
		if collectErr != nil && time.Since(lastWarning) >= time.Minute {
			bridge.log.Warn("请求运行元数据暂不可用，稍后重试；正常执行和投递不受此采集器控制")
			lastWarning = time.Now()
		}
		for _, candidate := range candidates {
			if ctx.Err() != nil {
				return
			}
			bridge.collectRequestTelemetry(ctx, candidate)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (bridge *Bridge) collectRequestTelemetry(parent context.Context, candidate store.TelemetryCandidate) {
	session, err := bridge.store.ByThread(candidate.ThreadID)
	if err != nil || session.Status != "active" || store.Hash(session.Address.Key()) != candidate.AddressKey {
		return
	}
	stateJSON, next, finished := candidate.StateJSON, candidate.Offset, candidate.Finished
	var snapshot telemetry.Snapshot
	if json.Unmarshal([]byte(candidate.DisplayJSON), &snapshot) != nil {
		snapshot = telemetry.Snapshot{Issue: "state_invalid"}
	}
	delay := 10 * time.Second
	if !finished {
		var state telemetry.State
		if len(stateJSON) > 16384 || json.Unmarshal([]byte(stateJSON), &state) != nil {
			snapshot.Issue, delay = "state_invalid", time.Minute
		} else {
			readCtx, cancel := context.WithTimeout(parent, 2*time.Second)
			updated, position, done, readErr := bridge.codex.ReadRequestTelemetry(readCtx, candidate.ThreadID, candidate.Path, candidate.Offset, candidate.UserItemID, state)
			cancel()
			if readErr != nil {
				snapshot.Issue, delay = "read_failed", time.Minute
			} else if raw, marshalErr := json.Marshal(updated); marshalErr == nil {
				stateJSON, next, finished, snapshot = string(raw), position, done, updated.Snapshot
			}
		}
	}
	writeCtx, cancel := context.WithTimeout(parent, 2*time.Second)
	saved, err := bridge.store.SaveRequestTelemetry(writeCtx, candidate, stateJSON, snapshot, next, finished, delay)
	cancel()
	if err != nil || !saved {
		return
	}
	report, err := bridge.store.RequestTelemetry(candidate.EventID, candidate.ThreadID, candidate.AddressKey)
	if err != nil || report.Revision <= candidate.PresentedRevision {
		return
	}
	card, err := bridge.store.StatusCard(candidate.ThreadID, candidate.EventID)
	if err != nil || card.State != "sent" || card.MessageID == "" || card.AddressKey != candidate.AddressKey || card.NextAttempt > time.Now().Unix() {
		return
	}
	_ = bridge.enqueueSessionStatus(candidate.ThreadID, false, candidate.EventID)
}

func (bridge *Bridge) observeStatusCard(view store.SessionStatus) error {
	if err := bridge.store.StatusCardObserved(view.Session.ThreadID, view.Task.ID, view.RequestVersion()); err != nil {
		return err
	}
	if view.Telemetry != nil {
		_ = bridge.store.AcknowledgeRequestTelemetry(view.Task.ID, view.Session.ThreadID,
			store.Hash(view.Session.Address.Key()), view.Telemetry.Revision)
	}
	return nil
}
