package extract

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// invoiceXMLNames are the file names ZUGFeRD, Factur-X and XRechnung use for
// the XML they embed into the PDF.
var invoiceXMLNames = []string{
	"factur-x.xml", "zugferd-invoice.xml", "xrechnung.xml", "zugferd-invoice.XML", "order-x.xml",
}

// EmbeddedInvoiceXML returns the ZUGFeRD/Factur-X XML embedded in a PDF/A-3
// file, or nil when the PDF carries no such attachment.
func EmbeddedInvoiceXML(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	attachments, err := api.ExtractAttachmentsRaw(f, "", nil, conf)
	if err != nil {
		return nil, "", err
	}
	for _, a := range attachments {
		if !isInvoiceXMLName(a.FileName) {
			continue
		}
		b, err := io.ReadAll(a)
		if err != nil {
			return nil, a.FileName, err
		}
		return b, a.FileName, nil
	}
	return nil, "", nil
}

func isInvoiceXMLName(name string) bool {
	for _, n := range invoiceXMLNames {
		if strings.EqualFold(name, n) {
			return true
		}
	}
	// Some issuers pick their own name; any XML attachment is worth a try.
	return strings.HasSuffix(strings.ToLower(name), ".xml")
}

// ParseInvoiceXMLFile reads a standalone invoice XML file (an XRechnung sent
// as a mail attachment, for instance).
func ParseInvoiceXMLFile(path string) (Data, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Data{}, err
	}
	return ParseInvoiceXML(b)
}

// ParseInvoiceXML understands the two formats used in Germany: UN/CEFACT CII
// (ZUGFeRD, Factur-X) and OASIS UBL (XRechnung).
func ParseInvoiceXML(b []byte) (Data, error) {
	root, err := rootElement(b)
	if err != nil {
		return Data{}, err
	}
	switch root {
	case "CrossIndustryInvoice":
		return parseCII(b)
	case "Invoice", "CreditNote":
		return parseUBL(b)
	default:
		return Data{}, fmt.Errorf("unknown invoice XML root element %q", root)
	}
}

func rootElement(b []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("not an XML document: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// Go's encoding/xml matches on the local element name when the struct tag
// carries no namespace, which keeps these definitions free of the ram:/cbc:
// namespace noise.

type ciiInvoice struct {
	Document struct {
		ID            string `xml:"ID"`
		IssueDateTime struct {
			String struct {
				Format string `xml:"format,attr"`
				Value  string `xml:",chardata"`
			} `xml:"DateTimeString"`
		} `xml:"IssueDateTime"`
	} `xml:"ExchangedDocument"`
	Transaction struct {
		Agreement struct {
			Seller struct {
				Name    string `xml:"Name"`
				Address struct {
					LineOne  string `xml:"LineOne"`
					LineTwo  string `xml:"LineTwo"`
					Postcode string `xml:"PostcodeCode"`
					City     string `xml:"CityName"`
					Country  string `xml:"CountryID"`
				} `xml:"PostalTradeAddress"`
				TaxRegistration []struct {
					ID struct {
						Scheme string `xml:"schemeID,attr"`
						Value  string `xml:",chardata"`
					} `xml:"ID"`
				} `xml:"SpecifiedTaxRegistration"`
			} `xml:"SellerTradeParty"`
		} `xml:"ApplicableHeaderTradeAgreement"`
		Settlement struct {
			Currency string `xml:"InvoiceCurrencyCode"`
			Tax      []struct {
				CalculatedAmount string `xml:"CalculatedAmount"`
				TypeCode         string `xml:"TypeCode"`
				RatePercent      string `xml:"RateApplicablePercent"`
			} `xml:"ApplicableTradeTax"`
			Summation struct {
				GrandTotal []string `xml:"GrandTotalAmount"`
				TaxTotal   []string `xml:"TaxTotalAmount"`
			} `xml:"SpecifiedTradeSettlementHeaderMonetarySummation"`
		} `xml:"ApplicableHeaderTradeSettlement"`
	} `xml:"SupplyChainTradeTransaction"`
}

func parseCII(b []byte) (Data, error) {
	var inv ciiInvoice
	if err := xml.Unmarshal(b, &inv); err != nil {
		return Data{}, err
	}
	seller := inv.Transaction.Agreement.Seller
	settle := inv.Transaction.Settlement

	d := Data{
		SenderName: strings.TrimSpace(seller.Name),
		Number:     strings.TrimSpace(inv.Document.ID),
		Date:       normalizeXMLDate(inv.Document.IssueDateTime.String.Value),
		Currency:   strings.ToUpper(strings.TrimSpace(settle.Currency)),
	}
	d.SenderAddress = joinAddress(seller.Address.LineOne, seller.Address.LineTwo,
		strings.TrimSpace(seller.Address.Postcode+" "+seller.Address.City), seller.Address.Country)
	for _, reg := range seller.TaxRegistration {
		if strings.EqualFold(reg.ID.Scheme, "VA") {
			d.SenderVATID = strings.TrimSpace(reg.ID.Value)
		}
	}
	d.TotalCents = firstAmount(settle.Summation.GrandTotal)
	d.VATCents = firstAmount(settle.Summation.TaxTotal)
	for _, t := range settle.Tax {
		if t.TypeCode != "" && !strings.EqualFold(t.TypeCode, "VAT") {
			continue
		}
		if rate, err := strconv.ParseFloat(strings.TrimSpace(t.RatePercent), 64); err == nil && rate > 0 {
			d.VATRate = rate
		}
		if d.VATCents == 0 {
			d.VATCents = firstAmount([]string{t.CalculatedAmount})
		}
	}
	return withDerivedVAT(d), nil
}

type ublInvoice struct {
	ID       string `xml:"ID"`
	Date     string `xml:"IssueDate"`
	Currency string `xml:"DocumentCurrencyCode"`
	Supplier struct {
		Party struct {
			Name struct {
				Name string `xml:"Name"`
			} `xml:"PartyName"`
			LegalEntity struct {
				RegistrationName string `xml:"RegistrationName"`
			} `xml:"PartyLegalEntity"`
			Address struct {
				Street     string `xml:"StreetName"`
				Additional string `xml:"AdditionalStreetName"`
				City       string `xml:"CityName"`
				PostalZone string `xml:"PostalZone"`
				Country    struct {
					Code string `xml:"IdentificationCode"`
				} `xml:"Country"`
			} `xml:"PostalAddress"`
			TaxScheme []struct {
				CompanyID string `xml:"CompanyID"`
			} `xml:"PartyTaxScheme"`
		} `xml:"Party"`
	} `xml:"AccountingSupplierParty"`
	TaxTotal []struct {
		TaxAmount   string `xml:"TaxAmount"`
		TaxSubtotal []struct {
			TaxCategory struct {
				Percent string `xml:"Percent"`
			} `xml:"TaxCategory"`
		} `xml:"TaxSubtotal"`
	} `xml:"TaxTotal"`
	Total struct {
		Payable      string `xml:"PayableAmount"`
		TaxInclusive string `xml:"TaxInclusiveAmount"`
	} `xml:"LegalMonetaryTotal"`
}

func parseUBL(b []byte) (Data, error) {
	var inv ublInvoice
	if err := xml.Unmarshal(b, &inv); err != nil {
		return Data{}, err
	}
	party := inv.Supplier.Party
	name := strings.TrimSpace(party.Name.Name)
	if name == "" {
		name = strings.TrimSpace(party.LegalEntity.RegistrationName)
	}
	d := Data{
		SenderName: name,
		Number:     strings.TrimSpace(inv.ID),
		Date:       normalizeXMLDate(inv.Date),
		Currency:   strings.ToUpper(strings.TrimSpace(inv.Currency)),
		SenderAddress: joinAddress(party.Address.Street, party.Address.Additional,
			strings.TrimSpace(party.Address.PostalZone+" "+party.Address.City),
			party.Address.Country.Code),
	}
	for _, ts := range party.TaxScheme {
		if id := strings.TrimSpace(ts.CompanyID); id != "" {
			d.SenderVATID = id
			break
		}
	}
	d.TotalCents = firstAmount([]string{inv.Total.Payable, inv.Total.TaxInclusive})
	for _, tt := range inv.TaxTotal {
		if d.VATCents == 0 {
			d.VATCents = firstAmount([]string{tt.TaxAmount})
		}
		for _, sub := range tt.TaxSubtotal {
			if rate, err := strconv.ParseFloat(strings.TrimSpace(sub.TaxCategory.Percent), 64); err == nil && rate > 0 {
				d.VATRate = rate
			}
		}
	}
	return withDerivedVAT(d), nil
}

// withDerivedVAT fills in the VAT rate from the amounts, or the other way
// round, when the document states only one of the two.
func withDerivedVAT(d Data) Data {
	net := d.TotalCents - d.VATCents
	switch {
	case d.VATRate == 0 && d.VATCents != 0 && net != 0:
		d.VATRate = math.Round(float64(d.VATCents)/float64(net)*1000) / 10
	case d.VATCents == 0 && d.VATRate != 0 && d.TotalCents != 0:
		gross := float64(d.TotalCents)
		d.VATCents = int64(math.Round(gross - gross/(1+d.VATRate/100)))
	}
	return d
}

func joinAddress(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

func firstAmount(values []string) int64 {
	for _, v := range values {
		if c, err := ParseAmount(v); err == nil && c != 0 {
			return c
		}
	}
	return 0
}

// normalizeXMLDate accepts both the CII "102" format (YYYYMMDD) and the ISO
// dates used by UBL.
func normalizeXMLDate(s string) string {
	s = strings.TrimSpace(s)
	switch {
	case len(s) == 8 && !strings.Contains(s, "-"):
		return s[:4] + "-" + s[4:6] + "-" + s[6:]
	case len(s) >= 10:
		return s[:10]
	default:
		return ""
	}
}
