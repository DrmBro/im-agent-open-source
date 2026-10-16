package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrTaskDeliveryUnavailable 表示证据不足或无法安全读取；调用方可忽略交付增强。
// 不透传数据库、路径或正文错误，避免普通日志意外记录敏感内容。
var ErrTaskDeliveryPruned = errors.New("该任务历史已按7天保留策略清理，无法再生成完整报告")

var ErrTaskDeliveryUnavailable = errors.New("任务报告暂不可用：完整回复证据不足或无法安全读取")

const (
	taskDeliveryMaxReplies = 512
	taskDeliveryMaxParts   = 512
	taskDeliveryMaxBody    = 1 << 20
	taskDeliveryMaxReport  = 20 << 20
	taskDeliverySummary    = 240
	taskDeliveryVerify     = "未采集可独立核验的编译/测试证据；执行完成不等于测试通过"
)

// TaskDelivery 仅描述本次封存回复；Delivery、DeliveredAt 均指原回复，非报告附件。
type TaskDelivery struct {
	EventID      string `json:"event_id"`
	ThreadID     string `json:"thread_id"`
	Summary      string `json:"summary"`
	Verification string `json:"verification"`
	ReportName   string `json:"report_name"`
	Report       string `json:"report"`
	Execution    string `json:"execution"`
	Delivery     string `json:"delivery"`
	CompletedAt  int64  `json:"completed_at"`
	DeliveredAt  int64  `json:"delivered_at"`
}

type taskDeliveryPart struct {
	body, delivery string
}

type taskDeliveryReply struct {
	item          string
	expectedParts int
	parts         []taskDeliveryPart
}

// TaskDeliverySnapshot 仅读取既有普通数据库文件，不初始化目录、不迁移或修改业务数据。
func TaskDeliverySnapshot(ctx context.Context, dir, thread, event string) (TaskDelivery, error) {
	if ctx == nil || dir == "" || thread == "" || event == "" {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	path, err := filepath.Abs(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	// URI 参数对每个连接生效；mode=ro 保证缺失文件不会被创建。
	params := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)"}}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: params.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	result, err := (&Store{db: db}).ReadTaskDelivery(ctx, thread, event)
	if err != nil {
		return TaskDelivery{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	return result, nil
}

// ReadTaskDelivery 在同一事务快照中核对地址、封存清单和每个分片，不读取工作目录。
func (s *Store) ReadTaskDelivery(ctx context.Context, thread, event string) (TaskDelivery, error) {
	if s == nil || s.db == nil || ctx == nil || thread == "" || event == "" {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	defer tx.Rollback()

	var result TaskDelivery
	var session Session
	var chatKey, addressKey, recordedDelivery string
	var sealed, expectedReplies int
	err = tx.QueryRowContext(ctx, `SELECT r.event_id, r.thread_id, r.address_key, e.chat_key,
       r.execution_state, r.sealed, r.expected_replies, r.delivery_state,
       r.completed_at, r.delivered_at, s.thread_id, s.status,
       s.channel, s.connection_id, s.chat_id, s.topic_id
FROM request_results r
         JOIN events e ON e.id = r.event_id
         JOIN sessions s ON s.thread_id = r.thread_id
WHERE r.event_id = ?
  AND r.thread_id = ?`, event, thread).Scan(
		&result.EventID, &result.ThreadID, &addressKey, &chatKey,
		&result.Execution, &sealed, &expectedReplies, &recordedDelivery,
		&result.CompletedAt, &result.DeliveredAt, &session.ThreadID, &session.Status,
		&session.Address.Channel, &session.Address.Connection, &session.Address.ChatID, &session.Address.TopicID)
	// 旧 schema 缺表/字段、缺记录和其它数据库异常统一安全降级。
	if err != nil || result.EventID != event || result.ThreadID != thread || session.ThreadID != thread ||
		session.Status != "active" || chatKey != addressKey || addressKey != Hash(session.Address.Key()) ||
		result.Execution != "completed" || sealed != 1 || expectedReplies <= 0 || expectedReplies > taskDeliveryMaxReplies {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}

	var hasPruned int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='history_content_pruned'").Scan(&hasPruned); err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	if hasPruned > 0 {
		var pruned int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM history_content_pruned h
WHERE (h.kind = 'event' AND h.key1 = ? AND h.key2 = '')
   OR (h.kind = 'reply' AND h.key1 = ?
       AND EXISTS (SELECT 1 FROM request_result_replies rr
                   WHERE rr.event_id = ? AND rr.thread_id = h.key1 AND rr.item_id = h.key2))`, event, thread, event).Scan(&pruned); err != nil {
			return TaskDelivery{}, ErrTaskDeliveryUnavailable
		}
		if pruned > 0 {
			return TaskDelivery{}, ErrTaskDeliveryPruned
		}
	}

	replies, err := taskDeliveryReplies(ctx, tx, thread, event, expectedReplies)
	if err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	bodyBytes, totalParts := 0, 0
	allSent, uncertain := true, false
	for i := range replies {
		reply := &replies[i]
		if reply.expectedParts > taskDeliveryMaxParts-totalParts {
			return TaskDelivery{}, ErrTaskDeliveryUnavailable
		}
		reply.parts, err = taskDeliveryParts(ctx, tx, thread, reply.item, reply.expectedParts, &bodyBytes)
		if err != nil {
			return TaskDelivery{}, ErrTaskDeliveryUnavailable
		}
		totalParts += len(reply.parts)
		for _, part := range reply.parts {
			if part.delivery != "sent" {
				allSent = false
			}
			if part.delivery == "unknown" {
				uncertain = true
			}
		}
	}
	// 封存只证明执行完成。请求总账与全部分片均确认送达，才能展示原回复已送达。
	result.Delivery = "unknown"
	switch recordedDelivery {
	case "sent":
		if allSent && result.DeliveredAt > 0 {
			result.Delivery = "sent"
		}
	case "pending":
		if !uncertain {
			result.Delivery = "pending"
		}
	}
	if result.Delivery != "sent" {
		result.DeliveredAt = 0
	}
	if err = tx.Commit(); err != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}

	result.Verification = taskDeliveryVerify
	result.Summary = taskDeliveryExcerpt(replies)
	result.ReportName = "task-report-" + Hash(event + "\x00" + thread)[:12] + ".md"
	result.Report = taskDeliveryMarkdown(result, replies)
	if len(result.Report) >= taskDeliveryMaxReport || ctx.Err() != nil {
		return TaskDelivery{}, ErrTaskDeliveryUnavailable
	}
	return result, nil
}

// 先完整关闭列表游标，再查询各条回复的分片，兼容只有一个连接的 Store。
func taskDeliveryReplies(ctx context.Context, tx *sql.Tx, thread, event string, expected int) ([]taskDeliveryReply, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.event_id, r.thread_id, r.item_id, r.expected_parts,
       e.thread_id, e.item_id, e.role, e.suppressed, e.seq
FROM request_result_replies r
         LEFT JOIN live_mirror_entries e ON e.thread_id = r.thread_id AND e.item_id = r.item_id
WHERE r.event_id = ?
ORDER BY e.seq
LIMIT ?`, event, taskDeliveryMaxReplies+1)
	if err != nil {
		return nil, ErrTaskDeliveryUnavailable
	}
	defer rows.Close()
	var replies []taskDeliveryReply
	var previousSeq int64
	for rows.Next() {
		var reply taskDeliveryReply
		var savedEvent, savedThread, entryThread, entryItem, role string
		var suppressed int
		var seq int64
		// LEFT JOIN 的缺失值会使 Scan 失败，不能静默漏掉清单条目。
		if err = rows.Scan(&savedEvent, &savedThread, &reply.item, &reply.expectedParts,
			&entryThread, &entryItem, &role, &suppressed, &seq); err != nil {
			return nil, ErrTaskDeliveryUnavailable
		}
		if len(replies) >= expected || savedEvent != event || savedThread != thread || entryThread != thread ||
			reply.item == "" || entryItem != reply.item || role != "assistant" || suppressed != 0 ||
			reply.expectedParts <= 0 || reply.expectedParts > taskDeliveryMaxParts ||
			(len(replies) > 0 && seq <= previousSeq) {
			return nil, ErrTaskDeliveryUnavailable
		}
		previousSeq = seq
		replies = append(replies, reply)
	}
	if rows.Err() != nil || rows.Close() != nil || len(replies) != expected {
		return nil, ErrTaskDeliveryUnavailable
	}
	return replies, nil
}

func taskDeliveryParts(ctx context.Context, tx *sql.Tx, thread, item string, expected int, bodyBytes *int) ([]taskDeliveryPart, error) {
	// 先按字节长度限制数据库返回的正文，超大单片不先加载到 Go 再截断。
	rows, err := tx.QueryContext(ctx, `SELECT thread_id, item_id, part_index, state, platform_id,
       length(CAST(body AS BLOB)),
       CASE WHEN length(CAST(body AS BLOB)) <= ? THEN body ELSE NULL END
FROM live_mirror_parts
WHERE thread_id = ?
  AND item_id = ?
ORDER BY part_index
LIMIT ?`, taskDeliveryMaxBody-*bodyBytes, thread, item, expected+1)
	if err != nil {
		return nil, ErrTaskDeliveryUnavailable
	}
	defer rows.Close()
	var parts []taskDeliveryPart
	for rows.Next() {
		var savedThread, savedItem, state, platformID string
		var index int
		var size int64
		var body sql.NullString
		if err = rows.Scan(&savedThread, &savedItem, &index, &state, &platformID, &size, &body); err != nil {
			return nil, ErrTaskDeliveryUnavailable
		}
		if savedThread != thread || savedItem != item || len(parts) >= expected || index != len(parts) ||
			!body.Valid || size < 0 || size > int64(taskDeliveryMaxBody-*bodyBytes) ||
			size != int64(len(body.String)) || !utf8.ValidString(body.String) {
			return nil, ErrTaskDeliveryUnavailable
		}
		*bodyBytes += len(body.String)
		delivery := "unknown"
		switch state {
		case "sent":
			if platformID != "" {
				delivery = "sent"
			}
		case "pending":
			delivery = "pending"
		default:
			// sending、unknown 以及未来新增状态均不推断成功或失败。
		}
		parts = append(parts, taskDeliveryPart{body: body.String, delivery: delivery})
	}
	if rows.Err() != nil || rows.Close() != nil || len(parts) != expected {
		return nil, ErrTaskDeliveryUnavailable
	}
	return parts, nil
}

func taskDeliveryExcerpt(replies []taskDeliveryReply) string {
	const prefix = "最终回复摘录（仅摘录，不推理）："
	text := "（无可摘录正文）"
	// 取排序后的最后一条有正文的助手回复，不拼接分片、不分析正文结论。
outer:
	for i := len(replies) - 1; i >= 0; i-- {
		for j, part := range replies[i].parts {
			body := part.body
			first, rest, _ := strings.Cut(body, "\n")
			const label = "🤖 Codex 回复"
			first = strings.TrimSuffix(first, "\r")
			numberedLabel := label + fmt.Sprintf("（%d/%d）", j+1, len(replies[i].parts))
			if first == label || first == numberedLabel {
				body = rest
			}
			if body = strings.TrimSpace(body); body != "" {
				text = body
				break outer
			}
		}
	}
	runes := []rune(prefix + text)
	if len(runes) > taskDeliverySummary {
		return string(runes[:taskDeliverySummary-1]) + "…"
	}
	return string(runes)
}

func taskDeliveryStateText(state string) string {
	switch state {
	case "sent":
		return "已确认送达"
	case "pending":
		return "待送达确认（不代表失败或已送达）"
	default:
		return "送达结果未确认，需人工核对（不自动推断成功或失败）"
	}
}

// 使用长于正文任意连续反引号的围栏，防止分片内 Markdown 混入报告元数据。
func taskDeliveryFence(out *strings.Builder, body string) {
	longest, run := 2, 0
	for _, c := range body {
		if c == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	out.WriteString(fence + "text\n")
	out.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		out.WriteByte('\n')
	}
	out.WriteString(fence + "\n\n")
}

func taskDeliveryMarkdown(result TaskDelivery, replies []taskDeliveryReply) string {
	var out strings.Builder
	out.WriteString("# 本次任务回复报告\n\n## 请求与会话身份\n\n")
	taskDeliveryFence(&out, fmt.Sprintf("请求 ID：%q\n会话 ID：%q", result.EventID, result.ThreadID))
	out.WriteString("## 执行与原回复送达状态\n\n- 执行状态：已完成（已有封存回复证据，不代表测试通过）\n")
	fmt.Fprintf(&out, "- 原回复送达状态：%s\n- 执行完成时间（Unix 秒）：%d\n", taskDeliveryStateText(result.Delivery), result.CompletedAt)
	if result.DeliveredAt > 0 {
		fmt.Fprintf(&out, "- 原回复确认送达时间（Unix 秒）：%d\n", result.DeliveredAt)
	}
	out.WriteString("- 报告附件送达状态：本报告读取不提供附件发送或送达证明，与原回复分别确认。\n\n")
	out.WriteString("## 验证范围与产物边界\n\n")
	out.WriteString(result.Verification + "。不能从助手正文猜测验证结果。\n\n")
	out.WriteString("本报告由本次精确关联、已封存且已脱敏的助手回复分片生成，是数据库回复报告，不是工作目录产物，也不是模型生成的原始文件。未扫描工作目录，不认领任何代码、SQL、图片或其它文件为本任务产物。分片中的文件与验证声明仅为助手原文，不构成独立证据。\n\n")
	out.WriteString("## 最终回复摘录\n\n")
	taskDeliveryFence(&out, result.Summary)
	out.WriteString("## 助手回复（按条目及分片顺序）\n\n每个代码块对应一个已保存分片，保留标签和分片边界，不拼接为原始 Markdown 文件；围栏前的换行仅用于报告排版。\n\n")
	for i, reply := range replies {
		fmt.Fprintf(&out, "### 助手回复 %d\n\n", i+1)
		taskDeliveryFence(&out, fmt.Sprintf("条目 ID：%q\n封存分片数：%d", reply.item, reply.expectedParts))
		for j, part := range reply.parts {
			fmt.Fprintf(&out, "#### 分片 %d/%d（索引 %d）\n\n原回复分片送达状态：%s\n\n", j+1, len(reply.parts), j, taskDeliveryStateText(part.delivery))
			taskDeliveryFence(&out, part.body)
		}
	}
	return out.String()
}
