package store

import "testing"

// TestAutoReadMarksNewItemsRead: new items whose title matches an
// auto-mark rule are stored as read (unread=0), the rest are not.
func TestAutoReadMarksNewItemsRead(t *testing.T) {
	st, uid := newTestStore(t)
	f, err := st.CreateFeed(uid, "https://r.example/f", nil, "t", "https://r.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAutoRead(uid, f.ID, `(?i)urgent`); err != nil {
		t.Fatal(err)
	}
	incoming := []NewItem{
		{GUID: "a", GUIDHash: "ha", Title: "URGENT: security update"},
		{GUID: "b", GUIDHash: "hb", Title: "normal story"},
	}
	if _, err := st.ReplaceFeedItems(f.ID, uid, "t", "https://r.example", incoming, false); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: f.ID, GetRead: true, BatchSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	byGUID := map[string]bool{}
	for _, x := range got {
		byGUID[x.GUID] = x.Unread
	}
	if byGUID["a"] {
		t.Error("item 'URGENT' should be marked as read by auto-read")
	}
	if !byGUID["b"] {
		t.Error("item 'normal story' should not be marked as read")
	}
}

// TestAutoReadRuleScoped: a rule with a specific feed_id does not apply to another feed.
func TestAutoReadRuleScoped(t *testing.T) {
	st, uid := newTestStore(t)
	f1, _ := st.CreateFeed(uid, "https://r1.example/f", nil, "t1", "https://r1.example", "", nil)
	f2, _ := st.CreateFeed(uid, "https://r2.example/f", nil, "t2", "https://r2.example", "", nil)
	if _, err := st.AddAutoRead(uid, f1.ID, `(?i)oferta`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceFeedItems(f2.ID, uid, "t2", "https://r2.example",
		[]NewItem{{GUID: "x", GUIDHash: "hx", Title: "Oferta en r2"}}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: f2.ID, GetRead: true, BatchSize: -1})
	if len(got) != 1 || !got[0].Unread {
		t.Error("the f1 rule should not mark f2 items")
	}
}
