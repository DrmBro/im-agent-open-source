package store

import (
	"context"
	"errors"
	"time"
)

const HistoryRetentionDays = 7

const historyPruneBatchSize = 100

// HistoryPruneResult 仅包含固定统计，不携带正文、路径、业务标识或底层错误。
// Pruned 按已处理的历史内容对象计数，包含空内容的一次性分页隐藏标记，不代表释放字节数。
// 一条回复的全部分片、一个事件的诊断及审计各算一个对象。
// More 表示本次预算已用完，可能还有候选；调用方应短暂让出后继续调用。
// 首次登记时钟不计入Pruned，但也占预算；已登记未过期及已有标记不重复入选。
// event/creation/transfer即使内容已空仍需一次性标记；其它类别保留空内容过滤。
type HistoryPruneResult struct {
	CheckedAt int64 `json:"checked_at"`
	Pruned    int   `json:"pruned"`
	More      bool  `json:"more"`
}

// 迁移仅创建内容标记和缺少终态时间的辅助时钟，不改旧表、不在启动时清理。
func (s *Store) migrateHistoryRetention() error {
	tx, err := s.db.Begin()
	if err != nil {
		return errors.New("历史保留标记初始化失败")
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS history_content_pruned (
    kind TEXT NOT NULL, key1 TEXT NOT NULL, key2 TEXT NOT NULL DEFAULT '', pruned_at INTEGER NOT NULL,
    PRIMARY KEY(kind, key1, key2)
)`,
		`CREATE TABLE IF NOT EXISTS history_terminal_observed (
    kind TEXT NOT NULL, key1 TEXT NOT NULL, key2 TEXT NOT NULL DEFAULT '',
    terminal_state TEXT NOT NULL, observed_at INTEGER NOT NULL,
    PRIMARY KEY(kind, key1, key2)
)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return errors.New("历史保留标记初始化失败")
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("历史保留标记初始化失败")
	}
	return nil
}

// PruneHistory 只处理本地内容；不删除原记录、文件、防重键、回执或游标。
// 每次至多选择100个候选（首次登记终态时钟也占预算），每类一个可取消的小事务。
// 本文件不负责调度、迁移调用、摘要防回填；这些由主流程接入。
func (s *Store) PruneHistory(ctx context.Context, now time.Time) (HistoryPruneResult, error) {
	result := HistoryPruneResult{CheckedAt: now.Unix()}
	if ctx == nil || now.IsZero() || now.Unix() <= 0 {
		return result, errors.New("历史清理参数无效")
	}
	cutoff := now.Add(-HistoryRetentionDays * 24 * time.Hour).Unix()
	if cutoff <= 0 {
		return result, errors.New("历史清理时间无效")
	}
	tables, err := s.historyRetentionTables(ctx)
	if err != nil {
		return result, historyPruneError(ctx)
	}
	if !tables["history_content_pruned"] || !tables["history_terminal_observed"] {
		return result, errors.New("历史保留标记尚未初始化")
	}
	remaining := historyPruneBatchSize
	for _, rule := range historyPruneRules() {
		available := true
		for _, table := range rule.tables {
			if !tables[table] {
				available = false
				break
			}
		}
		if rule.graph {
			for _, table := range historyRequestTables {
				if !tables[table] {
					available = false
					break
				}
			}
		}
		if !available {
			// 旧库缺表时保守跳过，不因缺少关联表而放宽完成证据。
			continue
		}
		selected, pruned, err := s.pruneHistoryRule(ctx, rule, now.Unix(), cutoff, remaining)
		if err != nil {
			// 之前已提交的小事务仍计数，当前失败事务不会计入。
			return result, err
		}
		result.Pruned += pruned
		remaining -= selected
		if remaining == 0 {
			result.More = true
			return result, nil
		}
	}
	if ctx.Err() != nil {
		return result, historyPruneError(ctx)
	}
	return result, nil
}

func historyPruneError(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("历史清理已取消或超时，可稍后重试")
	}
	return errors.New("历史清理失败，可稍后重试")
}

func (s *Store) historyRetentionTables(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

var historyRequestTables = []string{
	"events", "request_results", "request_result_replies", "request_result_audit",
	"live_mirror_entries", "live_mirror_parts", "live_mirror_origins",
}

const historyClockCTE = `WITH retention_clock AS (SELECT ? AS cutoff)`

// 完成证据按业务主键精确关联，不猜最近请求，不合并重复数据。
// terminal_requests 校验终态及清单完整性；old_requests 再校验所有相关时间。
const historyRequestCTEs = `,
     terminal_requests AS (SELECT r.event_id, r.thread_id
                           FROM request_results r
                                    JOIN events e ON e.id = r.event_id
                           WHERE r.sealed = 1 AND r.execution_state = 'completed'
                             AND r.delivery_state = 'sent' AND e.status = 'done'
                             AND e.chat_key = r.address_key AND r.expected_replies > 0
                             AND r.completed_at > 0 AND r.delivered_at > 0 AND r.updated_at > 0
                             AND e.updated_at > 0
                             AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o
                                             WHERE o.event_id = r.event_id
                                               AND (o.state NOT IN ('matched', 'cancelled') OR o.thread_id <> r.thread_id))
                             AND (SELECT count(*) FROM request_result_replies m
                                  WHERE m.event_id = r.event_id) = r.expected_replies
                             AND NOT EXISTS (SELECT 1
                                             FROM request_result_replies m
                                             WHERE m.event_id = r.event_id
                                               AND (m.thread_id <> r.thread_id OR m.expected_parts <= 0
                                                    OR NOT EXISTS (SELECT 1 FROM live_mirror_entries x
                                                                   WHERE x.thread_id = m.thread_id AND x.item_id = m.item_id
                                                                     AND x.role = 'assistant' AND x.suppressed = 0)
                                                    OR (SELECT count(*) FROM live_mirror_parts p
                                                        WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id) <> m.expected_parts
                                                    OR EXISTS (SELECT 1 FROM live_mirror_parts p
                                                               WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id
                                                                 AND (p.state <> 'sent' OR p.platform_id = '' OR p.updated_at <= 0
                                                                      OR p.part_index < 0 OR p.part_index >= m.expected_parts))))),
     old_requests AS (SELECT r.event_id, r.thread_id
                      FROM request_results r
                               JOIN terminal_requests t ON t.event_id = r.event_id
                               JOIN events e ON e.id = r.event_id
                      WHERE r.completed_at < (SELECT cutoff FROM retention_clock)
                        AND r.delivered_at < (SELECT cutoff FROM retention_clock)
                        AND r.updated_at < (SELECT cutoff FROM retention_clock)
                        AND r.reply_checked_at < (SELECT cutoff FROM retention_clock)
                        AND e.updated_at < (SELECT cutoff FROM retention_clock)
                        AND NOT EXISTS (SELECT 1 FROM request_result_audit a
                                        WHERE a.event_id = r.event_id
                                          AND (a.created_at <= 0 OR a.created_at >= (SELECT cutoff FROM retention_clock)))
                        AND NOT EXISTS (SELECT 1
                                        FROM request_result_replies m
                                                 JOIN live_mirror_parts p ON p.thread_id = m.thread_id AND p.item_id = m.item_id
                                        WHERE m.event_id = r.event_id
                                          AND p.updated_at >= (SELECT cutoff FROM retention_clock))),
     terminal_events AS (SELECT e.id
                         FROM events e
                         WHERE e.updated_at > 0
                           AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o
                                           WHERE o.event_id = e.id AND o.state NOT IN ('matched', 'cancelled'))
                           AND (EXISTS (SELECT 1 FROM terminal_requests r WHERE r.event_id = e.id)
                                OR (NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id = e.id)
                                    AND ((e.status = 'done' AND e.stage IN ('completed', 'command_completed', 'images_cancelled'))
                                         OR (((e.status = 'failed' AND e.stage IN ('not_sent', 'queue_full'))
                                              OR (e.status = 'queued_failed' AND e.stage = 'queue_full'))
                                             AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o
                                                             WHERE o.event_id = e.id AND o.state <> 'cancelled'))
                                         OR (e.status = 'cancelled' AND e.stage = 'images_cancelled')
                                         OR (e.status = 'expired' AND e.stage = 'images_expired'))))),
     old_events AS (SELECT e.id
                    FROM events e
                             JOIN terminal_events t ON t.id = e.id
                    WHERE e.updated_at < (SELECT cutoff FROM retention_clock)
                      AND NOT EXISTS (SELECT 1 FROM request_results r
                                      WHERE r.event_id = e.id
                                        AND NOT EXISTS (SELECT 1 FROM old_requests x WHERE x.event_id = r.event_id))
                      AND NOT EXISTS (SELECT 1 FROM request_result_audit a
                                      WHERE a.event_id = e.id
                                        AND (a.created_at <= 0 OR a.created_at >= (SELECT cutoff FROM retention_clock))))`

// 无精确请求关联的内容须额外保护同线程的恢复工作。
// queued等事件可能尚未登记request/origin，chat_key又是单向哈希：无法从SQL可靠
// 反查线程时保守保护全部无关联内容，不借最近时间猜测归属。
func historyIdleThread(thread string) string {
	return `NOT EXISTS (SELECT 1 FROM request_results r
                    WHERE r.thread_id = ` + thread + `
                      AND NOT EXISTS (SELECT 1 FROM terminal_requests t WHERE t.event_id = r.event_id))
  AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o
                  WHERE o.thread_id = ` + thread + ` AND o.state NOT IN ('matched', 'cancelled'))
  AND NOT EXISTS (SELECT 1 FROM events e
                  WHERE e.status NOT IN ('done', 'cancelled', 'expired', 'bundled')
                    AND NOT ((e.status = 'failed' AND e.stage IN ('not_sent', 'queue_full'))
                             OR (e.status = 'queued_failed' AND e.stage = 'queue_full'))
                    AND (EXISTS (SELECT 1 FROM live_mirror_origins o
                                 WHERE o.event_id = e.id AND o.thread_id = ` + thread + `)
                         OR EXISTS (SELECT 1 FROM request_results r
                                    WHERE r.event_id = e.id AND r.thread_id = ` + thread + `)
                         OR (NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id = e.id)
                             AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o WHERE o.event_id = e.id))))`
}

type historyPruneRule struct {
	kind     string
	tables   []string
	graph    bool
	observed bool
	// query固定返回key1、key2、终态、是否已过期；不读取正文。
	query   string
	updates []string
}

func historyPruneRules() []historyPruneRule {
	return []historyPruneRule{
		{
			kind: "reply", graph: true,
			query: `SELECT m.thread_id AS key1, m.item_id AS key2, 'sent' AS terminal_state, 1 AS expired
FROM live_mirror_entries m
WHERE m.suppressed = 0 AND m.role IN ('user', 'assistant')
  AND EXISTS (SELECT 1 FROM live_mirror_parts p
              WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id AND (p.body <> '' OR p.error <> ''))
  AND NOT EXISTS (SELECT 1 FROM live_mirror_parts p
                  WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id
                    AND (p.state <> 'sent' OR p.platform_id = '' OR p.updated_at <= 0
                         OR p.updated_at >= (SELECT cutoff FROM retention_clock)))
  AND NOT EXISTS (SELECT 1 FROM request_result_replies x
                  WHERE x.thread_id = m.thread_id AND x.item_id = m.item_id
                    AND NOT EXISTS (SELECT 1 FROM old_requests r WHERE r.event_id = x.event_id))
  AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o
                  WHERE o.thread_id = m.thread_id AND o.user_item_id = m.item_id
                    AND NOT EXISTS (SELECT 1 FROM old_requests r WHERE r.event_id = o.event_id))
  AND (EXISTS (SELECT 1 FROM request_result_replies x WHERE x.thread_id = m.thread_id AND x.item_id = m.item_id)
       OR EXISTS (SELECT 1 FROM live_mirror_origins o
                   WHERE o.thread_id = m.thread_id AND o.user_item_id = m.item_id
                     AND EXISTS (SELECT 1 FROM old_requests r WHERE r.event_id = o.event_id)))`,
			updates: []string{`UPDATE live_mirror_parts SET body = '', error = '' WHERE thread_id = ? AND item_id = ?`},
		},
		{
			kind: "event", graph: true,
			query: `SELECT e.id AS key1, '' AS key2, e.status AS terminal_state, 1 AS expired
FROM events e
WHERE EXISTS (SELECT 1 FROM old_events x WHERE x.id = e.id)`,
			updates: []string{
				`UPDATE events SET error_summary = '' WHERE id = ? AND ? = ''`,
				`UPDATE request_results SET observer_state = '', observation_error = ''
WHERE event_id = ? AND ? = '' AND sealed = 1 AND execution_state = 'completed' AND delivery_state = 'sent'`,
				`UPDATE request_result_audit SET detail = '' WHERE event_id = ? AND ? = ''`,
			},
		},
		{
			kind: "creation", tables: []string{"creation_forms"}, graph: true,
			query: `SELECT f.id AS key1, '' AS key2, f.state AS terminal_state, 1 AS expired
FROM creation_forms f
WHERE f.updated_at > 0 AND f.updated_at < (SELECT cutoff FROM retention_clock)
  AND (f.state IN ('cancelled', 'queue_failed')
       OR (f.state = 'done' AND f.stage = 'completed')
       OR (f.state = 'failed' AND f.stage = 'first_question'
           AND EXISTS (SELECT 1 FROM events e
                       WHERE e.id = f.id || ':first'
                         AND ((e.status = 'failed' AND e.stage IN ('not_sent', 'queue_full'))
                              OR (e.status = 'queued_failed' AND e.stage = 'queue_full')))))
  AND NOT EXISTS (SELECT 1 FROM events e
                  WHERE e.id = f.id || ':first'
                    AND NOT EXISTS (SELECT 1 FROM old_events x WHERE x.id = e.id))
  AND NOT EXISTS (SELECT 1 FROM request_results r
                  WHERE r.event_id = f.id || ':first'
                    AND NOT EXISTS (SELECT 1 FROM old_requests x WHERE x.event_id = r.event_id))
  AND (f.thread_id = '' OR (` + historyIdleThread("f.thread_id") + `))`,
			updates: []string{`UPDATE creation_forms SET catalog = '', selection = '', error = '' WHERE id = ? AND ? = ''`},
		},
		{
			kind: "transfer", tables: []string{"attachment_transfers"}, graph: true,
			query: `SELECT t.id AS key1, '' AS key2, t.state AS terminal_state, 1 AS expired
FROM attachment_transfers t
WHERE t.state IN ('sent', 'received')
  AND t.updated_at > 0 AND t.updated_at < (SELECT cutoff FROM retention_clock)
  AND ` + historyIdleThread("t.thread_id"),
			updates: []string{`UPDATE attachment_transfers SET path = '', error = '' WHERE id = ? AND ? = ''`},
		},
		{
			kind: "pending_actions", tables: []string{"pending_actions"}, graph: true,
			query: `SELECT a.id AS key1, '' AS key2, a.status AS terminal_state, 1 AS expired
FROM pending_actions a
WHERE (a.status IN ('accept', 'decline', 'timeout', 'cancelled')
       OR (a.kind = 'input' AND a.status <> '' AND a.status NOT GLOB '*[^0-9]*'
           AND a.status = CAST(CAST(a.status AS INTEGER) AS TEXT)))
  AND a.payload <> ''
  AND a.resolved_at > 0 AND a.resolved_at < (SELECT cutoff FROM retention_clock)
  AND ` + historyIdleThread("a.thread_id"),
			updates: []string{`UPDATE pending_actions SET payload = '' WHERE id = ? AND ? = ''`},
		},
		{
			kind: "outbox", tables: []string{"outbox"}, observed: true,
			query: `SELECT o.id AS key1, '' AS key2, 'sent' AS terminal_state, 1 AS expired
FROM outbox o
WHERE o.sent = 1 AND o.body <> ''`,
			updates: []string{`UPDATE outbox SET body = '' WHERE id = ? AND ? = ''`},
		},
		{
			kind: "image_drafts", tables: []string{"image_drafts", "image_draft_events"}, graph: true, observed: true,
			query: `SELECT d.id AS key1, '' AS key2, d.state AS terminal_state,
       CASE WHEN NOT EXISTS (SELECT 1
                             FROM image_draft_events x
                                      JOIN events e ON e.id = x.event_id
                             WHERE x.draft_id = d.id
                               AND (e.updated_at <= 0 OR e.updated_at >= (SELECT cutoff FROM retention_clock)))
                  AND (d.trigger_id = '' OR EXISTS (SELECT 1 FROM old_events e WHERE e.id = d.trigger_id))
            THEN 1 ELSE 0 END AS expired
FROM image_drafts d
WHERE d.images_json NOT IN ('', '[]', 'null')
  AND EXISTS (SELECT 1 FROM image_draft_events x WHERE x.draft_id = d.id)
  AND ((d.state IN ('cancelled', 'expired') AND d.trigger_id = '')
       OR (d.state = 'done' AND EXISTS (SELECT 1 FROM terminal_requests r WHERE r.event_id = d.trigger_id))
       OR (d.state = 'failed' AND EXISTS (SELECT 1 FROM events e
                                         WHERE e.id = d.trigger_id
                                           AND ((e.status = 'failed' AND e.stage IN ('not_sent', 'queue_full'))
                                                OR (e.status = 'queued_failed' AND e.stage = 'queue_full'))
                                           AND EXISTS (SELECT 1 FROM terminal_events t WHERE t.id = e.id))))
  AND NOT EXISTS (SELECT 1
                  FROM image_draft_events x
                           LEFT JOIN events e ON e.id = x.event_id
                  WHERE x.draft_id = d.id
                    AND (e.id IS NULL OR e.updated_at <= 0
                         OR NOT ((d.state = 'cancelled' AND e.status = 'cancelled' AND e.stage = 'images_cancelled')
                                 OR (d.state = 'expired' AND e.status = 'expired' AND e.stage = 'images_expired')
                                 OR (d.state IN ('done', 'failed') AND e.status = 'bundled' AND e.stage = 'images_bundled'))
                         OR EXISTS (SELECT 1 FROM request_results r WHERE r.event_id = e.id)
                         OR EXISTS (SELECT 1 FROM live_mirror_origins o WHERE o.event_id = e.id AND o.state NOT IN ('matched', 'cancelled'))))`,
			// 只清数据库图片负载；地址、来源事件、触发键和实体图片全部保留。
			updates: []string{`UPDATE image_drafts SET images_json = '[]' WHERE id = ? AND ? = ''`},
		},
	}
}

func (s *Store) pruneHistoryRule(parent context.Context, rule historyPruneRule, now, cutoff int64, limit int) (int, int, error) {
	// 查询、选取、清空及打标共用快照。写锁竞争或取消即回滚，不提交部分候选。
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, historyPruneError(ctx)
	}
	defer tx.Rollback()
	prefix := historyClockCTE
	if rule.graph {
		prefix += historyRequestCTEs
	}
	q := prefix + `,
     candidates AS (` + rule.query + `)
SELECT c.key1, c.key2, c.terminal_state`
	if rule.observed {
		q += `, CASE WHEN h.terminal_state = c.terminal_state AND h.observed_at > 0
                          AND h.observed_at < (SELECT cutoff FROM retention_clock) THEN 1 ELSE 0 END`
	}
	q += `
FROM candidates c`
	args := []any{cutoff}
	if rule.observed {
		q += `
         LEFT JOIN history_terminal_observed h ON h.kind = ? AND h.key1 = c.key1 AND h.key2 = c.key2`
		args = append(args, rule.kind)
	}
	q += `
WHERE NOT EXISTS (SELECT 1 FROM history_content_pruned p WHERE p.kind = ? AND p.key1 = c.key1 AND p.key2 = c.key2)`
	args = append(args, rule.kind)
	if rule.observed {
		q += `
  AND (h.kind IS NULL OR h.terminal_state <> c.terminal_state OR h.observed_at <= 0
       OR (c.expired = 1 AND h.observed_at < (SELECT cutoff FROM retention_clock)))`
	}
	q += "\nORDER BY c.key1, c.key2\nLIMIT ?"
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, historyPruneError(ctx)
	}
	type candidate struct {
		key1, key2, state string
		prune             bool
	}
	candidates := make([]candidate, 0, limit)
	for rows.Next() {
		c := candidate{prune: true}
		dest := []any{&c.key1, &c.key2, &c.state}
		if rule.observed {
			dest = append(dest, &c.prune)
		}
		if err = rows.Scan(dest...); err != nil {
			rows.Close()
			return 0, 0, historyPruneError(ctx)
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return 0, 0, historyPruneError(ctx)
	}
	pruned := 0
	for _, c := range candidates {
		if !c.prune {
			// outbox没有送达时间，image_drafts没有终态更新时间：不能拿created_at顶替。
			// 仅保存首次观察的终态；若终态发生变化重新起算，不在每日扫描时刷新。
			_, err = tx.ExecContext(ctx, `INSERT INTO history_terminal_observed(kind, key1, key2, terminal_state, observed_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(kind, key1, key2) DO UPDATE SET terminal_state = excluded.terminal_state, observed_at = excluded.observed_at`,
				rule.kind, c.key1, c.key2, c.state, now)
			if err != nil {
				return 0, 0, historyPruneError(ctx)
			}
			continue
		}
		for _, q := range rule.updates {
			if _, err = tx.ExecContext(ctx, q, c.key1, c.key2); err != nil {
				return 0, 0, historyPruneError(ctx)
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO history_content_pruned(kind, key1, key2, pruned_at)
VALUES (?, ?, ?, ?)`, rule.kind, c.key1, c.key2, now); err != nil {
			return 0, 0, historyPruneError(ctx)
		}
		pruned++
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, historyPruneError(ctx)
	}
	return len(candidates), pruned, nil
}

// 保留范围：sessions/meta/origins/cursors、所有原主键与业务状态、回执、防重哈希、
// observer_path/observer_offset/next_probe、审计元数据、附件实体与加密草稿文件均不改动。
// session_presentation由主代理按独立内容时钟处理；mobile_tasks等旧表在当前仓库中
// 没有可核验的写入状态机，保持原样，不把未知failed/done名称猜成可清理终态。
