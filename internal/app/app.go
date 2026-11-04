// Package app wires the server together and runs the background jobs.
package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"attendance/internal/bot"
	"attendance/internal/config"
	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/store"
	"attendance/internal/token"
	"attendance/internal/web"
)

type Options struct {
	NoBot bool
}

type App struct {
	Cfg    *config.Config
	DB     *store.DB
	Engine *core.Engine
	Hosts  *core.Hosts
	Bot    *bot.Bot
	Web    *web.Server
	Log    *slog.Logger
	Now    func() time.Time
}

// LoadTokenSecret reads the QR token HMAC key, creating it on first start.
func LoadTokenSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key := token.NewSecret(32)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("%s: expected at least 16 hex-encoded bytes", path)
	}
	return key, nil
}

// New opens everything the server needs. The caller runs it with Run.
func New(ctx context.Context, cfg *config.Config, opts Options, log *slog.Logger) (*App, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		return nil, err
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	secret, err := LoadTokenSecret(cfg.TokenSecretFile)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("token secret: %w", err)
	}
	hostKey, err := config.ReadSecret(cfg.HostKeyFile)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("host key: %w", err)
	}
	if len(hostKey) < 16 {
		db.Close()
		return nil, errors.New("host key must be at least 16 characters (use: attendance gen-key)")
	}
	hosts := core.NewHosts(cfg.HostsFile)
	if err := hosts.Load(ctx, db.DB); err != nil {
		db.Close()
		return nil, fmt.Errorf("hosts: %w", err)
	}
	engine := core.NewEngine(db, cfg.HeartbeatTimeout, cfg.CheckoutSlack)
	signer := token.NewSigner(secret)
	board := core.NewQRBoard(signer, cfg.QRRotate)
	checkin := &core.Checkin{Engine: engine, Signer: signer, Board: board, Hosts: hosts, TTL: cfg.QRTokenTTL}
	calc := hours.Calc{Loc: cfg.Timezone, Policy: hours.Policy{
		Rounding: cfg.HoursRounding, MinVisit: cfg.HoursMinVisit, DailyCap: cfg.HoursDailyCap,
	}}
	a := &App{Cfg: cfg, DB: db, Engine: engine, Hosts: hosts, Log: log, Now: time.Now}

	if !opts.NoBot {
		if cfg.BotTokenFile == "" {
			log.Warn("no bot_token_file configured: Telegram bot disabled")
		} else {
			botToken, err := config.ReadSecret(cfg.BotTokenFile)
			if err != nil {
				db.Close()
				return nil, fmt.Errorf("bot token: %w", err)
			}
			api := bot.NewAPI(botToken)
			a.Bot = bot.New(api, db, checkin, calc, cfg.Lang, log.With("part", "bot"))
			go a.checkBotIdentity(ctx, api)
		}
	}
	a.Web = &web.Server{
		Cfg: cfg, DB: db, Engine: engine, Checkin: checkin, Hosts: hosts, Board: board, Bot: a.Bot,
		Calc: calc, HostKey: hostKey, Log: log.With("part", "web"), Now: time.Now,
	}
	return a, nil
}

// checkBotIdentity verifies the configured username (it goes into every QR
// link) and registers the command menu. Failures are logged, not fatal:
// Telegram may simply be unreachable right now.
func (a *App) checkBotIdentity(ctx context.Context, api *bot.API) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		me, err := api.GetMe(cctx)
		cancel()
		if err == nil {
			if !strings.EqualFold(me.Username, a.Cfg.BotUsername) {
				a.Log.Error("bot_username does not match the bot token; QR links will open the wrong bot",
					"configured", a.Cfg.BotUsername, "actual", me.Username)
			}
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := bot.SetupCommands(cctx, api, a.Cfg.Lang); err != nil {
				a.Log.Warn("set bot commands", "err", err)
			}
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(min(attempt+1, 10)) * 30 * time.Second):
		}
	}
}

// Run serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	defer a.DB.Close()
	h, err := a.Web.Handler()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              a.Cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		a.Log.Info("listening", "addr", a.Cfg.Listen, "hosts", len(a.Hosts.All()), "bot", a.Bot != nil, "lang", a.Cfg.Lang, "tz", a.Cfg.Timezone.String())
		errc <- srv.ListenAndServe()
	}()
	if a.Bot != nil {
		go a.Bot.Run(ctx)
	}
	go a.jobs(ctx)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func (a *App) notify(ctx context.Context, cl []core.Closure) {
	if a.Bot != nil && len(cl) > 0 {
		a.Bot.Notify(ctx, cl)
	}
}

func (a *App) jobs(ctx context.Context) {
	sweep := time.NewTicker(30 * time.Second)
	minute := time.NewTicker(time.Minute)
	defer sweep.Stop()
	defer minute.Stop()
	a.daily(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			cl, err := a.Engine.Sweep(ctx, a.Now())
			if err != nil {
				a.Log.Error("sweep", "err", err)
			}
			for _, c := range cl {
				a.Log.Info("visit closed by timeout", "presence", c.PresenceID, "host", c.Host)
			}
			a.notify(ctx, cl)
		case <-minute.C:
			now := a.Now()
			if err := core.PruneNonces(ctx, a.DB, now); err != nil {
				a.Log.Error("prune nonces", "err", err)
			}
			if err := web.PruneStaffSessions(ctx, a.DB, now, a.Cfg.StaffSessionIdle, a.Cfg.StaffSessionMax); err != nil {
				a.Log.Error("prune staff sessions", "err", err)
			}
			if changed, err := a.Hosts.ReloadIfChanged(ctx, a.DB.DB); err != nil {
				a.Log.Error("reload hosts file (keeping the previous list)", "err", err)
			} else if changed {
				a.Log.Info("hosts file reloaded", "hosts", len(a.Hosts.All()))
			}
			a.daily(ctx)
		}
	}
}

// Due reports whether a daily job scheduled at clock should run now, given
// when it last ran. A run missed by less than window (server was down) is
// caught up; an older one is skipped so a late start cannot close the
// afternoon's visits.
func Due(now time.Time, at config.Clock, last time.Time, window time.Duration) (sched time.Time, due, missed bool) {
	sched = at.On(now)
	if now.Before(sched) {
		y, m, d := now.Date()
		sched = at.On(time.Date(y, m, d-1, 12, 0, 0, 0, now.Location()))
	}
	if !last.Before(sched) {
		return sched, false, false
	}
	if now.Sub(sched) > window {
		return sched, false, true
	}
	return sched, true, false
}

func (a *App) lastRun(ctx context.Context, key string) time.Time {
	v, err := store.GetSetting(ctx, a.DB, key)
	if err != nil || v == "" {
		return time.Time{}
	}
	ms, _ := strconv.ParseInt(v, 10, 64)
	return store.FromMs(ms)
}

func (a *App) setLastRun(ctx context.Context, key string, t time.Time) {
	if err := store.SetSetting(ctx, a.DB, key, strconv.FormatInt(store.Ms(t), 10)); err != nil {
		a.Log.Error("save job state", "key", key, "err", err)
	}
}

func (a *App) daily(ctx context.Context) {
	now := a.Now().In(a.Cfg.Timezone)

	lastCleanup := a.lastRun(ctx, "last_cleanup")
	if sched, due, missed := Due(now, a.Cfg.CleanupAt, lastCleanup, 2*time.Hour); due {
		cl, err := a.Engine.Cleanup(ctx, now)
		if err != nil {
			a.Log.Error("nightly cleanup", "err", err)
		} else {
			a.Log.Info("nightly cleanup", "closed", len(cl))
			a.notify(ctx, cl)
			a.setLastRun(ctx, "last_cleanup", sched)
			a.pruneHeartbeats(ctx, now)
		}
	} else if missed {
		if !lastCleanup.IsZero() { // a fresh install has nothing to catch up on
			a.Log.Warn("nightly cleanup missed (server was down); skipping until tomorrow", "scheduled", sched)
		}
		a.setLastRun(ctx, "last_cleanup", sched)
	}

	if sched, due, missed := Due(now, a.Cfg.BackupAt, a.lastRun(ctx, "last_backup"), 6*time.Hour); due || missed {
		if err := a.Backup(ctx, now); err != nil {
			a.Log.Error("nightly backup", "err", err)
		} else {
			a.setLastRun(ctx, "last_backup", sched)
		}
	}
}

func (a *App) pruneHeartbeats(ctx context.Context, now time.Time) {
	if a.Cfg.HeartbeatRetentionDays <= 0 {
		return
	}
	cut := now.AddDate(0, 0, -a.Cfg.HeartbeatRetentionDays)
	res, err := a.DB.ExecContext(ctx, "DELETE FROM events WHERE kind = 'heartbeat' AND at < ?", store.Ms(cut))
	if err != nil {
		a.Log.Error("prune heartbeat events", "err", err)
		return
	}
	n, _ := res.RowsAffected()
	a.Log.Info("pruned old heartbeat events", "rows", n)
}

// Backup writes a dated copy of the database and removes old copies.
func (a *App) Backup(ctx context.Context, now time.Time) error {
	dir := a.Cfg.BackupDir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	dst := filepath.Join(dir, "attendance-"+now.Format("2006-01-02")+".db")
	os.Remove(dst) // VACUUM INTO refuses to overwrite
	if err := a.DB.Backup(ctx, dst); err != nil {
		return err
	}
	a.Log.Info("backup written", "file", dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "attendance-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > a.Cfg.BackupKeep && a.Cfg.BackupKeep > 0 {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return nil
}
