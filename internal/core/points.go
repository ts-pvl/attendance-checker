package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"attendance/internal/store"
)

// Points for social activities, given by staff: to every student checked in
// right now, or to one student. Entries are never deleted; a mistake is
// cancelled (cancelled_at), and every change is in the journal.

const MaxPoints = 1000

var (
	ErrBadPoints  = fmt.Errorf("points must be a whole number from 1 to %d", MaxPoints)
	ErrNoReason   = errors.New("a reason is required")
	ErrNobodyHere = errors.New("nobody is checked in right now")
	ErrNoEntry    = errors.New("no such points entry, or it is already cancelled")
)

type PointsEntry struct {
	ID, StudentID int64
	Student       string
	Points        int
	Reason        string
	Batch         int64 // 0 for points given to one student
	Actor         string
	Created       time.Time
	Cancelled     time.Time // zero while it counts
	CancelledBy   string
}

// PointsBatch is one award to everyone present.
type PointsBatch struct {
	Batch     int64
	Points    int
	Reason    string
	Actor     string
	Created   time.Time
	Students  int // entries that still count
	Cancelled bool
}

func checkPoints(points int, reason string) (string, error) {
	if points < 1 || points > MaxPoints {
		return "", ErrBadPoints
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return "", ErrNoReason
	}
	if len([]rune(reason)) > 300 {
		reason = string([]rune(reason)[:300])
	}
	return reason, nil
}

// AwardPresent gives points to every student with an open visit now (not
// archived), as one batch. It returns how many students got them.
func AwardPresent(ctx context.Context, db *store.DB, now time.Time, points int, reason, actor string) (int, error) {
	reason, err := checkPoints(points, reason)
	if err != nil {
		return 0, err
	}
	n := 0
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT p.student_id FROM presence p JOIN students s ON s.id = p.student_id
			WHERE p.ended_at IS NULL AND s.archived = 0 ORDER BY p.student_id`)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrNobodyHere
		}
		var batch int64
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, "INSERT INTO points(student_id, points, reason, batch, actor, created_at) VALUES(?, ?, ?, ?, ?, ?)",
				id, points, reason, sql.NullInt64{Int64: batch, Valid: batch != 0}, actor, store.Ms(now))
			if err != nil {
				return err
			}
			if batch == 0 {
				batch, _ = res.LastInsertId()
				if _, err := tx.ExecContext(ctx, "UPDATE points SET batch = ? WHERE id = ?", batch, batch); err != nil {
					return err
				}
			}
		}
		n = len(ids)
		return store.Audit(ctx, tx, now, actor, "points_present", fmt.Sprintf("points:%d", batch),
			fmt.Sprintf("+%d x %d students: %s", points, n, reason))
	})
	return n, err
}

// AwardStudent gives points to one student.
func AwardStudent(ctx context.Context, db *store.DB, now time.Time, studentID int64, points int, reason, actor string) error {
	reason, err := checkPoints(points, reason)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := studentByID(ctx, tx, studentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO points(student_id, points, reason, actor, created_at) VALUES(?, ?, ?, ?, ?)",
			studentID, points, reason, actor, store.Ms(now)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "points_student", fmt.Sprintf("student:%d", studentID), fmt.Sprintf("+%d: %s", points, reason))
	})
}

// CancelPoints cancels one entry. It returns the student it belonged to.
func CancelPoints(ctx context.Context, db *store.DB, now time.Time, id int64, actor string) (int64, error) {
	var student int64
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		var points int
		var reason string
		err := tx.QueryRowContext(ctx, "SELECT student_id, points, reason FROM points WHERE id = ? AND cancelled_at IS NULL", id).Scan(&student, &points, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoEntry
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE points SET cancelled_at = ?, cancelled_by = ? WHERE id = ?", store.Ms(now), actor, id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "points_cancel", fmt.Sprintf("student:%d", student), fmt.Sprintf("-%d: %s", points, reason))
	})
	return student, err
}

// CancelBatch cancels what still counts of one award to everyone present.
func CancelBatch(ctx context.Context, db *store.DB, now time.Time, batch int64, actor string) (int, error) {
	n := 0
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE points SET cancelled_at = ?, cancelled_by = ? WHERE batch = ? AND cancelled_at IS NULL", store.Ms(now), actor, batch)
		if err != nil {
			return err
		}
		k, _ := res.RowsAffected()
		if k == 0 {
			return ErrNoEntry
		}
		n = int(k)
		return store.Audit(ctx, tx, now, actor, "points_cancel_batch", fmt.Sprintf("points:%d", batch), fmt.Sprintf("%d entries", n))
	})
	return n, err
}

// StudentPoints returns a student's entries, newest first, and the total
// of those that count.
func StudentPoints(ctx context.Context, q store.Querier, studentID int64) ([]PointsEntry, int, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, student_id, points, reason, COALESCE(batch, 0), actor, created_at, COALESCE(cancelled_at, 0), COALESCE(cancelled_by, '')
		FROM points WHERE student_id = ? ORDER BY created_at DESC, id DESC`, studentID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []PointsEntry
	total := 0
	for rows.Next() {
		var e PointsEntry
		var created, cancelled int64
		if err := rows.Scan(&e.ID, &e.StudentID, &e.Points, &e.Reason, &e.Batch, &e.Actor, &created, &cancelled, &e.CancelledBy); err != nil {
			return nil, 0, err
		}
		e.Created = store.FromMs(created)
		if cancelled != 0 {
			e.Cancelled = store.FromMs(cancelled)
		} else {
			total += e.Points
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// PointsTotals returns every student's total of points that count.
func PointsTotals(ctx context.Context, q store.Querier) (map[int64]int, error) {
	rows, err := q.QueryContext(ctx, "SELECT student_id, SUM(points) FROM points WHERE cancelled_at IS NULL GROUP BY student_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Batches returns the awards to everyone present made in [from, to), newest first.
func Batches(ctx context.Context, q store.Querier, from, to time.Time) ([]PointsBatch, error) {
	rows, err := q.QueryContext(ctx, `SELECT batch, MIN(points), MIN(reason), MIN(actor), MIN(created_at),
			SUM(CASE WHEN cancelled_at IS NULL THEN 1 ELSE 0 END)
		FROM points WHERE batch IS NOT NULL AND created_at >= ? AND created_at < ?
		GROUP BY batch ORDER BY MIN(created_at) DESC, batch DESC`, store.Ms(from), store.Ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PointsBatch
	for rows.Next() {
		var b PointsBatch
		var created int64
		if err := rows.Scan(&b.Batch, &b.Points, &b.Reason, &b.Actor, &created, &b.Students); err != nil {
			return nil, err
		}
		b.Created, b.Cancelled = store.FromMs(created), b.Students == 0
		out = append(out, b)
	}
	return out, rows.Err()
}
