package help

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"attendance/internal/store"
)

// One import per week: the answers as typed are kept, and resolved every
// time they are shown, so a new login (a sync, a student edit) or a new
// alias changes the counts of every week at once.

type Week struct {
	ID      int64
	Week    string // the Monday, YYYY-MM-DD
	File    string
	Count   int // answers in the file
	Actor   string
	Created time.Time
}

var ErrBadWeek = errors.New("the week must be a Monday, YYYY-MM-DD")

// Import stores a week's answers, replacing an earlier import of that week.
func Import(ctx context.Context, db *store.DB, now time.Time, week, fileName string, answers []Answer, actor string) (replaced bool, err error) {
	if d, err := time.Parse("2006-01-02", week); err != nil || d.Weekday() != time.Monday {
		return false, ErrBadWeek
	}
	if len([]rune(fileName)) > 200 {
		fileName = string([]rune(fileName)[:200])
	}
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM help_imports WHERE week = ?", week)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		replaced = n > 0
		res, err = tx.ExecContext(ctx, "INSERT INTO help_imports(week, file_name, answers, actor, created_at) VALUES(?, ?, ?, ?, ?)",
			week, fileName, len(answers), actor, store.Ms(now))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for _, a := range answers {
			peers, _ := json.Marshal(a.Peers)
			if _, err := tx.ExecContext(ctx, "INSERT INTO help_answers(import_id, row, respondent, name, peers) VALUES(?, ?, ?, ?, ?)",
				id, a.Row, a.Respondent, a.Name, string(peers)); err != nil {
				return err
			}
		}
		details := fmt.Sprintf("week %s: %d answers from %s", week, len(answers), fileName)
		if replaced {
			details += " (replaced the earlier import)"
		}
		return store.Audit(ctx, tx, now, actor, "help_import", "", details)
	})
	return replaced, err
}

// Delete removes a week's import.
func Delete(ctx context.Context, db *store.DB, now time.Time, week, actor string) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM help_imports WHERE week = ?", week)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrBadWeek
		}
		return store.Audit(ctx, tx, now, actor, "help_delete", "", "week "+week)
	})
}

// Weeks lists the imported weeks, newest first.
func Weeks(ctx context.Context, q store.Querier) ([]Week, error) {
	rows, err := q.QueryContext(ctx, "SELECT id, week, file_name, answers, actor, created_at FROM help_imports ORDER BY week DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Week
	for rows.Next() {
		var w Week
		var created int64
		if err := rows.Scan(&w.ID, &w.Week, &w.File, &w.Count, &w.Actor, &created); err != nil {
			return nil, err
		}
		w.Created = store.FromMs(created)
		out = append(out, w)
	}
	return out, rows.Err()
}

// LoadAnswers returns one import's answers in file order.
func LoadAnswers(ctx context.Context, q store.Querier, importID int64) ([]Answer, error) {
	rows, err := q.QueryContext(ctx, "SELECT row, respondent, name, peers FROM help_answers WHERE import_id = ? ORDER BY row, id", importID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Answer
	for rows.Next() {
		var a Answer
		var peers string
		if err := rows.Scan(&a.Row, &a.Respondent, &a.Name, &peers); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(peers), &a.Peers); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// People are everyone who can be named: students with a login (by their
// name) and platform users (by the name on the platform).
func People(ctx context.Context, q store.Querier) ([]Person, error) {
	rows, err := q.QueryContext(ctx, `SELECT login, name FROM students WHERE login <> ''
		UNION ALL SELECT login, trim(last_name || ' ' || first_name) FROM platform_users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		var name string
		if err := rows.Scan(&p.Login, &name); err != nil {
			return nil, err
		}
		if name != "" {
			p.Names = []string{name}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Aliases returns the logins staff gave to typed text (normalised).
func Aliases(ctx context.Context, q store.Querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT typed, login FROM help_aliases")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

var ErrUnknownLogin = errors.New("no student or platform user has this login")

// SetAlias says that typed means login; an empty login removes the alias.
// The login must be known (m.Known).
func SetAlias(ctx context.Context, db *store.DB, m *Matcher, now time.Time, typed, login, actor string) error {
	key := Normalize(typed)
	if key == "" {
		return ErrUnknownLogin
	}
	login = strings.TrimSpace(login)
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if login == "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM help_aliases WHERE typed = ?", key); err != nil {
				return err
			}
			return store.Audit(ctx, tx, now, actor, "help_alias", "", fmt.Sprintf("%q: removed", key))
		}
		l, ok := m.Known(login)
		if !ok {
			return ErrUnknownLogin
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO help_aliases(typed, login, actor, created_at) VALUES(?, ?, ?, ?)
			ON CONFLICT(typed) DO UPDATE SET login = excluded.login, actor = excluded.actor, created_at = excluded.created_at`,
			key, l, actor, store.Ms(now)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "help_alias", "", fmt.Sprintf("%q -> %s", key, l))
	})
}

// LoadMatcher builds a matcher from the database.
func LoadMatcher(ctx context.Context, q store.Querier) (*Matcher, error) {
	people, err := People(ctx, q)
	if err != nil {
		return nil, err
	}
	aliases, err := Aliases(ctx, q)
	if err != nil {
		return nil, err
	}
	return NewMatcher(people, aliases), nil
}

// WeekTally is one imported week, counted.
type WeekTally struct {
	Week
	Tally
}

// TallyAll counts every imported week, newest first.
func TallyAll(ctx context.Context, q store.Querier, m *Matcher) ([]WeekTally, error) {
	weeks, err := Weeks(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]WeekTally, 0, len(weeks))
	for _, w := range weeks {
		answers, err := LoadAnswers(ctx, q, w.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, WeekTally{w, Count(answers, m)})
	}
	return out, nil
}
