package telemetry

import "reflect"

type Usage struct {
	Input       *int64 `json:"input_tokens,omitempty"`
	CachedInput *int64 `json:"cached_input_tokens,omitempty"`
	CacheWrite  *int64 `json:"cache_write_input_tokens,omitempty"`
	Output      *int64 `json:"output_tokens,omitempty"`
	Reasoning   *int64 `json:"reasoning_output_tokens,omitempty"`
	Total       *int64 `json:"total_tokens,omitempty"`
}

func (usage *Usage) Valid() bool {
	if usage == nil || usage.Input == nil || usage.Output == nil {
		return false
	}
	for _, value := range []*int64{usage.Input, usage.CachedInput, usage.CacheWrite, usage.Output, usage.Reasoning, usage.Total} {
		if value != nil && (*value < 0 || *value > 1_000_000_000_000_000) {
			return false
		}
	}
	return (usage.CachedInput == nil || *usage.CachedInput <= *usage.Input) &&
		(usage.Reasoning == nil || *usage.Reasoning <= *usage.Output)
}

func (usage *Usage) Equal(other *Usage) bool {
	return usage.Valid() && other.Valid() && reflect.DeepEqual(usage, other)
}

type Snapshot struct {
	Model                    string `json:"model,omitempty"`
	StartedAt                int64  `json:"started_at,omitempty"`
	FirstOutputAt            int64  `json:"first_output_at,omitempty"`
	LastOutputAt             int64  `json:"last_output_at,omitempty"`
	LastActivityAt           int64  `json:"last_activity_at,omitempty"`
	Activity                 string `json:"activity,omitempty"`
	EndedAt                  int64  `json:"ended_at,omitempty"`
	UsageAt                  int64  `json:"usage_at,omitempty"`
	ContextAt                int64  `json:"context_at,omitempty"`
	ContextWindow            *int64 `json:"context_window,omitempty"`
	LastCall                 *Usage `json:"last_call,omitempty"`
	TurnUsage                *Usage `json:"turn_usage,omitempty"`
	Issue                    string `json:"issue,omitempty"`
	OutputConflict           bool   `json:"output_conflict,omitempty"`
	CompletionOutputVerified bool   `json:"completion_output_verified,omitempty"`
	ObservedAt               int64  `json:"observed_at,omitempty"`
}

type State struct {
	UserItemID         string   `json:"user_item_id"`
	MatchedTurn        string   `json:"matched_turn,omitempty"`
	CurrentTurn        string   `json:"current_turn,omitempty"`
	CurrentStartedAt   int64    `json:"current_started_at,omitempty"`
	CurrentModel       string   `json:"current_model,omitempty"`
	PendingContext     bool     `json:"pending_context,omitempty"`
	Halted             bool     `json:"halted,omitempty"`
	MatchedUserCount   int      `json:"matched_user_count,omitempty"`
	FinalCount         int      `json:"final_count,omitempty"`
	FinalID            string   `json:"final_id,omitempty"`
	FinalFingerprint   string   `json:"final_fingerprint,omitempty"`
	FinalAt            int64    `json:"final_at,omitempty"`
	UnconfirmedOutputs int      `json:"unconfirmed_outputs,omitempty"`
	Snapshot           Snapshot `json:"snapshot"`
}

type Report struct {
	Snapshot Snapshot
	Revision int64
}
