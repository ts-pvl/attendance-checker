// Package platform reads students' data from an 01Edu learning platform:
// logins, names, the events they are in with their level and XP in each,
// and their audit ratio. It uses the platform's read-only API,
// as 01Edu documents it for admins:
//
//  1. An admin creates an access token in the platform's Git service
//     (Settings → Applications → Generate New Token).
//  2. GET /api/auth/token?token=<access token> returns a JWT (a JSON string)
//     for the "admin read only" role.
//  3. POST /api/graphql-engine/v1/graphql with "Authorization: Bearer <JWT>"
//     runs GraphQL queries (Hasura).
//
// The access token travels in a URL, so no error message, log line or
// stored status may contain a URL or the token: see Client.redact.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Client talks to one platform.
type Client struct {
	Base  string // https://host, no trailing slash
	Token string // the admin's access token
	HTTP  *http.Client
}

// User is one platform user as the sync stores it. Of the platform's
// personal data only the names and the email are asked for: ID numbers,
// addresses, phones and the like never leave the platform.
type User struct {
	ID                  int64
	Login               string
	FirstName, LastName string // in Cyrillic when the platform has both
	OtherName           string // "First Last" in Latin letters, when it differs
	Email               string
	AuditRatio          float64
	HasAuditRatio       bool
	Events              []EventStat // sorted by event id
	Current             int64       // the current event (see current), 0: none
}

// EventStat is a user's level and XP in one event. The platform counts the
// XP of a piscine's checkpoints under the piscine itself.
type EventStat struct {
	EventID int64
	Level   int64
	XP      int64
	LastXP  time.Time // the latest XP in this event, zero: none
}

// Event is a platform event: a piscine, a module, a checkpoint.
type Event struct {
	ID         int64
	Path       string // e.g. /campus/piscinego
	ParentID   int64  // 0: a top-level event
	Start, End time.Time
}

// Request says whom to read: the users with these logins, and everyone in
// these events.
type Request struct {
	Logins []string
	Events []int64
}

// Data is what Fetch read.
type Data struct {
	Users  []User  // sorted by id
	Events []Event // the events those users keep, sorted by id
}

const maxBody = 64 << 20

// redact removes the access token from a message.
func (c *Client) redact(s string) string {
	if c.Token == "" {
		return s
	}
	for _, t := range []string{c.Token, url.QueryEscape(c.Token)} {
		s = strings.ReplaceAll(s, t, "***")
	}
	return s
}

// fail builds an error without the request URL (a *url.Error would print it).
func (c *Client) fail(what string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%s: %s", what, c.redact(err.Error()))
}

func (c *Client) do(req *http.Request, what string) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, c.fail(what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, c.fail(what, err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return nil, fmt.Errorf("%s: HTTP %d %s", what, resp.StatusCode, c.redact(snippet))
	}
	return body, nil
}

// JWT exchanges the access token for a read-only JWT.
func (c *Client) JWT(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/auth/token?token="+url.QueryEscape(c.Token), nil)
	if err != nil {
		return "", c.fail("token request", err)
	}
	body, err := c.do(req, "token request")
	if err != nil {
		return "", err
	}
	var jwt string
	if json.Unmarshal(body, &jwt) != nil {
		jwt = strings.TrimSpace(string(body))
	}
	if strings.Count(jwt, ".") != 2 || strings.ContainsAny(jwt, " \n\"") {
		return "", errors.New("token request: the platform did not answer with a token")
	}
	return jwt, nil
}

// Query runs one GraphQL query with its variables and decodes its data
// into out. Everything that varies goes in vars, never into the query text.
func (c *Client) Query(ctx context.Context, jwt, query string, vars map[string]any, out any) error {
	b, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("query: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/graphql-engine/v1/graphql", bytes.NewReader(b))
	if err != nil {
		return c.fail("query", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	body, err := c.do(req, "query")
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return fmt.Errorf("query: unreadable answer: %v", err)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("query: %s", c.redact(res.Errors[0].Message))
	}
	dec = json.NewDecoder(bytes.NewReader(res.Data))
	dec.UseNumber()
	return dec.Decode(out)
}

func number(n json.Number) (float64, bool) {
	f, err := n.Float64()
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// whole reads a number that must be an integer (an id, a level).
func whole(n json.Number) (int64, bool) {
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, ok := number(n)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// text reads a JSON value that should be a string (a name in attrs) with
// its spaces tidied; anything else is "".
func text(v any) string {
	s, _ := v.(string)
	return strings.Join(strings.Fields(s), " ")
}

// stamp reads a platform timestamp; zero if absent or unreadable.
func stamp(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, *s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05", *s); err == nil {
		return t
	}
	return time.Time{}
}

const (
	chunkSize = 500  // ids or logins in one query
	pageLimit = 1000 // rows asked for at once; the server may send fewer
	maxPages  = 10000
)

func chunks[T any](list []T) [][]T {
	var out [][]T
	for i := 0; i < len(list); i += chunkSize {
		out = append(out, list[i:min(i+chunkSize, len(list))])
	}
	return out
}

// pages runs a query over all its rows in id order: the query takes $after
// (the last id seen) and $limit and names its rows "rows". A page may be cut
// short by the server, so only an empty page ends the loop.
func pages[T any](ctx context.Context, c *Client, jwt, query string, vars map[string]any, id func(*T) json.Number, each func(*T)) error {
	var after int64
	for page := 0; ; page++ {
		if page >= maxPages {
			return errors.New("query: too many rows")
		}
		v := maps.Clone(vars)
		v["after"], v["limit"] = after, pageLimit
		var res struct {
			Rows []T `json:"rows"`
		}
		if err := c.Query(ctx, jwt, query, v, &res); err != nil {
			return err
		}
		if len(res.Rows) == 0 {
			return nil
		}
		for i := range res.Rows {
			n, ok := whole(id(&res.Rows[i]))
			if !ok || n <= after {
				return errors.New("query: rows came out of order")
			}
			after = n
			each(&res.Rows[i])
		}
	}
}

// The user fields read. attrs holds personal documents, so only the names
// are taken from it, one path at a time.
const userFields = `id login firstName lastName email auditRatio
	firstNameCyr: attrs(path: "firstNameCyr") lastNameCyr: attrs(path: "lastNameCyr")
	firstNameLat: attrs(path: "firstNameLat") lastNameLat: attrs(path: "lastNameLat")`

type userRow struct {
	ID           json.Number  `json:"id"`
	Login        string       `json:"login"`
	FirstName    *string      `json:"firstName"`
	LastName     *string      `json:"lastName"`
	Email        *string      `json:"email"`
	AuditRatio   *json.Number `json:"auditRatio"`
	FirstNameCyr any          `json:"firstNameCyr"`
	LastNameCyr  any          `json:"lastNameCyr"`
	FirstNameLat any          `json:"firstNameLat"`
	LastNameLat  any          `json:"lastNameLat"`
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// user builds a User from its row: the Cyrillic names when both are there,
// else the plain ones; the Latin names, else the plain ones, as OtherName
// when they differ.
func (r *userRow) user(id int64) User {
	u := User{ID: id, Login: strings.TrimSpace(r.Login), Email: text(deref(r.Email))}
	plainFirst, plainLast := text(deref(r.FirstName)), text(deref(r.LastName))
	u.FirstName, u.LastName = plainFirst, plainLast
	if f, l := text(r.FirstNameCyr), text(r.LastNameCyr); f != "" && l != "" {
		u.FirstName, u.LastName = f, l
	}
	latFirst, latLast := plainFirst, plainLast
	if f, l := text(r.FirstNameLat), text(r.LastNameLat); f != "" && l != "" {
		latFirst, latLast = f, l
	}
	if other := latFirst + " " + latLast; latFirst != "" && latLast != "" && !strings.EqualFold(other, u.FirstName+" "+u.LastName) {
		u.OtherName = other
	}
	if r.AuditRatio != nil {
		if f, ok := number(*r.AuditRatio); ok {
			u.AuditRatio, u.HasAuditRatio = f, true
		}
	}
	return u
}

type memberRow struct {
	ID      json.Number  `json:"id"`
	UserID  json.Number  `json:"userId"`
	EventID json.Number  `json:"eventId"`
	Level   *json.Number `json:"level"`
}

type xpRow struct {
	ID        json.Number  `json:"id"`
	UserID    json.Number  `json:"userId"`
	EventID   *json.Number `json:"eventId"`
	Amount    json.Number  `json:"amount"`
	CreatedAt *string      `json:"createdAt"`
}

type eventRow struct {
	ID       json.Number  `json:"id"`
	Path     string       `json:"path"`
	ParentID *json.Number `json:"parentId"`
	StartAt  *string      `json:"startAt"`
	EndAt    *string      `json:"endAt"`
}

func sortedIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Fetch reads the users with the requested logins and everyone in the
// requested events, with the events they are in, their level and XP in
// each, and their audit ratio. Logins that are not on the platform are
// simply not found: the stored data shows which students they belong to.
func (c *Client) Fetch(ctx context.Context, r Request) (Data, []string, error) {
	if len(r.Logins) == 0 && len(r.Events) == 0 {
		return Data{}, nil, ErrNothingToRead
	}
	jwt, err := c.JWT(ctx)
	if err != nil {
		return Data{}, nil, err
	}
	var warnings []string
	want := map[int64]bool{}

	// 1. The users with these logins. Platform logins are in lower case, so
	// a login typed with capitals is looked for both ways.
	var logins []string
	seen := map[string]bool{}
	for _, l := range r.Logins {
		for _, v := range []string{l, strings.ToLower(l)} {
			if v != "" && !seen[v] {
				seen[v] = true
				logins = append(logins, v)
			}
		}
	}
	for _, part := range chunks(logins) {
		var res struct {
			User []struct {
				ID json.Number `json:"id"`
			} `json:"user"`
		}
		if err := c.Query(ctx, jwt, `query($logins: [String!]) { user(where: {login: {_in: $logins}}) { id } }`,
			map[string]any{"logins": part}, &res); err != nil {
			return Data{}, nil, err
		}
		for _, u := range res.User {
			if id, ok := whole(u.ID); ok {
				want[id] = true
			}
		}
	}

	// 2. Everyone in these events.
	if len(r.Events) > 0 {
		var res struct {
			Event []struct {
				ID json.Number `json:"id"`
			} `json:"event"`
		}
		if err := c.Query(ctx, jwt, `query($ids: [Int!]) { event(where: {id: {_in: $ids}}) { id } }`,
			map[string]any{"ids": r.Events}, &res); err != nil {
			return Data{}, nil, err
		}
		found := map[int64]bool{}
		for _, e := range res.Event {
			if id, ok := whole(e.ID); ok {
				found[id] = true
			}
		}
		for _, e := range r.Events {
			if !found[e] {
				warnings = append(warnings, fmt.Sprintf("event %d was not found on the platform", e))
			}
		}
		err := pages(ctx, c, jwt, `query($ids: [Int!], $after: Int!, $limit: Int!) {
			rows: event_user(where: {eventId: {_in: $ids}, id: {_gt: $after}}, order_by: {id: asc}, limit: $limit) { id userId eventId } }`,
			map[string]any{"ids": r.Events}, func(m *memberRow) json.Number { return m.ID },
			func(m *memberRow) {
				if id, ok := whole(m.UserID); ok {
					want[id] = true
				}
			})
		if err != nil {
			return Data{}, nil, err
		}
	}

	// 3. The users themselves.
	ids := sortedIDs(want)
	users := map[int64]*User{}
	for _, part := range chunks(ids) {
		var res struct {
			User []userRow `json:"user"`
		}
		if err := c.Query(ctx, jwt, `query($ids: [Int!]) { user(where: {id: {_in: $ids}}) { `+userFields+` } }`,
			map[string]any{"ids": part}, &res); err != nil {
			return Data{}, nil, err
		}
		for i := range res.User {
			id, ok := whole(res.User[i].ID)
			if !ok || strings.TrimSpace(res.User[i].Login) == "" {
				continue
			}
			u := res.User[i].user(id)
			users[id] = &u
		}
	}
	ids = sortedIDs(users)

	// 4. Their events and levels, and 5. their XP, by event.
	type acc struct {
		member bool
		level  int64
		xp     float64
		last   time.Time
	}
	stats := map[int64]map[int64]*acc{} // user -> event -> …
	stat := func(uid, eid int64) *acc {
		if stats[uid] == nil {
			stats[uid] = map[int64]*acc{}
		}
		if stats[uid][eid] == nil {
			stats[uid][eid] = &acc{}
		}
		return stats[uid][eid]
	}
	for _, part := range chunks(ids) {
		err := pages(ctx, c, jwt, `query($ids: [Int!], $after: Int!, $limit: Int!) {
			rows: event_user(where: {userId: {_in: $ids}, id: {_gt: $after}}, order_by: {id: asc}, limit: $limit) { id userId eventId level } }`,
			map[string]any{"ids": part}, func(m *memberRow) json.Number { return m.ID },
			func(m *memberRow) {
				uid, ok1 := whole(m.UserID)
				eid, ok2 := whole(m.EventID)
				if !ok1 || !ok2 || users[uid] == nil {
					return
				}
				a := stat(uid, eid)
				a.member = true
				if m.Level != nil {
					if l, ok := whole(*m.Level); ok && l > a.level {
						a.level = l
					}
				}
			})
		if err != nil {
			return Data{}, nil, err
		}
		err = pages(ctx, c, jwt, `query($ids: [Int!], $after: Int!, $limit: Int!) {
			rows: transaction(where: {type: {_eq: "xp"}, userId: {_in: $ids}, id: {_gt: $after}}, order_by: {id: asc}, limit: $limit) { id userId eventId amount createdAt } }`,
			map[string]any{"ids": part}, func(x *xpRow) json.Number { return x.ID },
			func(x *xpRow) {
				uid, ok := whole(x.UserID)
				if !ok || users[uid] == nil || x.EventID == nil {
					return // XP outside any event counts nowhere
				}
				eid, ok := whole(*x.EventID)
				amount, ok2 := number(x.Amount)
				if !ok || !ok2 {
					return
				}
				a := stat(uid, eid)
				a.xp += amount
				if t := stamp(x.CreatedAt); t.After(a.last) {
					a.last = t
				}
			})
		if err != nil {
			return Data{}, nil, err
		}
	}

	// 6. What those events are.
	events := map[int64]*Event{}
	all := map[int64]bool{}
	for _, m := range stats {
		for eid := range m {
			all[eid] = true
		}
	}
	for _, part := range chunks(sortedIDs(all)) {
		var res struct {
			Event []eventRow `json:"event"`
		}
		if err := c.Query(ctx, jwt, `query($ids: [Int!]) { event(where: {id: {_in: $ids}}) { id path parentId startAt endAt } }`,
			map[string]any{"ids": part}, &res); err != nil {
			return Data{}, nil, err
		}
		for _, e := range res.Event {
			id, ok := whole(e.ID)
			if !ok {
				continue
			}
			ev := &Event{ID: id, Path: strings.TrimSpace(e.Path), Start: stamp(e.StartAt), End: stamp(e.EndAt)}
			if e.ParentID != nil {
				ev.ParentID, _ = whole(*e.ParentID)
			}
			events[id] = ev
		}
	}

	// Each user keeps the top-level events they are in and any event with a
	// level or XP; a piscine's checkpoints have neither.
	var out Data
	used := map[int64]bool{}
	for _, uid := range ids {
		u := users[uid]
		for _, eid := range sortedIDs(stats[uid]) {
			a := stats[uid][eid]
			xp := int64(math.Round(a.xp))
			ev := events[eid]
			if !(a.member && ev != nil && ev.ParentID == 0) && a.level == 0 && xp == 0 {
				continue
			}
			u.Events = append(u.Events, EventStat{EventID: eid, Level: a.level, XP: xp, LastXP: a.last})
			used[eid] = true
		}
		u.Current = current(u.Events, events)
		out.Users = append(out.Users, *u)
	}
	for _, eid := range sortedIDs(used) {
		if ev := events[eid]; ev != nil {
			out.Events = append(out.Events, *ev)
		}
	}
	return out, warnings, nil
}

// current picks the event a user is working in now: the one with the
// latest XP; with no XP anywhere, the top-level event that started last;
// 0 if there is none. Ties go to the higher event id.
func current(list []EventStat, events map[int64]*Event) int64 {
	var best int64
	var bestAt time.Time
	for _, s := range list {
		if s.XP != 0 && !s.LastXP.IsZero() && !s.LastXP.Before(bestAt) {
			best, bestAt = s.EventID, s.LastXP
		}
	}
	if best != 0 {
		return best
	}
	for _, s := range list {
		ev := events[s.EventID]
		if ev != nil && ev.ParentID == 0 && (best == 0 || !ev.Start.Before(bestAt)) {
			best, bestAt = s.EventID, ev.Start
		}
	}
	return best
}
