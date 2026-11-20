package store

import (
	"context"
	"database/sql"
	"time"
)

type GroupFollowView struct {
	Tasks      []GroupFollowTask  `json:"tasks"`
	Candidates []GroupFollowInput `json:"candidates"`
}

func GroupFollowSnapshot(ctx context.Context, dir string) (GroupFollowView, error) {
	out := GroupFollowView{Tasks: []GroupFollowTask{}, Candidates: []GroupFollowInput{}}
	err := readOnlySnapshot(ctx, dir, func(ctx context.Context, tx *sql.Tx, tables map[string]bool) error {
		if !tables["group_follow_tasks"] {
			return nil
		}
		rows, err := tx.QueryContext(ctx, followTaskSelect+" AND g.expires>? ORDER BY g.created_at DESC LIMIT 100", time.Now().Unix())
		if err != nil {
			return err
		}
		for rows.Next() {
			t, e := scanGroupTask(rows)
			if e != nil {
				rows.Close()
				return e
			}
			out.Tasks = append(out.Tasks, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT "+followInputFields+" FROM group_follow_inbox WHERE state='candidate' AND expires>? AND EXISTS(SELECT 1 FROM group_follow_tasks g WHERE g.wire_key=group_follow_inbox.wire_key AND g.state='active' AND g.expires>strftime('%s','now')) ORDER BY created_at DESC LIMIT 20", time.Now().Unix())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, e := scanFollowInput(rows)
			if e != nil {
				return e
			}
			out.Candidates = append(out.Candidates, r)
		}
		return rows.Err()
	})
	return out, err
}
