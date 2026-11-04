package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "a.conf")
	os.WriteFile(p, []byte(body), 0o600)
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, `
# comment
bot_username = @SchoolBot
host_key_file = /etc/attendance/host_key
timezone = Asia/Almaty
lang = en
qr_rotate = 45s
cleanup_at = 03:15
hours_rounding = 5m
admin_networks = 192.168.1.0/24, 10.1.0.0/16
admin_from_lab_pcs = true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.BotUsername != "SchoolBot" || c.Lang != "en" || c.QRRotate != 45*time.Second ||
		c.CleanupAt != (Clock{3, 15}) || c.HoursRounding != 5*time.Minute || len(c.AdminNetworks) != 2 ||
		!c.AdminFromLabPCs || c.Timezone.String() != "Asia/Almaty" {
		t.Errorf("%+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	base := "bot_username = b\nhost_key_file = /k\n"
	cases := map[string]string{
		"unknown key":        base + "qr_rotat = 30s\n",
		"no bot username":    "host_key_file = /k\n",
		"ttl over 2 minutes": base + "qr_token_ttl = 3m\n",
		"ttl below rotation": base + "qr_rotate = 40s\nqr_token_ttl = 30s\n",
		"bad clock":          base + "cleanup_at = 25:00\n",
		"bad language":       base + "lang = de\n",
		"missing equals":     base + "listen 0.0.0.0:80\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "no such file") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
