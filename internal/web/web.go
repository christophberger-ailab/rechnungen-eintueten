// Package web serves the configuration and dashboard UI. The pages are plain
// Go templates; htmx takes care of the few places that update without a full
// page load.
package web

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/christophberger-ailab/rechnungen-eintueten/internal/config"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/extract"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/pipeline"
	"github.com/christophberger-ailab/rechnungen-eintueten/internal/store"
)

//go:embed templates/*.html static/*
var files embed.FS

// Server answers the web UI requests.
type Server struct {
	db     *store.DB
	runner *pipeline.Runner
	pages  map[string]*template.Template
}

// New builds the HTTP handler for the web UI.
func New(db *store.DB, runner *pipeline.Runner) *Server {
	s := &Server{db: db, runner: runner, pages: map[string]*template.Template{}}
	for _, page := range []string{"dashboard", "settings", "invoice"} {
		s.pages[page] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(files, "templates/layout.html", "templates/partials.html", "templates/"+page+".html"))
	}
	return s
}

// Handler returns the routes of the web UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(files))

	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /partials/status", s.statusPartial)
	mux.HandleFunc("GET /partials/invoices", s.invoicesPartial)
	mux.HandleFunc("POST /run", s.startRun)

	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /senders", s.saveSender)
	mux.HandleFunc("POST /senders/{id}/delete", s.deleteSender)

	mux.HandleFunc("GET /invoices/{id}", s.invoice)
	mux.HandleFunc("POST /invoices/{id}", s.saveInvoice)
	return mux
}

// view is the data every page gets.
type view struct {
	Title    string
	Active   string
	Counts   map[string]int
	Running  bool
	LastRun  *store.Run
	Invoices []*store.Invoice
	Invoice  *store.Invoice
	Events   []*store.Event
	Groups   []configGroup
	Senders  []store.Sender
	Message  string
	Error    string
}

type configGroup struct {
	Name   string
	Fields []config.Field
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	v, err := s.dashboardView()
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Title, v.Active = "Übersicht", "dashboard"
	s.render(w, "dashboard", v)
}

func (s *Server) dashboardView() (*view, error) {
	counts, err := s.db.StatusCounts()
	if err != nil {
		return nil, err
	}
	invoices, err := s.db.RecentInvoices(100)
	if err != nil {
		return nil, err
	}
	last, err := s.db.LastRun()
	if err != nil {
		return nil, err
	}
	events, err := s.db.Events(0, 30)
	if err != nil {
		return nil, err
	}
	return &view{Counts: counts, Invoices: invoices, LastRun: last, Events: events,
		Running: s.runner.Running()}, nil
}

func (s *Server) statusPartial(w http.ResponseWriter, r *http.Request) {
	v, err := s.dashboardView()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderPartial(w, "status", v)
}

func (s *Server) invoicesPartial(w http.ResponseWriter, r *http.Request) {
	v, err := s.dashboardView()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderPartial(w, "invoices", v)
}

// startRun is the "Verarbeitung starten" button. The run itself happens in the
// background; the button swaps in the status panel, which then polls.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	// The run outlives this request, so it must not use the request context.
	if !s.runner.Start(context.Background(), "web") {
		s.db.Log(0, 0, "run", "warn", "Start abgelehnt: es läuft bereits eine Verarbeitung")
	}
	v, err := s.dashboardView()
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Running = true
	s.renderPartial(w, "status", v)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, "", "")
}

func (s *Server) renderSettings(w http.ResponseWriter, message, errMsg string) {
	values, err := s.db.Settings()
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg, err := config.Load(values)
	if err != nil {
		errMsg = err.Error()
	}
	senders, err := s.db.Senders()
	if err != nil {
		s.fail(w, err)
		return
	}
	v := &view{Title: "Einstellungen", Active: "settings", Senders: senders,
		Message: message, Error: errMsg}
	for _, g := range cfg.Groups() {
		group := configGroup{Name: g.Name}
		for _, f := range g.Fields {
			if f.Secret && f.Value != "" {
				f.Value = "" // never send a stored secret back to the browser
			}
			group.Fields = append(group.Fields, f)
		}
		v.Groups = append(v.Groups, group)
	}
	s.render(w, "settings", v)
}

// saveSettings stores the submitted configuration. An empty secret means
// "keep what is stored", so that the blanked-out password fields do not wipe
// the credentials.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, err)
		return
	}
	cfg := &config.Config{}
	values := map[string]string{}
	for _, f := range cfg.Fields() {
		submitted := strings.TrimSpace(r.PostFormValue(f.Key))
		if f.Kind == "checkbox" {
			submitted = strconv.FormatBool(r.PostFormValue(f.Key) != "")
		}
		if f.Secret && submitted == "" {
			continue
		}
		values[f.Key] = submitted
	}
	if err := s.db.SetSettings(values); err != nil {
		s.renderSettings(w, "", err.Error())
		return
	}
	s.renderSettings(w, "Einstellungen gespeichert.", "")
}

func (s *Server) saveSender(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	if email == "" {
		s.renderSettings(w, "", "Absender braucht eine E-Mail-Adresse.")
		return
	}
	sender := store.Sender{
		Email:  email,
		Name:   strings.TrimSpace(r.PostFormValue("name")),
		SKR04:  strings.TrimSpace(r.PostFormValue("skr04")),
		Active: r.PostFormValue("active") != "",
	}
	if err := s.db.SaveSender(sender); err != nil {
		s.renderSettings(w, "", err.Error())
		return
	}
	s.renderSettings(w, "Absender gespeichert.", "")
}

func (s *Server) deleteSender(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "unbekannter Absender", http.StatusBadRequest)
		return
	}
	if err := s.db.DeleteSender(id); err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettings(w, "Absender gelöscht.", "")
}

func (s *Server) invoice(w http.ResponseWriter, r *http.Request) {
	inv, err := s.loadInvoice(r)
	if err != nil {
		http.Error(w, "Rechnung nicht gefunden", http.StatusNotFound)
		return
	}
	events, err := s.db.Events(0, 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	var own []*store.Event
	for _, e := range events {
		if e.InvoiceID == inv.ID {
			own = append(own, e)
		}
	}
	s.render(w, "invoice", &view{Title: "Rechnung " + inv.Number, Active: "dashboard",
		Invoice: inv, Events: own})
}

// saveInvoice takes the corrections a human made to an invoice whose
// extraction came up short, and puts it back into the pipeline.
func (s *Server) saveInvoice(w http.ResponseWriter, r *http.Request) {
	inv, err := s.loadInvoice(r)
	if err != nil {
		http.Error(w, "Rechnung nicht gefunden", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, err)
		return
	}
	inv.SenderName = strings.TrimSpace(r.PostFormValue("sender_name"))
	inv.SenderAddress = strings.TrimSpace(r.PostFormValue("sender_address"))
	inv.SenderVATID = strings.TrimSpace(r.PostFormValue("sender_vat_id"))
	inv.Number = strings.TrimSpace(r.PostFormValue("number"))
	inv.Date = extract.NormalizeDate(r.PostFormValue("date"))
	inv.Currency = strings.ToUpper(strings.TrimSpace(r.PostFormValue("currency")))
	inv.SKR04 = strings.TrimSpace(r.PostFormValue("skr04"))
	inv.TotalCents, _ = extract.ParseAmount(r.PostFormValue("total"))
	inv.VATCents, _ = extract.ParseAmount(r.PostFormValue("vat"))
	if rate, err := strconv.ParseFloat(strings.Replace(r.PostFormValue("vat_rate"), ",", ".", 1), 64); err == nil {
		inv.VATRate = rate
	}
	if inv.Complete() {
		inv.Status, inv.LastError = store.StatusExtracted, ""
	}
	if err := s.db.SaveInvoice(inv); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/invoices/"+strconv.FormatInt(inv.ID, 10), http.StatusSeeOther)
}

func (s *Server) loadInvoice(r *http.Request) (*store.Invoice, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, err
	}
	return s.db.Invoice(id)
}

func (s *Server) render(w http.ResponseWriter, page string, v *view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout.html", v); err != nil {
		s.db.Log(0, 0, "web", "error", "Seite %s: %v", page, err)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, v *view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages["dashboard"].ExecuteTemplate(w, name, v); err != nil {
		s.db.Log(0, 0, "web", "error", "Fragment %s: %v", name, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.db.Log(0, 0, "web", "error", "%v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// statusLabels give each processing state a German name and a colour class.
var statusLabels = map[string][2]string{
	store.StatusNew:         {"Neu", "neutral"},
	store.StatusNeedsReview: {"Prüfen", "warn"},
	store.StatusExtracted:   {"In Arbeit", "info"},
	store.StatusDone:        {"Fertig", "ok"},
	store.StatusError:       {"Fehler", "bad"},
}

var funcs = template.FuncMap{
	"money": store.FormatCents,
	"label": func(status string) string {
		if l, ok := statusLabels[status]; ok {
			return l[0]
		}
		return status
	},
	"class": func(status string) string {
		if l, ok := statusLabels[status]; ok {
			return l[1]
		}
		return "neutral"
	},
	"datetime": func(t time.Time) string {
		if t.IsZero() {
			return "–"
		}
		return t.Local().Format("02.01.2006 15:04")
	},
	"orDash": func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "–"
		}
		return s
	},
	"rate": func(r float64) string {
		if r == 0 {
			return ""
		}
		return strconv.FormatFloat(r, 'f', -1, 64)
	},
	"statuses": func() []string {
		return []string{store.StatusNew, store.StatusNeedsReview, store.StatusExtracted,
			store.StatusDone, store.StatusError}
	},
}
