# rechnungen-eintueten

Automates incoming invoices (Rechnungseingang) end to end: read them from a
mailbox, extract the invoice data, file the original in the bookkeeping tree,
hand it to DATEV and book it in sevDesk.

Go backend and CLI, HTMX + Go templates for the web UI, SQLite for everything
persistent. No cgo, no build step, no runtime dependencies beyond the binary
and its database file.

## Status

All modules are implemented and covered by tests. One caveat before you point
it at a live account: the **sevDesk client is written against the publicly
documented shape of the API, and a few endpoints had to be inferred** (the
official spec was not reachable from the machine this was built on). Every
guess is marked with a `// NOTE:` comment in `internal/sevdesk/client.go`.
Verify those against your own sevDesk account before the first real upload —
the request plumbing is tested, the exact endpoint contracts are not.

## Quick start

```sh
go build -o rechnungen ./cmd/rechnungen

# Tell it who sends invoices and which SKR-04 account they book to.
./rechnungen sender rechnung@lieferant.de 6815 "Muster Lieferant"

# Configure the rest in the browser.
./rechnungen serve            # web UI on :8080, daily run at 03:00
```

For a cron setup, skip the server and run the pipeline directly:

```sh
0 3 * * *  /opt/rechnungen/rechnungen -db /var/lib/rechnungen/rechnungen.db run
```

### CLI

| Command | Purpose |
|---|---|
| `rechnungen serve` | web UI plus the daily scheduled run |
| `rechnungen run` | one pass over everything pending, for cron |
| `rechnungen config` | show the stored configuration (secrets masked) |
| `rechnungen config <key> <value>` | set one value without the web UI |
| `rechnungen sender` | list known senders |
| `rechnungen sender <mail> <skr04> [name]` | add or update a sender |
| `rechnungen sender rm <mail>` | remove a sender |

`-db` selects the database file (default `rechnungen.db`), `-addr` overrides
the web UI address.

## How a run works

Each stage is idempotent and driven by the state stored with the invoice, so a
run that dies halfway simply continues on the next one. Nothing is deleted
from the mailbox; documents are identified by the SHA-256 of their content, so
the same mail fetched twice produces one invoice.

1. **Mailbox** (`internal/mailbox`) — IMAP, envelopes first and bodies only for
   known senders, so a busy mailbox costs one cheap round trip. PDF and XML
   attachments land in the spool directory.
2. **Extraction** (`internal/extract`) — the chain below.
3. **Archive** (`internal/archive`) — copies the original to
   `FIBU <YYYY>/Rechnungseingang/E<YY>Q<Q>`, e.g.
   `FIBU 2026/Rechnungseingang/E26Q3`, named
   `2026-03-14_Musterlieferant-GmbH_RE-2026-0042.pdf`. An existing file is
   never overwritten.
4. **DATEV** (`internal/datev`) — mails the original to the configured DATEV
   address, then reads the confirmations. A reply that names neither success
   nor failure counts as a failure, so nothing is silently treated as booked.
5. **sevDesk** (`internal/sevdesk`) — creates the voucher in status *Offen*
   with the sender's SKR-04 account, then matches payments (below).
6. **Finish** — an invoice is *Fertig* once it is archived and every enabled
   module is through with it.

Failures never abort the run: they are recorded against the invoice and the
run, and retried next time.

### Extraction chain

Cheapest and most reliable technique first:

1. **Structured invoice** — a ZUGFeRD/Factur-X XML embedded in the PDF, or a
   standalone XRechnung file. Both UN/CEFACT CII and OASIS UBL are parsed.
2. **PDF text layer** — keyword scanning in German and English
   (`Rechnungs-Nr.`, `Invoice No.`, `Gesamtbetrag`, `Total due`, …), with
   amount parsing that handles both `1.234,56` and `1,234.56`.
3. **LLM** — whatever the keyword scan could not find. Defaults to Claude
   (`claude-opus-5`) through the official Anthropic SDK; set `llm.kind` to
   `openai` to use any OpenAI-compatible endpoint instead.
4. **OCR** — for PDFs that are nothing but a scan. Defaults to Mistral OCR
   (`mistral-ocr-latest`); the endpoint is configurable.

The method that produced the data is recorded per invoice and shown in the
dashboard. An invoice that stays incomplete is flagged *Prüfen* and can be
corrected by hand in the web UI, which puts it back into the pipeline.

VAT is treated as optional throughout — reverse-charge and small-business
invoices do not show any — and the rate is derived from the amounts, or the
other way round, when a document states only one of the two.

### Payment matching

For every voucher still *Offen*, the bank transactions are searched for the
payment. A transaction has to match on amount and, ideally, carry the invoice
number or the supplier name in its reference; a match on the amount alone is
only accepted when it is unique.

A payment that is close but not equal is accepted only for invoices in a
foreign currency (the generalisation of the USD case, capped by
`sevdesk.fx_tolerance`, default 5 %). The difference is booked as its own
voucher first:

- paid **less** than invoiced → *Erlös aus Währungsumrechnung* (`sevdesk.gain_account`, default 4840)
- paid **more** than invoiced → *Verlust aus Währungsumrechnung* (`sevdesk.loss_account`, default 6880)

Then the payment is linked to the voucher and the voucher's status is read
back from sevDesk — the invoice counts as paid only when sevDesk says
*Bezahlt*, not because the booking call returned without an error.

## Web UI

- **Übersicht** — counts per state, the last run, a "Verarbeitung starten"
  button that runs the pipeline in the background, a history table and the
  event log. While a run is in flight the panels refresh themselves via htmx.
- **Einstellungen** — known senders with their SKR-04 accounts, and every
  configuration value grouped by subsystem.
- **Rechnung** — one invoice with its events, and an editable form for the
  fields extraction could not determine.

Secrets are stored in the database but never sent back to the browser; an
empty password field leaves the stored value alone. htmx is served from the
binary, so the UI works on a machine without internet access.

## Configuration

All configuration lives in the `settings` table and is editable in the web UI
or with `rechnungen config`. The struct tags in `internal/config/config.go`
are the single source of truth — they drive loading, saving and the form.

| Group | Keys |
|---|---|
| Mailbox (IMAP) | `imap.host`, `imap.port`, `imap.user`, `imap.pass`, `imap.mailbox`, `imap.tls`, `imap.days` |
| Mailversand (SMTP) | `smtp.host`, `smtp.port`, `smtp.user`, `smtp.pass`, `smtp.from` |
| LLM | `llm.base_url`, `llm.api_key`, `llm.model`, `llm.kind`, `llm.timeout` |
| OCR | `ocr.base_url`, `ocr.api_key`, `ocr.model` |
| DATEV | `datev.to`, `datev.from`, `datev.subject`, `datev.enabled` |
| sevDesk | `sevdesk.base_url`, `sevdesk.token`, `sevdesk.enabled`, `sevdesk.fx_tolerance`, `sevdesk.gain_account`, `sevdesk.loss_account` |
| Ablage | `archive.base_dir`, `spool.dir` |
| Ablauf | `schedule.daily_at`, `http.addr` |

A module with no credentials configured is skipped rather than failing the
run, so you can put the pieces into service one at a time.

## Things worth knowing before production use

- **The LLM and OCR steps send invoice content to a third party.** That is a
  data-protection decision, not a technical one. Both steps are optional: with
  no API key configured the chain stops after the text scan and flags what it
  could not read.
- **The web UI has no authentication.** Bind it to localhost and put it behind
  your own reverse proxy, or keep it on a trusted network.
- **The archive copy is not a GoBD-compliant archive** on its own. It files the
  originals where your bookkeeping expects them; audit-proof storage is a
  separate concern.
- The spool directory keeps the originals the pipeline works on. It is the
  source for the DATEV mail and the sevDesk upload, so do not prune it while
  invoices are still in flight.

## Layout

```
cmd/rechnungen      CLI: serve, run, config, sender
internal/store      SQLite schema, invoices, runs, events, settings
internal/config     typed view of the settings, drives the settings form
internal/mailbox    IMAP reader and SMTP sender
internal/extract    XML / text / LLM / OCR extraction chain
internal/archive    the FIBU directory tree
internal/datev      mail to DATEV and its confirmations
internal/sevdesk    API client and payment matching
internal/pipeline   stage orchestration, scheduler, run guard
internal/web        HTMX UI, templates and htmx itself
```

## Tests

```sh
go test ./...
```

The suite covers amount and date parsing in both notations, ZUGFeRD (CII) and
XRechnung (UBL) parsing, the whole chain over a real PDF and over a PDF with
an embedded `factur-x.xml`, the archive paths and collision handling, payment
matching including the currency-difference cases, DATEV reply classification,
MIME composition and parsing, the sevDesk request plumbing against a stub
server, the store, the settings round trip, and the web UI routes. Nothing in
the suite talks to the network.
