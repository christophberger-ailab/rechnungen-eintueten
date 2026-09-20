// Command rechnungen processes incoming invoices: it reads them from a
// mailbox, extracts the invoice data, files the original, hands it to DATEV
// and books it in sevDesk.
//
// Run it as a server with a daily schedule and a web UI ("serve"), or once
// from cron ("run").
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/backup"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/config"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/pipeline"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/web"
)

const usage = `rechnungen – Rechnungseingang automatisch verarbeiten

Aufrufe:
  rechnungen serve                 Server mit Web-UI und täglichem Lauf
  rechnungen run                   einmalige Verarbeitung (für cron)
  rechnungen config                gespeicherte Konfiguration anzeigen
  rechnungen config <key> <wert>   Konfigurationswert setzen
  rechnungen sender                bekannte Absender anzeigen
  rechnungen sender <mail> <skr04> [name]
                                   Absender anlegen oder ändern
  rechnungen sender rm <mail>      Absender entfernen
  rechnungen passwort              Passwort für das Web-UI setzen
  rechnungen restore <ziel.db>     Datenbank aus dem Litestream-Backup holen

Optionen:
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "rechnungen.db", "Pfad zur SQLite-Datenbank")
	addr := flag.String("addr", "", "Adresse des Web-UI (überschreibt die Konfiguration)")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("kein Unterbefehl angegeben")
	}
	// Restoring runs before the database is opened: opening it would create
	// the very file the restore is supposed to bring back.
	if args[0] == "restore" {
		return restoreCmd(*dbPath, args[1:])
	}

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "serve":
		return serve(db, *dbPath, *addr)
	case "run":
		return runOnce(db)
	case "config":
		return configCmd(db, args[1:])
	case "sender":
		return senderCmd(db, args[1:])
	case "passwort", "password":
		return passwordCmd(db)
	default:
		flag.Usage()
		return fmt.Errorf("unbekannter Unterbefehl %q", args[0])
	}
}

// serve runs the web UI and the daily schedule until interrupted.
func serve(db *store.DB, dbPath, addrOverride string) error {
	cfg, err := loadConfig(db)
	if err != nil {
		return err
	}
	addr := cfg.HTTPAddr
	if addrOverride != "" {
		addr = addrOverride
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.BackupEnabled {
		replicator, err := backup.Start(ctx, dbPath, backupConfig(cfg), nil)
		if err != nil {
			return fmt.Errorf("Backup starten: %w", err)
		}
		defer func() {
			// Give the last changes a moment to reach the bucket.
			stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := replicator.Stop(stop); err != nil {
				fmt.Fprintln(os.Stderr, "Backup beenden:", err)
			}
		}()
		fmt.Printf("Backup nach s3://%s/%s aktiv\n", cfg.BackupBucket, cfg.BackupPath)
	}

	runner := pipeline.NewRunner(db)
	go runner.Schedule(ctx, cfg.DailyAt)

	server := &http.Server{
		Addr:              addr,
		Handler:           web.New(db, runner).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()

	fmt.Printf("Web-UI auf http://%s, täglicher Lauf um %s\n", addr, cfg.DailyAt)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runOnce processes everything pending and reports the result, for cron.
func runOnce(db *store.DB) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	result, err := pipeline.NewRunner(db).RunOnce(ctx, "cli")
	if err != nil {
		return err
	}
	fmt.Printf("Lauf %d: %s (%s)\n", result.ID, result.Summary, result.Status)
	if result.Status != "ok" {
		return errors.New("die Verarbeitung meldet Fehler, Details siehe Web-UI")
	}
	return nil
}

// configCmd shows or changes configuration values.
func configCmd(db *store.DB, args []string) error {
	switch len(args) {
	case 0:
		cfg, err := loadConfig(db)
		if err != nil {
			return err
		}
		fields := cfg.Fields()
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		for _, f := range fields {
			value := f.Value
			if f.Secret && value != "" {
				value = "********"
			}
			fmt.Printf("%-24s %s\n", f.Key, value)
		}
		return nil
	case 2:
		if err := db.SetSetting(args[0], args[1]); err != nil {
			return err
		}
		fmt.Printf("%s gesetzt\n", args[0])
		return nil
	default:
		return errors.New("Aufruf: rechnungen config [<key> <wert>]")
	}
}

// senderCmd lists, adds and removes known senders.
func senderCmd(db *store.DB, args []string) error {
	if len(args) == 0 {
		senders, err := db.Senders()
		if err != nil {
			return err
		}
		for _, s := range senders {
			state := ""
			if !s.Active {
				state = " (inaktiv)"
			}
			fmt.Printf("%-40s %-8s %s%s\n", s.Email, s.SKR04, s.Name, state)
		}
		return nil
	}
	if args[0] == "rm" {
		if len(args) != 2 {
			return errors.New("Aufruf: rechnungen sender rm <mail>")
		}
		senders, err := db.Senders()
		if err != nil {
			return err
		}
		for _, s := range senders {
			if strings.EqualFold(s.Email, args[1]) {
				return db.DeleteSender(s.ID)
			}
		}
		return fmt.Errorf("Absender %q ist nicht hinterlegt", args[1])
	}
	if len(args) < 2 {
		return errors.New("Aufruf: rechnungen sender <mail> <skr04> [name]")
	}
	sender := store.Sender{Email: strings.ToLower(args[0]), SKR04: args[1], Active: true}
	if len(args) > 2 {
		sender.Name = strings.Join(args[2:], " ")
	}
	return db.SaveSender(sender)
}

func loadConfig(db *store.DB) (*config.Config, error) {
	values, err := db.Settings()
	if err != nil {
		return nil, err
	}
	return config.Load(values)
}

// passwordCmd sets the password for the web UI. There is one user, so there
// is one password; an empty one switches the login off, which only makes
// sense when the network already restricts access.
func passwordCmd(db *store.DB) error {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	first, err := readPassword("Neues Passwort (leer lassen, um den Schutz abzuschalten): ", interactive)
	if err != nil {
		return err
	}
	if first != "" {
		// A script piping the password in cannot repeat it, so only ask a human.
		if interactive {
			second, err := readPassword("Wiederholen: ", true)
			if err != nil {
				return err
			}
			if first != second {
				return errors.New("die beiden Passwörter stimmen nicht überein")
			}
		}
		if len(first) < 8 {
			return errors.New("das Passwort braucht mindestens 8 Zeichen")
		}
	}
	if err := db.SetPassword(first); err != nil {
		return err
	}
	if first == "" {
		fmt.Println("Passwortschutz abgeschaltet.")
	} else {
		fmt.Println("Passwort gesetzt, alle Anmeldungen wurden beendet.")
	}
	return nil
}

// readPassword asks for a password without echoing it. When stdin is not a
// terminal - a script piping it in - it reads one line instead.
func readPassword(prompt string, interactive bool) (string, error) {
	if interactive {
		fmt.Print(prompt)
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return string(raw), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// backupConfig turns the stored settings into the replication configuration.
func backupConfig(cfg *config.Config) backup.Config {
	return backup.Config{
		Endpoint:        cfg.BackupEndpoint,
		Region:          cfg.BackupRegion,
		Bucket:          cfg.BackupBucket,
		Path:            cfg.BackupPath,
		AccessKeyID:     cfg.BackupKeyID,
		SecretAccessKey: cfg.BackupSecret,
		ForcePathStyle:  cfg.BackupPathStyle,
		SyncInterval:    time.Duration(cfg.BackupInterval) * time.Second,
	}
}

// backupEnv are the environment variables that describe the bucket when the
// database itself is gone - which is exactly the situation a restore is for.
var backupEnv = map[string]func(*backup.Config, string){
	"RECHNUNGEN_BACKUP_ENDPOINT":          func(c *backup.Config, v string) { c.Endpoint = v },
	"RECHNUNGEN_BACKUP_REGION":            func(c *backup.Config, v string) { c.Region = v },
	"RECHNUNGEN_BACKUP_BUCKET":            func(c *backup.Config, v string) { c.Bucket = v },
	"RECHNUNGEN_BACKUP_PATH":              func(c *backup.Config, v string) { c.Path = v },
	"RECHNUNGEN_BACKUP_ACCESS_KEY_ID":     func(c *backup.Config, v string) { c.AccessKeyID = v },
	"RECHNUNGEN_BACKUP_SECRET_ACCESS_KEY": func(c *backup.Config, v string) { c.SecretAccessKey = v },
	"RECHNUNGEN_BACKUP_FORCE_PATH_STYLE":  func(c *backup.Config, v string) { c.ForcePathStyle = v == "true" },
}

// restoreCmd fetches the newest replicated copy of the database.
//
// The bucket settings live in the database being restored, so they are taken
// from the environment. When the old database is still readable - restoring a
// copy to check the backup works - its settings are used as the base and the
// environment only fills the gaps.
func restoreCmd(dbPath string, args []string) error {
	if len(args) != 1 {
		return errors.New("Aufruf: rechnungen restore <ziel.db>")
	}
	target := args[0]

	var cfg backup.Config
	if db, err := openExisting(dbPath); err == nil {
		defer db.Close()
		if stored, err := loadConfig(db); err == nil {
			cfg = backupConfig(stored)
		}
	}
	for name, apply := range backupEnv {
		if value, ok := os.LookupEnv(name); ok {
			apply(&cfg, value)
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%w - setze %s", err, strings.Join(envNames(), ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := backup.Restore(ctx, target, cfg, nil); err != nil {
		return err
	}
	fmt.Printf("Datenbank nach %s wiederhergestellt\n", target)
	return nil
}

// openExisting opens the database only if it is already there, so that a
// restore never creates the file it is about to replace.
func openExisting(path string) (*store.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return store.Open(path)
}

func envNames() []string {
	names := make([]string, 0, len(backupEnv))
	for name := range backupEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
