package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// API is a minimal Telegram Bot API client: long polling and sending text.
// Reference: https://core.telegram.org/bots/api
type API struct {
	base   string // https://api.telegram.org/bot<token>
	client *http.Client
}

func NewAPI(botToken string) *API {
	return &API{
		base:   "https://api.telegram.org/bot" + botToken,
		client: &http.Client{Timeout: 75 * time.Second}, // longer than the poll timeout
	}
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private, group, supergroup, channel
}

type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// APIError is an error reported by Telegram.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (a *API) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		// The URL contains the bot token; keep only the cause so it never
		// reaches the logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: bad response (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return &APIError{Code: r.ErrorCode, Description: r.Description, RetryAfter: r.Parameters.RetryAfter}
	}
	if result != nil {
		return json.Unmarshal(r.Result, result)
	}
	return nil
}

// GetUpdates long-polls for new messages.
func (a *API) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	var ups []Update
	err := a.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSec,
		"allowed_updates": []string{"message"},
	}, &ups)
	return ups, err
}

func (a *API) SendMessage(ctx context.Context, chatID int64, text string) error {
	return a.call(ctx, "sendMessage", map[string]any{
		"chat_id":              chatID,
		"text":                 text,
		"link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}

func (a *API) GetMe(ctx context.Context) (User, error) {
	var u User
	err := a.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}

type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (a *API) SetMyCommands(ctx context.Context, cmds []Command) error {
	return a.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}
