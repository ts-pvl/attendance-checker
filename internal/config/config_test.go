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
school_name = My School
anomaly_long_visit = 5h
anomaly_late_from = 22:30
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
		!c.AdminFromLabPCs || c.Timezone.String() != "Asia/Almaty" || c.SchoolName != "My School" ||
		c.AnomalyLongVisit != 5*time.Hour || c.AnomalyLowActivityGap != 10*time.Minute {
		t.Errorf("%+v", c)
	}
}

// The CLI (user add, students import, …) runs outside systemd, where there is
// no host key credential; only serve and check-config need the key.
func TestHostKeyOnlyNeededByServer(t *testing.T) {
	c, err := Load(write(t, "bot_username = b\n"))
	if err != nil {
		t.Fatalf("config without host_key_file rejected: %v", err)
	}
	if _, err := c.ReadHostKey(); err == nil || !strings.Contains(err.Error(), "host_key_file is not set") {
		t.Errorf("ReadHostKey without host_key_file: %v", err)
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
		"platform over http": base + "platform_url = http://01.example.org\n",
		"platform with path": base + "platform_url = https://01.example.org/api\n",
		"platform no scheme": base + "platform_url = 01.example.org\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "no such file") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The platform address is fixed in the configuration, so staff never type
// it: the default is the school's platform; platform_url replaces it, and an
// empty one turns the integration off.
func TestPlatformURL(t *testing.T) {
	c, err := Load(write(t, "bot_username = b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.PlatformURL != "https://01.tomorrow-school.ai" {
		t.Errorf("default platform_url = %q", c.PlatformURL)
	}
	for in, want := range map[string]string{
		"https://01.Example.org":  "https://01.example.org",
		"https://01.example.org/": "https://01.example.org",
		"":                        "",
	} {
		c, err := Load(write(t, "bot_username = b\nplatform_url = "+in+"\n"))
		if err != nil {
			t.Errorf("platform_url = %q: %v", in, err)
		} else if c.PlatformURL != want {
			t.Errorf("platform_url = %q gave %q, want %q", in, c.PlatformURL, want)
		}
	}
}
