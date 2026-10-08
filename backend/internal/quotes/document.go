package quotes

import (
	"context"
	_ "embed"
	"sort"
	"strconv"
	"strings"
	"time"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	mailer "dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/pdf"
)

// document is everything printed on a quotation.
type document struct {
	Quote
	ClientAddress *string
	ClientTPIN    *string
}

func (s *Service) loadForDocument(ctx context.Context, q db.Querier, qt *Quote) (document, error) {
	d := document{Quote: *qt}
	var err error
	if d.Lines, err = listLines(ctx, q, qt.ID); err != nil {
		return d, err
	}
	cl, err := s.clients.Get(ctx, qt.clientID)
	if err != nil {
		return d, err
	}
	d.Client = ClientRef{ID: cl.ID, Reference: cl.Reference, LegalName: cl.LegalName, Status: string(cl.Status)}
	d.ClientAddress, d.ClientTPIN = cl.BillingAddress, cl.TPIN
	if qt.contactID != nil {
		for _, k := range cl.Contacts {
			if k.ID == *qt.contactID {
				d.Contact = &ContactRef{ID: k.ID, Name: k.Name, Position: k.Position, Email: k.Email, Phone: k.Phone}
			}
		}
	}
	if qt.dealID != nil {
		if dl, err := s.deals.Get(ctx, *qt.dealID); err == nil {
			d.Deal = &DealRef{ID: dl.ID, Reference: dl.Reference, Title: dl.Title, Stage: string(dl.Stage)}
		}
	}
	return d, nil
}

// The quotation follows DOS's own template (DOS QUOTE ZMW / USD): the
// letterhead and footer artwork come from the template itself (assets/), and
// the fields, table, totals, terms and bank details are drawn in the same
// places, colours and style. Long quotations continue on further pages.

//go:embed assets/header.jpg
var headerJPG []byte

//go:embed assets/footer.jpg
var footerJPG []byte

var (
	navy  = pdf.Hex("#003177") // DOS blue
	grey  = pdf.Hex("#dbdbdb") // field boxes
	black = pdf.Color{}
	ink   = pdf.Hex("#1d2433")
	faint = pdf.Hex("#6b7387")
	white = pdf.Color{R: 1, G: 1, B: 1}
)

// Measurements in points, taken from the template.
const (
	left       = 48.0
	right      = 547.5
	headerH    = 217.96
	footerTop  = 797.7
	contentEnd = 785.0 // nothing below this but the footer
	rowGap     = 2.0
	minRowH    = 18.0
	cellGap    = 2.5
	boxPad     = 5.0
)

// table columns, as in the template: part no., description, qty, unit price, total
var colEdges = [][2]float64{{47.5, 104}, {106.5, 269}, {271, 329}, {332, 427}, {430, 547.5}}

func currencyLabel(c string) string {
	if c == "USD" {
		return "U$D" // as printed on the DOS template
	}
	return c
}

func (s *Service) renderPDF(_ context.Context, d document) ([]byte, error) {
	doc := pdf.New()
	doc.Title = "Quotation " + d.Number
	doc.Author = s.company.Name
	doc.Subject = d.Title
	hdr, err := doc.AddJPEG(headerJPG)
	if err != nil {
		return nil, err
	}
	ftr, err := doc.AddJPEG(footerJPG)
	if err != nil {
		return nil, err
	}
	cur := currencyLabel(d.Currency)

	var p *pdf.Page
	y := 0.0
	newPage := func() {
		p = doc.AddPage()
		p.Image(hdr, 0, 0, pdf.A4Width, headerH)
		p.Image(ftr, 0, footerTop, pdf.A4Width, pdf.A4Height-footerTop)
		// Quotation number in the white box on the blue band.
		p.Text(437, 116.6, pdf.Bold, 10.5, navy, fit(pdf.Bold, 10.5, d.Number, 114))
		switch d.Status {
		case StatusDraft, StatusPendingApproval:
			p.Watermark("DRAFT", navy)
		case StatusCancelled:
			p.Watermark("CANCELLED", navy)
		case StatusSuperseded:
			p.Watermark("SUPERSEDED", navy)
		case StatusExpired:
			p.Watermark("EXPIRED", navy)
		case StatusDeclined:
			p.Watermark("DECLINED", navy)
		}
		y = 226
	}
	label := func(xRight, baseline float64, s string) {
		p.TextRight(xRight, baseline, pdf.Bold, 8, navy, s)
	}
	box := func(x1, top, x2, h float64) {
		p.Rect(x1, top, x2-x1, h, &grey, nil, 0)
	}

	newPage()

	// Company, date and attention.
	addr := ""
	if d.ClientAddress != nil {
		addr = strings.Join(strings.Fields(strings.ReplaceAll(*d.ClientAddress, "\n", ", ")), " ")
	}
	if d.ClientTPIN != nil {
		if addr != "" {
			addr += "  ·  "
		}
		addr += "TPIN " + *d.ClientTPIN
	}
	label(102, 238.2, "COMPANY:")
	box(106.5, 226, 329, 18)
	p.Text(106.5+boxPad, 238, pdf.Bold, 8.5, ink, fit(pdf.Bold, 8.5, d.Client.LegalName, 329-106.5-2*boxPad))
	box(106.5, 246.5, 329, 18)
	p.Text(106.5+boxPad, 258.5, pdf.Regular, 7.5, ink, fit(pdf.Regular, 7.5, addr, 329-106.5-2*boxPad))
	label(424, 238.2, "DATE:")
	box(430, 226, 546.5, 18)
	p.Text(430+boxPad, 238, pdf.Regular, 8.5, ink, longDate(d.QuoteDate))
	label(424, 258.7, "ATTENTION:")
	box(430, 246.5, 546.5, 18)
	att := ""
	if d.Contact != nil {
		att = d.Contact.Name
	}
	p.Text(430+boxPad, 258.5, pdf.Regular, 8.5, ink, fit(pdf.Regular, 8.5, att, 546.5-430-2*boxPad))
	y = 275

	// "SUPPLY & DELIVERY OF" bar, with the quotation's title.
	barText := strings.ToUpper(d.Title)
	if !strings.HasPrefix(barText, "SUPPLY") {
		barText = "SUPPLY & DELIVERY OF  " + barText
	}
	barLines := pdf.Wrap(pdf.Bold, 9.5, barText, right-left-14)
	barW := 280.0
	for _, l := range barLines {
		if w := pdf.Width(pdf.Bold, 9.5, l) + 14; w > barW {
			barW = w
		}
	}
	barH := 22.5 + float64(len(barLines)-1)*12
	p.Rect(left, y, barW, barH, &black, nil, 0)
	for i, l := range barLines {
		p.Text(left+6, y+15+float64(i)*12, pdf.Bold, 9.5, white, l)
	}
	y += barH + 2

	tableHead := func() {
		heads := []string{"PART NO.", "DESCRIPTION", "QTY", "UNIT PRICE " + cur, "TOTAL " + cur}
		p.Rect(left-0.5, y, right-left+0.5, 16, &navy, nil, 0)
		for i, h := range heads {
			c := colEdges[i]
			if i == 1 {
				p.Text(c[0]+15, y+11, pdf.Bold, 7.5, white, h)
			} else {
				p.TextCenter((c[0]+c[1])/2, y+11, pdf.Bold, 7.5, white, h)
			}
		}
		y += 16 + rowGap
	}
	tableHead()

	type row struct {
		part, qty, price, total string
		desc                    []string
		h                       float64
	}
	var rows []row
	for _, l := range d.Lines {
		desc := l.Description
		if l.DiscountPct != "0" {
			desc += "\n(less " + l.DiscountPct + "% discount)"
		}
		lines := pdf.Wrap(pdf.Regular, 7.5, desc, colEdges[1][1]-colEdges[1][0]-2*boxPad)
		qty := l.Quantity
		if l.Unit != "" && l.Unit != "each" {
			qty += " " + l.Unit
		}
		qtyH, priceH := hundredthsOf(l.Quantity), hundredthsOf(l.UnitPrice)
		gross, _, _, _ := lineAmounts(qtyH, priceH, 0, 0)
		h := float64(len(lines))*9.2 + 8.8
		if h < minRowH {
			h = minRowH
		}
		rows = append(rows, row{part: derefStr(l.SKU), qty: qty, price: grouped(l.UnitPrice), total: gross.Grouped(), desc: lines, h: h})
	}
	// Totals (drawn after the lines), with the client's notes beside them.
	totals := [][2]string{{"SUBTOTAL " + cur, grouped(d.Subtotal)}}
	if d.DiscountTotal != "0.00" {
		totals = append(totals, [2]string{"LESS DISCOUNT", "– " + grouped(d.DiscountTotal)})
	}
	totals = append(totals, [2]string{strings.ToUpper(vatLabel(d.Lines)), grouped(d.VATTotal)}, [2]string{"TOTAL " + cur, grouped(d.Total)})
	var notes []string
	if d.Notes != nil {
		notes = pdf.Wrap(pdf.Regular, 7.5, *d.Notes, 300)
	}
	need := float64(len(totals)) * 22
	if n := float64(len(notes))*9.5 + 14; n > need {
		need = n
	}
	valid := ""
	if qd, err := time.Parse("2006-01-02", d.QuoteDate); err == nil {
		if vu, err := time.Parse("2006-01-02", d.ValidUntil); err == nil {
			days := int(vu.Sub(qd).Hours()/24 + 0.5)
			valid = itoa(days) + " DAYS  (until " + longDate(d.ValidUntil) + ")"
		}
	}
	type field struct{ label, value string }
	leftFields := []field{
		{"QUOTATION VALID FOR:", valid},
		{"DELIVERY:", derefStr(d.DeliveryTerms)},
		{"PAYMENT TERMS:", derefStr(d.PaymentTerms)},
		{"WARRANTY:", derefStr(d.WarrantyTerms)},
	}
	contact := d.Owner.Phone
	if contact == "" {
		contact = s.company.Phone
	}
	var other []string
	if d.Terms != nil {
		other = pdf.Wrap(pdf.Regular, 7.5, *d.Terms, right-left)
	}
	fieldW := 329 - 177 - 2*boxPad
	heights := make([]float64, len(leftFields))
	blockH := 0.0
	for i, f := range leftFields {
		n := len(pdf.Wrap(pdf.Regular, 7, f.value, fieldW))
		heights[i] = maxf(14.5, float64(n)*8.4+6)
		blockH += heights[i] + 2.5
	}
	if blockH < 68 {
		blockH = 68
	}
	// Bank details: the account in the quotation's currency first.
	type account struct {
		title string
		lines []string
	}
	var accounts []account
	add := func(title, v string) {
		if strings.TrimSpace(v) != "" {
			accounts = append(accounts, account{title, strings.Split(v, "\n")})
		}
	}
	if d.Currency == "USD" {
		add("DOLLAR ACCOUNT", s.company.BankUSD)
		add("KWACHA ACCOUNT", s.company.BankZMW)
	} else {
		add("KWACHA ACCOUNT", s.company.BankZMW)
		add("DOLLAR ACCOUNT", s.company.BankUSD)
	}
	if len(accounts) == 0 {
		add("PAYMENT DETAILS", s.company.Bank)
	}
	tallest := 0
	for _, a := range accounts {
		if len(a.lines) > tallest {
			tallest = len(a.lines)
		}
	}
	bankH := 0.0
	if len(accounts) > 0 {
		bankH = 12 + 39.5 + float64(tallest)*10.1
	}
	otherH := 0.0
	if len(other) > 0 {
		otherH = 18 + float64(len(other))*9.5
	}
	tailH := need + 12 + 22 + blockH + 4 + otherH + bankH
	// Like the printed template, show at least ten rows when they fit on the page.
	rowsH := func() float64 {
		t := 0.0
		for _, r := range rows {
			t += r.h + rowGap
		}
		return t
	}
	for len(rows) < 10 && y+rowsH()+minRowH+rowGap+tailH <= contentEnd {
		rows = append(rows, row{h: minRowH})
	}
	for _, r := range rows {
		if y+r.h > contentEnd {
			newPage()
			p.Text(left, y+8, pdf.Bold, 8, navy, "CONTINUED")
			y += 14
			tableHead()
		}
		for _, c := range colEdges {
			box(c[0], y, c[1], r.h)
		}
		mid := y + r.h/2 + 2.6
		top := y + 11.6
		if len(r.desc) <= 1 {
			top = mid
		}
		p.Text(colEdges[0][0]+boxPad, mid, pdf.Regular, 7, ink, fit(pdf.Regular, 7, r.part, colEdges[0][1]-colEdges[0][0]-2*boxPad))
		for i, t := range r.desc {
			p.Text(colEdges[1][0]+boxPad, top+float64(i)*9.2, pdf.Regular, 7.5, ink, t)
		}
		p.TextCenter((colEdges[2][0]+colEdges[2][1])/2, mid, pdf.Regular, 8, ink, r.qty)
		p.TextRight(colEdges[3][1]-boxPad, mid, pdf.Regular, 8, ink, r.price)
		p.TextRight(colEdges[4][1]-boxPad-5, mid, pdf.Bold, 8, ink, r.total)
		y += r.h + rowGap
	}

	// Totals, with the client's notes beside them.
	if y+need > contentEnd {
		newPage()
	}
	ty := y + 0.5
	for i, t := range totals {
		label(422, ty+13, t[0])
		p.Rect(430, ty, 112.5, 19.5, &grey, nil, 0)
		p.Rect(542.5, ty, 5, 19.5, &navy, nil, 0)
		f := pdf.Regular
		if i == len(totals)-1 {
			f = pdf.Bold
		}
		p.TextRight(537, ty+13, f, 8.5, ink, t[1])
		ty += 22.5
	}
	if len(notes) > 0 {
		p.Text(left, y+9, pdf.Bold, 7.5, navy, "NOTES")
		for i, l := range notes {
			p.Text(left, y+20+float64(i)*9.5, pdf.Regular, 7.5, ink, l)
		}
	}
	y += need + 12

	// Special terms and conditions.
	if y+22+blockH+float64(len(other))*9.5+14 > contentEnd {
		newPage()
	}
	p.Text(56, y+4.5, pdf.Bold, 7.5, navy, "SPECIAL TERMS AND CONDITIONS")
	p.Line(left, y+13, right+3, y+13, navy, 0.8)
	fy := y + 21.5
	for i, f := range leftFields {
		label(172, fy+10, f.label)
		box(177, fy, 329, heights[i])
		for j, l := range pdf.Wrap(pdf.Regular, 7, f.value, fieldW) {
			off := 10.0
			if heights[i] > 14.5 {
				off = 9.6
			}
			p.Text(177+boxPad, fy+off+float64(j)*8.4, pdf.Regular, 7, ink, l)
		}
		fy += heights[i] + 2.5
	}
	ry := y + 21.5
	label(424, ry+10, "PREPARED BY:")
	box(429, ry, 547.5, 14.5)
	p.Text(429+boxPad, ry+10, pdf.Regular, 7.5, ink, fit(pdf.Regular, 7.5, d.Owner.Name, 547.5-429-2*boxPad))
	ry += 17
	label(424, ry+10, "CONTACT NO:")
	box(429, ry, 547.5, 14.5)
	p.Text(429+boxPad, ry+10, pdf.Regular, 7.5, ink, fit(pdf.Regular, 7.5, contact, 547.5-429-2*boxPad))
	ry += 17
	label(424, ry+18, "SIGNATURE")
	box(429, ry, 547.5, 31)
	y = maxf(fy, ry+31) + 4
	if len(other) > 0 {
		p.Text(56, y+9, pdf.Bold, 7.5, navy, "OTHER TERMS")
		y += 18
		for _, l := range other {
			p.Text(56, y, pdf.Regular, 7.5, ink, l)
			y += 9.5
		}
	}

	if len(accounts) > 0 {
		if y+28+float64(tallest+1)*10.2 > contentEnd {
			newPage()
		}
		y += 12
		p.Text(56, y+4.5, pdf.Bold, 7.5, navy, "BANK DETAILS")
		p.Line(left, y+13, right+3, y+13, navy, 0.8)
		for i, a := range accounts {
			if i > 1 {
				break
			}
			x := 59.0 + float64(i)*229
			p.Text(x, y+29, pdf.Bold, 8, navy, a.title)
			for j, l := range a.lines {
				p.Text(x, y+39.5+float64(j)*10.1, pdf.Bold, 6.8, navy, strings.ToUpper(strings.TrimSpace(l)))
			}
		}
	}

	// Page numbers when there's more than one page.
	if n := doc.PageCount(); n > 1 {
		for i, pg := range doc.Pages() {
			pg.TextRight(right, 793, pdf.Regular, 7, faint, d.Number+"  ·  Page "+itoa(i+1)+" of "+itoa(n))
		}
	}
	return doc.Bytes()
}

// fit shortens s with "…" so it fits in width.
func fit(f pdf.Font, size float64, s string, width float64) string {
	if pdf.Width(f, size, s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 1 && pdf.Width(f, size, string(r)+"…") > width {
		r = r[:len(r)-1]
	}
	return strings.TrimSpace(string(r)) + "…"
}

func hundredthsOf(s string) hundredths {
	h, _ := parseHundredths(s)
	return h
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func vatLabel(lines []Line) string {
	rate := ""
	for _, l := range lines {
		if rate == "" {
			rate = l.VATRate
		} else if rate != l.VATRate {
			return "VAT"
		}
	}
	if rate == "" {
		return "VAT"
	}
	return "VAT " + rate + "%"
}

func longDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("2 January 2006")
}

func itoa(n int) string { return strconv.Itoa(n) }

// quoteEmail is the message that carries the quotation PDF.
func (s *Service) quoteEmail(d document, actor auth.Principal, to, personal string) mailer.Message {
	greeting := "Dear Sir or Madam,"
	toName := d.Client.LegalName
	if d.Contact != nil && d.Contact.Email != nil && strings.EqualFold(*d.Contact.Email, to) {
		greeting = "Dear " + d.Contact.Name + ","
		toName = d.Contact.Name
	}
	paras := []string{greeting}
	if personal != "" {
		paras = append(paras, personal)
	}
	paras = append(paras,
		"Thank you for the opportunity. Please find attached our quotation "+d.Number+" for "+d.Title+".",
		"To go ahead, reply to this email or send us your order quoting our reference. If anything needs changing, just let us know and we'll revise it.")
	content := mailer.Content{
		Preheader:  "Quotation " + d.Number + " · " + d.Currency + " " + grouped(d.Total) + " · valid until " + longDate(d.ValidUntil),
		Heading:    "Quotation " + d.Number,
		Paragraphs: paras,
		Details: [][2]string{
			{"For", d.Title},
			{"Total (incl. VAT)", d.Currency + " " + grouped(d.Total)},
			{"Valid until", longDate(d.ValidUntil)},
			{"Your contact", actor.Name},
		},
		Footnote: "The quotation is attached as a PDF. " + s.company.Name + ".",
	}
	html, text := mailer.Render(content)
	return mailer.Message{To: to, ToName: toName, Kind: "quotation",
		Subject: "Quotation " + d.Number + " — " + d.Title + " | " + s.company.Name, HTML: html, Text: text}
}

func sortEvents(evs []audit.Event) {
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].OccurredAt.Equal(evs[j].OccurredAt) {
			return evs[i].ID > evs[j].ID
		}
		return evs[i].OccurredAt.After(evs[j].OccurredAt)
	})
}
