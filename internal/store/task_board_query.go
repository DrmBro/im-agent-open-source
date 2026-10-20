package store

import (
	"context"
	"database/sql"
	"strings"
)

// 看板仅统一展示字段，不更改原始账本或把连接存活作为执行证据。
const taskBoardFields = `source,id,thread_id,connection,title,state,stage,execution,delivery,detail,received_at,updated_at`

func taskBoardQuery(ctx context.Context, tx *sql.Tx, tables map[string]bool) (string, string, error) {
	var sources []string
	var warnings []string
	if tables["events"] {
		columns, err := historyPageColumns(ctx, tx, "events")
		if err != nil {
			return "", "", err
		}
		field := func(name, fallback string) string {
			if columns[name] {
				return "e." + name
			}
			return fallback
		}
		stage, detail, updated := field("stage", "''"), field("error_summary", "''"), field("updated_at", "e.created_at")
		joins := " FROM events e"
		thread, connection, title := "''", "''", "'会话请求'"
		execution, delivery := "''", "''"
		if tables["request_results"] {
			joins += " LEFT JOIN request_results r ON r.event_id=e.id"
			if columns["chat_key"] {
				joins += " AND r.address_key=e.chat_key"
			}
			thread, execution, delivery = "coalesce(r.thread_id,'')", "coalesce(r.execution_state,'')", "coalesce(r.delivery_state,'')"
			updated = "max(" + updated + ",coalesce(r.updated_at,0))"
			detail = "coalesce(nullif(" + detail + ",''),r.observation_error,'')"
			if tables["request_result_replies"] && tables["live_mirror_parts"] {
				joins += ` LEFT JOIN (SELECT m.event_id,
 max(p.state='unknown') AS unknown_delivery,
 max(p.state='pending' AND p.error<>'') AS failed_delivery,
 max(p.updated_at) AS updated_at
 FROM request_result_replies m JOIN live_mirror_parts p ON p.thread_id=m.thread_id AND p.item_id=m.item_id
 GROUP BY m.event_id) parts ON parts.event_id=e.id`
				delivery = "CASE WHEN parts.unknown_delivery=1 THEN 'unknown' WHEN parts.failed_delivery=1 THEN 'failed' ELSE coalesce(r.delivery_state,'') END"
				updated = "max(" + updated + ",coalesce(parts.updated_at,0))"
			}
			if tables["sessions"] {
				joins += " LEFT JOIN sessions s ON s.thread_id=r.thread_id"
				connection = "coalesce(s.connection_id,'')"
			}
			if tables["session_presentation"] {
				joins += " LEFT JOIN session_presentation sp ON sp.thread_id=r.thread_id"
				title = "coalesce(nullif(sp.target_name,''),'会话请求')"
			}
		}
		// 管理命令保留在运行诊断，不把 /status 等命令当成模型任务。
		where := " WHERE " + stage + " NOT IN ('command_running','command_completed','command_failed')"
		if tables["history_content_pruned"] {
			where += " AND NOT (e.status IN ('done','failed','queued_failed','cancelled','expired') AND EXISTS(SELECT 1 FROM history_content_pruned h WHERE h.kind='event' AND h.key1=e.id AND h.key2=''))"
		}
		// done 仅表示旧执行入口已结束；没有完成与送达证据时保持待确认。
		state := `CASE
 WHEN ` + delivery + ` IN ('failed','unknown') THEN 'attention'
 WHEN ` + execution + `='completed' AND ` + delivery + `='sent' THEN 'completed'
 WHEN e.status IN ('failed','uncertain','interrupted','queued_failed','queue_failed') OR ` + execution + `='failed' THEN 'attention'
 WHEN e.status IN ('cancelled','expired','bundled','rejected') THEN 'closed'
 WHEN ` + execution + `='completed' AND ` + delivery + ` IN ('pending','awaiting_replies') THEN 'active'
 WHEN e.status='waiting_input' OR (e.status='processing' AND ` + stage + ` IN ('waiting_draft','waiting_input','waiting_approval')) THEN 'waiting'
 WHEN e.status='processing' THEN 'active'
 ELSE 'attention' END`
		sources = append(sources, "SELECT 'session' AS source,e.id,"+thread+" AS thread_id,"+connection+" AS connection,"+title+" AS title,"+state+" AS state,"+stage+" AS stage,"+execution+" AS execution,"+delivery+" AS delivery,substr("+detail+",1,1024) AS detail,e.created_at AS received_at,"+updated+" AS updated_at,"+title+" AS search_text"+joins+where)
		if !columns["stage"] || !tables["request_results"] {
			warnings = append(warnings, "部分旧会话任务缺少执行证据，按待确认展示")
		}
	}
	if len(sources) == 0 {
		return "", "", nil
	}
	return "WITH board AS (" + strings.Join(sources, " UNION ALL ") + ") ", strings.Join(warnings, "；"), nil
}
