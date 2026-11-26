// Package platform reads students' data from an 01Edu learning platform:
// logins, names, XP and audit ratio. It uses the platform's read-only API,
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
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client talks to one platform.
type Client struct {
	Base  string // https://host, no trailing slash
	Token string // the admin's access token
	HTTP  *http.Client
}

// User is one platform user as the sync stores it. Personal documents in
// the platform's attrs (ID numbers, phone, address) are not read.
type User struct {
	ID                  int64
	Login               string
	FirstName, LastName string
	Email               string
	Events              []int64 // the configured events the user is registered in
	XP                  int64   // XP earned in those events
	AuditRatio          float64
	HasAuditRatio       bool
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

// Query runs one GraphQL query and decodes its data into out.
func (c *Client) Query(ctx context.Context, jwt, query string, out any) error {
	b, _ := json.Marshal(map[string]string{"query": query})
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

// intList writes ids for a GraphQL query. The ids are numbers we parsed, so
// nothing from outside goes into the query text.
func intList(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return "[" + strings.Join(s, ", ") + "]"
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

func number(n json.Number) (float64, bool) {
	f, err := n.Float64()
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Fetch reads the users registered in the given events, with their XP in
// those events and their audit ratio.
func (c *Client) Fetch(ctx context.Context, events []int64) ([]User, []string, error) {
	if len(events) == 0 {
		return nil, nil, errors.New("no events are configured")
	}
	jwt, err := c.JWT(ctx)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string

	// 1. Who is registered (the query the school's existing script uses).
	var ev struct {
		Event []struct {
			ID            json.Number `json:"id"`
			Registrations []struct {
				Users []struct {
					ID    json.Number    `json:"id"`
					Login string         `json:"login"`
					Attrs map[string]any `json:"attrs"`
				} `json:"users"`
			} `json:"registrations"`
		} `json:"event"`
	}
	q := fmt.Sprintf(`{ event(where: {id: {_in: %s}}) { id registrations { users { id login attrs } } } }`, intList(events))
	if err := c.Query(ctx, jwt, q, &ev); err != nil {
		return nil, nil, err
	}
	users := map[int64]*User{}
	found := map[int64]bool{}
	for _, e := range ev.Event {
		eid, _ := e.ID.Int64()
		found[eid] = true
		for _, reg := range e.Registrations {
			for _, u := range reg.Users {
				id, err := u.ID.Int64()
				login := strings.TrimSpace(u.Login)
				if err != nil || login == "" {
					continue
				}
				x := users[id]
				if x == nil {
					x = &User{ID: id, Login: login,
						FirstName: str(u.Attrs, "firstNameCyr", "firstName"),
						LastName:  str(u.Attrs, "lastNameCyr", "lastName"),
						Email:     str(u.Attrs, "email")}
					users[id] = x
				}
				if !containsID(x.Events, eid) {
					x.Events = append(x.Events, eid)
				}
			}
		}
	}
	for _, e := range events {
		if !found[e] {
			warnings = append(warnings, fmt.Sprintf("event %d was not found on the platform", e))
		}
	}

	// 2. XP in those events, page by page in id order. A page may be cut
	// short by the server, so only an empty page ends the loop.
	var last int64
	for page := 0; ; page++ {
		if page >= 10000 {
			return nil, nil, errors.New("too many XP transactions")
		}
		var tx struct {
			Transaction []struct {
				ID     json.Number `json:"id"`
				UserID json.Number `json:"userId"`
				Amount json.Number `json:"amount"`
			} `json:"transaction"`
		}
		q := fmt.Sprintf(`{ transaction(where: {type: {_eq: "xp"}, eventId: {_in: %s}, id: {_gt: %d}}, order_by: {id: asc}, limit: 1000) { id userId amount } }`,
			intList(events), last)
		if err := c.Query(ctx, jwt, q, &tx); err != nil {
			return nil, nil, err
		}
		if len(tx.Transaction) == 0 {
			break
		}
		for _, t := range tx.Transaction {
			id, err := t.ID.Int64()
			if err != nil || id <= last {
				return nil, nil, errors.New("XP transactions came out of order")
			}
			last = id
			uid, _ := t.UserID.Int64()
			if amount, ok := number(t.Amount); ok && users[uid] != nil {
				users[uid].XP += int64(math.Round(amount))
			}
		}
	}

	// 3. Audit ratio. Not essential: a failure is a warning.
	ids := make([]int64, 0, len(users))
	for id := range users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 0; i < len(ids); i += 500 {
		chunk := ids[i:min(i+500, len(ids))]
		var ur struct {
			User []struct {
				ID         json.Number  `json:"id"`
				AuditRatio *json.Number `json:"auditRatio"`
			} `json:"user"`
		}
		if err := c.Query(ctx, jwt, fmt.Sprintf(`{ user(where: {id: {_in: %s}}) { id auditRatio } }`, intList(chunk)), &ur); err != nil {
			warnings = append(warnings, "audit ratio not read: "+err.Error())
			break
		}
		for _, u := range ur.User {
			id, _ := u.ID.Int64()
			if x := users[id]; x != nil && u.AuditRatio != nil {
				if f, ok := number(*u.AuditRatio); ok {
					x.AuditRatio, x.HasAuditRatio = f, true
				}
			}
		}
	}

	out := make([]User, 0, len(users))
	for _, id := range ids {
		u := users[id]
		sort.Slice(u.Events, func(i, j int) bool { return u.Events[i] < u.Events[j] })
		out = append(out, *u)
	}
	return out, warnings, nil
}

func containsID(list []int64, id int64) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}
