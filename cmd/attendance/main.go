// Command attendance is the QR presence check-in server and its admin CLI.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"attendance/internal/app"
	"attendance/internal/config"
	"attendance/internal/core"
	"attendance/internal/store"
	"attendance/internal/token"
)

var version = "dev"

const usage = `attendance — QR presence check-in server

Usage:
  attendance serve [-no-bot]                 run the server, bot and jobs
  attendance check-config                    validate the config and secrets
  attendance user add <name> -role admin|teacher
  attendance user passwd <name>
  attendance user disable|enable <name>
  attendance user list
  attendance students import <file.csv>
  attendance students list
  attendance code <student-id>               issue a one-time enrollment link
  attendance unlink <student-id> -reason "…" unlink a Telegram account
  attendance replay                          rebuild visits from raw events (stop the service first)
  attendance backup <file>                   write a consistent copy of the database
  attendance gen-key                         print a random host key
  attendance version

Every command accepts -config <file> (default /etc/attendance/attendance.conf
or $ATTENDANCE_CONFIG).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if (cmd == "user" || cmd == "students") && len(args) > 0 {
		cmd, args = cmd+" "+args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "check-config":
		err = checkConfig(args)
	case "user add":
		err = userAdd(args)
	case "user passwd":
		err = userPasswd(args)
	case "user disable", "user enable":
		err = userDisable(args, cmd == "user disable")
	case "user list":
		err = userList(args)
	case "students import":
		err = studentsImport(args)
	case "students list":
		err = studentsList(args)
	case "code":
		err = code(args)
	case "unlink":
		err = unlink(args)
	case "replay":
		err = replay(args)
	case "backup":
		err = backup(args)
	case "gen-key":
		fmt.Println(hex.EncodeToString(token.NewSecret(24)))
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// flags parses the common -config flag plus any extras, allowing flags after
// positional arguments ("user add bob -role admin").
func flags(name string, args []string, extra func(*flag.FlagSet)) (*flag.FlagSet, *string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	def := os.Getenv("ATTENDANCE_CONFIG")
	if def == "" {
		def = "/etc/attendance/attendance.conf"
	}
	cfgPath := fs.String("config", def, "config file")
	if extra != nil {
		extra(fs)
	}
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	fs.Parse(pos) // leave positionals in fs.Args()
	return fs, cfgPath, nil
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func openDB(path string) (*config.Config, *store.DB, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(cfg.DBPath)
	return cfg, db, err
}

func serve(args []string) error {
	var noBot bool
	_, cfgPath, err := flags("serve", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&noBot, "no-bot", false, "run without the Telegram bot")
	})
	if err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, app.Options{NoBot: noBot}, log)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func checkConfig(args []string) error {
	_, cfgPath, err := flags("check-config", args, nil)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := config.ReadSecret(cfg.HostKeyFile); err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	if cfg.BotTokenFile != "" {
		if _, err := config.ReadSecret(cfg.BotTokenFile); err != nil {
			return fmt.Errorf("bot token: %w", err)
		}
	}
	f, err := os.Open(cfg.HostsFile)
	if err != nil {
		return fmt.Errorf("hosts: %w", err)
	}
	defer f.Close()
	hosts, err := core.ParseHosts(f)
	if err != nil {
		return fmt.Errorf("hosts: %w", err)
	}
	fmt.Printf("config OK: %d hosts, lang=%s, tz=%s, bot=@%s\n", len(hosts), cfg.Lang, cfg.Timezone, cfg.BotUsername)
	return nil
}

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	restore := noEcho(int(os.Stdin.Fd()))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	restore()
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func newPassword() (string, error) {
	pw, err := readPassword("Password: ")
	if err != nil {
		return "", err
	}
	if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice != 0 {
		again, err := readPassword("Repeat: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", fmt.Errorf("passwords do not match")
		}
	}
	return pw, nil
}

func userAdd(args []string) error {
	var role string
	fs, cfgPath, err := flags("user add", args, func(fs *flag.FlagSet) {
		fs.StringVar(&role, "role", "teacher", "admin or teacher")
	})
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: attendance user add <name> -role admin|teacher")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	pw, err := newPassword()
	if err != nil {
		return err
	}
	if err := core.CreateStaff(context.Background(), db, time.Now(), fs.Arg(0), pw, role); err != nil {
		return err
	}
	fmt.Printf("created %s (%s)\n", fs.Arg(0), role)
	return nil
}

func userPasswd(args []string) error {
	fs, cfgPath, err := flags("user passwd", args, nil)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: attendance user passwd <name>")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	pw, err := newPassword()
	if err != nil {
		return err
	}
	return core.SetStaffPassword(context.Background(), db, time.Now(), fs.Arg(0), pw)
}

func userDisable(args []string, disable bool) error {
	fs, cfgPath, err := flags("user disable", args, nil)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: attendance user disable|enable <name>")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return core.SetStaffDisabled(context.Background(), db, time.Now(), fs.Arg(0), disable)
}

func userList(args []string) error {
	_, cfgPath, err := flags("user list", args, nil)
	if err != nil {
		return err
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	staff, err := core.ListStaff(context.Background(), db)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tROLE\tDISABLED")
	for _, s := range staff {
		fmt.Fprintf(tw, "%s\t%s\t%v\n", s.Username, s.Role, s.Disabled)
	}
	return tw.Flush()
}

func studentsImport(args []string) error {
	fs, cfgPath, err := flags("students import", args, nil)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: attendance students import <file.csv>")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	res, err := core.ImportStudents(context.Background(), db, time.Now(), f, "cli")
	if err != nil {
		return err
	}
	fmt.Printf("added %d, updated %d, unchanged %d\n", res.Added, res.Updated, res.Unchanged)
	for _, w := range res.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}

func studentsList(args []string) error {
	_, cfgPath, err := flags("students list", args, nil)
	if err != nil {
		return err
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT s.id, s.name, s.grp, s.archived, l.tg_user_id IS NOT NULL FROM students s
		LEFT JOIN telegram_links l ON l.student_id = s.id ORDER BY s.grp, s.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tGROUP\tLINKED\tARCHIVED")
	for rows.Next() {
		var id int64
		var name, grp string
		var archived, linked bool
		if err := rows.Scan(&id, &name, &grp, &archived, &linked); err != nil {
			return err
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%v\t%v\n", id, name, grp, linked, archived)
	}
	return tw.Flush()
}

func studentArg(fs *flag.FlagSet, use string) (int64, error) {
	if fs.NArg() != 1 {
		return 0, fmt.Errorf("usage: %s", use)
	}
	return strconv.ParseInt(fs.Arg(0), 10, 64)
}

func code(args []string) error {
	fs, cfgPath, err := flags("code", args, nil)
	if err != nil {
		return err
	}
	id, err := studentArg(fs, "attendance code <student-id>")
	if err != nil {
		return err
	}
	cfg, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := core.GenerateCode(context.Background(), db, time.Now(), id, cfg.EnrollCodeTTL, "cli")
	if err != nil {
		return err
	}
	fmt.Println(core.StartLink(cfg.BotUsername, c))
	return nil
}

func unlink(args []string) error {
	var reason string
	fs, cfgPath, err := flags("unlink", args, func(fs *flag.FlagSet) {
		fs.StringVar(&reason, "reason", "", "why (required, goes to the audit log)")
	})
	if err != nil {
		return err
	}
	id, err := studentArg(fs, `attendance unlink <student-id> -reason "…"`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("-reason is required")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return core.Unlink(context.Background(), db, time.Now(), id, "cli", reason)
}

func replay(args []string) error {
	_, cfgPath, err := flags("replay", args, nil)
	if err != nil {
		return err
	}
	cfg, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	e := core.NewEngine(db, cfg.HeartbeatTimeout, cfg.CheckoutSlack)
	n, err := e.Replay(context.Background(), time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d events\n", n)
	return nil
}

func backup(args []string) error {
	fs, cfgPath, err := flags("backup", args, nil)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: attendance backup <file>")
	}
	_, db, err := openDB(*cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Backup(context.Background(), fs.Arg(0))
}
