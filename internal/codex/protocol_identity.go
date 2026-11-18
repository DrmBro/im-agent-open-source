package codex

// Adapted from slopus/happy codexAppServerClient.ts (MIT).
// Copyright (c) 2026 Happy Coder Contributors.
// Source revision and full license: third_party/happy/NOTICE.md and LICENSE.

// 协议代际允许 turn.id、turnId 或 turn_id。多个显式标识冲突时拒绝关联。
func protocolIdentity(values ...string) (string, bool) {
	result := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if result != "" && result != value {
			return "", false
		}
		result = value
	}
	return result, true
}

// 按实际轮次判断事件归属，不依赖可能缺失的 turn/started 通知。
func matchesProtocolTurn(expected, actual string) bool {
	return expected != "" && actual != "" && expected == actual
}
