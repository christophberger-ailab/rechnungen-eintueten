# rechnungen-eintueten

Automates incoming invoices (Rechnungseingang) end to end: read them from a
mailbox, extract the invoice data, file the original in the bookkeeping tree,
hand it to DATEV and book it in sevDesk.

Go backend and CLI, HTMX + Go templates for the web UI, SQLite for everything
persistent. No cgo, no build step, no runtime dependencies beyond the binary
and its database file.

## Status

All modules are implemented and covered by tests. The sevDesk client follows
sevDesk's own OpenAPI description and targets **sevdesk-Update 2.0**, which
books voucher positions to an `accountDatev` and states the VAT regulation as
a `taxRule`. Accounts still on Update 1.0 expect an `accountingType` instead,
whose ids live in a different namespace; that form is not implemented.

Still worth a dry run on a test account before the first real upload: the
requests match the spec and are tested against a stub server, but no call in
this repository has ever reached sevDesk itself.

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
| `rechnungen passwort` | set the web UI password (prompts, no echo) |
| `rechnungen restore <target.db>` | fetch the newest copy from the backup bucket |

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

Then the payment is linked to the voucher. A payment that settles the invoice
exactly is booked as `FULL_PAYMENT`; one that differs is booked as `O`
("reduced/higher amount due to other reasons"), which settles the voucher
despite the difference — sevDesk's dedicated currency-fluctuation code `CF` is
deprecated. Finally the voucher's status is read back from sevDesk: the
invoice counts as paid only when sevDesk says *Bezahlt*, not because the
booking call returned without an error.

The VAT regulation sent with a voucher comes from `sevdesk.tax_rule`
(`1` = Umsatzsteuerpflichtige Umsätze, `5` = Reverse Charge gem. §13b,
`11` = §19 UStG). sevDesk rejects a voucher whose tax rule does not fit its
booking account, so the account is asked which rules it allows and the
configured one is used only if it is among them.

## Access control

One installation, one password — there are no user accounts. Set it with
`rechnungen passwort` (it prompts without echoing; a script may pipe the
password in) or on the settings page. The password is stored as a bcrypt hash.

Logging in creates a session that lasts 30 days. Only a SHA-256 of the session
token is stored, so a copy of the database hands nobody a live session.
Changing the password ends every session. The cookie is `HttpOnly` and
`SameSite=Lax`, and carries `Secure` only when the request arrived over TLS —
over plain HTTP inside a VPN it would otherwise be dropped by the browser.

**With no password configured the web UI is open to anyone who can reach it**,
and every page says so in a banner. That is a usable setup when the network
already restricts access — a Tailscale tailnet, for instance — and it is also
what lets you set the first password without a chicken-and-egg problem. Once a
password is set, changing it requires being logged in.

Brute force is not much of a concern behind a VPN, but a wrong password costs a
bcrypt round plus a one-second delay, and lands in the event log with the
client IP.

## Backup

The SQLite database is replicated continuously to S3-compatible object storage
with [Litestream](https://litestream.io), embedded in this process — no second
binary, no YAML, no cron. Switch it on under *Backup (Litestream)* in the
settings and restart; the server prints the bucket it replicates to on start
and flushes the last changes on shutdown.

The defaults point at Hetzner Object Storage (`https://fsn1.your-objectstorage.com`,
region `fsn1`, path-style URLs); change the endpoint and region if your bucket
lives in another location. Any S3-compatible endpoint works.

To restore, `rechnungen restore <target.db>`. It refuses to overwrite an
existing file. Since the bucket settings live in the database being restored,
they are read from the environment:

```sh
export RECHNUNGEN_BACKUP_ENDPOINT=https://fsn1.your-objectstorage.com
export RECHNUNGEN_BACKUP_REGION=fsn1
export RECHNUNGEN_BACKUP_BUCKET=meine-backups
export RECHNUNGEN_BACKUP_PATH=rechnungen
export RECHNUNGEN_BACKUP_ACCESS_KEY_ID=...
export RECHNUNGEN_BACKUP_SECRET_ACCESS_KEY=...
rechnungen restore ./wiederhergestellt.db
```

When the old database is still readable — restoring a copy to check that the
backup actually works — its settings are used as the base and the environment
only fills the gaps, so a test restore needs no variables at all.

Keep the credentials somewhere outside the database too. A backup you cannot
reach without the thing you are restoring is not a backup.

## Web UI

- **Anmelden** — the password form, the only page reachable without a session.
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
| sevDesk | `sevdesk.base_url`, `sevdesk.token`, `sevdesk.enabled`, `sevdesk.tax_rule`, `sevdesk.fx_tolerance`, `sevdesk.gain_account`, `sevdesk.loss_account` |
| Ablage | `archive.base_dir`, `spool.dir` |
| Backup (Litestream) | `backup.enabled`, `backup.endpoint`, `backup.region`, `backup.bucket`, `backup.path`, `backup.access_key_id`, `backup.secret_access_key`, `backup.force_path_style`, `backup.sync_interval` |
| Ablauf | `schedule.daily_at`, `http.addr` |

A module with no credentials configured is skipped rather than failing the
run, so you can put the pieces into service one at a time.

## Things worth knowing before production use

- **The LLM and OCR steps send invoice content to a third party.** That is a
  data-protection decision, not a technical one. Both steps are optional: with
  no API key configured the chain stops after the text scan and flags what it
  could not read.
- **The web UI serves plain HTTP.** The password protects it, but on an open
  network the password itself travels in the clear — keep it inside a VPN or
  behind a TLS-terminating proxy that sets `X-Forwarded-Proto: https`.
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
internal/backup     Litestream replication to S3, embedded
internal/web        HTMX UI, templates, login and htmx itself
```

## Tests

```sh
go test ./...
```

The suite covers amount and date parsing in both notations, ZUGFeRD (CII) and
XRechnung (UBL) parsing, the whole chain over a real PDF and over a PDF with
an embedded `factur-x.xml`, the archive paths and collision handling, payment
matching including the currency-difference cases, DATEV reply classification,
MIME composition and parsing, the store, the settings round trip, the login
flow (guard, session cookie, logout, password changes, the htmx redirect and
the off-site redirect that must not happen), a Litestream replicate/restore
round trip through its file backend, and the web UI routes. The sevDesk tests run against a stub server and assert the request
shapes the OpenAPI description defines — the tax rule, the `dd.mm.yyyy` dates,
the gross/net position fields, the booking types and which side a currency
gain or loss books to. Nothing in the suite talks to the network.
