package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
)

// bibsok is the national search (bibsok.no), used to order books from
// other libraries. Orders are always for the logged-in card.
type bibsok struct {
	s        *session
	pickupMu sync.Mutex
	pickup   string // default pickup library number; empty = the card's home library
}

func newBibsok(base, user, pin, pickup string) *bibsok {
	form := func(user, pin string) url.Values {
		return url.Values{"mode": {"dologin"}, "neste": {"home"}, "lnr": {user}, "passord": {pin}, "hskmg": {"1"}}
	}
	return &bibsok{s: newSession(base, "/?mode=login", "/", form, user, pin), pickup: pickup}
}

type NationalHit struct {
	TitleID string `json:"title_id"`
	Title   string `json:"title"`
	Author  string `json:"author,omitempty"`
	Info    string `json:"info,omitempty"`
}

func (b *bibsok) Search(ctx context.Context, query string, limit int) (int, []NationalHit, error) {
	doc, err := b.s.page(ctx, "/", url.Values{"mode": {"vt"}, "pubsok_txt_0": {query}, "nyttsok": {"1"}})
	if err != nil {
		return 0, nil, err
	}
	total := 0
	fmt.Sscan(first(hitsRe, doc.Find(".js-treff").Text()), &total)
	var out []NationalHit
	doc.Find("div.c-post[id^='post_']").EachWithBreak(func(_ int, post *goquery.Selection) bool {
		id, _ := post.Attr("id")
		titleA := post.Find("a.o-adaptive-title").First()
		h := NationalHit{TitleID: strings.TrimPrefix(id, "post_"), Title: titleA.AttrOr("title", clean(titleA.Text()))}
		meta := post.Find(".u-font--10").First()
		who := meta.Find("span.u-inlineblock").First()
		h.Author = who.Find("span[title]").First().AttrOr("title", clean(who.Text()))
		c := meta.Clone()
		c.Find("span.u-inlineblock").Remove()
		h.Info = strings.Trim(clean(c.Text()), "· ")
		out = append(out, h)
		return limit <= 0 || len(out) < limit
	})
	return total, out, nil
}

type Holding struct {
	Library      string `json:"library"`
	Available    int    `json:"available"`
	Total        int    `json:"total"`
	Interlibrary bool   `json:"interlibrary_loan_possible"`
}

type TitleHoldings struct {
	TitleID   string    `json:"title_id"`
	Title     string    `json:"title"`
	Libraries int       `json:"libraries"`
	Holdings  []Holding `json:"holdings"`
}

var holdingRe = regexp.MustCompile(`^(.+?)\s+har\s+(\d+)\s+av\s+(\d+)\s+ledig`)

// Holdings lists which libraries own a title and how many copies are free.
func (b *bibsok) Holdings(ctx context.Context, titleID string, onlyAvailable bool) (*TitleHoldings, error) {
	doc, err := b.s.page(ctx, "/", url.Values{"tnr": {titleID}})
	if err != nil {
		return nil, err
	}
	res := &TitleHoldings{TitleID: titleID, Title: pageTitle(doc)}
	// Each holding is a block of text: "<library> har N av M ledig", maybe
	// followed by "OBS! Ingen ledig for fjernlån".
	var lines []string
	doc.Find("li, p, div").Each(func(_ int, s *goquery.Selection) {
		if s.Children().Filter("li, p, div").Length() > 0 {
			return
		}
		if t := clean(s.Text()); t != "" {
			lines = append(lines, t)
		}
	})
	for i, l := range lines {
		m := holdingRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		h := Holding{Library: clean(m[1]), Interlibrary: true}
		h.Available, _ = strconv.Atoi(m[2])
		h.Total, _ = strconv.Atoi(m[3])
		if strings.Contains(l, "Ingen ledig for fjernlån") || (i+1 < len(lines) && strings.Contains(lines[i+1], "Ingen ledig for fjernlån")) {
			h.Interlibrary = false
		}
		res.Libraries++
		if onlyAvailable && (h.Available == 0 || !h.Interlibrary) {
			continue
		}
		res.Holdings = append(res.Holdings, h)
	}
	return res, nil
}

// PickupLocations lists the libraries Biblioteksøk can deliver to, read
// from the order form of an arbitrary title. filter matches the name.
func (b *bibsok) PickupLocations(ctx context.Context, filter string) ([]PickupOption, error) {
	_, hits, err := b.Search(ctx, "norge", 1)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, errors.New("could not find a title to read the pickup list from")
	}
	doc, err := b.s.page(ctx, "/", url.Values{"mode": {"bestill"}, "tnr": {hits[0].TitleID}})
	if err != nil {
		return nil, err
	}
	form := doc.Find("form#resform").First()
	f := strings.ToLower(clean(filter))
	var out []PickupOption
	form.Find("input[name='hentebib']").Each(func(_ int, r *goquery.Selection) {
		o := PickupOption{Code: r.AttrOr("value", ""), Name: labelFor(form, r.AttrOr("id", ""))}
		_, o.CardDefault = r.Attr("checked")
		o.Configured = b.defaultPickup() != "" && o.Code == b.defaultPickup()
		if f == "" || strings.Contains(strings.ToLower(o.Name), f) || o.Code == f {
			out = append(out, o)
		}
	})
	return out, nil
}

type OrderResult struct {
	TitleID string `json:"title_id"`
	Title   string `json:"title"`
	Pickup  string `json:"pickup"`
	Lender  string `json:"lending_library,omitempty"`
	Ordered bool   `json:"ordered"`
	Message string `json:"message,omitempty"`
}

type lenderCheck struct {
	Titler []struct {
		Valgt      int     `json:"valgt"`
		ValgtListe [][]any `json:"valgtliste"`
	} `json:"titler"`
	Error string `json:"error"`
}

// lender asks Biblioteksøk which library would lend the title to the
// pickup library, the same check the order page runs before it enables
// the order button. It returns "" when nobody can lend it right now.
func (b *bibsok) lender(ctx context.Context, titleID, pickup string) (string, error) {
	var c lenderCheck
	q := url.Values{"mode": {"oppdaterbestskjema"}, "hentebib": {pickup}, "tnr": {titleID}, "erbib": {"0"}, "id": {""}}
	if err := b.s.jsonCall(ctx, http.MethodGet, "/cgi-bin/bibsokdyn.htmc", q, true, &c); err != nil {
		return "", err
	}
	if c.Error != "" {
		return "", fmt.Errorf("availability check failed: %s", c.Error)
	}
	if len(c.Titler) == 0 || c.Titler[0].Valgt <= 0 {
		return "", nil
	}
	for _, l := range c.Titler[0].ValgtListe {
		if nr, ok := l[0].(float64); ok && len(l) > 2 && int(nr) == c.Titler[0].Valgt {
			return fmt.Sprint(l[2]), nil
		}
	}
	return strconv.Itoa(c.Titler[0].Valgt), nil
}

type orderResponse struct {
	RedirURL string `json:"redirurl"`
	Melding  string `json:"melding"`
	Status   any    `json:"status"`
}

// Order orders a title from another library, delivered to pickup (a
// library number or part of its name). Empty pickup uses the default.
func (b *bibsok) Order(ctx context.Context, titleID, pickup string) (*OrderResult, error) {
	doc, err := b.s.page(ctx, "/", url.Values{"mode": {"bestill"}, "tnr": {titleID}})
	if err != nil {
		return nil, err
	}
	form := doc.Find("form#resform").First()
	if form.Length() == 0 {
		msg := textOf(doc.Find("main").First())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("no order form for this title: %s", msg)
	}
	vals := formValues(form)
	vals.Del("sokbib")

	if pickup == "" {
		pickup = b.defaultPickup()
	}
	res := &OrderResult{TitleID: titleID}
	if pickup != "" {
		var chosen, label string
		p := strings.ToLower(pickup)
		form.Find("input[name='hentebib']").EachWithBreak(func(_ int, r *goquery.Selection) bool {
			v := r.AttrOr("value", "")
			l := labelFor(form, r.AttrOr("id", ""))
			if v == pickup || (chosen == "" && strings.Contains(strings.ToLower(l), p)) {
				chosen, label = v, l
				return v != pickup
			}
			return true
		})
		if chosen == "" {
			return nil, fmt.Errorf("unknown pickup library %q", pickup)
		}
		vals.Set("hentebib", chosen)
		res.Pickup = label
	} else if r := form.Find("input[name='hentebib'][checked]").First(); r.Length() > 0 {
		res.Pickup = labelFor(form, r.AttrOr("id", ""))
	}
	if res.Title = pageTitle(doc); res.Title == "" || strings.HasPrefix(res.Title, "Biblioteksøk") {
		res.Title = titleID
	}

	lender, err := b.lender(ctx, titleID, vals.Get("hentebib"))
	if err != nil {
		return nil, err
	}
	if lender == "" {
		return nil, errors.New("no library has a free copy to lend right now, so Biblioteksøk cannot order it (try again later, or check national_holdings)")
	}
	res.Lender = lender

	var resp orderResponse
	if err := b.s.jsonCall(ctx, http.MethodPost, "/", vals, false, &resp); err != nil {
		return nil, fmt.Errorf("order may or may not have been placed, check list_reservations: %w", err)
	}
	if resp.RedirURL == "" {
		msg := resp.Melding
		if msg == "" {
			msg = "the library did not confirm the order"
		}
		return nil, errors.New(msg)
	}
	res.Ordered = true
	if rdoc, err := b.s.page(ctx, resp.RedirURL, nil); err == nil {
		msg := textOf(rdoc.Find("main").First())
		if len(msg) > 400 {
			msg = msg[:400]
		}
		res.Message = msg
	}
	return res, nil
}

func (b *bibsok) setPickup(v string) {
	b.pickupMu.Lock()
	b.pickup = v
	b.pickupMu.Unlock()
}

func (b *bibsok) defaultPickup() string {
	b.pickupMu.Lock()
	defer b.pickupMu.Unlock()
	return b.pickup
}
