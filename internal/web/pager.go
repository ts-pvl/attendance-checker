package web

import (
	"strconv"
)

// Server-side paging shared by every list of the new admin UI: the
// dashboard table and matrix, the students list and the journal.

type pageLink struct {
	Num     int
	URL     string
	Current bool
	Gap     bool
}

type perOption struct {
	Value, Label string
	Selected     bool
}

type pager struct {
	Total, First, Last int // First/Last: 1-based rows shown
	Page, Pages        int
	Links              []pageLink
	Prev, Next         string
}

// paginate splits total rows into pages of per rows (0 = all on one page),
// clamps page into range and returns the rows [lo, hi) to show. link(p)
// builds the URL of page p.
func paginate(total, page, per int, link func(int) string) (pg pager, lo, hi int) {
	if per <= 0 {
		per = max(total, 1)
	}
	pages := max((total+per-1)/per, 1)
	page = min(max(page, 1), pages)
	lo, hi = (page-1)*per, min(page*per, total)
	pg = pager{Total: total, Page: page, Pages: pages}
	if total > 0 {
		pg.First, pg.Last = lo+1, hi
	}
	if pages > 1 {
		for p := 1; p <= pages; p++ {
			// First, last and two either side of the current page; gaps between.
			if p != 1 && p != pages && (p < page-2 || p > page+2) {
				if n := len(pg.Links); n > 0 && !pg.Links[n-1].Gap {
					pg.Links = append(pg.Links, pageLink{Gap: true})
				}
				continue
			}
			pg.Links = append(pg.Links, pageLink{Num: p, URL: link(p), Current: p == page})
		}
		if page > 1 {
			pg.Prev = link(page - 1)
		}
		if page < pages {
			pg.Next = link(page + 1)
		}
	}
	return pg, lo, hi
}

// parsePer reads a page size: "all" is 0; any number 1–1000 is accepted
// (the menus offer a few); anything else gives def.
func parsePer(v string, def int) int {
	if v == "all" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 1000 {
		return n
	}
	return def
}

func parsePage(v string) int {
	if p, err := strconv.Atoi(v); err == nil && p > 1 {
		return p
	}
	return 1
}

// perValue is how a page size appears in a URL.
func perValue(per int) string {
	if per == 0 {
		return "all"
	}
	return strconv.Itoa(per)
}

// perChoices lists the menu options plus "all", marking the current size; a
// size from the URL that is not on the menu is added so it shows as chosen.
func perChoices(cur int, options []int, allLabel string) []perOption {
	var out []perOption
	listed := cur == 0
	for _, n := range options {
		if n == cur {
			listed = true
		}
		if !listed && n > cur {
			out = append(out, perOption{strconv.Itoa(cur), strconv.Itoa(cur), true})
			listed = true
		}
		out = append(out, perOption{strconv.Itoa(n), strconv.Itoa(n), n == cur})
	}
	if !listed {
		out = append(out, perOption{strconv.Itoa(cur), strconv.Itoa(cur), true})
	}
	return append(out, perOption{"all", allLabel, cur == 0})
}
