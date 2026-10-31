package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Tools for the family's library account (Bibliofil "Min side") and the national search Biblioteksøk.
One login covers the whole family folder: use list_family to see who is in it, and pass "person" (a first name, "meg", or a card number) to act for someone. Leave "person" empty to cover everyone.
Pickup places come from BIBLIOFIL_PICKUP / BIBSOK_PICKUP (else the card's default); list_pickup_locations shows valid codes.
To get a book: search_catalog first. If the local library has it, use reserve. If not, search_national, check national_holdings, then order_interlibrary (orders are always for the logged-in card holder).
list_reservations shows both local reservations and interlibrary orders. Dates are dd.mm.yyyy.
Always confirm with the user before reserve, order_interlibrary, cancel_reservation or renew_loans.`

func ptr[T any](v T) *T { return &v }

var (
	readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	changes  = &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	removes  = &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)}
)

type personIn struct {
	Person string `json:"person,omitempty" jsonschema:"family member: first name, 'meg' or card number; empty means everyone"`
}

type familyOut struct {
	Family []Person `json:"family"`
}
type loansOut struct {
	Count int    `json:"count"`
	Loans []Loan `json:"loans"`
}
type reservationsOut struct {
	Count        int           `json:"count"`
	Reservations []Reservation `json:"reservations"`
}

type renewIn struct {
	IDs    []string `json:"ids,omitempty" jsonschema:"loan ids from list_loans (e.g. 668773/13)"`
	All    bool     `json:"all,omitempty" jsonschema:"renew every renewable loan instead of the listed ids"`
	Person string   `json:"person,omitempty" jsonschema:"with all: only this family member's loans"`
}
type renewOut struct {
	Results []RenewResult `json:"results"`
}

type cancelIn struct {
	ID string `json:"id" jsonschema:"reservation id from list_reservations (res:... or ill:...)"`
}
type cancelOut struct {
	Cancelled Reservation `json:"cancelled"`
}

type searchIn struct {
	Query         string `json:"query" jsonschema:"title, author, series or ISBN"`
	AvailableOnly bool   `json:"available_only,omitempty" jsonschema:"only titles with a free copy somewhere in the library"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max hits to return (default 15)"`
}
type searchOut struct {
	TotalHits int          `json:"total_hits"`
	Hits      []CatalogHit `json:"hits"`
}

type reserveIn struct {
	TitleID string `json:"title_id" jsonschema:"title_id from search_catalog"`
	Person  string `json:"person,omitempty" jsonschema:"who the reservation is for; empty means the logged-in card holder"`
	Pickup  string `json:"pickup,omitempty" jsonschema:"pickup branch code or name, see list_pickup_locations; default from BIBLIOFIL_PICKUP, else the card's default"`
}
type reserveOut struct {
	Result  *ReserveResult `json:"result,omitempty"`
	Pickups []PickupOption `json:"pickup_options,omitempty"`
}

type historyIn struct {
	Query string `json:"query,omitempty" jsonschema:"filter on title, author or series"`
	Year  int    `json:"year,omitempty" jsonschema:"only loans from this year"`
	Limit int    `json:"limit,omitempty" jsonschema:"max items to return (default 50)"`
}
type historyOut struct {
	Matches int           `json:"matches"`
	Items   []HistoryItem `json:"items"`
}

type nationalSearchIn struct {
	Query string `json:"query" jsonschema:"title, author or ISBN"`
	Limit int    `json:"limit,omitempty" jsonschema:"max hits to return (default 15)"`
}
type nationalSearchOut struct {
	TotalHits int           `json:"total_hits"`
	Hits      []NationalHit `json:"hits"`
}

type holdingsIn struct {
	TitleID       string `json:"title_id" jsonschema:"title_id from search_national"`
	AvailableOnly bool   `json:"available_only,omitempty" jsonschema:"only libraries with a free copy that can be lent out to other libraries"`
}

type pickupsIn struct {
	Kind   string `json:"kind,omitempty" jsonschema:"'local' for reserve (default) or 'national' for order_interlibrary"`
	Filter string `json:"filter,omitempty" jsonschema:"national only: part of a library name, e.g. a town"`
}
type pickupsOut struct {
	Kind      string         `json:"kind"`
	EnvVar    string         `json:"env_var"`
	EnvValue  string         `json:"env_value,omitempty"`
	Locations []PickupOption `json:"locations"`
}

type orderIn struct {
	TitleID string `json:"title_id" jsonschema:"title_id from search_national"`
	Pickup  string `json:"pickup,omitempty" jsonschema:"pickup library number or name, see list_pickup_locations; default from BIBSOK_PICKUP, else the card's home library"`
}

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

func need(v, name string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func newServer(a *app) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "bibliofil", Title: "Bibliotek", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	s.AddReceivingMiddleware(traceMCP)

	mcp.AddTool(s, &mcp.Tool{Name: "list_family", Description: "List the people in the family folder (name and card number). The logged-in card holder has self=true.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, familyOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, familyOut{}, err
			}
			_, _ = p, b
			f, err := p.Family(ctx)
			return nil, familyOut{Family: f}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_loans", Description: "List current loans with due date, days left, who borrowed it and whether it can be renewed.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in personIn) (*mcp.CallToolResult, loansOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, loansOut{}, err
			}
			_, _ = p, b
			l, err := p.Loans(ctx, in.Person)
			return nil, loansOut{Count: len(l), Loans: l}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_reservations", Description: "List reservations in the local library and interlibrary orders (made via Biblioteksøk), with queue position, pickup place and whether it is ready for pickup.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in personIn) (*mcp.CallToolResult, reservationsOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, reservationsOut{}, err
			}
			_, _ = p, b
			r, err := p.Reservations(ctx, in.Person)
			return nil, reservationsOut{Count: len(r), Reservations: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "search_catalog", Description: "Search the local library's catalog. Shows availability per branch and if you already have it on loan.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, searchOut{}, err
			}
			_, _ = p, b
			if err := need(in.Query, "query"); err != nil {
				return nil, searchOut{}, err
			}
			n, h, err := p.Search(ctx, in.Query, in.AvailableOnly, limitOr(in.Limit, 15))
			return nil, searchOut{TotalHits: n, Hits: h}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "loan_history", Description: "Search the logged-in card holder's loan history (what has been borrowed before).", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, historyOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, historyOut{}, err
			}
			_, _ = p, b
			n, items, err := p.History(ctx, in.Query, in.Year, limitOr(in.Limit, 50))
			return nil, historyOut{Matches: n, Items: items}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "search_national", Description: "Search all Norwegian libraries via Biblioteksøk. Use when the local library does not have a title.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in nationalSearchIn) (*mcp.CallToolResult, nationalSearchOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, nationalSearchOut{}, err
			}
			_, _ = p, b
			if err := need(in.Query, "query"); err != nil {
				return nil, nationalSearchOut{}, err
			}
			n, h, err := b.Search(ctx, in.Query, limitOr(in.Limit, 15))
			return nil, nationalSearchOut{TotalHits: n, Hits: h}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "national_holdings", Description: "Show which libraries in Norway own a title (from search_national) and how many copies are free.", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in holdingsIn) (*mcp.CallToolResult, TitleHoldings, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, TitleHoldings{}, err
			}
			_, _ = p, b
			if err := need(in.TitleID, "title_id"); err != nil {
				return nil, TitleHoldings{}, err
			}
			h, err := b.Holdings(ctx, in.TitleID, in.AvailableOnly)
			if err != nil {
				return nil, TitleHoldings{}, err
			}
			return nil, *h, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_pickup_locations", Description: "List valid pickup locations and their codes, which default the card uses, and which one is set in the environment (BIBLIOFIL_PICKUP for reserve, BIBSOK_PICKUP for order_interlibrary).", Annotations: readOnly},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pickupsIn) (*mcp.CallToolResult, pickupsOut, error) {
			p, b, err := a.clients()
			if err != nil {
				return nil, pickupsOut{}, err
			}
			_, _ = p, b
			if strings.EqualFold(in.Kind, "national") {
				l, err := b.PickupLocations(ctx, in.Filter)
				return nil, pickupsOut{Kind: "national", EnvVar: "BIBSOK_PICKUP", EnvValue: b.pickup, Locations: l}, err
			}
			l, err := p.PickupLocations(ctx)
			return nil, pickupsOut{Kind: "local", EnvVar: "BIBLIOFIL_PICKUP", EnvValue: p.pickup, Locations: l}, err
		})

	a.writeTools = []string{"renew_loans", "cancel_reservation", "reserve", "order_interlibrary"}
	a.addWrite = func() {
		mcp.AddTool(s, &mcp.Tool{Name: "renew_loans", Description: "Renew loans, by id from list_loans or all renewable loans.", Annotations: changes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in renewIn) (*mcp.CallToolResult, renewOut, error) {
				p, b, err := a.clients()
				if err != nil {
					return nil, renewOut{}, err
				}
				_, _ = p, b
				if !in.All && len(in.IDs) == 0 {
					return nil, renewOut{}, fmt.Errorf("give ids or set all=true")
				}
				r, err := p.Renew(ctx, in.IDs, in.All, in.Person)
				return nil, renewOut{Results: r}, err
			})

		mcp.AddTool(s, &mcp.Tool{Name: "cancel_reservation", Description: "Cancel a reservation or interlibrary order by id from list_reservations.", Annotations: removes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, cancelOut, error) {
				p, b, err := a.clients()
				if err != nil {
					return nil, cancelOut{}, err
				}
				_, _ = p, b
				if err := need(in.ID, "id"); err != nil {
					return nil, cancelOut{}, err
				}
				r, err := p.CancelReservation(ctx, in.ID)
				if err != nil {
					return nil, cancelOut{}, err
				}
				return nil, cancelOut{Cancelled: *r}, nil
			})

		mcp.AddTool(s, &mcp.Tool{Name: "reserve", Description: "Reserve a title in the local library for a family member, picked up at a branch (default from BIBLIOFIL_PICKUP). Returns reserved=true once it shows up in the reservation list.", Annotations: changes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in reserveIn) (*mcp.CallToolResult, reserveOut, error) {
				p, b, err := a.clients()
				if err != nil {
					return nil, reserveOut{}, err
				}
				_, _ = p, b
				if err := need(in.TitleID, "title_id"); err != nil {
					return nil, reserveOut{}, err
				}
				r, opts, err := p.Reserve(ctx, in.TitleID, in.Person, in.Pickup)
				if err != nil && len(opts) > 0 {
					return nil, reserveOut{Pickups: opts}, fmt.Errorf("%w; valid pickup options are listed in pickup_options", err)
				}
				return nil, reserveOut{Result: r}, err
			})

		mcp.AddTool(s, &mcp.Tool{Name: "order_interlibrary", Description: "Order a title from another library via Biblioteksøk, delivered to the pickup library (default from BIBSOK_PICKUP). Always for the logged-in card holder.", Annotations: changes},
			func(ctx context.Context, _ *mcp.CallToolRequest, in orderIn) (*mcp.CallToolResult, OrderResult, error) {
				p, b, err := a.clients()
				if err != nil {
					return nil, OrderResult{}, err
				}
				_, _ = p, b
				if err := need(in.TitleID, "title_id"); err != nil {
					return nil, OrderResult{}, err
				}
				r, err := b.Order(ctx, in.TitleID, in.Pickup)
				if err != nil {
					return nil, OrderResult{}, err
				}
				return nil, *r, nil
			})

	}

	return s
}
