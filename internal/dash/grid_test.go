package dash

import (
	"fmt"
	"strings"
	"testing"
)

// Expected grids are written out by hand, one row per "/", "_" for an empty
// cell.
func show(rows int, cells []string) string {
	cols := len(cells) / rows
	var out []string
	for r := 0; r < rows; r++ {
		row := make([]string, cols)
		for c := range row {
			if row[c] = cells[r*cols+c]; row[c] == "" {
				row[c] = "_"
			}
		}
		out = append(out, strings.Join(row, " "))
	}
	return strings.Join(out, " / ")
}

func placed(l Layout, hosts []string) string {
	rows, cells := l.Place(hosts)
	if len(cells) != rows*l.norm().Cols {
		return fmt.Sprintf("%d cells for %d rows", len(cells), rows)
	}
	return show(rows, cells)
}

func TestGridDefault(t *testing.T) {
	// 50 PCs named A01…E10 in name order fill 5 rows of 10, a letter per row.
	var hosts []string
	for _, row := range "ABCDE" {
		for i := 1; i <= 10; i++ {
			hosts = append(hosts, fmt.Sprintf("%c%02d", row, i))
		}
	}
	rows, cells := DefaultLayout().Place(hosts)
	if rows != 5 || len(cells) != 50 {
		t.Fatalf("%d rows, %d cells", rows, len(cells))
	}
	for i, want := range map[int]string{0: "A01", 9: "A10", 10: "B01", 25: "C06", 40: "E01", 49: "E10"} {
		if cells[i] != want {
			t.Errorf("cell %d: %s, want %s", i, cells[i], want)
		}
	}
	if DefaultLayout().Custom() {
		t.Error("the default layout is not custom")
	}

	ab := []string{"a", "b", "c", "d", "e", "f", "g"}
	// Fewer PCs than cells: the rest stays empty.
	if got := placed(Layout{Rows: 2, Cols: 3}, ab[:4]); got != "a b c / d _ _" {
		t.Errorf("4 PCs in 2x3: %s", got)
	}
	// More PCs than cells: a row is added, nothing is hidden.
	if got := placed(Layout{Rows: 2, Cols: 3}, ab); got != "a b c / d e f / g _ _" {
		t.Errorf("7 PCs in 2x3: %s", got)
	}
	// A damaged size falls back to 5 x 10.
	if rows, cells := (Layout{Rows: 0, Cols: 99}).Place(ab); rows != 5 || len(cells) != 50 {
		t.Errorf("bad size: %d rows, %d cells", rows, len(cells))
	}
}

func TestGridSwap(t *testing.T) {
	hosts := []string{"a", "b", "c", "d", "e", "f"}
	l := Layout{Rows: 2, Cols: 3}
	// First and last cell.
	s, ok := l.Swap(hosts, 0, 5)
	if got := placed(s, hosts); !ok || got != "f b c / d e a" {
		t.Errorf("swap 0,5: %v %s", ok, got)
	}
	if !s.Custom() {
		t.Error("a layout with a moved PC is custom")
	}
	// A second swap builds on the first.
	s2, ok := s.Swap(hosts, 1, 3)
	if got := placed(s2, hosts); !ok || got != "f d c / b e a" {
		t.Errorf("then swap 1,3: %v %s", ok, got)
	}
	// Onto an empty cell: the PC moves there.
	four := hosts[:4]
	m, ok := l.Swap(four, 1, 5)
	if got := placed(m, four); !ok || got != "a _ c / d _ b" {
		t.Errorf("move to an empty cell: %v %s", ok, got)
	}
	for _, c := range [][2]int{{0, 0}, {-1, 2}, {0, 6}, {4, 5}} { // same cell, out of range, two empty cells
		if _, ok := l.Swap(four, c[0], c[1]); ok {
			t.Errorf("swap %v accepted", c)
		}
	}
}

// PCs added to or removed from the hosts file after the layout was saved.
func TestGridHostsChange(t *testing.T) {
	l := Layout{Rows: 2, Cols: 3, Cells: []string{"f", "b", "c", "d", "e", "a"}}
	// d is gone and g is new: d's cell empties and g takes the first empty cell, which is that one.
	if got := placed(l, []string{"a", "b", "c", "e", "f", "g"}); got != "f b c / g e a" {
		t.Errorf("d removed, g added: %s", got)
	}
	// Only removed: the cells stay empty.
	if got := placed(l, []string{"a", "c", "e"}); got != "_ _ c / _ e a" {
		t.Errorf("b, d, f removed: %s", got)
	}
	// The grid is full: a new PC gets a new row.
	if got := placed(l, []string{"a", "b", "c", "d", "e", "f", "g"}); got != "f b c / d e a / g _ _" {
		t.Errorf("g added to a full grid: %s", got)
	}
	// A name stored twice (a damaged setting) is shown once.
	dup := Layout{Rows: 1, Cols: 3, Cells: []string{"a", "a", "b"}}
	if got := placed(dup, []string{"a", "b"}); got != "a _ b" {
		t.Errorf("duplicate: %s", got)
	}
}

func TestGridResize(t *testing.T) {
	hosts := []string{"a", "b", "c", "d", "e", "f"}
	// Nobody has moved a PC: the PCs fill the new size in name order.
	r, ok := DefaultLayout().Resize(hosts, 3, 2)
	if got := placed(r, hosts); !ok || r.Cells != nil || got != "a b / c d / e f" {
		t.Errorf("default to 3x2: %v %s", ok, got)
	}
	custom := Layout{Rows: 2, Cols: 3, Cells: []string{"f", "b", "c", "d", "e", "a"}}
	// 3 x 2: f, b, d, e keep their row and column; c and a (third column)
	// move to the empty cells of the new third row, in that order.
	r, ok = custom.Resize(hosts, 3, 2)
	if got := placed(r, hosts); !ok || got != "f b / d e / c a" {
		t.Errorf("custom to 3x2: %v %s", ok, got)
	}
	// 2 x 4: a wider grid adds an empty column.
	r, ok = custom.Resize(hosts, 2, 4)
	if got := placed(r, hosts); !ok || got != "f b c _ / d e a _" {
		t.Errorf("custom to 2x4: %v %s", ok, got)
	}
	// 1 x 2: only f and b fit; c, d, e, a follow in added rows.
	r, ok = custom.Resize(hosts, 1, 2)
	if got := placed(r, hosts); !ok || r.Rows != 1 || got != "f b / c d / e a" {
		t.Errorf("custom to 1x2: %v %s", ok, got)
	}
	for _, c := range [][2]int{{0, 5}, {5, 0}, {31, 1}, {1, 31}, {-2, 3}} {
		if _, ok := custom.Resize(hosts, c[0], c[1]); ok {
			t.Errorf("size %v accepted", c)
		}
	}
	if _, ok := custom.Resize(hosts, 30, 30); !ok {
		t.Error("30 x 30 refused")
	}
}
