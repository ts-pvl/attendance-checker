package help

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"attendance/internal/store"
)

// Every login and name here is invented. Expected results are worked out
// by hand from the rules in help.go's package comment.

var people = []Person{
	{Login: "jdoe", Names: []string{"Петров Иван"}},
	{Login: "asmith", Names: []string{"Smith Anna"}},
	{Login: "bkim", Names: []string{"Ким Борис"}},
	{Login: "mlee", Names: []string{"Ли Мария"}},
	{Login: "tnur", Names: []string{"Нуров Тимур"}},
	{Login: "aiko"},
	{Login: "dana42", Names: []string{"Сейт Дана"}},
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		" JDoe\u00a0": "jdoe",
		"@asmith":     "asmith",
		"«bkim»":      "bkim",
		"аsmith":      "asmith", // Cyrillic а
		"ВКIМ":        "bkim",   // Cyrillic В, К, М
		"tnur\u200b":  "tnur",
		"":            "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	m := NewMatcher(people, map[string]string{"petrov": "jdoe"})
	for _, c := range []struct {
		typed string
		want  Match
	}{
		{"jdoe", Match{Login: "jdoe", How: Exact}},
		{" JDoe ", Match{Login: "jdoe", How: Exact}},
		{"@asmith", Match{Login: "asmith", How: Exact}},
		{"аsmith", Match{Login: "asmith", How: Exact}},     // a Cyrillic а is not a typo
		{"jdoee", Match{Login: "jdoe", How: Typo}},         // one letter too many
		{"asmiht", Match{Login: "asmith", How: Typo}},      // two letters swapped
		{"bkin", Match{Login: "bkim", How: Typo}},          // one letter wrong, 4 letters
		{"dana4", Match{Login: "dana42", How: Typo}},       // one letter missing
		{"j doe", Match{Login: "jdoe", How: Typo}},         // a space inside
		{"asmxxh", Match{How: Unknown, Suggest: "asmith"}}, // two letters wrong: only a suggestion
		{"tnu", Match{How: Unknown, Suggest: "tnur"}},      // too short to correct on its own
		{"aik", Match{How: Unknown, Suggest: "aiko"}},
		{"petrov", Match{Login: "jdoe", How: Alias}},
		{"PETROV ", Match{Login: "jdoe", How: Alias}},
		{"Петров Иван", Match{Login: "jdoe", How: Name}},
		{"иван петров", Match{Login: "jdoe", How: Name}},
		{"Smith Anna", Match{Login: "asmith", How: Name}},
		{"Иван", Match{How: Unknown}}, // one word is not enough for a name
		{"zzzz", Match{How: Unknown}},
		{"   ", Match{How: Empty}},
	} {
		if got := m.Resolve(c.typed); got != c.want {
			t.Errorf("Resolve(%q) = %+v, want %+v", c.typed, got, c.want)
		}
	}
	// Two logins one letter away: no correction, no suggestion.
	m2 := NewMatcher([]Person{{Login: "abcd1"}, {Login: "abcd2"}}, nil)
	if got := m2.Resolve("abcd3"); got != (Match{How: Unknown}) {
		t.Errorf("ambiguous typo: %+v", got)
	}
	// An alias wins over an exact login (staff said so).
	m3 := NewMatcher(people, map[string]string{"aiko": "mlee"})
	if got := m3.Resolve("aiko"); got != (Match{Login: "mlee", How: Alias}) {
		t.Errorf("alias over exact: %+v", got)
	}
}

func TestCount(t *testing.T) {
	m := NewMatcher(people, nil)
	answers := []Answer{
		{Row: 2, Respondent: "jdoe", Peers: []string{"asmith", "bkim", "asmith ", "jdoe"}},
		{Row: 3, Respondent: "asmith", Peers: []string{"jdoe", "zzzz", "asmxxh"}},
		{Row: 4, Respondent: "BKim", Peers: []string{"jdoe", "asmith"}},
		{Row: 5, Name: "Нуров Тимур", Peers: []string{"jdoee", "mlee"}},
		{Row: 6, Respondent: "bkim ", Peers: []string{"mlee"}}, // bkim again: replaces row 4
		{Row: 7, Respondent: "dana42", Peers: []string{"dana4", "Петров Иван"}},
	}
	tl := Count(answers, m)
	// Row 4 is replaced by row 6. Counted: asmith 1 (row 2), bkim 1 (row 2),
	// jdoe 3 (rows 3, 5 as a typo, 7 by name), mlee 2 (rows 5, 6).
	if got := fmt.Sprint(tl.Counts); got != "map[asmith:1 bkim:1 jdoe:3 mlee:2]" {
		t.Errorf("counts: %s", got)
	}
	if tl.Answers != 6 || tl.Counted != 5 {
		t.Errorf("answers %d counted %d, want 6 and 5", tl.Answers, tl.Counted)
	}
	var got []string
	for _, x := range tl.Mentions {
		got = append(got, fmt.Sprintf("%d %s %s %s", x.Row, x.Typed, x.Status, x.Login))
	}
	want := []string{
		"2 asmith counted asmith", "2 bkim counted bkim", "2 asmith  repeat asmith", "2 jdoe self jdoe",
		"3 jdoe counted jdoe", "3 zzzz unresolved ", "3 asmxxh unresolved ",
		"4 jdoe superseded jdoe", "4 asmith superseded asmith",
		"5 jdoee counted jdoe", "5 mlee counted mlee",
		"6 mlee counted mlee",
		"7 dana4 self dana42", "7 Петров Иван counted jdoe",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("mentions:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if tl.Mentions[6].Suggest != "asmith" || tl.Mentions[9].How != Typo || tl.Mentions[13].How != Name {
		t.Errorf("details: %+v %+v %+v", tl.Mentions[6], tl.Mentions[9], tl.Mentions[13])
	}
	// An answer from someone we cannot identify still counts, once.
	tl = Count([]Answer{{Row: 2, Peers: []string{"mlee"}}, {Row: 3, Peers: []string{"mlee"}}}, m)
	if tl.Counts["mlee"] != 2 {
		t.Errorf("anonymous answers: %v", tl.Counts)
	}
}

const header = "Timestamp,Ваше ФИО,Ваш Login (с платформы),Peer #1 (Введите login человека),Peer #2 (Введите login человека),Peer #3 (Введите login человека),Что улучшить?\n"

func TestReadCSV(t *testing.T) {
	csv := "\xef\xbb\xbf" + header +
		"11/09/2026 18:00:01,Петров Иван,jdoe ,asmith,,bkim,всё хорошо\n" +
		"11/09/2026 18:05:00,Нуров Тимур,,mlee,jdoe,,\n" +
		",,,,,,\n" +
		"11/09/2026 18:07:00,Smith Anna,asmith,,,,нет\n"
	got, err := Read(strings.NewReader(csv), "answers.csv")
	if err != nil {
		t.Fatal(err)
	}
	want := `[{2 jdoe Петров Иван [asmith bkim]} {3  Нуров Тимур [mlee jdoe]} {5 asmith Smith Anna []}]`
	if fmt.Sprint(got) != want {
		t.Errorf("CSV:\n%v\nwant\n%s", got, want)
	}
	// Semicolons, and no peer columns.
	if got, err := Read(strings.NewReader(strings.ReplaceAll(header, ",", ";")+"x;Петров Иван;jdoe;asmith;;;\n"), "a.csv"); err != nil || len(got) != 1 || got[0].Peers[0] != "asmith" {
		t.Errorf("semicolons: %v %v", got, err)
	}
	if _, err := Read(strings.NewReader("name,login\nx,y\n"), "a.csv"); err != ErrNoPeerColumns {
		t.Errorf("no peer columns: %v", err)
	}
}

// buildXLSX writes a minimal workbook the way Google Sheets does: shared
// strings, a numeric timestamp, an inline string, and empty cells left out.
func buildXLSX(t *testing.T) []byte {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`)
	add("xl/workbook.xml", `<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Form Responses 1" sheetId="1" r:id="rId3"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="styles.xml"/><Relationship Id="rId3" Target="worksheets/sheet7.xml"/></Relationships>`)
	strs := []string{"Timestamp", "Ваше ФИО", "Ваш Login (с платформы)", "Peer #1", "Peer #2", "Peer #3", "Петров Иван", "jdoe", "asmith", "Нуров Тимур", "mlee"}
	var sst strings.Builder
	sst.WriteString(`<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for i, s := range strs {
		if i == 6 { // a rich-text string, in runs
			sst.WriteString(`<si><r><t>Петров</t></r><r><t xml:space="preserve"> Иван</t></r></si>`)
			continue
		}
		fmt.Fprintf(&sst, "<si><t>%s</t></si>", s)
	}
	sst.WriteString("</sst>")
	add("xl/sharedStrings.xml", sst.String())
	add("xl/worksheets/sheet7.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c><c r="E1" t="s"><v>4</v></c><c r="F1" t="s"><v>5</v></c></row>
<row r="2"><c r="A2"><v>46335.75</v></c><c r="B2" t="s"><v>6</v></c><c r="C2" t="s"><v>7</v></c><c r="D2" t="s"><v>8</v></c><c r="F2" t="inlineStr"><is><t>bkim</t></is></c></row>
<row r="4"><c r="A4"><v>46335.8</v></c><c r="B4" t="s"><v>9</v></c><c r="E4" t="s"><v>10</v></c></row>
</sheetData></worksheet>`)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadXLSX(t *testing.T) {
	got, err := Read(bytes.NewReader(buildXLSX(t)), "Week 1 (Responses).xlsx")
	if err != nil {
		t.Fatal(err)
	}
	want := `[{2 jdoe Петров Иван [asmith bkim]} {4  Нуров Тимур [mlee]}]`
	if fmt.Sprint(got) != want {
		t.Errorf("XLSX:\n%v\nwant\n%s", got, want)
	}
	if _, err := Read(bytes.NewReader([]byte("PK\x03\x04 not really a zip")), "x.xlsx"); err == nil {
		t.Error("a broken XLSX was read")
	}
}

// Storage: answers are kept as typed and resolved when counted, so an
// alias added later changes the count of a week already imported.
func TestStore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	db.Exec(`INSERT INTO students(name, login, created_at) VALUES('Петров Иван', 'jdoe', 0), ('Ли Мария', 'mlee', 0), ('No Login', '', 0)`)
	db.Exec(`INSERT INTO platform_users(id, login, first_name, last_name, synced_at) VALUES(1, 'asmith', 'Anna', 'Smith', 0), (2, 'jdoe', 'Иван', 'Петров', 0)`)
	answers := []Answer{{Row: 2, Respondent: "jdoe", Name: "Петров Иван", Peers: []string{"asmith", "malee", "mlee1x"}}}
	if _, err := Import(ctx, db, now, "2026-11-10", "w.xlsx", answers, "boss"); err != ErrBadWeek {
		t.Errorf("a Tuesday: %v", err)
	}
	if replaced, err := Import(ctx, db, now, "2026-11-09", "w.xlsx", answers, "boss"); err != nil || replaced {
		t.Fatal(replaced, err)
	}
	m, err := LoadMatcher(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	all, err := TallyAll(ctx, db, m)
	// asmith exact; "malee" is one edit from mlee: a typo; "mlee1x" is two
	// edits away and 6 long: a suggestion only.
	if err != nil || len(all) != 1 || fmt.Sprint(all[0].Counts) != "map[asmith:1 mlee:1]" || all[0].Mentions[2].Suggest != "mlee" {
		t.Fatalf("first count: %+v %v", all, err)
	}
	if err := SetAlias(ctx, db, m, now, "mlee1x", "nobody", "boss"); err != ErrUnknownLogin {
		t.Errorf("alias to an unknown login: %v", err)
	}
	if err := SetAlias(ctx, db, m, now, " MLEE1X ", "MLee", "boss"); err != nil {
		t.Fatal(err)
	}
	m, _ = LoadMatcher(ctx, db)
	all, _ = TallyAll(ctx, db, m)
	// mlee is now named twice in one answer: once by the typo, once by the alias; it counts once.
	if fmt.Sprint(all[0].Counts) != "map[asmith:1 mlee:1]" || all[0].Mentions[2].How != Alias || all[0].Mentions[2].Status != Repeat {
		t.Errorf("after the alias: %v %+v", all[0].Counts, all[0].Mentions[2])
	}
	// Importing the week again replaces it.
	if replaced, err := Import(ctx, db, now, "2026-11-09", "w2.csv", answers[:0], "boss"); err != nil || !replaced {
		t.Errorf("re-import: %v %v", replaced, err)
	}
	if w, _ := Weeks(ctx, db); len(w) != 1 || w[0].File != "w2.csv" || w[0].Count != 0 {
		t.Errorf("weeks: %+v", w)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM help_answers").Scan(&n)
	if n != 0 {
		t.Errorf("answers of the replaced import left: %d", n)
	}
	if err := Delete(ctx, db, now, "2026-11-09", "boss"); err != nil {
		t.Fatal(err)
	}
	db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action LIKE 'help%'").Scan(&n)
	if n != 4 { // 2 imports, 1 alias, 1 delete
		t.Errorf("journal: %d", n)
	}
}
