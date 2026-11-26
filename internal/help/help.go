// Package help counts how often each student is named in the weekly
// peer-help form ("write the logins of up to five people who helped you
// most"). Students type the logins by hand, so each one is resolved to a
// known login (a student's or a platform user's) by these rules, in order:
//
//  1. Normalise: trim spaces (also non-breaking and zero-width ones), quotes
//     and a leading '@'; replace Cyrillic letters that look like Latin ones
//     (а→a, В→B, …); lower case.
//  2. Alias: a typed login that staff have resolved by hand.
//  3. Exact: a known login.
//  4. Name: two or more words that are all words of exactly one person's
//     name (the student's name, or the platform's name in Cyrillic or in
//     Latin letters). Words are compared as core.NameWords does: without
//     case, with ё as е, the Kazakh letters as their Russian look-alikes
//     (ә as а, і as и, ұ and ү as у, …) and Latin look-alikes as one.
//  5. Typo: removing spaces gives a known login; or exactly one known login
//     is one edit away (a letter added, missing, wrong, or two neighbours
//     swapped) and the text has at least 4 characters.
//  6. Otherwise it is unresolved. If exactly one login is one edit away (text
//     shorter than 4) or two edits away (text of 6 or more), it is offered
//     to staff as a suggestion; a suggestion is not counted until staff
//     confirm it, which saves an alias.
//
// Counting: one person who answers several times in a week counts with
// their last answer only (later rows are later answers). In one answer each
// person counts once, and naming oneself does not count. A person's count
// for the week is the number of answers that name them.
package help

import (
	"sort"
	"strings"
	"unicode"

	"attendance/internal/core"
)

// Answer is one row of the form.
type Answer struct {
	Row        int      // row in the file; a later row is a later answer
	Respondent string   // the respondent's own login, as typed
	Name       string   // the respondent's name, as typed
	Peers      []string // the logins they typed, in order, empty cells left out
}

// Person is someone who can be named: a login and the names they go by.
type Person struct {
	Login string
	Names []string
}

type How string

const (
	Empty   How = "empty"
	Exact   How = "exact"
	Alias   How = "alias"
	Name    How = "name"
	Typo    How = "typo"
	Unknown How = "unknown"
)

// Match is how a typed login was resolved.
type Match struct {
	Login   string // "" when unresolved
	How     How
	Suggest string // for Unknown: a likely login for staff to confirm
}

func trimJunk(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\u200b' || r == '\ufeff' || strings.ContainsRune(`"'«»“”„@`, r)
	})
}

// Normalize is rule 1, the key for exact matches and aliases.
func Normalize(s string) string {
	return strings.ToLower(core.LatinLookalikes(trimJunk(s)))
}

func loginLike(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func nameWords(s string) []string { return core.NameWords(trimJunk(s)) }

// Matcher resolves typed logins against the known people.
type Matcher struct {
	logins  map[string]string // normalised -> login as stored
	list    []string          // normalised, sorted
	names   []nameEntry
	aliases map[string]string // normalised typed -> login
}

type nameEntry struct {
	login string
	words map[string]bool
}

// NewMatcher: aliases map normalised typed text to a login.
func NewMatcher(people []Person, aliases map[string]string) *Matcher {
	m := &Matcher{logins: map[string]string{}, aliases: map[string]string{}}
	for _, p := range people {
		k := Normalize(p.Login)
		if k == "" {
			continue
		}
		if _, seen := m.logins[k]; !seen {
			m.logins[k] = p.Login
			m.list = append(m.list, k)
		}
		for _, n := range p.Names {
			e := nameEntry{login: m.logins[k], words: map[string]bool{}}
			for _, w := range nameWords(n) {
				e.words[w] = true
			}
			if len(e.words) > 0 {
				m.names = append(m.names, e)
			}
		}
	}
	sort.Strings(m.list)
	for k, v := range aliases {
		m.aliases[Normalize(k)] = v
	}
	return m
}

// Known reports whether login is a known login (any case).
func (m *Matcher) Known(login string) (string, bool) {
	l, ok := m.logins[Normalize(login)]
	return l, ok
}

// Resolve applies rules 2-6 to one typed login.
func (m *Matcher) Resolve(typed string) Match {
	k := Normalize(typed)
	if k == "" {
		return Match{How: Empty}
	}
	if l, ok := m.aliases[k]; ok {
		return Match{Login: l, How: Alias}
	}
	if l, ok := m.logins[k]; ok {
		return Match{Login: l, How: Exact}
	}
	if words := nameWords(typed); len(words) >= 2 {
		if l := m.byName(words); l != "" {
			return Match{Login: l, How: Name}
		}
	}
	squeezed := strings.Join(strings.Fields(k), "")
	if l, ok := m.logins[squeezed]; ok {
		return Match{Login: l, How: Typo}
	}
	if !loginLike(squeezed) {
		return Match{How: Unknown}
	}
	// Only distances 1 and 2 matter; distance() reports anything larger as 3.
	best, bestN, bestLogin := 3, 0, ""
	for _, cand := range m.list {
		d := distance(squeezed, cand, 3)
		switch {
		case d >= 3:
		case d < best:
			best, bestN, bestLogin = d, 1, cand
		case d == best:
			bestN++
		}
	}
	n := len([]rune(squeezed))
	switch {
	case bestN != 1:
		return Match{How: Unknown}
	case best == 1 && n >= 4:
		return Match{Login: m.logins[bestLogin], How: Typo}
	case best == 1 || best == 2 && n >= 6:
		return Match{How: Unknown, Suggest: m.logins[bestLogin]}
	}
	return Match{How: Unknown}
}

// byName returns the one login whose name contains all the words, or "".
func (m *Matcher) byName(words []string) string {
	found := ""
	for _, e := range m.names {
		all := true
		for _, w := range words {
			if !e.words[w] {
				all = false
				break
			}
		}
		if !all || e.login == found {
			continue
		}
		if found != "" {
			return "" // two people
		}
		found = e.login
	}
	return found
}

// distance is the optimal-string-alignment edit distance (insertions,
// deletions, substitutions and swaps of neighbours), cut off at limit.
func distance(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d >= limit || -d >= limit {
		return limit
	}
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
			rowMin = min(rowMin, cur[j])
		}
		if rowMin >= limit {
			return limit
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return min(prev[len(rb)], limit)
}

// Status of one typed login in the count.
type Status string

const (
	Counted    Status = "counted"
	Self       Status = "self"
	Repeat     Status = "repeat"     // the same person twice in one answer
	Superseded Status = "superseded" // the respondent answered again later
	Unresolved Status = "unresolved"
)

// Mention is one typed login of one answer.
type Mention struct {
	Row        int
	Respondent string // as typed (login, else name)
	Typed      string
	Match
	Status Status
}

// Tally is the count for one week.
type Tally struct {
	Counts   map[string]int // login -> answers naming them
	Mentions []Mention      // every typed login, in file order
	Answers  int
	Counted  int // answers that count (not replaced by a later one)
}

// respondentKey identifies who answered, to keep only their last answer.
func (m *Matcher) respondent(a Answer) (key, login string) {
	if r := m.Resolve(a.Respondent); r.Login != "" && r.How != Unknown {
		return "login:" + strings.ToLower(r.Login), r.Login
	}
	if k := Normalize(a.Respondent); k != "" {
		return "typed:" + k, ""
	}
	if words := nameWords(a.Name); len(words) >= 2 {
		if l := m.byName(words); l != "" {
			return "login:" + strings.ToLower(l), l
		}
	}
	if words := nameWords(a.Name); len(words) > 0 {
		sort.Strings(words)
		return "name:" + strings.Join(words, " "), ""
	}
	return "row:" + itoa(a.Row), ""
}

// Count applies the counting rules to one week's answers.
func Count(answers []Answer, m *Matcher) Tally {
	t := Tally{Counts: map[string]int{}, Answers: len(answers)}
	last := map[string]int{} // respondent key -> latest row
	keys := make([]string, len(answers))
	selfs := make([]string, len(answers))
	for i, a := range answers {
		keys[i], selfs[i] = m.respondent(a)
		if a.Row >= last[keys[i]] {
			last[keys[i]] = a.Row
		}
	}
	for i, a := range answers {
		latest := last[keys[i]] == a.Row
		if latest {
			t.Counted++
		}
		who := strings.TrimSpace(a.Respondent)
		if who == "" {
			who = strings.TrimSpace(a.Name)
		}
		seen := map[string]bool{}
		for _, typed := range a.Peers {
			mt := m.Resolve(typed)
			if mt.How == Empty {
				continue
			}
			x := Mention{Row: a.Row, Respondent: who, Typed: typed, Match: mt}
			key := strings.ToLower(mt.Login)
			switch {
			case mt.Login == "":
				x.Status = Unresolved
			case !latest:
				x.Status = Superseded
			case selfs[i] != "" && strings.EqualFold(selfs[i], mt.Login):
				x.Status = Self
			case seen[key]:
				x.Status = Repeat
			default:
				x.Status = Counted
				seen[key] = true
				t.Counts[mt.Login]++
			}
			t.Mentions = append(t.Mentions, x)
		}
	}
	return t
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
