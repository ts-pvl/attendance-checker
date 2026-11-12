// Command devseed fills a development database with invented attendance, so
// the admin UI has something to show. It is for `make dev` only: it is not
// part of a release and refuses any database outside a ".dev" directory.
//
// Everything goes through the real code paths as raw events: students are
// linked with one-time codes, and each visit is a PC login, a QR scan,
// heartbeats and a logout (or an idle logout, /checkout or a power cut), in
// time order, so `attendance replay` reproduces the result. All names are
// made up.
//
//	devseed -config .dev/attendance.conf [-days 28] [-students 30]
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"attendance/internal/app"
	"attendance/internal/config"
	"attendance/internal/core"
	"attendance/internal/store"
	"attendance/internal/token"
)

const marker = "dev_seed"

func main() {
	cfgPath := flag.String("config", ".dev/attendance.conf", "config file")
	days := flag.Int("days", 28, "days of history to create")
	students := flag.Int("students", 30, "students to create")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()
	if err := run(*cfgPath, *days, *students, *seed); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

var (
	lastNames  = []string{"Абенов", "Ахметова", "Байжанов", "Волкова", "Галиев", "Демченко", "Ерланова", "Жумабеков", "Захарова", "Исаев", "Калиева", "Лим", "Мухамедов", "Носова", "Омаров", "Петренко", "Рахимова", "Сабитов", "Тулегенова", "Уразов", "Фёдорова", "Хасенов", "Цой", "Шайхина", "Юсупов", "Яковлева"}
	firstNames = []string{"Алмас", "Айгерим", "Дамир", "Жанна", "Ерлан", "Камила", "Нурлан", "Мадина", "Олжас", "Дана", "Тимур", "Асель", "Руслан", "Алина", "Санжар", "Диана"}
	groups     = []string{"Go-1", "Go-2", "JS-1"}
)

type student struct {
	id, tg    int64
	p         float64 // chance of coming on a weekday
	arrive    float64 // usual arrival, hours after midnight
	stay      float64 // usual stay, hours
	dropAfter int     // last day index they come, or -1
	archived  bool
}

// step is one input at one moment; all steps run in time order.
type step struct {
	at  time.Time
	seq int
	do  func(time.Time) error
}

func run(cfgPath string, days, nStudents int, seed int64) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if filepath.Base(filepath.Dir(cfg.DBPath)) != ".dev" {
		return fmt.Errorf("refusing to touch %s: devseed only writes databases in a .dev directory", cfg.DBPath)
	}
	if c, err := net.DialTimeout("tcp", localAddr(cfg.Listen), 300*time.Millisecond); err == nil {
		c.Close()
		return errors.New("the dev server is running: stop `make dev` first (it would close the sessions being created)")
	}
	ctx := context.Background()
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	var done string
	if err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", marker).Scan(&done); err == nil {
		fmt.Printf("devseed: already seeded on %s; delete .dev to start over\n", done)
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	secret, err := app.LoadTokenSecret(cfg.TokenSecretFile)
	if err != nil {
		return err
	}
	hosts := core.NewHosts(cfg.HostsFile)
	if err := hosts.Load(ctx, db.DB); err != nil {
		return err
	}
	pcs := hosts.All()
	if len(pcs) == 0 {
		return errors.New("no PCs in the hosts file")
	}
	engine := core.NewEngine(db, cfg.HeartbeatTimeout, cfg.CheckoutSlack)
	signer := token.NewSigner(secret)
	board := core.NewQRBoard(signer, cfg.QRRotate)
	checkin := &core.Checkin{Engine: engine, Signer: signer, Board: board, Hosts: hosts, TTL: cfg.QRTokenTTL}

	rng := rand.New(rand.NewSource(seed))
	loc := cfg.Timezone
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	first := today.AddDate(0, 0, -(days - 1))
	setup := first.AddDate(0, 0, -1).Add(10 * time.Hour)

	// Students, each linked to an invented Telegram account through a
	// one-time code, the way staff and students do it. The ids are far above
	// any real Telegram id.
	used := map[string]bool{}
	var studs []*student
	for i := 0; i < nStudents; i++ {
		var name string
		for name == "" || used[name] {
			name = lastNames[rng.Intn(len(lastNames))] + " " + firstNames[rng.Intn(len(firstNames))]
		}
		used[name] = true
		id, err := core.AddStudent(ctx, db, setup, name, groups[i%len(groups)], "devseed")
		if err != nil {
			return err
		}
		code, err := core.GenerateCode(ctx, db, setup, id, 24*time.Hour, "devseed")
		if err != nil {
			return err
		}
		tg := int64(7_000_000_000_000) + id
		r, err := core.Enroll(ctx, db, setup.Add(time.Minute), core.TGUser{ID: tg, Name: name}, code)
		if err != nil || r.Result != core.EnrollLinked {
			return fmt.Errorf("link %s: %v %s", name, err, r.Result)
		}
		s := &student{id: id, tg: tg, p: 0.95 - 0.7*float64(i)/float64(nStudents),
			arrive: 13 + 3*rng.Float64(), stay: 3 + 3.5*rng.Float64(), dropAfter: -1}
		if i >= nStudents*2/3 { // a third of them stop coming at some point
			s.dropAfter = rng.Intn(days)
			s.archived = i >= nStudents-2 && s.dropAfter < days-7
		}
		studs = append(studs, s)
	}

	var steps []step
	add := func(at time.Time, do func(time.Time) error) {
		steps = append(steps, step{at, len(steps), do})
	}
	busy := map[string]time.Time{} // PC -> free again at
	sid := 0
	nVisits := 0

	visit := func(s *student, start time.Time, stay time.Duration) {
		// A free PC; the visit is skipped if all are taken.
		var free []core.Host
		for _, h := range pcs {
			if !busy[h.Name].After(start) {
				free = append(free, h)
			}
		}
		if len(free) == 0 {
			return
		}
		pc := free[rng.Intn(len(free))]
		last := start.Add(stay) // last activity
		var end time.Time       // when the PC is free again
		kind := rng.Float64()
		const (
			logout    = 0.69
			idleOut   = 0.91
			checkout  = 0.95
			powerCut  = 0.99
			overnight = 1.0 // left logged in, idle unknown: the 04:00 cleanup closes the visit
		)
		switch {
		case kind < logout:
			end = last.Add(time.Duration(5+rng.Intn(90)) * time.Second)
		case kind < idleOut:
			end = last.Add(30*time.Minute + time.Duration(rng.Intn(60))*time.Second)
		case kind < checkout:
			end = last.Add(3 * time.Minute)
		case kind < powerCut:
			end = last // silence after the last heartbeat
		default:
			d := last.In(loc)
			end = time.Date(d.Year(), d.Month(), d.Day()+1, 4, 10, 0, 0, loc)
		}
		if end.After(now.Add(-5 * time.Minute)) {
			return // keep everything in the past: no session is left open
		}
		busy[pc.Name] = end.Add(time.Minute)
		nVisits++
		sid++
		id := fmt.Sprintf("seed-%d", sid)
		idle := func(t time.Time) sql.NullInt64 {
			if kind >= powerCut && t.After(last) {
				return sql.NullInt64{} // the agent cannot read the idle time
			}
			if t.After(last) {
				return sql.NullInt64{Int64: t.Sub(last).Milliseconds(), Valid: true}
			}
			return sql.NullInt64{Int64: int64(rng.Intn(40_000)), Valid: true}
		}
		add(start, func(t time.Time) error {
			_, err := engine.AgentEvent(ctx, t, pc.Name, core.KindSessionStart, id, sql.NullInt64{})
			return err
		})
		scanAt := start.Add(time.Duration(50+rng.Intn(100)) * time.Second)
		if rng.Float64() < 0.02 { // first tries a photo of an old code
			add(scanAt.Add(-30*time.Second), func(t time.Time) error {
				tok, _ := board.Current(pc.ID, t.Add(-3*time.Minute))
				r, err := checkin.Scan(ctx, t, s.tg, tok)
				if err == nil && r.Result != core.ScanExpired {
					err = fmt.Errorf("old code on %s: %s", pc.Name, r.Result)
				}
				return err
			})
		}
		age := 5 * time.Second
		if rng.Float64() < 0.03 { // a slow scan: the code was sent around first
			age = 50 * time.Second
		}
		add(scanAt, func(t time.Time) error {
			tok, _ := board.Current(pc.ID, t.Add(-age))
			r, err := checkin.Scan(ctx, t, s.tg, tok)
			if err == nil && r.Result != core.ScanOK {
				err = fmt.Errorf("scan %s: %s", pc.Name, r.Result)
			}
			return err
		})
		if stay > 90*time.Minute && rng.Float64() < 0.02 { // someone else checks in on this login
			other := studs[rng.Intn(len(studs))]
			if other != s {
				add(start.Add(stay/2), func(t time.Time) error {
					tok, _ := board.Current(pc.ID, t.Add(-5*time.Second))
					_, err := checkin.Scan(ctx, t, other.tg, tok)
					return err
				})
			}
		}
		hbEnd := end
		if kind >= checkout && kind < powerCut {
			hbEnd = last.Add(time.Second)
		}
		for t := start.Add(2 * time.Minute); t.Before(hbEnd); t = t.Add(time.Duration(115+rng.Intn(10)) * time.Second) {
			add(t, func(t time.Time) error {
				_, err := engine.AgentEvent(ctx, t, pc.Name, core.KindHeartbeat, id, idle(t))
				return err
			})
		}
		switch {
		case kind < logout, kind >= powerCut:
			add(end, func(t time.Time) error {
				_, err := engine.AgentEvent(ctx, t, pc.Name, core.KindLogout, id, idle(t))
				return err
			})
		case kind < idleOut:
			add(end, func(t time.Time) error {
				_, err := engine.AgentEvent(ctx, t, pc.Name, core.KindIdleLogout, id, idle(t))
				return err
			})
		case kind < checkout:
			add(last, func(t time.Time) error {
				_, err := engine.Checkout(ctx, t, s.tg, s.id)
				return err
			})
			add(end, func(t time.Time) error {
				_, err := engine.AgentEvent(ctx, t, pc.Name, core.KindLogout, id, idle(t))
				return err
			})
		}
	}

	for d := 0; d < days; d++ {
		day := first.AddDate(0, 0, d)
		weekend := day.Weekday() == time.Saturday || day.Weekday() == time.Sunday
		add(day.Add(4*time.Hour), func(t time.Time) error { // the nightly cleanup
			_, err := engine.Cleanup(ctx, t)
			return err
		})
		for _, s := range studs {
			if s.dropAfter >= 0 && d > s.dropAfter {
				continue
			}
			p := s.p
			if weekend {
				p *= 0.55
			}
			if rng.Float64() > p {
				continue
			}
			arrive := s.arrive + rng.NormFloat64()*0.8
			stay := s.stay + rng.NormFloat64()*1.2
			arrive = max(9, min(arrive, 19))
			stay = max(0.75, min(stay, 22.5-arrive))
			start := day.Add(time.Duration(arrive * float64(time.Hour)))
			visit(s, start, time.Duration(stay*float64(time.Hour)))
			if rng.Float64() < 0.12 { // back after a break for a while
				back := start.Add(time.Duration((stay + 0.5 + rng.Float64()) * float64(time.Hour)))
				if back.Hour() < 22 {
					visit(s, back, time.Duration((0.75+rng.Float64())*float64(time.Hour)))
				}
			}
		}
	}

	sort.Slice(steps, func(i, j int) bool {
		if !steps[i].at.Equal(steps[j].at) {
			return steps[i].at.Before(steps[j].at)
		}
		return steps[i].seq < steps[j].seq
	})
	fmt.Printf("devseed: %d students, %d visits on %d PCs over %d days (%d events)…\n", nStudents, nVisits, len(pcs), days, len(steps))
	for i, st := range steps {
		if err := st.do(st.at); err != nil {
			return fmt.Errorf("step %d at %s: %w", i, st.at.Format(time.RFC3339), err)
		}
	}
	if _, err := engine.Sweep(ctx, now); err != nil {
		return err
	}

	// Staff corrections: a visit added for a forgotten phone.
	for i := 0; i < 2 && i < len(studs); i++ {
		s := studs[rng.Intn(len(studs)/2)]
		day := first.AddDate(0, 0, rng.Intn(max(days-1, 1)))
		st := day.Add(10 * time.Hour)
		if _, err := core.AddCorrection(ctx, db, now, core.CorrectionInput{Action: "add", StudentID: s.id, Host: pcs[0].Name,
			Start: st, End: st.Add(2 * time.Hour), Reason: "забыл телефон (пример)", Actor: "devseed"}); err != nil {
			return err
		}
	}
	for _, s := range studs {
		if s.archived {
			if err := core.SetArchived(ctx, db, now, s.id, true, "devseed"); err != nil {
				return err
			}
		}
	}
	_, err = db.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?)", marker, now.Format(time.RFC3339))
	if err == nil {
		fmt.Println("devseed: done")
	}
	return err
}

// localAddr turns a listen address such as "0.0.0.0:8080" into one to dial.
func localAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
