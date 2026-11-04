// Package bot is the phone side: a Telegram bot that links accounts with
// enrollment codes, checks students in from scanned QR tokens, and tells
// them when a visit closes. It only answers in private chats.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"attendance/internal/core"
	"attendance/internal/hours"
	"attendance/internal/i18n"
	"attendance/internal/store"
	"attendance/internal/token"
)

// Telegram is the part of the Bot API the bot uses (faked in tests).
type Telegram interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
}

type Bot struct {
	TG      Telegram
	DB      *store.DB
	Checkin *core.Checkin
	Calc    hours.Calc
	Lang    string
	Now     func() time.Time
	Log     *slog.Logger

	limiter *limiter
	queue   chan outgoing

	mu     sync.Mutex
	status Status
}

// Status is shown on the Today screen and used by /healthz.
type Status struct {
	Enabled    bool
	LastPollOK time.Time
	LastError  string
	LastErrAt  time.Time
	LastReply  time.Time
}

type outgoing struct {
	chatID int64
	text   string
}

func New(tg Telegram, db *store.DB, ci *core.Checkin, calc hours.Calc, lang string, log *slog.Logger) *Bot {
	return &Bot{
		TG: tg, DB: db, Checkin: ci, Calc: calc, Lang: lang, Now: time.Now, Log: log,
		limiter: newLimiter(10, 3*time.Second),
		queue:   make(chan outgoing, 1000),
		status:  Status{Enabled: true},
	}
}

func (b *Bot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

func (b *Bot) t(key string, args ...any) string { return i18n.T(b.Lang, key, args...) }

func (b *Bot) clock(t time.Time) string { return t.In(b.Calc.Loc).Format("15:04") }

const offsetKey = "telegram_offset"

// Run polls Telegram until ctx is cancelled. Errors are retried with backoff;
// the process never exits because Telegram is unreachable.
func (b *Bot) Run(ctx context.Context) {
	go b.sender(ctx)
	offStr, _ := store.GetSetting(ctx, b.DB, offsetKey)
	offset, _ := strconv.ParseInt(offStr, 10, 64)
	backoff := time.Second
	for ctx.Err() == nil {
		ups, err := b.TG.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.setError(err)
			wait := backoff
			var ae *APIError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				wait = time.Duration(ae.RetryAfter) * time.Second
			}
			b.Log.Warn("telegram poll failed", "err", err, "retry_in", wait)
			sleep(ctx, wait)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		b.mu.Lock()
		b.status.LastPollOK = b.Now()
		b.mu.Unlock()
		if len(ups) == 0 {
			continue
		}
		// Handle a batch concurrently (a burst of scans at lesson start),
		// then confirm it to Telegram by advancing the offset.
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, u := range ups {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(u Update) {
				defer wg.Done()
				defer func() { <-sem }()
				b.HandleUpdate(ctx, u)
			}(u)
		}
		wg.Wait()
		if err := store.SetSetting(ctx, b.DB, offsetKey, strconv.FormatInt(offset, 10)); err != nil {
			b.Log.Error("save telegram offset", "err", err)
		}
	}
}

func (b *Bot) setError(err error) {
	b.mu.Lock()
	b.status.LastError = err.Error()
	b.status.LastErrAt = b.Now()
	b.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// HandleUpdate processes one update and sends the reply.
func (b *Bot) HandleUpdate(ctx context.Context, u Update) {
	m := u.Message
	if m == nil || m.From == nil || m.From.IsBot || m.Chat.Type != "private" {
		return // groups, channels and bots are ignored
	}
	if !b.limiter.allow(m.From.ID, b.Now()) {
		if b.limiter.shouldWarn(m.From.ID, b.Now()) {
			b.reply(ctx, m.Chat.ID, b.t("bot.rate_limited"))
		}
		return
	}
	reply, err := b.handle(ctx, m)
	if err != nil {
		b.Log.Error("bot handler", "err", err, "tg_user", m.From.ID)
		reply = b.t("bot.error")
	}
	if reply != "" {
		b.reply(ctx, m.Chat.ID, reply)
	}
}

func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	if err := b.send(ctx, chatID, text); err != nil {
		b.Log.Warn("telegram send failed", "err", err)
		return
	}
}

func (b *Bot) send(ctx context.Context, chatID int64, text string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = b.TG.SendMessage(ctx, chatID, text); err == nil {
			b.mu.Lock()
			b.status.LastReply = b.Now()
			b.mu.Unlock()
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) {
			if ae.RetryAfter == 0 {
				return err // 400/403 (user blocked the bot) will not get better
			}
			sleep(ctx, time.Duration(ae.RetryAfter)*time.Second)
			continue
		}
		sleep(ctx, time.Duration(attempt+1)*time.Second)
	}
	return err
}

func displayName(u *User) string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

func (b *Bot) handle(ctx context.Context, m *Message) (string, error) {
	cmd, arg := parseCommand(m.Text)
	now := b.Now()
	switch cmd {
	case "/start":
		switch {
		case arg == "":
			st, err := core.LinkedStudent(ctx, b.DB, m.From.ID)
			if err != nil {
				return "", err
			}
			if st == nil {
				return b.t("bot.welcome"), nil
			}
			return b.t("bot.already_same", st.Name) + "\n\n" + b.t("bot.help"), nil
		case token.IsCheckin(arg):
			return b.scan(ctx, now, m.From.ID, arg)
		case token.IsEnrollCode(arg):
			return b.enroll(ctx, now, m.From, arg)
		default:
			return b.t("bot.code_invalid"), nil
		}
	case "/checkout":
		return b.checkout(ctx, now, m.From.ID)
	case "/status":
		return b.statusText(ctx, now, m.From.ID)
	case "/id":
		return b.t("bot.your_id", m.From.ID), nil
	case "/help":
		st, err := core.LinkedStudent(ctx, b.DB, m.From.ID)
		if err != nil {
			return "", err
		}
		if st == nil {
			return b.t("bot.welcome") + "\n\n" + b.t("bot.help"), nil
		}
		return b.t("bot.help"), nil
	default:
		st, err := core.LinkedStudent(ctx, b.DB, m.From.ID)
		if err != nil {
			return "", err
		}
		if st == nil {
			return b.t("bot.welcome"), nil
		}
		return b.t("bot.help"), nil
	}
}

// parseCommand splits "/start@SomeBot abc" into ("/start", "abc").
func parseCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	return strings.ToLower(cmd), strings.TrimSpace(arg)
}

func (b *Bot) scan(ctx context.Context, now time.Time, tgUser int64, tok string) (string, error) {
	r, err := b.Checkin.Scan(ctx, now, tgUser, tok)
	if err != nil {
		return "", err
	}
	switch r.Result {
	case core.ScanOK:
		msg := b.t("bot.checked_in", r.Host)
		if r.Moved != nil {
			msg += "\n" + b.t("bot.moved", r.Moved.Host, b.clock(r.Moved.End))
		}
		return msg, nil
	case core.ScanAlready:
		return b.t("bot.already_in", r.Host), nil
	case core.ScanNoSession:
		return b.t("bot.no_session"), nil
	case core.ScanExpired:
		return b.t("bot.expired"), nil
	case core.ScanUsed:
		return b.t("bot.used"), nil
	case core.ScanNotLinked:
		return b.t("bot.not_linked"), nil
	default:
		return b.t("bot.bad_token"), nil
	}
}

func (b *Bot) enroll(ctx context.Context, now time.Time, from *User, code string) (string, error) {
	r, err := core.Enroll(ctx, b.DB, now, core.TGUser{ID: from.ID, Username: from.Username, Name: displayName(from)}, code)
	if err != nil {
		return "", err
	}
	switch r.Result {
	case core.EnrollLinked:
		return b.t("bot.linked", r.Student.Name), nil
	case core.EnrollAlreadySame:
		return b.t("bot.already_same", r.Student.Name), nil
	case core.EnrollTGTaken:
		return b.t("bot.tg_taken", r.Student.Name), nil
	case core.EnrollStudentTaken:
		return b.t("bot.student_taken"), nil
	case core.EnrollUsed:
		return b.t("bot.code_used"), nil
	case core.EnrollExpired:
		return b.t("bot.code_expired"), nil
	default:
		return b.t("bot.code_invalid"), nil
	}
}

func (b *Bot) checkout(ctx context.Context, now time.Time, tgUser int64) (string, error) {
	st, err := core.LinkedStudent(ctx, b.DB, tgUser)
	if err != nil {
		return "", err
	}
	if st == nil || st.Archived {
		return b.t("bot.not_linked"), nil
	}
	cl, err := b.Checkin.Engine.Checkout(ctx, now, tgUser, st.ID)
	if err != nil {
		return "", err
	}
	if cl == nil {
		return b.t("bot.checkout_none"), nil
	}
	today, err := b.Calc.Today(ctx, b.DB, st.ID, now)
	if err != nil {
		return "", err
	}
	return b.t("bot.checkout_done", b.clock(cl.End), i18n.Duration(b.Lang, today)), nil
}

func (b *Bot) statusText(ctx context.Context, now time.Time, tgUser int64) (string, error) {
	st, err := core.LinkedStudent(ctx, b.DB, tgUser)
	if err != nil {
		return "", err
	}
	if st == nil || st.Archived {
		return b.t("bot.not_linked"), nil
	}
	today, err := b.Calc.Today(ctx, b.DB, st.ID, now)
	if err != nil {
		return "", err
	}
	var host string
	var since int64
	err = b.DB.QueryRowContext(ctx, "SELECT host, confirmed_at FROM presence WHERE student_id = ? AND ended_at IS NULL", st.ID).Scan(&host, &since)
	if err != nil {
		return b.t("bot.status_none", i18n.Duration(b.Lang, today)), nil
	}
	return b.t("bot.status_open", host, b.clock(store.FromMs(since)), i18n.Duration(b.Lang, today)), nil
}

// Notify queues a message for every visit that closed on its own (idle
// logout, logout, timeout, nightly cleanup). Scans and /checkout already
// reply directly.
func (b *Bot) Notify(ctx context.Context, closures []core.Closure) {
	for _, c := range closures {
		if c.Reason == core.ReasonMoved || c.Reason == core.ReasonCheckout {
			continue
		}
		var chatID int64
		if err := b.DB.QueryRowContext(ctx, "SELECT tg_user_id FROM telegram_links WHERE student_id = ?", c.StudentID).Scan(&chatID); err != nil {
			continue // unlinked meanwhile
		}
		today, err := b.Calc.Today(ctx, b.DB, c.StudentID, b.Now())
		if err != nil {
			b.Log.Error("today total", "err", err)
			continue
		}
		text := b.t("bot.closed", b.clock(c.End), c.Host, i18n.Reason(b.Lang, c.Reason), i18n.Duration(b.Lang, today))
		switch c.Reason {
		case core.ReasonTimeout, core.ReasonReplaced, core.ReasonAutoClosed:
			text += "\n" + b.t("bot.rescan_hint")
		}
		select {
		case b.queue <- outgoing{chatID, text}:
		default:
			b.Log.Warn("notification queue full, dropping message")
		}
	}
}

func (b *Bot) sender(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-b.queue:
			if err := b.send(ctx, m.chatID, m.text); err != nil {
				b.Log.Warn("notification failed", "err", err)
			}
		}
	}
}

// SetupCommands registers the command menu shown in Telegram clients.
func SetupCommands(ctx context.Context, api *API, lang string) error {
	desc := map[string][2]string{
		"status":   {"Current visit and today's hours", "Текущее посещение и часы за сегодня"},
		"checkout": {"Check out now", "Отметить уход сейчас"},
		"help":     {"How to check in", "Как отметиться"},
	}
	var cmds []Command
	for _, c := range []string{"status", "checkout", "help"} {
		d := desc[c][0]
		if lang == "ru" {
			d = desc[c][1]
		}
		cmds = append(cmds, Command{Command: c, Description: d})
	}
	return api.SetMyCommands(ctx, cmds)
}

// limiter is a per-user token bucket for bot messages.
type limiter struct {
	mu     sync.Mutex
	burst  float64
	every  time.Duration
	users  map[int64]*bucket
	pruned time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
	warned time.Time
}

func newLimiter(burst int, every time.Duration) *limiter {
	return &limiter{burst: float64(burst), every: every, users: map[int64]*bucket{}}
}

func (l *limiter) allow(user int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.pruned) > 10*time.Minute {
		for id, b := range l.users {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.users, id)
			}
		}
		l.pruned = now
	}
	b := l.users[user]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.users[user] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.last))/float64(l.every))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// shouldWarn limits the "too many messages" reply to once a minute.
func (l *limiter) shouldWarn(user int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.users[user]
	if b == nil || now.Sub(b.warned) < time.Minute {
		return false
	}
	b.warned = now
	return true
}

// String is a one-line summary for logs.
func (s Status) String() string {
	return fmt.Sprintf("poll_ok=%s reply=%s err=%q", s.LastPollOK.Format(time.RFC3339), s.LastReply.Format(time.RFC3339), s.LastError)
}
