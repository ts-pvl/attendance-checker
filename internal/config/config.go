// Package config loads the server configuration from a simple "key = value"
// file. Lines starting with '#' are comments. Unknown keys are an error so a
// typo cannot silently fall back to a default.
package config

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen   string
	DBPath   string
	Timezone *time.Location
	Lang     string // "ru" or "en"

	BotUsername  string // without '@'
	BotTokenFile string // root-only file; empty disables the bot
	HostKeyFile  string
	HostsFile    string
	// TokenSecretFile holds the HMAC key for QR tokens; created on first start.
	TokenSecretFile string

	AdminNetworks []netip.Prefix
	// AdminFromLabPCs allows the admin UI from addresses in the hosts file.
	// Off by default so students cannot even reach the staff login.
	AdminFromLabPCs bool

	QRRotate         time.Duration // how often the QR on screen changes
	QRTokenTTL       time.Duration // how long a shown token is accepted
	HeartbeatTimeout time.Duration // no event for this long closes a session
	CheckoutSlack    time.Duration // /checkout uses "now" if the PC was active within this window
	EnrollCodeTTL    time.Duration

	CleanupAt              Clock // nightly cleanup, local time
	BackupAt               Clock
	BackupDir              string
	BackupKeep             int // days
	HeartbeatRetentionDays int // 0 keeps heartbeat events forever

	StaffSessionIdle time.Duration
	StaffSessionMax  time.Duration

	// Hours policy (section 10.2 item 5). Zero disables each rule.
	HoursRounding time.Duration // daily totals rounded to the nearest multiple
	HoursMinVisit time.Duration // visits shorter than this count as zero
	HoursDailyCap time.Duration

	// Anomaly thresholds.
	AnomalyLatency time.Duration // scan later than this after the token was issued
	AnomalyMultiPC time.Duration // scans on two PCs within this window while the first is still live
	AlertAfter     time.Duration // /healthz turns red after this long without heartbeats or bot polls
}

// Clock is a time of day.
type Clock struct{ Hour, Minute int }

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", c.Hour, c.Minute) }

// On returns the instant of this clock time on the local day containing t.
func (c Clock) On(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, c.Hour, c.Minute, 0, 0, t.Location())
}

func Default() *Config {
	return &Config{
		Listen:                 "0.0.0.0:8080",
		DBPath:                 "/var/lib/attendance/attendance.db",
		Timezone:               time.Local,
		Lang:                   "ru",
		BotTokenFile:           credentialPath("bot_token"),
		HostKeyFile:            credentialPath("host_key"),
		HostsFile:              "/etc/attendance/hosts.csv",
		TokenSecretFile:        "/var/lib/attendance/token_secret",
		AdminNetworks:          mustPrefixes("127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"),
		QRRotate:               30 * time.Second,
		QRTokenTTL:             60 * time.Second,
		HeartbeatTimeout:       3 * time.Minute,
		CheckoutSlack:          2 * time.Minute,
		EnrollCodeTTL:          14 * 24 * time.Hour,
		CleanupAt:              Clock{4, 0},
		BackupAt:               Clock{3, 30},
		BackupDir:              "/var/backups/attendance",
		BackupKeep:             14,
		HeartbeatRetentionDays: 0,
		StaffSessionIdle:       2 * time.Hour,
		StaffSessionMax:        12 * time.Hour,
		AnomalyLatency:         45 * time.Second,
		AnomalyMultiPC:         15 * time.Minute,
		AlertAfter:             15 * time.Minute,
	}
}

// credentialPath points at a systemd LoadCredential= file when running under
// systemd, so secrets can stay root-only on disk.
func credentialPath(name string) string {
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(s))
	for _, p := range s {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}

func Load(path string) (*Config, error) {
	c := Default()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, line)
		}
		if err := c.set(strings.TrimSpace(k), strings.TrimSpace(v)); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

func (c *Config) set(k, v string) error {
	var err error
	dur := func(dst *time.Duration) { *dst, err = parseDuration(v) }
	switch k {
	case "listen":
		c.Listen = v
	case "db":
		c.DBPath = v
	case "timezone":
		c.Timezone, err = time.LoadLocation(v)
	case "lang":
		c.Lang = v
	case "bot_username":
		c.BotUsername = strings.TrimPrefix(v, "@")
	case "bot_token_file":
		c.BotTokenFile = v
	case "host_key_file":
		c.HostKeyFile = v
	case "hosts_file":
		c.HostsFile = v
	case "token_secret_file":
		c.TokenSecretFile = v
	case "admin_networks":
		c.AdminNetworks = nil
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			pfx, perr := netip.ParsePrefix(p)
			if perr != nil {
				return perr
			}
			c.AdminNetworks = append(c.AdminNetworks, pfx)
		}
	case "admin_from_lab_pcs":
		c.AdminFromLabPCs, err = strconv.ParseBool(v)
	case "qr_rotate":
		dur(&c.QRRotate)
	case "qr_token_ttl":
		dur(&c.QRTokenTTL)
	case "heartbeat_timeout":
		dur(&c.HeartbeatTimeout)
	case "checkout_slack":
		dur(&c.CheckoutSlack)
	case "enroll_code_ttl":
		dur(&c.EnrollCodeTTL)
	case "cleanup_at":
		c.CleanupAt, err = parseClock(v)
	case "backup_at":
		c.BackupAt, err = parseClock(v)
	case "backup_dir":
		c.BackupDir = v
	case "backup_keep_days":
		c.BackupKeep, err = strconv.Atoi(v)
	case "heartbeat_retention_days":
		c.HeartbeatRetentionDays, err = strconv.Atoi(v)
	case "staff_session_idle":
		dur(&c.StaffSessionIdle)
	case "staff_session_max":
		dur(&c.StaffSessionMax)
	case "hours_rounding":
		dur(&c.HoursRounding)
	case "hours_min_visit":
		dur(&c.HoursMinVisit)
	case "hours_daily_cap":
		dur(&c.HoursDailyCap)
	case "anomaly_latency":
		dur(&c.AnomalyLatency)
	case "anomaly_multi_pc":
		dur(&c.AnomalyMultiPC)
	case "alert_after":
		dur(&c.AlertAfter)
	default:
		return fmt.Errorf("unknown key %q", k)
	}
	return err
}

// parseDuration accepts Go durations plus a bare "0".
func parseDuration(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	return time.ParseDuration(v)
}

func parseClock(v string) (Clock, error) {
	h, m, ok := strings.Cut(v, ":")
	if !ok {
		return Clock{}, fmt.Errorf("expected HH:MM, got %q", v)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return Clock{}, fmt.Errorf("expected HH:MM, got %q", v)
	}
	return Clock{hh, mm}, nil
}

func (c *Config) Validate() error {
	switch {
	case c.Lang != "ru" && c.Lang != "en":
		return fmt.Errorf("lang must be ru or en")
	case c.BotUsername == "":
		return fmt.Errorf("bot_username is required (it goes into every QR link)")
	case c.QRRotate < 5*time.Second:
		return fmt.Errorf("qr_rotate must be at least 5s")
	case c.QRTokenTTL < c.QRRotate:
		return fmt.Errorf("qr_token_ttl must be at least qr_rotate")
	case c.QRTokenTTL > 2*time.Minute:
		// Acceptance criterion 7.3: a photo older than two minutes must always fail.
		return fmt.Errorf("qr_token_ttl must not exceed 2m")
	case c.HeartbeatTimeout < time.Minute:
		return fmt.Errorf("heartbeat_timeout must be at least 1m")
	case len(c.AdminNetworks) == 0:
		return fmt.Errorf("admin_networks must not be empty")
	}
	return nil
}

// ReadHostKey reads the PC host key. Only the server needs it, so Validate
// does not require host_key_file: the other CLI commands (user, students,
// code, backup, …) run outside systemd, where the credential default is empty.
func (c *Config) ReadHostKey() (string, error) {
	if c.HostKeyFile == "" {
		return "", fmt.Errorf("host_key_file is not set (under systemd it comes from LoadCredential=host_key)")
	}
	return ReadSecret(c.HostKeyFile)
}

// ReadSecret reads a secret file and trims surrounding whitespace.
func ReadSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}
