package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"attendance/internal/store"
	"attendance/internal/token"
)

// Enrollment results.
const (
	EnrollLinked       = "linked"
	EnrollAlreadySame  = "already_same"    // this account is already linked to this student
	EnrollTGTaken      = "tg_linked_other" // this account is linked to someone else
	EnrollStudentTaken = "student_linked_other"
	EnrollInvalid      = "code_invalid"
	EnrollUsed         = "code_used"
	EnrollExpired      = "code_expired"
)

var ErrAlreadyLinked = errors.New("student already has a linked Telegram account; unlink first")

// TGUser is the sender of a Telegram message.
type TGUser struct {
	ID       int64
	Username string
	Name     string
}

type EnrollReply struct {
	Result  string
	Student *Student // the student the code belongs to, or the account's current student
}

// Enroll handles "/start <code>": it links the sender's Telegram account to
// the student the one-time code was issued for.
func Enroll(ctx context.Context, db *store.DB, now time.Time, u TGUser, code string) (EnrollReply, error) {
	var reply EnrollReply
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		reply = EnrollReply{}
		// Every attempt goes to the raw event log, successful or not.
		ev := &Event{At: now, Kind: KindEnroll, TGUserID: u.ID}
		var (
			studentID     int64
			expires       int64
			used, revoked sql.NullInt64
		)
		err := tx.QueryRowContext(ctx, "SELECT student_id, expires_at, used_at, revoked_at FROM enroll_codes WHERE code = ?", code).
			Scan(&studentID, &expires, &used, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			reply.Result = EnrollInvalid
			return recordEnroll(ctx, tx, ev, &reply)
		}
		if err != nil {
			return err
		}
		st, err := studentByID(ctx, tx, studentID)
		if err != nil {
			return err
		}
		current, err := LinkedStudent(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		switch {
		case current != nil && current.ID == st.ID:
			reply = EnrollReply{Result: EnrollAlreadySame, Student: current}
		case revoked.Valid || st.Archived:
			reply = EnrollReply{Result: EnrollInvalid}
		case used.Valid:
			reply = EnrollReply{Result: EnrollUsed}
		case now.After(store.FromMs(expires)):
			reply = EnrollReply{Result: EnrollExpired}
		case current != nil:
			reply = EnrollReply{Result: EnrollTGTaken, Student: current}
		}
		if reply.Result != "" {
			return recordEnroll(ctx, tx, ev, &reply)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM telegram_links WHERE student_id = ?", st.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			reply = EnrollReply{Result: EnrollStudentTaken}
			return recordEnroll(ctx, tx, ev, &reply)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE enroll_codes SET used_at = ?, used_by_tg = ? WHERE code = ?", store.Ms(now), u.ID, code); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_links(student_id, tg_user_id, tg_username, tg_name, linked_at)
			VALUES(?, ?, ?, ?, ?)`, st.ID, u.ID, u.Username, u.Name, store.Ms(now)); err != nil {
			return err
		}
		reply = EnrollReply{Result: EnrollLinked, Student: st}
		return recordEnroll(ctx, tx, ev, &reply)
	})
	return reply, err
}

func recordEnroll(ctx context.Context, tx *sql.Tx, ev *Event, r *EnrollReply) error {
	ev.Result = r.Result
	if r.Student != nil {
		ev.StudentID = r.Student.ID
	}
	return record(ctx, tx, ev)
}

func studentByID(ctx context.Context, q store.Querier, id int64) (*Student, error) {
	var s Student
	err := q.QueryRowContext(ctx, "SELECT id, name, grp, archived FROM students WHERE id = ?", id).
		Scan(&s.ID, &s.Name, &s.Group, &s.Archived)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// StudentByID returns a student or sql.ErrNoRows.
func StudentByID(ctx context.Context, q store.Querier, id int64) (*Student, error) {
	return studentByID(ctx, q, id)
}

// GenerateCode issues a new one-time enrollment code for a student and
// revokes any earlier unused ones.
func GenerateCode(ctx context.Context, db *store.DB, now time.Time, studentID int64, ttl time.Duration, actor string) (string, error) {
	code := token.NewEnrollCode()
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		st, err := studentByID(ctx, tx, studentID)
		if err != nil {
			return err
		}
		if st.Archived {
			return fmt.Errorf("student %d is archived", studentID)
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM telegram_links WHERE student_id = ?", studentID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrAlreadyLinked
		}
		if _, err := tx.ExecContext(ctx, "UPDATE enroll_codes SET revoked_at = ? WHERE student_id = ? AND used_at IS NULL AND revoked_at IS NULL",
			store.Ms(now), studentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO enroll_codes(code, student_id, created_at, expires_at) VALUES(?, ?, ?, ?)",
			code, studentID, store.Ms(now), store.Ms(now.Add(ttl))); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "enroll_code", fmt.Sprintf("student:%d", studentID), st.Name)
	})
	return code, err
}

// Unlink removes a student's Telegram link. The student can then be given a
// new code. Open visits stay open and close normally.
func Unlink(ctx context.Context, db *store.DB, now time.Time, studentID int64, actor, reason string) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		var tgID int64
		var tgName, tgUser string
		err := tx.QueryRowContext(ctx, "SELECT tg_user_id, tg_name, tg_username FROM telegram_links WHERE student_id = ?", studentID).
			Scan(&tgID, &tgName, &tgUser)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("student %d has no linked account", studentID)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM telegram_links WHERE student_id = ?", studentID); err != nil {
			return err
		}
		details := fmt.Sprintf("tg_user_id=%d name=%q username=%q reason=%q", tgID, tgName, tgUser, reason)
		return store.Audit(ctx, tx, now, actor, "unlink", fmt.Sprintf("student:%d", studentID), details)
	})
}

// StartLink builds the Telegram deep link for a code or token.
func StartLink(botUsername, param string) string {
	return "https://t.me/" + botUsername + "?start=" + param
}
