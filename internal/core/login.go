package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"attendance/internal/store"
)

// A student's login on the learning platform links the student to the
// platform's data (internal/platform) and to the peer-help form
// (internal/help). Logins are compared case-insensitively.

var loginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var ErrBadPlatformLogin = errors.New("a login has only Latin letters, digits, '.', '_' and '-' (at most 64)")

// LoginTakenError: the login already belongs to another student.
type LoginTakenError struct{ Login, Owner string }

func (e *LoginTakenError) Error() string {
	return fmt.Sprintf("login %s already belongs to %s", e.Login, e.Owner)
}

// lookalikes are Cyrillic letters that look like Latin ones, as a login
// typed with a Cyrillic keyboard layout has them.
var lookalikes = strings.NewReplacer(
	"А", "A", "В", "B", "Е", "E", "К", "K", "М", "M", "Н", "H", "О", "O", "Р", "P", "С", "C", "Т", "T", "Х", "X", "У", "Y", "І", "I", "Ј", "J", "Ѕ", "S",
	"а", "a", "е", "e", "о", "o", "р", "p", "с", "c", "у", "y", "х", "x", "к", "k", "і", "i", "ј", "j", "ѕ", "s",
)

// LatinLookalikes replaces Cyrillic letters that look like Latin ones with
// the Latin ones.
func LatinLookalikes(s string) string { return lookalikes.Replace(s) }

// nameFold: the letters people write either way in a name: ё as е, and the
// Kazakh letters as the Russian ones written instead of them.
var nameFold = strings.NewReplacer("ё", "е", "ә", "а", "ғ", "г", "қ", "к", "ң", "н", "ө", "о", "ұ", "у", "ү", "у", "һ", "х", "і", "и")

// NameWords splits a name into words for comparing names: lower case, with
// ё as е, the Kazakh letters as their Russian look-alikes (ә as а, і as и,
// ұ and ү as у, …) and Cyrillic letters that look like Latin ones as Latin,
// so "Нұрлан", "Нурлан" and a "Нурлaн" with a Latin a are the same word.
// Words are split at spaces, hyphens, dots and commas.
func NameWords(s string) []string {
	s = LatinLookalikes(nameFold.Replace(strings.ToLower(s)))
	return strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '.' || r == ',' })
}

// CleanLogin trims a login and checks it. "" is valid (no login). Cyrillic
// look-alikes become Latin letters: logins have no Cyrillic.
func CleanLogin(s string) (string, error) {
	s = LatinLookalikes(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	if s == "" || loginRe.MatchString(s) {
		return s, nil
	}
	return "", ErrBadPlatformLogin
}

// loginOwner returns the student (other than self) who has login, or 0.
func loginOwner(ctx context.Context, q store.Querier, login string, self int64) (int64, string, error) {
	var id int64
	var name string
	err := q.QueryRowContext(ctx, "SELECT id, name FROM students WHERE lower(login) = lower(?) AND login <> '' AND id <> ?", login, self).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, name, err
}

// SetLogin sets (or with "" clears) a student's platform login.
func SetLogin(ctx context.Context, db *store.DB, now time.Time, id int64, login, actor string) error {
	login, err := CleanLogin(login)
	if err != nil {
		return err
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		var old string
		if err := tx.QueryRowContext(ctx, "SELECT login FROM students WHERE id = ?", id).Scan(&old); err != nil {
			return err
		}
		if old == login {
			return nil
		}
		if login != "" {
			if other, name, err := loginOwner(ctx, tx, login, id); err != nil {
				return err
			} else if other != 0 {
				return &LoginTakenError{login, name}
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE students SET login = ? WHERE id = ?", login, id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "set_login", fmt.Sprintf("student:%d", id), fmt.Sprintf("%q -> %q", old, login))
	})
}
