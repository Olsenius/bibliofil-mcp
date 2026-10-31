//go:build live

package main

// Read-only checks against a real library. Nothing is reserved, ordered,
// renewed or cancelled. Run with ./run-live-test.sh.
import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func dump(t *testing.T, name string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("%s:\n%s", name, b)
}

func TestLiveReadOnly(t *testing.T) {
	ctx := context.Background()
	user, pin, base := os.Getenv("BIBLIOFIL_USER"), os.Getenv("BIBLIOFIL_PIN"), os.Getenv("BIBLIOFIL_URL")
	if user == "" || pin == "" || base == "" {
		t.Skip("BIBLIOFIL_URL, BIBLIOFIL_USER and BIBLIOFIL_PIN must be set")
	}
	query := env("LIVE_TEST_QUERY", "norge")
	p := newPortal(base, user, pin, os.Getenv("BIBLIOFIL_PICKUP"))
	b := newBibsok(env("BIBSOK_URL", "https://bibsok.no"), user, pin, os.Getenv("BIBSOK_PICKUP"))

	fam, err := p.Family(ctx)
	if err != nil || len(fam) == 0 {
		t.Fatalf("family: %v %v", fam, err)
	}
	t.Logf("family members: %d", len(fam))

	loans, err := p.Loans(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("loans: %d", len(loans))
	for i, f := range fam {
		mine, err := p.Loans(ctx, f.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("  member %d: %d loans", i+1, len(mine))
	}

	res, err := p.Reservations(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reservations: %d", len(res))

	n, hits, err := p.Search(ctx, query, false, 3)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v %v", hits, err)
	}
	t.Logf("search %q total=%d", query, n)
	dump(t, "search", hits)

	hn, _, err := p.History(ctx, "", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("history items: %d", hn)

	pl, err := p.PickupLocations(ctx)
	if err != nil || len(pl) == 0 {
		t.Fatalf("local pickups: %v %v", pl, err)
	}
	dump(t, "local pickups", pl)

	nn, nh, err := b.Search(ctx, query, 3)
	if err != nil || len(nh) == 0 {
		t.Fatalf("national: %v %v", nh, err)
	}
	t.Logf("national %q total=%d", query, nn)
	dump(t, "national", nh)

	h, err := b.Holdings(ctx, nh[0].TitleID, true)
	if err != nil || h.Libraries == 0 {
		t.Fatalf("holdings: %+v %v", h, err)
	}
	t.Logf("holdings: %s libraries=%d available=%d", h.Title, h.Libraries, len(h.Holdings))

	np, err := b.PickupLocations(ctx, "")
	if err != nil || len(np) == 0 {
		t.Fatalf("national pickups: %v %v", np, err)
	}
	for _, o := range np {
		if o.CardDefault || o.Configured {
			dump(t, "national pickup default/configured", o)
		}
	}
	t.Logf("national pickups: %d", len(np))
}
