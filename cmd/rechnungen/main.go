// Command rechnungen processes incoming invoices: it reads them from a
// mailbox, extracts the invoice data, files the original, hands it to DATEV
// and books it in sevDesk.
//
// Run it as a server with a daily schedule and a web UI ("serve"), or once
// from cron ("run").
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

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

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return errors.New("kein Unterbefehl angegeben")
	}
	switch args[0] {
	case "serve":
		return serve(db, *addr)
	case "run":
		return runOnce(db)
	case "config":
		return configCmd(db, args[1:])
	case "sender":
		return senderCmd(db, args[1:])
	default:
		flag.Usage()
		return fmt.Errorf("unbekannter Unterbefehl %q", args[0])
	}
}

// serve runs the web UI and the daily schedule until interrupted.
func serve(db *store.DB, addrOverride string) error {
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
