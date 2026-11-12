package i18n

import (
	"regexp"
	"testing"
	"time"
)

var verb = regexp.MustCompile(`%[-+# 0]*[0-9]*[a-z]`)

// Both languages must be present and take the same format arguments.
func TestCatalogComplete(t *testing.T) {
	for key, m := range Keys() {
		if m[0] == "" || m[1] == "" {
			t.Errorf("%s: missing translation", key)
			continue
		}
		en, ru := verb.FindAllString(m[0], -1), verb.FindAllString(m[1], -1)
		if len(en) != len(ru) {
			t.Errorf("%s: %v vs %v", key, en, ru)
			continue
		}
		for i := range en {
			if en[i] != ru[i] {
				t.Errorf("%s: verb %d is %s in English and %s in Russian", key, i, en[i], ru[i])
			}
		}
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d      time.Duration
		en, ru string
	}{
		{0, "0 min", "0 мин"},
		{59 * time.Second, "0 min", "0 мин"},
		{45 * time.Minute, "45 min", "45 мин"},
		{3*time.Hour + 5*time.Minute + 59*time.Second, "3 h 05 min", "3 ч 05 мин"},
		{-time.Minute, "0 min", "0 мин"},
	}
	for _, c := range cases {
		if got := Duration("en", c.d); got != c.en {
			t.Errorf("en %v: %q", c.d, got)
		}
		if got := Duration("ru", c.d); got != c.ru {
			t.Errorf("ru %v: %q", c.d, got)
		}
	}
}

func TestPlural(t *testing.T) {
	ru := map[int]string{1: "1 учебный день", 2: "2 учебных дня", 4: "4 учебных дня", 5: "5 учебных дней", 11: "11 учебных дней",
		12: "12 учебных дней", 21: "21 учебный день", 22: "22 учебных дня", 27: "27 учебных дней", 111: "111 учебных дней"}
	for n, want := range ru {
		if got := Plural("ru", "dash.school_days", n); got != want {
			t.Errorf("ru %d: %q, want %q", n, got, want)
		}
	}
	if got := Plural("en", "dash.school_days", 1); got != "1 school day" {
		t.Errorf("en 1: %q", got)
	}
	if got := Plural("en", "dash.school_days", 21); got != "21 school days" {
		t.Errorf("en 21: %q", got)
	}
}

func TestNum(t *testing.T) {
	cases := []struct {
		x      float64
		dec    int
		en, ru string
	}{
		{1811, 0, "1,811", "1 811"},
		{4.56, 1, "4.6", "4,6"},
		{124.6, 1, "124.6", "124,6"},
		{1234567.25, 2, "1,234,567.25", "1 234 567,25"},
		{0, 0, "0", "0"},
		{-0.04, 1, "0.0", "0,0"},
		{-12.5, 1, "-12.5", "-12,5"},
	}
	for _, c := range cases {
		if got := Num("en", c.x, c.dec); got != c.en {
			t.Errorf("en %v: %q, want %q", c.x, got, c.en)
		}
		if got := Num("ru", c.x, c.dec); got != c.ru {
			t.Errorf("ru %v: %q, want %q", c.x, got, c.ru)
		}
	}
}

func TestDay(t *testing.T) {
	d := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if got := Day("ru", d, false); got != "31 авг" {
		t.Errorf("ru: %q", got)
	}
	if got := Day("ru", d, true); got != "31 авг 2026" {
		t.Errorf("ru year: %q", got)
	}
	if got := Day("en", d, true); got != "Aug 31, 2026" {
		t.Errorf("en year: %q", got)
	}
}
