package tmdb

import "testing"

func yr(y int) *int { return &y }

// TestPickBest covers the year-aware ranking that replaced "take results[0]":
// title relation is required, and the year disambiguates among title matches —
// so an obscure film beats a more-popular namesake, and a pure year coincidence
// with no title relation never matches.
func TestPickBest(t *testing.T) {
	cases := []struct {
		name  string
		cands []searchCandidate
		title string
		year  *int
		want  int64 // 0 => expect not-found
	}{
		{
			name: "obscure beats popular namesake by year (Spring)",
			// TMDB returns the popular one first.
			cands: []searchCandidate{{122081, "Spring Breakers", 2013}, {593048, "Spring", 2019}},
			title: "Spring", year: yr(2019), want: 593048,
		},
		{
			name:  "obscure beats popular namesake by year (Hero)",
			cands: []searchCandidate{{34672, "Chestnut: Hero of Central Park", 2004}, {615324, "HERO", 2018}},
			title: "Hero", year: yr(2018), want: 615324,
		},
		{
			name:  "substring title match with year (Popeye prefix)",
			cands: []searchCandidate{{174266, "A Haul in One", 1956}},
			title: "Popeye - A Haul in One", year: yr(1956), want: 174266,
		},
		{
			name:  "punctuation-insensitive exact match (Caminandes)",
			cands: []searchCandidate{{253777, "Caminandes: Llama Drama", 2013}},
			title: "Caminandes Llama Drama", year: yr(2013), want: 253777,
		},
		{
			name:  "same-year popular film with no title relation is NOT matched",
			cands: []searchCandidate{{999, "Some Blockbuster", 2008}},
			title: "50 Years of NASA Part 1", year: yr(2008), want: 0,
		},
		{
			name:  "two exact titles: year picks the right one",
			cands: []searchCandidate{{1, "Spring", 2014}, {2, "Spring", 2019}},
			title: "Spring", year: yr(2019), want: 2,
		},
		{
			name:  "year off by one still matches (Duck and Cover)",
			cands: []searchCandidate{{52230, "Duck and Cover", 1952}},
			title: "Duck and Cover", year: yr(1951), want: 52230,
		},
		{
			name:  "no candidates",
			cands: nil,
			title: "Anything", year: yr(2000), want: 0,
		},
		{
			name:  "short substring token does NOT match a popular namesake (Up -> Growing Up)",
			cands: []searchCandidate{{1, "Growing Up", 1985}},
			title: "Up", year: yr(2009), want: 0, // overlap "up" < 4 chars => rejected
		},
		{
			name:  "substring with mismatched year is rejected (precision over wrong match)",
			cands: []searchCandidate{{1, "A Haul in One", 1990}},
			title: "Popeye - A Haul in One", year: yr(1956), want: 0, // year off by 34
		},
		{
			name:  "substring with no year is rejected (needs year corroboration)",
			cands: []searchCandidate{{174266, "A Haul in One", 1956}},
			title: "A Haul in One Restored", year: nil, want: 0,
		},
	}
	for _, c := range cases {
		got, ok := pickBest(c.cands, c.title, c.year)
		if c.want == 0 {
			if ok {
				t.Errorf("%s: expected not-found, got id %d", c.name, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: got (%d,%v), want %d", c.name, got, ok, c.want)
		}
	}
}

// TestMergeCandidates: the year-filtered page's extras are appended after the
// plain-query page, de-duplicated by id, preserving popularity order.
func TestMergeCandidates(t *testing.T) {
	a := []searchCandidate{{1, "a", 2000}, {2, "b", 2001}}
	b := []searchCandidate{{2, "b", 2001}, {3, "c", 2002}} // id 2 overlaps
	got := mergeCandidates(a, b)
	if len(got) != 3 {
		t.Fatalf("merge len = %d, want 3 (%v)", len(got), got)
	}
	want := []int64{1, 2, 3}
	for i, c := range got {
		if c.ID != want[i] {
			t.Errorf("merge[%d].ID = %d, want %d", i, c.ID, want[i])
		}
	}
	if got := mergeCandidates(a, nil); len(got) != 2 {
		t.Errorf("merge with empty b = %d, want 2", len(got))
	}
}

func TestNormTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Caminandes: Llama Drama", "caminandesllamadrama"},
		{"HERO", "hero"},
		{"Agent 327: Operation Barbershop", "agent327operationbarbershop"},
		{"  Spring  ", "spring"},
	}
	for _, c := range cases {
		if got := normTitle(c.in); got != c.want {
			t.Errorf("normTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
