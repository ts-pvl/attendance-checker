package core

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"attendance/internal/store"
	"attendance/internal/token"
)

// QRBoard tracks the token currently shown on each PC. It rotates tokens
// every rotate interval and immediately after one is used, so the next
// student at the same PC gets a fresh code.
type QRBoard struct {
	signer *token.Signer
	rotate time.Duration

	mu  sync.Mutex
	cur map[int64]shownToken
}

type shownToken struct {
	tok    string
	claims token.Claims
	until  time.Time
}

func NewQRBoard(signer *token.Signer, rotate time.Duration) *QRBoard {
	return &QRBoard{signer: signer, rotate: rotate, cur: map[int64]shownToken{}}
}

// Current returns the token to display for hostID and when it rotates.
func (b *QRBoard) Current(hostID int64, now time.Time) (string, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.cur[hostID]
	if !ok || !now.Before(s.until) {
		tok, c := b.signer.Issue(uint16(hostID), now)
		s = shownToken{tok: tok, claims: c, until: now.Add(b.rotate)}
		b.cur[hostID] = s
	}
	return s.tok, s.until
}

// IsCurrent reports whether nonce is the token on screen right now. A valid
// but superseded token means the scan came from an old picture.
func (b *QRBoard) IsCurrent(hostID int64, nonce string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.cur[hostID]
	return ok && s.claims.NonceString() == nonce
}

// Consume drops the token on screen if it is the one just used.
func (b *QRBoard) Consume(hostID int64, nonce string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.cur[hostID]; ok && s.claims.NonceString() == nonce {
		delete(b.cur, hostID)
	}
}

// Student is a row of the students table.
type Student struct {
	ID       int64
	Name     string
	Group    string
	Archived bool
}

// LinkedStudent returns the student linked to a Telegram account, or nil.
func LinkedStudent(ctx context.Context, q store.Querier, tgUserID int64) (*Student, error) {
	var s Student
	err := q.QueryRowContext(ctx, `SELECT s.id, s.name, s.grp, s.archived FROM telegram_links l
		JOIN students s ON s.id = l.student_id WHERE l.tg_user_id = ?`, tgUserID).
		Scan(&s.ID, &s.Name, &s.Group, &s.Archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Checkin validates scanned tokens and turns them into visits.
type Checkin struct {
	Engine *Engine
	Signer *token.Signer
	Board  *QRBoard
	Hosts  *Hosts
	TTL    time.Duration
}

type ScanReply struct {
	Result  string // one of the Scan* constants
	Host    string
	Student *Student
	Moved   *Closure // the scanner's earlier visit on another PC, if this scan closed one
	// Displaced lists the visits of other students on this PC that the scan closed.
	Displaced []Closure
}

// Scan handles "/start <token>" from Telegram user tgUserID.
func (c *Checkin) Scan(ctx context.Context, now time.Time, tgUserID int64, tok string) (ScanReply, error) {
	ev := &Event{At: now, Kind: KindScan, TGUserID: tgUserID}
	reject := func(result string) (ScanReply, error) {
		ev.Result = result
		return ScanReply{Result: result, Host: ev.Host}, c.Engine.recordOnly(ctx, ev)
	}
	claims, err := c.Signer.Parse(tok)
	if err != nil {
		ev.Detail.Note = err.Error()
		return reject(ScanBadToken)
	}
	ev.Detail.Nonce = claims.NonceString()
	ev.Detail.LatencyMs = now.Sub(claims.Issued).Milliseconds()
	host, ok := c.Hosts.ByID(int64(claims.HostID))
	if !ok {
		return reject(ScanUnknownHost)
	}
	ev.Host = host.Name
	if err := claims.Check(now, c.TTL); err != nil {
		ev.Detail.Note = err.Error()
		return reject(ScanExpired)
	}
	st, err := LinkedStudent(ctx, c.Engine.db, tgUserID)
	if err != nil {
		return ScanReply{}, err
	}
	if st == nil || st.Archived {
		return reject(ScanNotLinked)
	}
	ev.StudentID = st.ID
	ev.Detail.Stale = !c.Board.IsCurrent(host.ID, ev.Detail.Nonce)

	closures, err := c.Engine.scan(ctx, ev, claims.Issued.Add(c.TTL+time.Minute))
	if err != nil {
		return ScanReply{}, err
	}
	c.Board.Consume(host.ID, ev.Detail.Nonce)
	r := ScanReply{Result: ev.Result, Host: host.Name, Student: st}
	for i := range closures {
		switch closures[i].Reason {
		case ReasonMoved:
			r.Moved = &closures[i]
		case ReasonTakenOver:
			r.Displaced = append(r.Displaced, closures[i])
		}
	}
	return r, nil
}

// scan enforces single use of the nonce and applies the scan atomically.
func (e *Engine) scan(ctx context.Context, ev *Event, nonceExpires time.Time) ([]Closure, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Closure
	err := e.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO used_nonces(nonce, expires_at) VALUES(?, ?) ON CONFLICT(nonce) DO NOTHING",
			ev.Detail.Nonce, store.Ms(nonceExpires))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			ev.Result = ScanUsed
			return record(ctx, tx, ev)
		}
		if err := record(ctx, tx, ev); err != nil {
			return err
		}
		cl, result, err := e.apply(ctx, tx, ev)
		if err != nil {
			return err
		}
		out, ev.Result = cl, result
		return setResult(ctx, tx, ev.ID, result)
	})
	return out, err
}

// PruneNonces forgets used nonces whose tokens can no longer be accepted.
func PruneNonces(ctx context.Context, db *store.DB, now time.Time) error {
	_, err := db.ExecContext(ctx, "DELETE FROM used_nonces WHERE expires_at < ?", store.Ms(now))
	return err
}
