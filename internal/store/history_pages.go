package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HistoryPage 的页码从 1 开始；仅指定类别的数组有值，其余数组不参与读取。
type HistoryPage struct {
	Kind          string          `json:"kind"`
	Page          int             `json:"page"`
	PageSize      int             `json:"page_size"`
	Total         int             `json:"total"`
	Tasks         []TaskRow       `json:"tasks"`
	Deliveries    []DeliveryRow   `json:"deliveries"`
	Creations     []CreationForm  `json:"creations"`
	Attachments   []Transfer      `json:"attachments"`
	Presentations []Presentation  `json:"presentations"`
	Closures      []Closure       `json:"closures"`
	Waiting       *WaitingSummary `json:"waiting,omitempty"`
	Warning       string          `json:"warning"`
}

var errHistoryPageRead = errors.New("历史列表暂不可读取，请稍后重试")

const historyPageMissing = "部分历史表尚未就绪；仅展示已有数据，未创建数据库或执行迁移"
const historyPageLegacyTasks = "旧版任务表诊断字段不完整；仅展示已有记录，不推断缺失的执行证据或等待状态"
const historyPageLegacyClosure = "旧版会话表缺少群管理标识；暂不展示归档记录，未推断管理权限或执行迁移"

// HistoryPageSnapshot 不调用 New 或旧 Snapshot，不读取正文，不修改业务状态。
// COUNT、分页、等待汇总及当前页结果补全都在同一个只读连接的事务快照内。
func HistoryPageSnapshot(ctx context.Context, dir, kind string, page, size int) (HistoryPage, error) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 20
	} else if size > 50 {
		size = 50
	}
	out := HistoryPage{Kind: kind, Page: page, PageSize: size}
	switch kind {
	case "tasks":
		out.Tasks = []TaskRow{}
	case "deliveries":
		out.Deliveries = []DeliveryRow{}
	case "creations":
		out.Creations = []CreationForm{}
	case "attachments":
		out.Attachments = []Transfer{}
	case "presentations":
		out.Presentations = []Presentation{}
	case "closures":
		out.Closures = []Closure{}
	default:
		return out, errors.New("不支持的历史列表类别")
	}
	if ctx == nil || dir == "" {
		return out, errHistoryPageRead
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 出错不返回已读取的半页或底层 SQL、路径、正文错误。
	empty := out
	empty.Page = 1
	path, err := filepath.Abs(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return empty, errHistoryPageRead
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return empty, errHistoryPageRead
	}
	// URI 保证连接重新建立时仍只读且 busy_timeout 为 1000 毫秒。
	params := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)"}}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: params.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return empty, errHistoryPageRead
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, errHistoryPageRead
	}
	defer tx.Rollback()
	if err = readHistoryPage(ctx, tx, &out); err != nil {
		return empty, errHistoryPageRead
	}
	if err = tx.Commit(); err != nil {
		return empty, errHistoryPageRead
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return empty, errHistoryPageRead
	}
	return out, nil
}

// 这里只探测表名元数据，不读取各列表的全量业务记录。
func historyPageTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

// 列探测与分页共用事务；表名只允许内部固定值，不能由 kind 拼接 SQL。
func historyPageColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	switch table {
	case "events", "sessions":
	default:
		return nil, errHistoryPageRead
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, required, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&cid, &name, &kind, &required, &value, &pk); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func readHistoryPage(ctx context.Context, tx *sql.Tx, out *HistoryPage) error {
	tables, err := historyPageTables(ctx, tx)
	if err != nil {
		return err
	}
	mainTable := map[string]string{
		"tasks": "events", "deliveries": "live_mirror_parts", "creations": "creation_forms",
		"attachments": "attachment_transfers", "presentations": "session_presentation", "closures": "session_closures",
	}[out.Kind]
	if !tables[mainTable] || ((out.Kind == "presentations" || out.Kind == "closures") && !tables["sessions"]) {
		out.Page, out.Warning = 1, historyPageMissing
		return nil
	}
	// sessions 的地址组合有唯一约束，展示表以 thread_id 为主键；JOIN 不扩大记录数。
	topic := "''"
	if tables["sessions"] && (out.Kind == "tasks" || out.Kind == "presentations" || out.Kind == "closures") {
		sessionColumns, err := historyPageColumns(ctx, tx, "sessions")
		if err != nil {
			return err
		}
		if sessionColumns["topic_id"] {
			topic = "s.topic_id"
		}
		if out.Kind == "closures" && !sessionColumns["managed_chat"] {
			out.Page, out.Warning = 1, historyPageLegacyClosure
			return nil
		}
	}
	var fields, from, order string
	taskDiagnosticsReady := false
	marked := tables["history_content_pruned"]
	switch out.Kind {
	case "tasks":
		eventColumns, err := historyPageColumns(ctx, tx, "events")
		if err != nil {
			return err
		}
		stage, summary, updatedAt, chatKey := "''", "''", "e.created_at", "''"
		if eventColumns["stage"] {
			stage = "e.stage"
			out.Waiting, err = historyPageWaiting(ctx, tx)
			if err != nil {
				return err
			}
		}
		if eventColumns["error_summary"] {
			summary = "e.error_summary"
		}
		if eventColumns["updated_at"] {
			updatedAt = "e.updated_at"
		}
		if eventColumns["chat_key"] {
			chatKey = "e.chat_key"
		}
		taskDiagnosticsReady = eventColumns["stage"] && eventColumns["error_summary"] &&
			eventColumns["updated_at"] && eventColumns["chat_key"]
		if !taskDiagnosticsReady {
			out.Warning = historyPageLegacyTasks
		}
		// created_at 仅作为缺失更新时间的展示回退，不用于推断终态、执行完成或保留期限。
		fields = "e.id, e.status, " + stage + ", " + summary + ", e.created_at, " + updatedAt + ", " + chatKey
		from = "FROM events e"
		if marked {
			// 未知/中断/处理中即使有旧标记也不能隐藏；标记不改变执行状态。
			from += `
WHERE NOT (e.status IN ('done', 'failed', 'queued_failed', 'cancelled', 'expired')
           AND EXISTS (SELECT 1 FROM history_content_pruned h
                       WHERE h.kind = 'event' AND h.key1 = e.id AND h.key2 = ''))`
		}
		order = "e.created_at DESC, e.id"
	case "deliveries":
		fields = "d.thread_id, d.item_id, d.part_index, d.state, d.error, d.platform_id, d.updated_at, ''"
		from = "FROM live_mirror_parts d"
		if tables["session_presentation"] {
			fields = strings.TrimSuffix(fields, "''") + "coalesce(p.target_name, '')"
			from += "\n     LEFT JOIN session_presentation p ON p.thread_id = d.thread_id"
		} else {
			out.Warning = historyPageMissing
		}
		from += "\nWHERE d.state <> 'sent'"
		order = "d.updated_at DESC, d.thread_id, d.item_id, d.part_index"
	case "creations":
		fields = "f.id, f.state, f.stage, f.thread_id, f.error, f.expires_at"
		from = "FROM creation_forms f"
		if marked {
			from += `
WHERE NOT (f.state IN ('done', 'cancelled', 'failed', 'send_failed', 'queue_failed')
           AND EXISTS (SELECT 1 FROM history_content_pruned h
                       WHERE h.kind = 'creation' AND h.key1 = f.id AND h.key2 = ''))`
		}
		order = "f.updated_at DESC, f.id"
	case "attachments":
		fields = "t.id, t.thread_id, t.direction, t.kind, t.name, t.state, t.error, t.platform_id, t.updated_at"
		from = "FROM attachment_transfers t"
		if marked {
			from += `
WHERE NOT (t.state IN ('sent', 'received', 'failed')
           AND EXISTS (SELECT 1 FROM history_content_pruned h
                       WHERE h.kind = 'transfer' AND h.key1 = t.id AND h.key2 = ''))`
		}
		order = "t.updated_at DESC, t.id"
	case "presentations":
		// 保持既有扫描顺序；正文列直接用空串替代，不先读取再清空。
		fields = strings.Replace(presentationBaseColumns, "p.history_payload", "''", 1) + ", " + topic
		from = "FROM session_presentation p\n     JOIN sessions s ON s.thread_id = p.thread_id"
		order = "p.updated_at DESC, p.thread_id"
	case "closures":
		fields = closureBaseColumns + ", " + topic
		from = "FROM session_closures c\n     JOIN sessions s ON s.thread_id = c.thread_id"
		order = "c.updated_at DESC, c.thread_id"
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*)\n"+from).Scan(&out.Total); err != nil {
		return err
	}
	last := 1
	if out.Total > 0 {
		last = (out.Total-1)/out.PageSize + 1
	}
	if out.Page > last {
		out.Page = last
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+fields+"\n"+from+"\nORDER BY "+order+"\nLIMIT ? OFFSET ?", out.PageSize, (out.Page-1)*out.PageSize)
	if err != nil {
		return err
	}
	keys, err := scanHistoryPage(rows, out)
	// 所有后续查询必须在关闭本页游标后执行，且只能使用 tx，不能回到 db。
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// 旧表缺少诊断列时保守跳过映射和结果补全，保留 warning，不推断缺失执行证据。
	if out.Kind == "tasks" && taskDiagnosticsReady {
		return completeHistoryTasks(ctx, tx, out, keys, tables, topic)
	}
	return nil
}

func scanHistoryPage(rows *sql.Rows, out *HistoryPage) ([]string, error) {
	var keys []string
	for rows.Next() {
		switch out.Kind {
		case "tasks":
			var r TaskRow
			var key string
			if err := rows.Scan(&r.ID, &r.Status, &r.Stage, &r.Error, &r.ReceivedAt, &r.UpdatedAt, &key); err != nil {
				return nil, err
			}
			r.WaitingHint = RequestWaitingHint(r)
			out.Tasks = append(out.Tasks, r)
			keys = append(keys, key)
		case "deliveries":
			var r DeliveryRow
			if err := rows.Scan(&r.ThreadID, &r.ItemID, &r.Part, &r.State, &r.Error, &r.PlatformID, &r.UpdatedAt, &r.Name); err != nil {
				return nil, err
			}
			out.Deliveries = append(out.Deliveries, r)
		case "creations":
			var r CreationForm
			if err := rows.Scan(&r.ID, &r.State, &r.Stage, &r.ThreadID, &r.Error, &r.ExpiresAt); err != nil {
				return nil, err
			}
			out.Creations = append(out.Creations, r)
		case "attachments":
			var r Transfer
			if err := rows.Scan(&r.ID, &r.ThreadID, &r.Direction, &r.Kind, &r.Name, &r.State, &r.Error, &r.PlatformID, &r.UpdatedAt); err != nil {
				return nil, err
			}
			out.Attachments = append(out.Attachments, r)
		case "presentations":
			r, err := scanPresentation(rows)
			if err != nil {
				return nil, err
			}
			out.Presentations = append(out.Presentations, r)
		case "closures":
			r, err := scanClosure(rows)
			if err != nil {
				return nil, err
			}
			out.Closures = append(out.Closures, r)
		}
	}
	return keys, rows.Err()
}

func historyPageWaiting(ctx context.Context, tx *sql.Tx) (*WaitingSummary, error) {
	var out WaitingSummary
	// 与 waitingSummarySnapshot 相同口径；这是业务聚合，不是去重或页内统计。
	err := tx.QueryRowContext(ctx, `SELECT count(CASE WHEN stage = 'queued' THEN 1 END),
       count(CASE WHEN stage = 'waiting_desktop_submit' THEN 1 END),
       count(CASE WHEN stage = 'waiting_draft' THEN 1 END),
       count(CASE WHEN stage IN ('checking_desktop', 'waiting_desktop', 'switching_desktop', 'preserving_draft', 'restoring_draft') THEN 1 END),
       count(CASE WHEN stage = 'waiting_model' THEN 1 END)
FROM events
WHERE status = 'processing'`).Scan(&out.Queued, &out.DesktopSubmit, &out.Draft, &out.DesktopReady, &out.Model)
	if err != nil {
		return nil, err
	}
	out.CapturedAt = time.Now().Unix()
	return &out, nil
}

func completeHistoryTasks(ctx context.Context, tx *sql.Tx, out *HistoryPage, keys []string, tables map[string]bool, topic string) error {
	if len(out.Tasks) == 0 {
		return nil
	}
	if tables["sessions"] {
		// SQLite 无地址散列函数，只允许预读会话元数据；不读取全量 events。
		// 唯一地址映射遇到异常冲突时拒绝读取，不擅自挑选最新或去重。
		q := "SELECT s.thread_id, s.channel, s.connection_id, s.chat_id, " + topic + ", ''\nFROM sessions s"
		if tables["session_presentation"] {
			q = "SELECT s.thread_id, s.channel, s.connection_id, s.chat_id, " + topic + ", coalesce(p.target_name, '')\nFROM sessions s\n     LEFT JOIN session_presentation p ON p.thread_id = s.thread_id"
		} else {
			out.Warning = historyPageMissing
		}
		rows, err := tx.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		bindings, err := historyPageBindings(rows)
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		for i, key := range keys {
			out.Tasks[i].ThreadID, out.Tasks[i].Name = bindings[key].ThreadID, bindings[key].GroupName
		}
	} else {
		out.Warning = historyPageMissing
	}
	if !tables["request_results"] {
		out.Warning = historyPageMissing
		return nil
	}
	if !tables["request_result_replies"] || !tables["live_mirror_parts"] || !tables["request_result_audit"] {
		out.Warning = historyPageMissing
	}
	for i := range out.Tasks {
		result, err := historyPageResult(ctx, tx, out.Tasks[i].ID, tables)
		if err != nil {
			return err
		}
		out.Tasks[i].Result = result
	}
	return nil
}

func historyPageBindings(rows *sql.Rows) (map[string]Session, error) {
	out := map[string]Session{}
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ThreadID, &s.Address.Channel, &s.Address.Connection, &s.Address.ChatID, &s.Address.TopicID, &s.GroupName); err != nil {
			return nil, err
		}
		key := Hash(s.Address.Key())
		if _, exists := out[key]; exists {
			return nil, errHistoryPageRead
		}
		out[key] = s
	}
	return out, rows.Err()
}

// 与 requestResultSnapshot 保持相同字段和聚合口径，但严格使用当前快照和请求 ID。
func historyPageResult(ctx context.Context, tx *sql.Tx, event string, tables map[string]bool) (*RequestResult, error) {
	var r RequestResult
	err := tx.QueryRowContext(ctx, `SELECT event_id, turn_ref, execution_state, delivery_state, expected_replies,
       completed_at, delivered_at, recovered, observation_error
FROM request_results
WHERE event_id = ?`, event).Scan(&r.EventID, &r.TurnRef, &r.Execution, &r.Delivery, &r.ExpectedReplies, &r.CompletedAt, &r.DeliveredAt, &r.Recovered, &r.ObservationError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if tables["request_result_replies"] && tables["live_mirror_parts"] {
		err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(m.expected_parts), 0),
       coalesce(sum((SELECT count(*) FROM live_mirror_parts p
                     WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id AND p.state = 'sent')), 0)
FROM request_result_replies m
WHERE m.event_id = ?`, event).Scan(&r.ExpectedParts, &r.ConfirmedParts)
		if err != nil {
			return nil, err
		}
	}
	if tables["request_result_audit"] {
		rows, err := tx.QueryContext(ctx, `SELECT reason, previous_status, previous_stage, detail, created_at
FROM request_result_audit
WHERE event_id = ?
ORDER BY seq DESC
LIMIT 5`, event)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var a RequestAudit
			if err = rows.Scan(&a.Reason, &a.PreviousStatus, &a.PreviousStage, &a.Detail, &a.At); err != nil {
				return nil, err
			}
			r.Audit = append(r.Audit, a)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	return &r, nil
}
