package core

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"attendance/internal/store"
)

type ImportResult struct {
	Added, Updated, Unchanged int
	Warnings                  []string
}

var (
	nameHeaders  = []string{"name", "full name", "fullname", "student", "имя", "фио", "студент", "ученик"}
	groupHeaders = []string{"group", "grp", "class", "группа", "класс", "поток"}
	idHeaders    = []string{"id", "external_id", "student_id", "ид"}
	loginHeaders = []string{"login", "логин", "username", "platform login", "логин на платформе"}
)

// ImportStudents reads a CSV (comma, semicolon or tab separated, optional
// UTF-8 BOM) and adds or updates students. Columns are recognised by header
// (name / group / id / login, in English or Russian); without a header the
// columns are name[,group]. Rows match existing students by id when given,
// else by platform login, else by exact name. A login column sets the
// student's platform login. Students missing from the file are left alone
// (archive them explicitly).
//
// Before logins had their own column, a "login" column was read as the id;
// such students are still found by that id.
func ImportStudents(ctx context.Context, db *store.DB, now time.Time, r io.Reader, actor string) (ImportResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return ImportResult{}, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma = sniffDelimiter(data)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return ImportResult{}, err
	}
	if len(records) == 0 {
		return ImportResult{}, errors.New("the file is empty")
	}
	nameCol, groupCol, idCol, loginCol := 0, 1, -1, -1
	firstLine := 1
	if n, g, i, l, ok := headerColumns(records[0]); ok {
		nameCol, groupCol, idCol, loginCol = n, g, i, l
		records = records[1:]
		firstLine = 2
	}
	var res ImportResult
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		res = ImportResult{}
		for i, rec := range records {
			line := firstLine + i
			name := strings.Join(strings.Fields(field(rec, nameCol)), " ")
			if name == "" {
				if strings.TrimSpace(strings.Join(rec, "")) != "" {
					res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: no name, skipped", line))
				}
				continue
			}
			group := strings.TrimSpace(field(rec, groupCol))
			extID := strings.TrimSpace(field(rec, idCol))
			login, err := CleanLogin(field(rec, loginCol))
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: %v", line, err))
				continue
			}
			outcome, err := upsertStudent(ctx, tx, now, name, group, extID, login)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: %v", line, err))
				continue
			}
			switch outcome {
			case "added":
				res.Added++
			case "updated":
				res.Updated++
			default:
				res.Unchanged++
			}
		}
		return store.Audit(ctx, tx, now, actor, "import_students", "",
			fmt.Sprintf("added=%d updated=%d unchanged=%d warnings=%d", res.Added, res.Updated, res.Unchanged, len(res.Warnings)))
	})
	return res, err
}

func field(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}

func sniffDelimiter(data []byte) rune {
	first, _, _ := bufio.NewReader(bytes.NewReader(data)).ReadLine()
	best, bestN := ',', strings.Count(string(first), ",")
	for _, d := range []rune{';', '\t'} {
		if n := strings.Count(string(first), string(d)); n > bestN {
			best, bestN = d, n
		}
	}
	return best
}

func headerColumns(rec []string) (name, group, id, login int, ok bool) {
	name, group, id, login = -1, -1, -1, -1
	for i, h := range rec {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case contains(nameHeaders, h):
			name = i
		case contains(groupHeaders, h):
			group = i
		case contains(idHeaders, h):
			id = i
		case contains(loginHeaders, h):
			login = i
		}
	}
	return name, group, id, login, name >= 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func upsertStudent(ctx context.Context, tx *sql.Tx, now time.Time, name, group, extID, login string) (string, error) {
	var id int64
	var oldName, oldGroup, oldLogin string
	var err error
	cols := "SELECT id, name, grp, login FROM students WHERE "
	switch {
	case extID != "":
		err = tx.QueryRowContext(ctx, cols+"external_id = ?", extID).Scan(&id, &oldName, &oldGroup, &oldLogin)
	case login != "":
		err = tx.QueryRowContext(ctx, cols+"lower(login) = lower(?) AND login <> ''", login).Scan(&id, &oldName, &oldGroup, &oldLogin)
		if errors.Is(err, sql.ErrNoRows) { // imported before logins had their own column
			err = tx.QueryRowContext(ctx, cols+"external_id = ?", login).Scan(&id, &oldName, &oldGroup, &oldLogin)
		}
		if err == nil {
			// The login says it is this student. If the name is another
			// student's, the row is ambiguous: change nothing.
			if other, _, _, _, nerr := studentByName(ctx, tx, name); nerr == nil && other != id {
				return "", fmt.Errorf("login %s belongs to %s, but %s is another student", login, oldName, name)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			id, oldName, oldGroup, oldLogin, err = studentByName(ctx, tx, name)
		}
	default:
		id, oldName, oldGroup, oldLogin, err = studentByName(ctx, tx, name)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var ext any
		if extID != "" {
			ext = extID
		}
		if login != "" {
			if other, owner, err := loginOwner(ctx, tx, login, 0); err != nil {
				return "", err
			} else if other != 0 {
				return "", &LoginTakenError{login, owner}
			}
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO students(name, grp, external_id, login, created_at) VALUES(?, ?, ?, ?, ?)", name, group, ext, login, store.Ms(now))
		return "added", err
	}
	if err != nil {
		return "", err
	}
	if login == "" || strings.EqualFold(login, oldLogin) {
		login = oldLogin // none in the file, or the same one: keep ours as it is spelt
	}
	if oldName == name && oldGroup == group && oldLogin == login {
		return "unchanged", nil
	}
	if login != oldLogin && login != "" {
		if other, owner, err := loginOwner(ctx, tx, login, id); err != nil {
			return "", err
		} else if other != 0 {
			return "", &LoginTakenError{login, owner}
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE students SET name = ?, grp = ?, login = ? WHERE id = ?", name, group, login, id)
	return "updated", err
}

// studentByName finds the one student with this name (case-insensitive).
func studentByName(ctx context.Context, tx *sql.Tx, name string) (id int64, oldName, oldGroup, oldLogin string, err error) {
	{
		rows, qerr := tx.QueryContext(ctx, "SELECT id, name, grp, login FROM students WHERE lower(name) = lower(?)", name)
		if qerr != nil {
			return 0, "", "", "", qerr
		}
		n := 0
		for rows.Next() {
			n++
			if err := rows.Scan(&id, &oldName, &oldGroup, &oldLogin); err != nil {
				rows.Close()
				return 0, "", "", "", err
			}
		}
		rows.Close()
		switch n {
		case 0:
			return 0, "", "", "", sql.ErrNoRows
		case 1:
			return id, oldName, oldGroup, oldLogin, nil
		default:
			return 0, "", "", "", fmt.Errorf("%d students are named %q; add an id or login column to tell them apart", n, name)
		}
	}
}

// AddStudent creates one student.
func AddStudent(ctx context.Context, db *store.DB, now time.Time, name, group, actor string) (int64, error) {
	return AddStudentLogin(ctx, db, now, name, group, "", actor)
}

// AddStudentLogin adds a student with a platform login ("" for none). A
// login that is malformed or another student's refuses the whole student.
func AddStudentLogin(ctx context.Context, db *store.DB, now time.Time, name, group, login, actor string) (int64, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return 0, errors.New("name is required")
	}
	login, err := CleanLogin(login)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		if login != "" {
			if other, owner, err := loginOwner(ctx, tx, login, 0); err != nil {
				return err
			} else if other != 0 {
				return &LoginTakenError{login, owner}
			}
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO students(name, grp, login, created_at) VALUES(?, ?, ?, ?)", name, strings.TrimSpace(group), login, store.Ms(now))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		details := name
		if login != "" {
			details += fmt.Sprintf(", login %q", login)
		}
		return store.Audit(ctx, tx, now, actor, "add_student", fmt.Sprintf("student:%d", id), details)
	})
	return id, err
}

// UpdateStudent changes a student's name and group.
func UpdateStudent(ctx context.Context, db *store.DB, now time.Time, id int64, name, group, actor string) error {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return errors.New("name is required")
	}
	return db.InTx(ctx, func(tx *sql.Tx) error {
		old, err := studentByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE students SET name = ?, grp = ? WHERE id = ?", name, strings.TrimSpace(group), id); err != nil {
			return err
		}
		return store.Audit(ctx, tx, now, actor, "edit_student", fmt.Sprintf("student:%d", id),
			fmt.Sprintf("%q/%q -> %q/%q", old.Name, old.Group, name, group))
	})
}

// SetArchived archives a leaver (or restores them). Archived students cannot
// check in or enroll, but their history stays.
func SetArchived(ctx context.Context, db *store.DB, now time.Time, id int64, archived bool, actor string) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE students SET archived = ? WHERE id = ?", archived, id); err != nil {
			return err
		}
		if archived {
			if _, err := tx.ExecContext(ctx, "UPDATE enroll_codes SET revoked_at = ? WHERE student_id = ? AND used_at IS NULL AND revoked_at IS NULL",
				store.Ms(now), id); err != nil {
				return err
			}
		}
		action := "unarchive"
		if archived {
			action = "archive"
		}
		return store.Audit(ctx, tx, now, actor, action, fmt.Sprintf("student:%d", id), "")
	})
}
