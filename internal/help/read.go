package help

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Read reads the form's answers from an XLSX workbook (the form's response
// sheet downloaded as .xlsx; the first sheet is read) or a CSV file. The
// columns are found by their headers:
//   - peers: "Peer …", or a header with "помог"/"поблагодар"/"helped"/"thank";
//   - the respondent's login: "login", "логин" or "username";
//   - the respondent's name: "ФИО", "name" or "имя".
//
// Other columns (time stamp, comments) are ignored and not kept.
func Read(r io.Reader, fileName string) ([]Answer, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, ErrTooBig
	}
	var rows []row
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) || strings.HasSuffix(strings.ToLower(fileName), ".xlsx") {
		rows, err = readXLSX(data)
	} else {
		rows, err = readCSV(data)
	}
	if err != nil {
		return nil, err
	}
	return answers(rows)
}

const (
	maxFile = 10 << 20 // an upload
	maxPart = 64 << 20 // one decompressed part of a workbook
)

var (
	ErrNoPeerColumns = errors.New("no peer columns: the first row must have headers such as \"Peer #1\"")
	ErrTooBig        = errors.New("the file is larger than 10 MB")
	ErrNotWorkbook   = errors.New("this is not an XLSX workbook")
)

type row struct {
	n     int // 1-based row number in the file
	cells []string
}

func readCSV(data []byte) ([]row, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	first, _, _ := bufio.NewReader(bytes.NewReader(data)).ReadLine()
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comma = ','
	for _, d := range []rune{';', '\t'} {
		if strings.Count(string(first), string(d)) > strings.Count(string(first), string(cr.Comma)) {
			cr.Comma = d
		}
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	var out []row
	for n := 1; ; n++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		line, _ := cr.FieldPos(0)
		out = append(out, row{n: line, cells: rec})
	}
}

// --- XLSX: a zip of XML parts; only the parts needed are read. ---

type xRels struct {
	Rels []struct {
		ID     string `xml:"Id,attr"`
		Target string `xml:"Target,attr"`
	} `xml:"Relationship"`
}

type xWorkbook struct {
	Sheets []struct {
		RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
	} `xml:"sheets>sheet"`
}

type xText struct {
	T    string `xml:"t"`
	Runs []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

func (x xText) String() string {
	s := x.T
	for _, r := range x.Runs {
		s += r.T
	}
	return s
}

type xSST struct {
	SI []xText `xml:"si"`
}

type xSheet struct {
	Rows []struct {
		R     int `xml:"r,attr"`
		Cells []struct {
			Ref string `xml:"r,attr"`
			T   string `xml:"t,attr"`
			V   string `xml:"v"`
			Is  xText  `xml:"is"`
		} `xml:"c"`
	} `xml:"sheetData>row"`
}

func readPart(z *zip.Reader, name string, v any) (bool, error) {
	for _, f := range z.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return true, err
		}
		defer rc.Close()
		lr := &io.LimitedReader{R: rc, N: maxPart}
		if err := xml.NewDecoder(lr).Decode(v); err != nil {
			return true, fmt.Errorf("%s: %w", name, err)
		}
		if lr.N <= 0 {
			return true, fmt.Errorf("%s is too large", name)
		}
		return true, nil
	}
	return false, nil
}

// column turns "AB12" into 27 (0-based).
func column(ref string) int {
	n := 0
	for _, c := range ref {
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
	}
	return n - 1
}

func readXLSX(data []byte) ([]row, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrNotWorkbook
	}
	var wb xWorkbook
	if ok, err := readPart(z, "xl/workbook.xml", &wb); err != nil {
		return nil, err
	} else if !ok || len(wb.Sheets) == 0 {
		return nil, ErrNotWorkbook
	}
	sheet := "xl/worksheets/sheet1.xml"
	var rels xRels
	if _, err := readPart(z, "xl/_rels/workbook.xml.rels", &rels); err != nil {
		return nil, err
	}
	for _, rel := range rels.Rels {
		if rel.ID == wb.Sheets[0].RID {
			if strings.HasPrefix(rel.Target, "/") {
				sheet = strings.TrimPrefix(rel.Target, "/")
			} else {
				sheet = path.Join("xl", rel.Target)
			}
		}
	}
	var sst xSST
	if _, err := readPart(z, "xl/sharedStrings.xml", &sst); err != nil {
		return nil, err
	}
	var ws xSheet
	if ok, err := readPart(z, sheet, &ws); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNotWorkbook
	}
	var out []row
	for i, r := range ws.Rows {
		n := r.R
		if n == 0 {
			n = i + 1
		}
		var cells []string
		for j, c := range r.Cells {
			col := j
			if c.Ref != "" {
				col = column(c.Ref)
			}
			if col < 0 || col > 1000 {
				continue
			}
			var v string
			switch c.T {
			case "s":
				k, err := strconv.Atoi(strings.TrimSpace(c.V))
				if err == nil && k >= 0 && k < len(sst.SI) {
					v = sst.SI[k].String()
				}
			case "inlineStr":
				v = c.Is.String()
			default:
				v = c.V
			}
			for len(cells) <= col {
				cells = append(cells, "")
			}
			cells[col] = v
		}
		out = append(out, row{n: n, cells: cells})
	}
	return out, nil
}

// --- columns ---

func has(h string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(h, w) {
			return true
		}
	}
	return false
}

func answers(rows []row) ([]Answer, error) {
	for len(rows) > 0 && strings.TrimSpace(strings.Join(rows[0].cells, "")) == "" {
		rows = rows[1:]
	}
	if len(rows) == 0 {
		return nil, ErrNoPeerColumns
	}
	loginCol, nameCol := -1, -1
	var peerCols []int
	for i, h := range rows[0].cells {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case has(h, "peer", "помог", "поблагодар", "helped", "thank"):
			peerCols = append(peerCols, i)
		case loginCol < 0 && has(h, "login", "логин", "username"):
			loginCol = i
		case nameCol < 0 && has(h, "фио", "ф.и.о", "name", "имя"):
			nameCol = i
		}
	}
	if len(peerCols) == 0 {
		return nil, ErrNoPeerColumns
	}
	cell := func(r row, i int) string {
		if i < 0 || i >= len(r.cells) {
			return ""
		}
		return strings.TrimSpace(r.cells[i])
	}
	var out []Answer
	for _, r := range rows[1:] {
		a := Answer{Row: r.n, Respondent: cell(r, loginCol), Name: strings.Join(strings.Fields(cell(r, nameCol)), " ")}
		for _, c := range peerCols {
			if p := cell(r, c); p != "" {
				a.Peers = append(a.Peers, p)
			}
		}
		if a.Respondent == "" && a.Name == "" && len(a.Peers) == 0 {
			continue
		}
		if a.Peers == nil {
			a.Peers = []string{}
		}
		out = append(out, a)
	}
	return out, nil
}
