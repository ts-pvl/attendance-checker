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
