package core

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"attendance/internal/store"
	"attendance/internal/token"
)

// TestLoad50PCs simulates the start of a lesson: 50 PCs sending heartbeats
// every minute for 20 minutes, plus a burst of 50 scans at once. Run with
// -v to see timings; skipped with -short.
func TestLoad50PCs(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "load.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const n = 50
	hosts := NewHosts("")
	var entries []HostEntry
	for i := 1; i <= n; i++ {
		entries = append(entries, HostEntry{Name: fmt.Sprintf("PC-%02d", i), IP: netip.AddrFrom4([4]byte{10, 0, 1, byte(i)})})
		db.Exec("INSERT INTO students(id, name, created_at) VALUES(?, ?, 0)", i, fmt.Sprintf("S%02d", i))
		db.Exec("INSERT INTO telegram_links(student_id, tg_user_id, linked_at) VALUES(?, ?, 0)", i, 5000+i)
	}
	if err := hosts.Sync(ctx, db.DB, entries); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(db, 3*time.Minute, 2*time.Minute)
	signer := token.NewSigner(bytes.Repeat([]byte{2}, 32))
	ci := &Checkin{Engine: engine, Signer: signer, Board: NewQRBoard(signer, 30*time.Second), Hosts: hosts, TTL: time.Minute}

	base := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	parallel := func(at time.Time, fn func(i int, at time.Time) error) []time.Duration {
		var wg sync.WaitGroup
		lat := make([]time.Duration, n)
		errs := make(chan error, n)
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				t0 := time.Now()
				if err := fn(i, at); err != nil {
					errs <- err
				}
				lat[i-1] = time.Since(t0)
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		return lat
	}
	start := time.Now()
	parallel(base, func(i int, at time.Time) error {
		_, err := engine.AgentEvent(ctx, at, fmt.Sprintf("PC-%02d", i), KindSessionStart, "s", idle(0))
		return err
	})
	var hb []time.Duration
	for r := 1; r <= 20; r++ {
		hb = append(hb, parallel(base.Add(time.Duration(r)*time.Minute), func(i int, at time.Time) error {
			_, err := engine.AgentEvent(ctx, at, fmt.Sprintf("PC-%02d", i), KindHeartbeat, "s", idle(int64(i*100)))
			return err
		})...)
		if r == 5 {
			at := base.Add(5*time.Minute + time.Second)
			scans := parallel(at, func(i int, at time.Time) error {
				h, _ := hosts.ByName(fmt.Sprintf("PC-%02d", i))
				tok, _ := ci.Board.Current(h.ID, at.Add(-3*time.Second))
				r, err := ci.Scan(ctx, at, int64(5000+i), tok)
				if err == nil && r.Result != ScanOK {
					err = fmt.Errorf("scan %d: %s", i, r.Result)
				}
				return err
			})
			p := percentiles(scans)
			t.Logf("burst of %d scans: p50 %v, p95 %v, max %v", n, p[0], p[1], p[2])
			if p[2] > 2*time.Second {
				t.Errorf("slowest scan took %v (acceptance: a scan completes in under 5 s end to end)", p[2])
			}
		}
	}
	p := percentiles(hb)
	t.Logf("%d heartbeats in %v: p50 %v, p95 %v, max %v", len(hb), time.Since(start), p[0], p[1], p[2])
	var open int
	db.QueryRow("SELECT COUNT(*) FROM presence WHERE ended_at IS NULL").Scan(&open)
	if open != n {
		t.Errorf("open visits %d, want %d", open, n)
	}
}

func percentiles(d []time.Duration) [3]time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return [3]time.Duration{s[len(s)/2], s[len(s)*95/100], s[len(s)-1]}
}
