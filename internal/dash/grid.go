package dash

// The PC grid of the Today tab: a room plan of rows × columns in which every
// PC has a cell. Staff can change the size and swap two cells. The layout is
// stored as JSON in the settings table (key today_layout).

const (
	GridRows    = 5  // default size: 5 rows of 10 PCs
	GridCols    = 10 //
	GridMaxSide = 30 // largest number of rows or columns
)

// Layout is the stored grid. Cells lists the cells row by row ("" is an
// empty cell). A nil Cells means nobody has moved a PC yet: the PCs fill the
// grid row by row in name order, whatever its size.
type Layout struct {
	Rows  int      `json:"rows"`
	Cols  int      `json:"cols"`
	Cells []string `json:"cells,omitempty"`
}

func DefaultLayout() Layout { return Layout{Rows: GridRows, Cols: GridCols} }

// Custom reports whether the layout differs from the default one.
func (l Layout) Custom() bool {
	l = l.norm()
	return l.Cells != nil || l.Rows != GridRows || l.Cols != GridCols
}

// ValidGridSize reports whether rows × cols is a size the grid accepts.
func ValidGridSize(rows, cols int) bool {
	return rows >= 1 && rows <= GridMaxSide && cols >= 1 && cols <= GridMaxSide
}

// norm replaces a size that cannot be used (a damaged setting) by the default.
func (l Layout) norm() Layout {
	if !ValidGridSize(l.Rows, l.Cols) {
		l.Rows, l.Cols = GridRows, GridCols
	}
	return l
}

// Place returns the grid to show for the given PCs (in name order): the
// number of rows and rows × Cols cells, row by row. No PC is ever hidden:
//   - a PC that is not in the stored cells (new in the hosts file) takes the
//     first empty cell;
//   - a stored name that is no longer a PC leaves its cell empty;
//   - when the PCs do not fit, rows are added at the bottom.
func (l Layout) Place(hosts []string) (rows int, cells []string) {
	l = l.norm()
	if l.Cells == nil {
		cells = append(cells, hosts...)
	} else {
		known := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			known[h] = true
		}
		placed := map[string]bool{}
		for _, c := range l.Cells {
			if !known[c] || placed[c] {
				c = ""
			}
			placed[c] = true
			cells = append(cells, c)
		}
		free := 0
		for _, h := range hosts {
			if placed[h] {
				continue
			}
			for free < len(cells) && cells[free] != "" {
				free++
			}
			if free < len(cells) {
				cells[free] = h
			} else {
				cells = append(cells, h)
			}
		}
	}
	for len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	rows = max(l.Rows, (len(cells)+l.Cols-1)/l.Cols)
	for len(cells) < rows*l.Cols {
		cells = append(cells, "")
	}
	return rows, cells
}

// Swap exchanges the contents of cells a and b (indexes into Place's cells).
// One of them may be empty, which moves a PC to an empty cell. ok is false
// when an index is out of range, a == b or both cells are empty.
func (l Layout) Swap(hosts []string, a, b int) (out Layout, ok bool) {
	l = l.norm()
	_, cells := l.Place(hosts)
	if a < 0 || b < 0 || a >= len(cells) || b >= len(cells) || a == b || cells[a] == "" && cells[b] == "" {
		return l, false
	}
	cells[a], cells[b] = cells[b], cells[a]
	return Layout{Rows: l.Rows, Cols: l.Cols, Cells: cells}, true
}

// Resize changes the size of the grid. While nobody has moved a PC, the PCs
// simply fill the new grid in name order. After that every PC keeps its row
// and column if they still exist; the others move to the empty cells, first
// row first, and to added rows if there are none left.
func (l Layout) Resize(hosts []string, rows, cols int) (out Layout, ok bool) {
	l = l.norm()
	if !ValidGridSize(rows, cols) {
		return l, false
	}
	if l.Cells == nil {
		return Layout{Rows: rows, Cols: cols}, true
	}
	_, old := l.Place(hosts)
	cells := make([]string, rows*cols)
	var moved []string
	for i, h := range old {
		if h == "" {
			continue
		}
		if r, c := i/l.Cols, i%l.Cols; r < rows && c < cols {
			cells[r*cols+c] = h
		} else {
			moved = append(moved, h)
		}
	}
	free := 0
	for _, h := range moved {
		for free < len(cells) && cells[free] != "" {
			free++
		}
		if free < len(cells) {
			cells[free] = h
		} else {
			cells = append(cells, h)
		}
	}
	return Layout{Rows: rows, Cols: cols, Cells: cells}, true
}
