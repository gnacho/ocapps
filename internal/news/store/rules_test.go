package store

import (
	"testing"
)

// TestFeedRulesFilter: a feed block rule discards existing items when
// re-applied, and they are unfrozen when it is deleted.
func TestFeedRulesFilter(t *testing.T) {
	st, uid := newTestStore(t)
	items := []NewItem{
		{GUID: "a", GUIDHash: "h1", Title: "Buy summer deal"},
		{GUID: "b", GUIDHash: "h2", Title: "Normal story"},
		{GUID: "c", GUIDHash: "h3", Title: "Flash deal"},
	}
	f, err := st.CreateFeed(uid, "https://r.example/f", nil, "t", "https://r.example", "", items)
	if err != nil {
		t.Fatal(err)
	}
	feedID := f.ID

	if err := st.SaveFeedRules(feedID, Rules{Block: `EntryTitle=(?i)deal`}); err != nil {
		t.Fatal(err)
	}
	marked, err := st.ReapplyFeedFilter(feedID, FeedFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if marked != 2 {
		t.Fatalf("reapply: expected 2 flagged, I have %d", marked)
	}
	got, err := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: feedID, GetRead: true, BatchSize: -1, IncludeFiltered: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		if x.GUID == "b" && x.Filtered {
			t.Error("item 'Normal story' should not be filtered")
		}
		if x.GUID != "b" && !x.Filtered {
			t.Errorf("item %q should be filtered", x.GUID)
		}
	}

	if err := st.DeleteFeedRules(feedID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReapplyFeedFilter(feedID, FeedFilter{}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: feedID, GetRead: true, BatchSize: -1, IncludeFiltered: true})
	for _, x := range got {
		if x.Filtered {
			t.Errorf("item %q should be unfrozen", x.GUID)
		}
	}
}

// TestGlobalRulesFilter: a global keep rule discards what does not match.
func TestGlobalRulesFilter(t *testing.T) {
	st, uid := newTestStore(t)
	items := []NewItem{
		{GUID: "a", GUIDHash: "h1", Title: "Python 3.13 release"},
		{GUID: "b", GUIDHash: "h2", Title: "Paella recipe"},
	}
	f, err := st.CreateFeed(uid, "https://r.example/f", nil, "t", "https://r.example", "", items)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGlobalRules(uid, Rules{Keep: `EntryTitle=(?i)python`}); err != nil {
		t.Fatal(err)
	}
	marked, err := st.ReapplyGlobalRules(uid)
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("global keep: expected 1 flagged, I have %d", marked)
	}
	got, err := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: f.ID, GetRead: true, BatchSize: -1, IncludeFiltered: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range got {
		if x.GUID == "a" && x.Filtered {
			t.Error("python should not be filtered")
		}
		if x.GUID == "b" && !x.Filtered {
			t.Error("recipe should be filtered (does not match python)")
		}
	}
}

// TestApplyFilterToItemsWithRules: the ingestion flags new items according to rules.
func TestApplyFilterToItemsWithRules(t *testing.T) {
	st, uid := newTestStore(t)
	f, err := st.CreateFeed(uid, "https://r.example/f", nil, "t", "https://r.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFeedRules(f.ID, Rules{Block: `EntryTitle=spam`}); err != nil {
		t.Fatal(err)
	}
	incoming := []NewItem{
		{GUID: "x", GUIDHash: "hx", Title: "spam entries"},
		{GUID: "y", GUIDHash: "hy", Title: "story"},
	}
	if err := st.ApplyFilterToItems(f.ID, incoming); err != nil {
		t.Fatal(err)
	}
	if !incoming[0].Filtered {
		t.Error("spam item should remain filtered in the ingestion")
	}
	if incoming[1].Filtered {
		t.Error("normal item should not remain filtered")
	}
}
