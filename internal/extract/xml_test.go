package extract

import "testing"

const ciiInvoiceXML = `<?xml version="1.0" encoding="UTF-8"?>
<rsm:CrossIndustryInvoice xmlns:rsm="urn:un:unece:uncefact:data:standard:CrossIndustryInvoice:100"
  xmlns:ram="urn:un:unece:uncefact:data:standard:ReusableAggregateBusinessInformationEntity:100"
  xmlns:udt="urn:un:unece:uncefact:data:standard:UnqualifiedDataType:100">
  <rsm:ExchangedDocument>
    <ram:ID>ZF-2026-7</ram:ID>
    <ram:IssueDateTime><udt:DateTimeString format="102">20260314</udt:DateTimeString></ram:IssueDateTime>
  </rsm:ExchangedDocument>
  <rsm:SupplyChainTradeTransaction>
    <ram:ApplicableHeaderTradeAgreement>
      <ram:SellerTradeParty>
        <ram:Name>Lieferant AG</ram:Name>
        <ram:PostalTradeAddress>
          <ram:PostcodeCode>50667</ram:PostcodeCode>
          <ram:LineOne>Domplatz 1</ram:LineOne>
          <ram:CityName>Köln</ram:CityName>
          <ram:CountryID>DE</ram:CountryID>
        </ram:PostalTradeAddress>
        <ram:SpecifiedTaxRegistration><ram:ID schemeID="VA">DE987654321</ram:ID></ram:SpecifiedTaxRegistration>
      </ram:SellerTradeParty>
    </ram:ApplicableHeaderTradeAgreement>
    <ram:ApplicableHeaderTradeSettlement>
      <ram:InvoiceCurrencyCode>EUR</ram:InvoiceCurrencyCode>
      <ram:ApplicableTradeTax>
        <ram:CalculatedAmount>38.00</ram:CalculatedAmount>
        <ram:TypeCode>VAT</ram:TypeCode>
        <ram:RateApplicablePercent>19.00</ram:RateApplicablePercent>
      </ram:ApplicableTradeTax>
      <ram:SpecifiedTradeSettlementHeaderMonetarySummation>
        <ram:TaxTotalAmount currencyID="EUR">38.00</ram:TaxTotalAmount>
        <ram:GrandTotalAmount>238.00</ram:GrandTotalAmount>
      </ram:SpecifiedTradeSettlementHeaderMonetarySummation>
    </ram:ApplicableHeaderTradeSettlement>
  </rsm:SupplyChainTradeTransaction>
</rsm:CrossIndustryInvoice>`

func TestParseCII(t *testing.T) {
	d, err := ParseInvoiceXML([]byte(ciiInvoiceXML))
	if err != nil {
		t.Fatal(err)
	}
	if d.Number != "ZF-2026-7" || d.Date != "2026-03-14" {
		t.Errorf("number/date = %q/%q", d.Number, d.Date)
	}
	if d.SenderName != "Lieferant AG" || d.SenderVATID != "DE987654321" {
		t.Errorf("sender = %q / %q", d.SenderName, d.SenderVATID)
	}
	if d.SenderAddress != "Domplatz 1, 50667 Köln, DE" {
		t.Errorf("address = %q", d.SenderAddress)
	}
	if d.TotalCents != 23800 || d.VATCents != 3800 || d.VATRate != 19 {
		t.Errorf("amounts = %d / %d / %v", d.TotalCents, d.VATCents, d.VATRate)
	}
	if d.Currency != "EUR" {
		t.Errorf("currency = %q", d.Currency)
	}
	if !d.Complete() {
		t.Errorf("incomplete, missing %v", d.missing())
	}
}

const ublInvoiceXML = `<?xml version="1.0" encoding="UTF-8"?>
<Invoice xmlns="urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
  xmlns:cac="urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2"
  xmlns:cbc="urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2">
  <cbc:ID>XR-2026-123</cbc:ID>
  <cbc:IssueDate>2026-03-14</cbc:IssueDate>
  <cbc:DocumentCurrencyCode>EUR</cbc:DocumentCurrencyCode>
  <cac:AccountingSupplierParty>
    <cac:Party>
      <cac:PartyName><cbc:Name>Behörden IT GmbH</cbc:Name></cac:PartyName>
      <cac:PostalAddress>
        <cbc:StreetName>Rathausweg 3</cbc:StreetName>
        <cbc:CityName>Bonn</cbc:CityName>
        <cbc:PostalZone>53111</cbc:PostalZone>
        <cac:Country><cbc:IdentificationCode>DE</cbc:IdentificationCode></cac:Country>
      </cac:PostalAddress>
      <cac:PartyTaxScheme><cbc:CompanyID>DE111222333</cbc:CompanyID></cac:PartyTaxScheme>
    </cac:Party>
  </cac:AccountingSupplierParty>
  <cac:TaxTotal>
    <cbc:TaxAmount currencyID="EUR">95.00</cbc:TaxAmount>
    <cac:TaxSubtotal><cac:TaxCategory><cbc:Percent>19.00</cbc:Percent></cac:TaxCategory></cac:TaxSubtotal>
  </cac:TaxTotal>
  <cac:LegalMonetaryTotal>
    <cbc:TaxInclusiveAmount currencyID="EUR">595.00</cbc:TaxInclusiveAmount>
    <cbc:PayableAmount currencyID="EUR">595.00</cbc:PayableAmount>
  </cac:LegalMonetaryTotal>
</Invoice>`

func TestParseUBL(t *testing.T) {
	d, err := ParseInvoiceXML([]byte(ublInvoiceXML))
	if err != nil {
		t.Fatal(err)
	}
	if d.Number != "XR-2026-123" || d.Date != "2026-03-14" {
		t.Errorf("number/date = %q/%q", d.Number, d.Date)
	}
	if d.SenderName != "Behörden IT GmbH" || d.SenderVATID != "DE111222333" {
		t.Errorf("sender = %q / %q", d.SenderName, d.SenderVATID)
	}
	if d.TotalCents != 59500 || d.VATCents != 9500 || d.VATRate != 19 {
		t.Errorf("amounts = %d / %d / %v", d.TotalCents, d.VATCents, d.VATRate)
	}
	if !d.Complete() {
		t.Errorf("incomplete, missing %v", d.missing())
	}
}

func TestParseInvoiceXMLUnknownRoot(t *testing.T) {
	if _, err := ParseInvoiceXML([]byte(`<Something/>`)); err == nil {
		t.Error("want error for unknown root element")
	}
}

func TestWithDerivedVAT(t *testing.T) {
	// Rate stated, amount missing: 119.00 gross at 19% is 19.00 VAT.
	d := withDerivedVAT(Data{TotalCents: 11900, VATRate: 19})
	if d.VATCents != 1900 {
		t.Errorf("derived vat = %d, want 1900", d.VATCents)
	}
	// Amount stated, rate missing.
	d = withDerivedVAT(Data{TotalCents: 11900, VATCents: 1900})
	if d.VATRate != 19 {
		t.Errorf("derived rate = %v, want 19", d.VATRate)
	}
}
