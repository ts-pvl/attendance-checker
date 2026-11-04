package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"attendance/internal/store"
)

// CorrectionInput is a staff change to a visit. Corrections are separate,
// audited rows; presence rows are never modified by staff.
type CorrectionInput struct {
	Action    string // add, edit, delete
	Target    string // visit key ("p12", "m3") for edit and delete
	StudentID int64
	Host      string
	Start     time.Time
	End       time.Time
	Reason    string
	Actor     string
}

var (
	ErrReasonRequired = errors.New("a reason is required")
	ErrBadTimes       = errors.New("the end must not be before the start")
	ErrNoSuchVisit    = errors.New("no such visit for this student")
)

// AddCorrection validates and stores a correction and writes the audit log.
func AddCorrection(ctx context.Context, db *store.DB, now time.Time, in CorrectionInput) (int64, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	in.Host = strings.TrimSpace(in.Host)
	if in.Reason == "" {
		return 0, ErrReasonRequired
	}
	switch in.Action {
	case "add", "edit":
		if in.Start.IsZero() || in.End.IsZero() || in.End.Before(in.Start) {
			return 0, ErrBadTimes
		}
		if in.End.Sub(in.Start) > 48*time.Hour {
			return 0, errors.New("a visit cannot be longer than 48 hours")
		}
	case "delete":
	default:
		return 0, fmt.Errorf("unknown correction action %q", in.Action)
	}
	var id int64
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := studentByID(ctx, tx, in.StudentID); err != nil {
			return err
		}
		if in.Action != "add" {
			if err := checkTarget(ctx, tx, in.Target, in.StudentID); err != nil {
				return err
			}
		} else {
			in.Target = ""
		}
		var start, end any
		if in.Action != "delete" {
			start, end = store.Ms(in.Start), store.Ms(in.End)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO corrections(action, target, student_id, host, start_at, end_at, reason, actor, created_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.Action, in.Target, in.StudentID, in.Host, start, end, in.Reason, in.Actor, store.Ms(now))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		details := fmt.Sprintf("target=%s host=%s reason=%q", in.Target, in.Host, in.Reason)
		if in.Action != "delete" {
			details = fmt.Sprintf("target=%s host=%s start=%s end=%s reason=%q", in.Target, in.Host,
				in.Start.Format(time.RFC3339), in.End.Format(time.RFC3339), in.Reason)
		}
		return store.Audit(ctx, tx, now, in.Actor, "correction_"+in.Action, fmt.Sprintf("student:%d", in.StudentID), details)
	})
	return id, err
}

func checkTarget(ctx context.Context, tx *sql.Tx, target string, studentID int64) error {
	if len(target) < 2 {
		return ErrNoSuchVisit
	}
	n, err := strconv.ParseInt(target[1:], 10, 64)
	if err != nil {
		return ErrNoSuchVisit
	}
	var owner int64
	switch target[0] {
	case 'p':
		err = tx.QueryRowContext(ctx, "SELECT student_id FROM presence WHERE id = ?", n).Scan(&owner)
	case 'm':
		err = tx.QueryRowContext(ctx, "SELECT student_id FROM corrections WHERE id = ? AND action = 'add'", n).Scan(&owner)
	default:
		return ErrNoSuchVisit
	}
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != studentID) {
		return ErrNoSuchVisit
	}
	return err
}
