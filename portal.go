package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// portal is the library's own Bibliofil "Min side" (m2), e.g.
// https://<library host>/cgi-bin/m2. One login covers the whole
// family folder (familiemappe).
type portal struct {
	s        *session
	pickupMu sync.Mutex
	pickup   string // default pickup branch code; empty = the card's default
}

const m2 = "/cgi-bin/m2"

func newPortal(base, user, pin, pickup string) *portal {
	form := func(user, pin string) url.Values {
		return url.Values{
			"mode": {"dologin"}, "neste": {"vislaan"}, "lnr": {user}, "passord": {pin},
			"hskmg": {"1"}, "sourceid": {"m2"},
		}
	}
	return &portal{
		s:      newSession(base, m2+"?mode=login", m2, form, user, pin),
		pickup: pickup,
	}
}

// ---------- shared parsing helpers ----------

var spaceRe = regexp.MustCompile(`[\s\x{00a0}]+`)

func clean(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

// textOf returns the visible text of a node, without hidden forms/dialogs.
func textOf(sel *goquery.Selection) string {
	c := sel.Clone()
	c.Find("form, script, style, .is-hidden, [style*='display:none']").Remove()
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			parts = append(parts, n.Data)
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	for _, n := range c.Nodes {
		walk(n)
	}
	return clean(strings.Join(parts, " "))
}

// pageTitle returns the first part of <title>, e.g. "Some title" from
// "Some title - detaljert visning - Biblioteksøk".
func pageTitle(doc *goquery.Document) string {
	t := clean(doc.Find("title").First().Text())
	if i := strings.Index(t, " - "); i > 0 {
		t = t[:i]
	}
	return t
}

func first(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return clean(m[1])
	}
	return ""
}

var tnrRe = regexp.MustCompile(`tnr=(\d+)`)

func tnrOf(sel *goquery.Selection) string {
	href, _ := sel.Attr("href")
	return first(tnrRe, href)
}

func queryParam(href, key string) string {
	if i := strings.IndexByte(href, '?'); i >= 0 {
		href = href[i+1:]
	}
	href = strings.ReplaceAll(href, "&amp;", "&")
	v, err := url.ParseQuery(href)
	if err != nil {
		return ""
	}
	return v.Get(key)
}

// nameMatches reports whether a person query ("kari", "meg", a card
// number) matches a displayed name. Short names like "Kari Nordmann"
// match full names like "Kari Nordmann Hansen" via the first name.
func nameMatches(query, name string) bool {
	q := strings.ToLower(clean(query))
	n := strings.ToLower(clean(name))
	if q == "" || n == "" {
		return false
	}
	if strings.Contains(n, q) || strings.Contains(q, n) {
		return true
	}
	qf, nf := strings.Fields(q), strings.Fields(n)
	return len(qf) > 0 && len(nf) > 0 && qf[0] == nf[0]
}

// ---------- family ----------

type Person struct {
	Name       string `json:"name"`
	CardNumber string `json:"card_number"`
	Self       bool   `json:"self"`
}

func parseFamily(doc *goquery.Document) []Person {
	var out []Person
	doc.Find("select#familielanekort option").Each(func(_ int, o *goquery.Selection) {
		name, ok := o.Attr("data-navn")
		self := !ok
		if self {
			name = strings.TrimSuffix(clean(o.Text()), "(meg)")
		}
		card, _ := o.Attr("value")
		out = append(out, Person{Name: clean(name), CardNumber: card, Self: self})
	})
	return out
}

func (p *portal) loansPage(ctx context.Context) (*goquery.Document, error) {
	return p.s.page(ctx, m2, url.Values{"mode": {"vislaan"}})
}

func (p *portal) Family(ctx context.Context) ([]Person, error) {
	doc, err := p.loansPage(ctx)
	if err != nil {
		return nil, err
	}
	fam := parseFamily(doc)
	if len(fam) == 0 {
		return nil, errors.New("could not find the family list on the page")
	}
	return fam, nil
}

// resolvePerson turns "kari", "meg" or a card number into a family
// member. Empty input means everyone (nil, nil).
func resolvePerson(fam []Person, query string) (*Person, error) {
	q := strings.ToLower(clean(query))
	if q == "" || q == "alle" || q == "all" {
		return nil, nil
	}
	for i, f := range fam {
		if (f.Self && (q == "meg" || q == "me" || q == "self")) || strings.EqualFold(f.CardNumber, q) || nameMatches(q, f.Name) {
			return &fam[i], nil
		}
	}
	var names []string
	for _, f := range fam {
		names = append(names, f.Name)
	}
	return nil, fmt.Errorf("no family member matches %q (known: %s)", query, strings.Join(names, ", "))
}

// ---------- loans ----------

type Loan struct {
	ID                 string `json:"id"`
	TitleID            string `json:"title_id"`
	Title              string `json:"title"`
	Author             string `json:"author,omitempty"`
	MaterialType       string `json:"material_type,omitempty"`
	Copy               string `json:"copy,omitempty"`
	DueDate            string `json:"due_date"`
	DaysLeft           *int   `json:"days_left,omitempty"`
	LoanedAt           string `json:"loaned_at,omitempty"`
	Borrower           string `json:"borrower"`
	Renewable          bool   `json:"renewable"`
	NotRenewableReason string `json:"not_renewable_reason,omitempty"`

	renewRef string
	forLnr   string
}

var (
	loanMetaRe = regexp.MustCompile(`^\s*av\s+(.*?)\s*\(([^,()]+),\s*eksemplar:\s*([^)]*?)\s*\)`)
	dueRe      = regexp.MustCompile(`Forfaller:\s*(\d\d\.\d\d\.\d{4})`)
	loanedRe   = regexp.MustCompile(`Lånt:\s*(\d\d\.\d\d\.\d{4}(?:,\s*kl\s*\d\d:\d\d)?)`)
	borrowerRe = regexp.MustCompile(`Lånt:[^·]*?\bav\s+(.+?)(?:\s*Forleng lånet|\s*Kan ikke forlenges|$)`)
	noRenewRe  = regexp.MustCompile(`Kan ikke forlenges\*?\s*(.*)$`)
	osloTZ, _  = time.LoadLocation("Europe/Oslo")
	dateLayout = "02.01.2006"
)

func daysUntil(d string) *int {
	t, err := time.ParseInLocation(dateLayout, d, osloTZ)
	if err != nil {
		return nil
	}
	now := time.Now().In(osloTZ)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, osloTZ)
	n := int(t.Sub(today).Hours() / 24)
	return &n
}

func parseLoans(doc *goquery.Document, self string) []Loan {
	var out []Loan
	doc.Find("div.c-historikkpost").Each(func(_ int, post *goquery.Selection) {
		if post.Find("span[data-name^='forfaller_']").Length() == 0 {
			return
		}
		content := post.Find(".c-historikkpost__content")
		titleA := content.Find("a").First()
		l := Loan{TitleID: tnrOf(titleA), Title: clean(titleA.Text())}
		text := textOf(content)
		rest := strings.TrimPrefix(text, l.Title)
		if m := loanMetaRe.FindStringSubmatch(rest); m != nil {
			l.Author, l.MaterialType, l.Copy = clean(m[1]), clean(m[2]), clean(m[3])
		}
		l.DueDate = first(dueRe, text)
		l.DaysLeft = daysUntil(l.DueDate)
		l.LoanedAt = first(loanedRe, text)
		l.Borrower = first(borrowerRe, text)
		if l.Borrower == "" {
			l.Borrower = self
		}
		if a := content.Find("a[href*='mode=forny&']"); a.Length() > 0 {
			href, _ := a.Attr("href")
			l.renewRef = queryParam(href, "fornyRef")
			l.forLnr = queryParam(href, "forlnr")
			l.Renewable = l.renewRef != ""
		} else {
			l.NotRenewableReason = first(noRenewRe, text)
			if l.NotRenewableReason == "" && strings.Contains(text, "Kan ikke forlenges") {
				l.NotRenewableReason = "Kan ikke forlenges"
			}
		}
		l.ID = l.TitleID + "/" + l.Copy
		out = append(out, l)
	})
	return out
}

func selfName(fam []Person) string {
	for _, f := range fam {
		if f.Self {
			return f.Name
		}
	}
	return ""
}

func (p *portal) Loans(ctx context.Context, person string) ([]Loan, error) {
	doc, err := p.loansPage(ctx)
	if err != nil {
		return nil, err
	}
	fam := parseFamily(doc)
	who, err := resolvePerson(fam, person)
	if err != nil {
		return nil, err
	}
	loans := parseLoans(doc, selfName(fam))
	if who == nil {
		return loans, nil
	}
	var out []Loan
	for _, l := range loans {
		if nameMatches(l.Borrower, who.Name) {
			out = append(out, l)
		}
	}
	return out, nil
}

type RenewResult struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Borrower   string `json:"borrower"`
	Renewed    bool   `json:"renewed"`
	NewDueDate string `json:"new_due_date,omitempty"`
	Message    string `json:"message,omitempty"`
}

type renewResponse struct {
	Status  any    `json:"status"`
	Forfall string `json:"forfall"`
	Message string `json:"message"`
	Melding string `json:"melding"`
	Fornyet []struct {
		Forfall string `json:"forfall"`
	} `json:"fornyet"`
	IkkeFornyet []struct {
		Message string `json:"message"`
	} `json:"ikkefornyet"`
}

func truthy(v any) bool {
	switch t := v.(type) {
	case float64:
		return t != 0
	case string:
		return t != "" && t != "0"
	case bool:
		return t
	}
	return false
}

// Renew renews the given loans (ids from Loans). With all set it renews
// every renewable loan, optionally only for one person.
func (p *portal) Renew(ctx context.Context, ids []string, all bool, person string) ([]RenewResult, error) {
	loans, err := p.Loans(ctx, person)
	if err != nil {
		return nil, err
	}
	var pick []Loan
	if all {
		pick = loans
	} else {
		byID := map[string]Loan{}
		for _, l := range loans {
			byID[l.ID] = l
			byID[l.TitleID] = l
		}
		for _, id := range ids {
			l, ok := byID[strings.TrimSpace(id)]
			if !ok {
				return nil, fmt.Errorf("no current loan with id %q; call list_loans first", id)
			}
			pick = append(pick, l)
		}
	}
	var out []RenewResult
	for _, l := range pick {
		r := RenewResult{ID: l.ID, Title: l.Title, Borrower: l.Borrower}
		if !l.Renewable {
			r.Message = "kan ikke forlenges"
			if l.NotRenewableReason != "" {
				r.Message = l.NotRenewableReason
			}
			out = append(out, r)
			continue
		}
		q := url.Values{"idx": {l.renewRef}, "instans": {"m2"}}
		if l.forLnr != "" && l.forLnr != "0" {
			q.Set("forenlnr", l.forLnr)
		}
		var resp renewResponse
		if err := p.s.jsonCall(ctx, http.MethodGet, "/cgi-bin/rest_service/web_lnr_forny/1.0/data/", q, true, &resp); err != nil {
			r.Message = err.Error()
			out = append(out, r)
			continue
		}
		r.Renewed = truthy(resp.Status)
		if r.Renewed {
			r.NewDueDate = strings.ReplaceAll(resp.Forfall, "/", ".")
			if r.NewDueDate == "" && len(resp.Fornyet) > 0 {
				r.NewDueDate = strings.ReplaceAll(resp.Fornyet[0].Forfall, "/", ".")
			}
		} else {
			switch {
			case len(resp.IkkeFornyet) > 0 && resp.IkkeFornyet[0].Message != "":
				r.Message = resp.IkkeFornyet[0].Message
			case resp.Message != "":
				r.Message = resp.Message
			case resp.Melding != "":
				r.Message = resp.Melding
			default:
				r.Message = "lånet ble ikke forlenget"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------- reservations ----------

type Reservation struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"` // "local" (this library) or "interlibrary" (from another library)
	TitleID        string `json:"title_id"`
	Title          string `json:"title"`
	Author         string `json:"author,omitempty"`
	MaterialType   string `json:"material_type,omitempty"`
	Owner          string `json:"owner"`
	Queue          string `json:"queue,omitempty"`
	Pickup         string `json:"pickup,omitempty"`
	ReadyForPickup bool   `json:"ready_for_pickup"`
	OrderedAt      string `json:"ordered_at,omitempty"`
	FromLibrary    string `json:"from_library,omitempty"`
	Details        string `json:"details"`

	resRef string
	resLnr string
	famLnr string
}

var (
	resMetaRe   = regexp.MustCompile(`^\s*av\s+(.*?)\s*\(([^()]+)\)`)
	queueRe     = regexp.MustCompile(`(Nr\.\s*\d+[^()]*(?:\([^)]*\))?)`)
	pickupRe    = regexp.MustCompile(`hente ved:\s*(.+?)(?:\s+Avbestill|$)`)
	readyRe     = regexp.MustCompile(`(?i)kan hentes|klar til henting|ligger klar|hentes innen|venter på deg`)
	flRe        = regexp.MustCompile(`^(.*?)\s+av\s+(.*?)\s*\(([^()]*)\)\s*\(Bestilt:\s*([\d.]+)\s*Fra:\s*(.*?)\)`)
	famLnrRe    = regexp.MustCompile(`famlnr\s*:\s*'(\d+)'`)
	dividerCnt  = regexp.MustCompile(`\s*\(\d+\)\s*:?\s*$`)
	ownInternal = regexp.MustCompile(`mode=fornyAlle&(?:amp;)?lnr=(\d+)`)
)

func trimDetails(s string) string {
	for _, junk := range []string{" Endre", " Avbestill"} {
		s = strings.ReplaceAll(s, junk, "")
	}
	return clean(s)
}

func parseReservations(doc *goquery.Document, self string) []Reservation {
	var out []Reservation
	owners := map[string]string{}
	doc.Find("li[data-role='list-divider'] a[data-toggles^='alleres_']").Each(func(_ int, a *goquery.Selection) {
		id, _ := a.Attr("data-toggles")
		name := dividerCnt.ReplaceAllString(clean(a.Text()), "")
		if strings.HasPrefix(strings.ToLower(name), "mine reserveringer") {
			name = self
		} else {
			name = strings.TrimSuffix(strings.TrimSuffix(name, " sine reserveringer"), "s reserveringer")
		}
		owners[id] = name
	})
	doc.Find("div.c-historikkpost[id^='resLi']").Each(func(_ int, post *goquery.Selection) {
		content := post.Find(".c-historikkpost__content")
		titleA := content.Find("a").First()
		r := Reservation{Kind: "local", TitleID: tnrOf(titleA), Title: clean(titleA.Text()), Owner: self}
		if box := post.ParentsFiltered("div[id^='alleres_']").First(); box.Length() > 0 {
			id, _ := box.Attr("id")
			if o := owners[id]; o != "" {
				r.Owner = o
			}
		}
		text := textOf(content)
		rest := strings.TrimPrefix(text, r.Title)
		if m := resMetaRe.FindStringSubmatch(rest); m != nil {
			r.Author, r.MaterialType = clean(m[1]), clean(m[2])
		}
		r.Queue = first(queueRe, text)
		r.Pickup = first(pickupRe, text)
		r.ReadyForPickup = readyRe.MatchString(text)
		r.Details = trimDetails(text)
		cancel := post.Find("a[href*='mode=slettres']")
		href, _ := cancel.Attr("href")
		r.resRef = queryParam(href, "resRef")
		r.resLnr = queryParam(href, "lnr")
		onclick, _ := cancel.Attr("onclick")
		r.famLnr = first(famLnrRe, onclick)
		r.ID = "res:" + r.resRef
		out = append(out, r)
	})
	doc.Find("div.c-historikkpost[id^='fl']").Each(func(_ int, post *goquery.Selection) {
		id, _ := post.Attr("id")
		tnr := strings.TrimPrefix(id, "fl")
		text := textOf(post.Find(".c-historikkpost__content"))
		r := Reservation{Kind: "interlibrary", ID: "ill:" + tnr, TitleID: tnr, Owner: self, Details: trimDetails(text)}
		if m := flRe.FindStringSubmatch(text); m != nil {
			r.Title, r.Author, r.MaterialType, r.OrderedAt, r.FromLibrary = clean(m[1]), clean(m[2]), clean(m[3]), m[4], clean(m[5])
		} else {
			r.Title = text
		}
		r.ReadyForPickup = readyRe.MatchString(text)
		out = append(out, r)
	})
	return out
}

func (p *portal) Reservations(ctx context.Context, person string) ([]Reservation, error) {
	doc, err := p.loansPage(ctx)
	if err != nil {
		return nil, err
	}
	fam := parseFamily(doc)
	who, err := resolvePerson(fam, person)
	if err != nil {
		return nil, err
	}
	all := parseReservations(doc, selfName(fam))
	if who == nil {
		return all, nil
	}
	var out []Reservation
	for _, r := range all {
		if nameMatches(r.Owner, who.Name) {
			out = append(out, r)
		}
	}
	return out, nil
}

type statusResponse struct {
	Status  any    `json:"status"`
	Melding string `json:"melding"`
	Message string `json:"message"`
}

func (p *portal) CancelReservation(ctx context.Context, id string) (*Reservation, error) {
	doc, err := p.loansPage(ctx)
	if err != nil {
		return nil, err
	}
	fam := parseFamily(doc)
	var target *Reservation
	for _, r := range parseReservations(doc, selfName(fam)) {
		if r.ID == id {
			r := r
			target = &r
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("no reservation with id %q; call list_reservations first", id)
	}
	var resp statusResponse
	switch target.Kind {
	case "local":
		vals := url.Values{"resref": {target.resRef}, "mode": {"avbestill"}}
		if target.famLnr != "" {
			vals.Set("forenlnr", target.famLnr)
		}
		err = p.s.jsonCall(ctx, http.MethodPost, "/cgi-bin/rest_service/web_lnr_avbestill/1.0/data", vals, true, &resp)
	default:
		lnr := first(ownInternal, docHTML(doc))
		vals := url.Values{"tnr": {target.TitleID}, "lnr": {lnr}, "mode": {"avbestillfl"}, "sourceid": {"m"}, "json": {"1"}}
		err = p.s.jsonCall(ctx, http.MethodGet, m2, vals, true, &resp)
	}
	if err != nil {
		return nil, err
	}
	ok := truthy(resp.Status) || fmt.Sprint(resp.Status) == "OK"
	if !ok {
		msg := resp.Melding
		if msg == "" {
			msg = resp.Message
		}
		return nil, fmt.Errorf("the library refused to cancel: %s", msg)
	}
	return target, nil
}

func docHTML(doc *goquery.Document) string {
	h, _ := doc.Html()
	return h
}

// ---------- catalog search ----------

type CatalogHit struct {
	TitleID      string `json:"title_id"`
	Title        string `json:"title"`
	Author       string `json:"author,omitempty"`
	Series       string `json:"series,omitempty"`
	Info         string `json:"info,omitempty"`
	Availability string `json:"availability,omitempty"`
	OnLoanByYou  string `json:"on_loan_by_you,omitempty"`
}

var (
	hitsRe  = regexp.MustCompile(`(\d+)\+?[\s\x{00a0}]+treff`)
	availRe = regexp.MustCompile(`((?:Ikke ledig|\d+\s+ledig|Ledig).*)$`)
	freeRe  = regexp.MustCompile(`(?:^|\s)(?:\d+\s+ledig|Ledig)`)
)

func (p *portal) Search(ctx context.Context, query string, availableOnly bool, limit int) (int, []CatalogHit, error) {
	vals := url.Values{"mode": {"vt"}, "pubsok_txt_0": {query}, "nyttsok": {"1"}}
	doc, err := p.s.page(ctx, m2, vals)
	if err != nil {
		return 0, nil, err
	}
	total := 0
	fmt.Sscan(first(hitsRe, textOf(doc.Find("h2"))), &total)
	var out []CatalogHit
	doc.Find("div.c-post[id^='vtpost_']").EachWithBreak(func(_ int, post *goquery.Selection) bool {
		id, _ := post.Attr("id")
		titleA := post.Find("a.c-tittellenke").First()
		h := CatalogHit{TitleID: strings.TrimPrefix(id, "vtpost_")}
		h.Title, _ = titleA.Attr("title")
		if h.Title == "" {
			h.Title = clean(titleA.Text())
		}
		content := post.Find(".c-post--simple__content")
		h.Author = clean(content.Find("span.o-adaptive-title a").First().Text())
		h.Series = clean(content.Find("a[href*='visserie']").First().Text())
		var info []string
		content.Find("span.bs-type").Each(func(_ int, s *goquery.Selection) { info = append(info, clean(s.Text())) })
		h.Info = strings.Join(info, ", ")
		text := textOf(content)
		if i := strings.Index(text, "Bestill..."); i >= 0 {
			text = text[:i]
		}
		if i := strings.Index(text, "Du låner denne nå"); i >= 0 {
			h.OnLoanByYou = clean(text[i:])
			text = text[:i]
		}
		h.Availability = first(availRe, text)
		if availableOnly && !freeRe.MatchString(h.Availability) {
			return true
		}
		out = append(out, h)
		return limit <= 0 || len(out) < limit
	})
	return total, out, nil
}

// ---------- reserve ----------

type PickupOption struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	CardDefault bool   `json:"card_default,omitempty"`
	Configured  bool   `json:"configured_default,omitempty"`
}

// PickupLocations lists the branches a reservation can be picked up at,
// read from the reservation form of an arbitrary title (nothing is sent).
func (p *portal) PickupLocations(ctx context.Context) ([]PickupOption, error) {
	_, hits, err := p.Search(ctx, "norge", false, 1)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, errors.New("could not find a title to read the pickup list from")
	}
	doc, err := p.s.page(ctx, m2, url.Values{"mode": {"p"}, "tnr": {hits[0].TitleID}, "bestill": {"1"}})
	if err != nil {
		return nil, err
	}
	var out []PickupOption
	doc.Find("form#resForm select[name='henteavdeling'] option").Each(func(_ int, o *goquery.Selection) {
		opt := PickupOption{Code: o.AttrOr("value", ""), Name: clean(o.Text())}
		_, opt.CardDefault = o.Attr("selected")
		opt.Configured = p.defaultPickup() != "" && strings.EqualFold(opt.Code, p.defaultPickup())
		out = append(out, opt)
	})
	if len(out) == 0 {
		return nil, errors.New("no pickup list found on the reservation form")
	}
	return out, nil
}

type ReserveResult struct {
	TitleID  string `json:"title_id"`
	Title    string `json:"title"`
	For      string `json:"for"`
	Pickup   string `json:"pickup"`
	Reserved bool   `json:"reserved"`
	Message  string `json:"message,omitempty"`
}

// formValues serializes a form the way a browser would (without the
// submit button).
func formValues(form *goquery.Selection) url.Values {
	v := url.Values{}
	form.Find("input, select, textarea").Each(func(_ int, f *goquery.Selection) {
		name, ok := f.Attr("name")
		if !ok || name == "" {
			return
		}
		if _, disabled := f.Attr("disabled"); disabled {
			return
		}
		switch goquery.NodeName(f) {
		case "select":
			opt := f.Find("option[selected]").First()
			if opt.Length() == 0 {
				opt = f.Find("option").First()
			}
			val, ok := opt.Attr("value")
			if !ok {
				val = clean(opt.Text())
			}
			v.Add(name, val)
		case "textarea":
			v.Add(name, f.Text())
		default:
			typ := strings.ToLower(f.AttrOr("type", "text"))
			switch typ {
			case "submit", "button", "image", "reset", "file":
				return
			case "radio", "checkbox":
				if _, checked := f.Attr("checked"); !checked {
					return
				}
				v.Add(name, f.AttrOr("value", "on"))
			default:
				v.Add(name, f.AttrOr("value", ""))
			}
		}
	})
	return v
}

func labelFor(form *goquery.Selection, id string) string {
	return clean(form.Find("label[for='" + id + "']").Text())
}

func (p *portal) Reserve(ctx context.Context, titleID, person, pickup string) (*ReserveResult, []PickupOption, error) {
	doc, err := p.s.page(ctx, m2, url.Values{"mode": {"p"}, "tnr": {titleID}, "bestill": {"1"}})
	if err != nil {
		return nil, nil, err
	}
	form := doc.Find("form#resForm").First()
	title := pageTitle(doc)
	if form.Length() == 0 {
		msg := textOf(doc.Find(".c-box, .c-feilet, .c-melding").First())
		if msg == "" {
			msg = "the title page has no reservation form (already on loan or reserved, or not reservable)"
		}
		return nil, nil, errors.New(msg)
	}
	vals := formValues(form)
	vals.Del("husksomhjemmeavd")

	res := &ReserveResult{TitleID: titleID, Title: title}

	// Who: radio buttons "bestillfor" (empty value = me).
	res.For = labelFor(form, "meg")
	if p := strings.ToLower(clean(person)); p != "" && p != "meg" && p != "me" {
		found := false
		form.Find("input[name='bestillfor']").Each(func(_ int, r *goquery.Selection) {
			id := r.AttrOr("id", "")
			label := strings.TrimSuffix(labelFor(form, id), "(meg)")
			if !found && nameMatches(p, label) {
				vals.Set("bestillfor", r.AttrOr("value", ""))
				res.For = clean(label)
				found = true
			}
		})
		if !found {
			return nil, nil, fmt.Errorf("no family member matches %q on the reservation form", person)
		}
	}
	res.For = strings.TrimSpace(strings.TrimSuffix(res.For, "(meg)"))

	// Where: select "henteavdeling".
	var opts []PickupOption
	form.Find("select[name='henteavdeling'] option").Each(func(_ int, o *goquery.Selection) {
		opts = append(opts, PickupOption{Code: o.AttrOr("value", ""), Name: clean(o.Text())})
	})
	if pickup == "" {
		pickup = p.defaultPickup()
	}
	if pickup != "" && len(opts) > 0 {
		var chosen *PickupOption
		for i, o := range opts {
			if strings.EqualFold(o.Code, pickup) || strings.Contains(strings.ToLower(o.Name), strings.ToLower(pickup)) {
				chosen = &opts[i]
				break
			}
		}
		if chosen == nil {
			return nil, opts, fmt.Errorf("unknown pickup location %q", pickup)
		}
		vals.Set("henteavdeling", chosen.Code)
		res.Pickup = chosen.Name
	} else {
		for _, o := range opts {
			if o.Code == vals.Get("henteavdeling") {
				res.Pickup = o.Name
			}
		}
	}

	body, err := p.s.request(ctx, http.MethodGet, m2, vals)
	if err != nil {
		return nil, opts, err
	}
	rdoc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err == nil {
		res.Message = textOf(rdoc.Find(".c-box, .c-feilet, .c-melding, .bs-melding").First())
	}

	// Confirm by looking for the title among the reservations.
	list, err := p.Reservations(ctx, "")
	if err != nil {
		return res, opts, nil
	}
	for _, r := range list {
		if r.TitleID == titleID && nameMatches(res.For, r.Owner) {
			res.Reserved = true
			if res.Title == "" {
				res.Title = r.Title
			}
			if r.Pickup != "" {
				res.Pickup = r.Pickup
			}
		}
	}
	return res, opts, nil
}

// ---------- history ----------

type HistoryItem struct {
	LoanedAt string `json:"loaned_at"`
	TitleID  string `json:"title_id"`
	Title    string `json:"title"`
	Author   string `json:"author,omitempty"`
	Series   string `json:"series,omitempty"`
	Info     string `json:"info,omitempty"`
}

var histDateRe = regexp.MustCompile(`Lånt:\s*(\d\d\.\d\d\.\d{4})`)

func (p *portal) History(ctx context.Context, query string, year int, limit int) (int, []HistoryItem, error) {
	doc, err := p.s.page(ctx, m2, url.Values{"mode": {"historikk"}, "histvisalle": {"1"}})
	if err != nil {
		return 0, nil, err
	}
	q := strings.ToLower(clean(query))
	var out []HistoryItem
	total := 0
	doc.Find("div.c-historikkpost").Each(func(_ int, post *goquery.Selection) {
		content := post.Find(".c-historikkpost__content")
		titleA := content.Find("a.c-tittellenke").First()
		if titleA.Length() == 0 {
			return
		}
		h := HistoryItem{TitleID: tnrOf(titleA), Title: titleA.AttrOr("title", clean(titleA.Text()))}
		h.LoanedAt = first(histDateRe, content.Text())
		meta := content.Find(".u-font--10").First()
		who := meta.Find("span.u-inlineblock").First()
		h.Author = who.Find("span[title]").First().AttrOr("title", clean(who.Text()))
		if c := meta.Clone(); c.Length() > 0 {
			c.Find("span, a, br").Remove()
			h.Series = clean(c.Text())
		}
		var info []string
		meta.Find("span.bs-type").Each(func(_ int, s *goquery.Selection) {
			info = append(info, clean(s.Text()))
			s.NextAll().Filter("span").Each(func(_ int, n *goquery.Selection) {
				if t := clean(n.Text()); t != "" && t != "·" {
					info = append(info, t)
				}
			})
		})
		h.Info = strings.Join(info, " · ")
		if year > 0 && !strings.HasSuffix(h.LoanedAt, fmt.Sprint(year)) {
			return
		}
		if q != "" && !strings.Contains(strings.ToLower(h.Title+" "+h.Author+" "+h.Series), q) {
			return
		}
		total++
		if limit <= 0 || len(out) < limit {
			out = append(out, h)
		}
	})
	return total, out, nil
}

func (p *portal) setPickup(v string) {
	p.pickupMu.Lock()
	p.pickup = v
	p.pickupMu.Unlock()
}

func (p *portal) defaultPickup() string {
	p.pickupMu.Lock()
	defer p.pickupMu.Unlock()
	return p.pickup
}
