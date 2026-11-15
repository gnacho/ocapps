package store

import "testing"

// TestClusterAggregation: items from different feeds with the same cluster_key
// get grouped and the primary is the one with the highest id (#42).
func TestClusterAggregation(t *testing.T) {
	st, uid := newTestStore(t)
	f1, err := st.CreateFeed(uid, "https://r1.example/f", nil, "t1", "https://r1.example", "",
		[]NewItem{{GUID: "a", GUIDHash: "ha", Title: "The same story", Body: "<p>body</p>", ClusterKey: "key1"}})
	if err != nil {
		t.Fatal(err)
	}
	f2, err := st.CreateFeed(uid, "https://r2.example/f", nil, "t2", "https://r2.example", "",
		[]NewItem{{GUID: "b", GUIDHash: "hb", Title: "the same story", Body: "<p>body</p>", ClusterKey: "key1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFeed(uid, "https://r3.example/f", nil, "t3", "https://r3.example", "",
		[]NewItem{{GUID: "c", GUIDHash: "hc", Title: "Another story", Body: "<p>x</p>", ClusterKey: "key2"}}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Clusters(uid, []string{"key1", "key2"})
	if err != nil {
		t.Fatal(err)
	}
	c1, ok := got["key1"]
	if !ok {
		t.Fatalf("cluster key1 not found: %+v", got)
	}
	if c1.Size != 2 {
		t.Errorf("key1 expected size 2, I have %d", c1.Size)
	}
	// primary = the item with the highest id (the f2 one, inserted later)
	itemsF1, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: f1.ID, GetRead: true, BatchSize: -1})
	itemsF2, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: f2.ID, GetRead: true, BatchSize: -1})
	if c1.PrimaryID != itemsF2[0].ID {
		t.Errorf("primary should be the f2 one (%d), I have %d", itemsF2[0].ID, c1.PrimaryID)
	}
	_ = itemsF1
	if _, ok := got["key2"]; ok {
		t.Errorf("key2 (1 item) should not show up as a cluster")
	}
}
