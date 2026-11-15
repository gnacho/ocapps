package rules

import "testing"

func TestParseAndBlock(t *testing.T) {
	rs, err := Parse(
		"EntryTitle=(?i)miniflux\nEntryURL=utm_\nEntryAuthor=(?i)spam",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Ignore(Fields{Title: "New Miniflux release"}) {
		t.Error("title with miniflux not ignored")
	}
	if !rs.Ignore(Fields{URL: "https://x.com/a?utm_source=rss"}) {
		t.Error("URL with utm_ not ignored")
	}
	if !rs.Ignore(Fields{Author: "Spam Bot"}) {
		t.Error("spam author not ignored")
	}
	if rs.Ignore(Fields{Title: "Normal news"}) {
		t.Error("normal article ignored for no reason")
	}
}

func TestKeepRules(t *testing.T) {
	rs, err := Parse("", "EntryTitle=(?i)odoo\nEntryTitle=(?i)python")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Ignore(Fields{Title: "Assorted news"}) {
		t.Error("keep: article outside the rules not ignored")
	}
	if rs.Ignore(Fields{Title: "Odoo 17 released"}) {
		t.Error("keep: article matching odoo ignored")
	}
	if rs.Ignore(Fields{Title: "python 3.13"}) {
		t.Error("keep: article matching python ignored")
	}
}

func TestBlockPlusKeep(t *testing.T) {
	// block first: it matches block even if it does not match keep
	rs, err := Parse("EntryTitle=(?i)ads", "EntryTitle=(?i)odoo")
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Ignore(Fields{Title: "Ads for Odoo"}) {
		t.Error("block should ignore even if keep matches")
	}
	if rs.Ignore(Fields{Title: "Odoo news"}) {
		t.Error("keep: odoo not ignored")
	}
	if !rs.Ignore(Fields{Title: "Random"}) {
		t.Error("keep: random should be ignored")
	}
}

func TestDatePatterns(t *testing.T) {
	rs, err := Parse("EntryDate=future", "")
	if err != nil {
		t.Fatal(err)
	}
	future := int64(4102444800) // 2100-01-01
	if !rs.Ignore(Fields{PubDate: future}) {
		t.Error("future date not ignored")
	}
	if rs.Ignore(Fields{PubDate: 1700000000}) {
		t.Error("past date ignored")
	}

	rs2, err := Parse("EntryDate=before:2024-01-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if !rs2.Ignore(Fields{PubDate: 1600000000}) { // 2020
		t.Error("2020 article not ignored by before:2024")
	}
	if rs2.Ignore(Fields{PubDate: 1735689600}) { // 2025-01-01
		t.Error("2025 article ignored by before:2024")
	}

	rs3, err := Parse("EntryDate=max-age:7d", "")
	if err != nil {
		t.Fatal(err)
	}
	if !rs3.Ignore(Fields{PubDate: 1700000000}) { // 2023, very old
		t.Error("old article not ignored by max-age:7d")
	}
}

func TestInvalidRules(t *testing.T) {
	if _, err := Parse("EntryTitle=(", ""); err == nil {
		t.Error("invalid regex should fail")
	}
	if _, err := Parse("EntryFoo=bar", ""); err == nil {
		t.Error("unknown field should fail")
	}
	if _, err := Parse("withoutEquals", ""); err == nil {
		t.Error("line without '=' should fail")
	}
	if _, err := Parse("EntryDate=max-age:xyz", ""); err == nil {
		t.Error("invalid max-age should fail")
	}
	if _, err := Parse("EntryDate=before:31-12-2023", ""); err == nil {
		t.Error("date with a wrong format should fail")
	}
}

func TestCommentsAndBlanks(t *testing.T) {
	rs, err := Parse("# comentario\n\nEntryTitle=(?i)x\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Ignore(Fields{Title: "abc"}) {
		t.Error("without a match it must not ignore")
	}
	if !rs.Ignore(Fields{Title: "X marks"}) {
		t.Error("match x not ignored")
	}
}
