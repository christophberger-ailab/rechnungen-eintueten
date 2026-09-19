# Invoice processing

I need a CLI tool that processes invoices. Backend/CLI: Go. Frontend: HTMX and Go templates.

The idea: A multi-step pipeline, with these modules:

Module 1: Email reader. 

- Has access to email (configurable IMAP/SMTP)
- Scans incoming emails in regular intervals for emails from known addresses (configurable list)
- Saves attachments of these emails

Module 2: Invoice extractor

- Extracts invoice data: 
	- Sender (name, address, VAT ID if applicable)
	- Invoice number
	- Invoice date
	- Total
	- Currency
	- VAT if present
	- VAT % if present
- Extraction techniques:
	- First, determine if the invoice is a proper X-Rechnung or ZuGFERD invoice & read the invoice data from the XML part.
	- If no embedded XML, use a PDF library to extract the data. Scan for keywords in German and English (e.g., "Total", "Summe", "Invoice No.", "Rechnungs-Nr.",...). 
	- If the data cannot be completely extracted (or not extracted at all), use an LLM to extract the data (LLM connection configurable).
	- If the PDF is a pure image, call an OCR LLM (configurable, defaults to Mistral OCR, https://docs.mistral.ai/api/endpoint/ocr) to extract the data.
- Save the extracted data in a SQLite database. 

Module 2: Datev uploader

- Sends original invoice PDFs to Datev by email (sender & Datev email address configurable)
- Checks for email replies from Datev (success or failure messages) & raise failures to user 

Module 3: SevDesk uploader

- Uses the SevDesk API (https://api.sevdesk.de/)
	- Uploads each invoice
		- Sets SKR-04 category according to configured data
		- Saves the invoice. Status should change to "Offen"
	- For every invoice in status "Offen":
	- Looks for matching payments
	- If a payment matches, connect payment to invoice
	- If payments almost match a USD invoice (only the amount differs by a few cent or dollars, not more than 5%):
		- Create a new receipt for the delta amount
		- Type:
			- "Erlös aus Währungsumrechnung" if paid amount is smaller than invoice amount
			- "Verlust aus Währungsumrechnung" if paid amount is bigger than invoice amount
		- Connect payment to invoice
	- Verify that invoice is set to "Bezahlt"

Module 4: local doc saver

- Saves invoices to the local file system
	- Configurable base directory
	- Path: "FIBU <YYYY>/Rechnungseingang/E<YY>Q<Q>", where YYYY and YY are the year and Q is the quarter (1-4). E.g. "FIBU 2026/Rechnungseingang/E26Q3"

Module 5: configuration

- Config saved in SQLite database
- Web UI 
- Configurations:
	- Known sender
		- Email
		- SKR-04 category of invoices from this sender
	- EMail server access URLs and credentials
	- API connection to LLM and access token for the same
	- API connection to OCR LLM and access token for the same

Module 6: Dashboard

- Web UI
- Show invoice processing status
- Show table of invoice processing history
- Has a button for starting the processing manually

Execution modes:

- A steadily running server
	- Runs processing once a day and on demand (web UI)
- Invocation of processing through CLI subcommand (for CRON usage scenario)

Your task:

- Evaluate the idea
- If feasible, implement each module with minimal code, idiomatic Go
