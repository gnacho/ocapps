package store

import "testing"

// TestNestedFolders: create subfolders, list them with parentId and check that the
// parent folder scope includes the feeds of its subfolders (#41).
func TestNestedFolders(t *testing.T) {
	st, uid := newTestStore(t)
	parent, err := st.CreateFolder(uid, "Padre", nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateFolder(uid, "Hijo", &parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFeed(uid, "https://r.example/f", &child.ID, "t", "https://r.example", "",
		[]NewItem{{GUID: "a", GUIDHash: "ha", Title: "x"}}); err != nil {
		t.Fatal(err)
	}

	// list folders: parentId present
	folders, err := st.ListFolders(uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("expected 2 folders: %+v", folders)
	}
	for _, f := range folders {
		if f.ID == child.ID && (f.ParentID == nil || *f.ParentID != parent.ID) {
			t.Errorf("child without the right parentId: %+v", f)
		}
		if f.ID == parent.ID && f.ParentID != nil {
			t.Errorf("parent with parentId: %+v", f)
		}
	}

	// the parent folder scope includes the feeds of the subfolder
	got, err := st.ListItems(ItemFilter{UserID: uid, Type: 1, ID: parent.ID, GetRead: true, BatchSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the parent folder should include 1 item from the subfolder, I have %d", len(got))
	}
}

// TestDeleteParentFolderPromotesChildren: deleting a parent folder moves its
// subfolders up to the root.
func TestDeleteParentFolderPromotesChildren(t *testing.T) {
	st, uid := newTestStore(t)
	parent, err := st.CreateFolder(uid, "Padre", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFolder(uid, "Hijo", &parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteFolder(uid, parent.ID); err != nil {
		t.Fatal(err)
	}
	folders, _ := st.ListFolders(uid)
	if len(folders) != 1 {
		t.Fatalf("after deleting the parent 1 subfolder should remain, I have %d", len(folders))
	}
	if folders[0].ParentID != nil {
		t.Errorf("the subfolder should be at the root: %+v", folders[0])
	}
}
