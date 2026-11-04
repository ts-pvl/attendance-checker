package core

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Host is a school PC. Its identity is its fixed LAN address; the name is
// what students and staff see ("PC-17").
type Host struct {
	ID   int64
	Name string
	IP   netip.Addr
}

// Hosts is the allowlist of PCs, loaded from a CSV file (name,ip) that
// Ansible generates from its inventory. Host ids live in the database so
// they stay stable when the file is reordered.
type Hosts struct {
	path string

	mu     sync.RWMutex
	byIP   map[netip.Addr]Host
	byID   map[int64]Host
	byName map[string]Host
	mtime  time.Time
}

func NewHosts(path string) *Hosts {
	return &Hosts{path: path, byIP: map[netip.Addr]Host{}, byID: map[int64]Host{}, byName: map[string]Host{}}
}

var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

type HostEntry struct {
	Name string
	IP   netip.Addr
}

// ParseHosts reads "name,ip" lines. A header line and '#' comments are allowed.
func ParseHosts(r io.Reader) ([]HostEntry, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	var out []HostEntry
	seenName := map[string]bool{}
	seenIP := map[netip.Addr]bool{}
	for line := 1; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		if len(rec) < 2 {
			return nil, fmt.Errorf("hosts line %d: expected name,ip", line)
		}
		name, ipStr := strings.TrimSpace(rec[0]), strings.TrimSpace(rec[1])
		if line == 1 && strings.EqualFold(name, "name") {
			continue
		}
		if !hostNameRe.MatchString(name) {
			return nil, fmt.Errorf("hosts line %d: bad host name %q", line, name)
		}
		ip, err := netip.ParseAddr(ipStr)
		if err != nil {
			return nil, fmt.Errorf("hosts line %d: %w", line, err)
		}
		ip = ip.Unmap()
		if seenName[name] || seenIP[ip] {
			return nil, fmt.Errorf("hosts line %d: duplicate name or address", line)
		}
		seenName[name], seenIP[ip] = true, true
		out = append(out, HostEntry{Name: name, IP: ip})
	}
	return out, nil
}

// Load reads the hosts file and syncs it into the database.
func (h *Hosts) Load(ctx context.Context, db *sql.DB) error {
	st, err := os.Stat(h.path)
	if err != nil {
		return err
	}
	f, err := os.Open(h.path)
	if err != nil {
		return err
	}
	entries, err := ParseHosts(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", h.path, err)
	}
	if err := h.Sync(ctx, db, entries); err != nil {
		return err
	}
	h.mu.Lock()
	h.mtime = st.ModTime()
	h.mu.Unlock()
	return nil
}

// ReloadIfChanged reloads the file when its modification time changed.
func (h *Hosts) ReloadIfChanged(ctx context.Context, db *sql.DB) (bool, error) {
	st, err := os.Stat(h.path)
	if err != nil {
		return false, err
	}
	h.mu.RLock()
	same := st.ModTime().Equal(h.mtime)
	h.mu.RUnlock()
	if same {
		return false, nil
	}
	return true, h.Load(ctx, db)
}

// Sync upserts entries into the hosts table, deactivates hosts that are no
// longer listed and refreshes the in-memory maps.
func (h *Hosts) Sync(ctx context.Context, db *sql.DB, entries []HostEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE hosts SET active = 0"); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO hosts(name, ip, active) VALUES(?, ?, 1)
			ON CONFLICT(name) DO UPDATE SET ip = excluded.ip, active = 1`, e.Name, e.IP.String()); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, name, ip FROM hosts WHERE active = 1")
	if err != nil {
		return err
	}
	byIP, byID, byName := map[netip.Addr]Host{}, map[int64]Host{}, map[string]Host{}
	for rows.Next() {
		var hst Host
		var ip string
		if err := rows.Scan(&hst.ID, &hst.Name, &ip); err != nil {
			rows.Close()
			return err
		}
		if hst.ID > 0xFFFF {
			rows.Close()
			return fmt.Errorf("host id %d does not fit in a QR token", hst.ID)
		}
		hst.IP, err = netip.ParseAddr(ip)
		if err != nil {
			rows.Close()
			return err
		}
		byIP[hst.IP], byID[hst.ID], byName[hst.Name] = hst, hst, hst
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.mu.Lock()
	h.byIP, h.byID, h.byName = byIP, byID, byName
	h.mu.Unlock()
	return nil
}

func (h *Hosts) ByIP(ip netip.Addr) (Host, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hst, ok := h.byIP[ip.Unmap()]
	return hst, ok
}

func (h *Hosts) ByID(id int64) (Host, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hst, ok := h.byID[id]
	return hst, ok
}

func (h *Hosts) ByName(name string) (Host, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hst, ok := h.byName[name]
	return hst, ok
}

// All returns active hosts sorted by name (natural order, so PC-2 < PC-10).
func (h *Hosts) All() []Host {
	h.mu.RLock()
	out := make([]Host, 0, len(h.byID))
	for _, hst := range h.byID {
		out = append(out, hst)
	}
	h.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return NaturalLess(out[i].Name, out[j].Name) })
	return out
}

// NaturalLess compares strings treating digit runs as numbers.
func NaturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
