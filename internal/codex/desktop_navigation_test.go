package codex

import (
	"strings"
	"testing"
)

func TestDesktopTimeoutExplainsObservedReason(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{{"waiting_draft", "附件说明"}, {"waiting_composer", "可编辑"}, {"navigation_pending", "目标会话"}, {"", "未取得足够信息"}} {
		if err := desktopNavigationTimeout(tc.reason); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.reason, err)
		}
	}
}
