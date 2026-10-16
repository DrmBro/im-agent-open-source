package codex

import (
	"bytes"
	"encoding/json"
)

// task_complete 也可能带传输错误，不能只因事件名称就认定业务成功。
func hasCompletionError(raw json.RawMessage) bool {
	value := bytes.TrimSpace(raw)
	return len(value) > 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`))
}
