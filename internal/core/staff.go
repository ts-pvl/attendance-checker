package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"

	"attendance/internal/store"
)

type Staff struct {
	ID       int64
	Username string
	Role     string // admin or teacher
	Disabled bool
}

func (s *Staff) IsAdmin() bool { return s != nil && s.Role == "admin" }

const MinPasswordLen = 10

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{2,32}$`)

var ErrBadLogin = errors.New("wrong username or password")

// dummyHash makes failed logins for unknown users cost the same as for known ones.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser"), bcrypt.DefaultCost)

func CreateStaff(ctx context.Context, db *store.DB, now time.Time, username, password, role string) error {
	if !usernameRe.MatchString(username) {
		return errors.New("username: 2-32 characters, lowercase letters, digits, . _ -")
	}
	if role != "admin" && role != "teacher" {
		return errors.New("role must be admin or teacher")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO staff(username, pw_hash, role, created_at) VALUES(?, ?, ?, ?)",
			username, hash, role, store.Ms(now)); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, "cli", "staff_add", username, role)
	})
}

func hashPassword(pw string) (string, error) {
	if len([]rune(pw)) < MinPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > 72 {
		return "", errors.New("password must be at most 72 bytes") // bcrypt limit
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// SetStaffPassword changes a password and logs the user out everywhere.
func SetStaffPassword(ctx context.Context, db *store.DB, now time.Time, username, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE staff SET pw_hash = ? WHERE username = ?", hash, username)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no staff user %q", username)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM staff_sessions WHERE staff_id = (SELECT id FROM staff WHERE username = ?)", username); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, "cli", "staff_password", username, "")
	})
}

// SetStaffDisabled disables or re-enables an account.
func SetStaffDisabled(ctx context.Context, db *store.DB, now time.Time, username string, disabled bool) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE staff SET disabled = ? WHERE username = ?", disabled, username)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no staff user %q", username)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM staff_sessions WHERE staff_id = (SELECT id FROM staff WHERE username = ?)", username); err != nil {
			return err
		}
		action := "staff_enable"
		if disabled {
			action = "staff_disable"
		}
		return store.Audit(ctx, tx, now, "cli", action, username, "")
	})
}

func ListStaff(ctx context.Context, db *store.DB) ([]Staff, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, username, role, disabled FROM staff ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Staff
	for rows.Next() {
		var s Staff
		if err := rows.Scan(&s.ID, &s.Username, &s.Role, &s.Disabled); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Authenticate checks a username and password.
func Authenticate(ctx context.Context, db *store.DB, username, password string) (*Staff, error) {
	var s Staff
	var hash string
	err := db.QueryRowContext(ctx, "SELECT id, username, role, disabled, pw_hash FROM staff WHERE username = ?", username).
		Scan(&s.ID, &s.Username, &s.Role, &s.Disabled, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrBadLogin
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || s.Disabled {
		return nil, ErrBadLogin
	}
	return &s, nil
}
