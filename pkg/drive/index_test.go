package drive

import (
	"path/filepath"
	"sort"
	"testing"
)

// tree:
//   A/            (folder fA, parent = drive root)
//   A/B/          (folder fB, parent fA)
//   A/f1.mp4      (file i1, parent fA, 100)
//   A/B/f2.mp4    (file i2, parent fB, 200)
//   A/f3.mp4      (file i3, parent fA, 300, trashed)
func seedIndex(t *testing.T, driveID, rootID string) *Index {
	t.Helper()
	idx, err := OpenIndex(filepath.Join(t.TempDir(), "idx.sqlite"), driveID, rootID)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	items := []Item{
		{ID: "fA", Name: "A", Parent: driveID, IsFolder: true},
		{ID: "fB", Name: "B", Parent: "fA", IsFolder: true},
		{ID: "i1", Name: "f1.mp4", Parent: "fA", Size: 100},
		{ID: "i2", Name: "f2.mp4", Parent: "fB", Size: 200},
		{ID: "i3", Name: "f3.mp4", Parent: "fA", Size: 300, Trashed: true},
	}
	if err := idx.Upsert(items); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return idx
}

func TestIndexCountAndLookup(t *testing.T) {
	idx := seedIndex(t, "drv", "drv")

	if n, err := idx.Count(); err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}

	it, ok, err := idx.LookupPath("A/f1.mp4")
	if err != nil || !ok || it.ID != "i1" || it.Size != 100 {
		t.Fatalf("LookupPath(A/f1.mp4) = %+v ok=%v err=%v", it, ok, err)
	}
	if it, ok, _ := idx.LookupPath("A/B/f2.mp4"); !ok || it.ID != "i2" {
		t.Fatalf("LookupPath(A/B/f2.mp4) = %+v ok=%v", it, ok)
	}
	if _, ok, _ := idx.LookupPath("A/missing.mp4"); ok {
		t.Fatal("LookupPath(A/missing.mp4) unexpectedly found")
	}
}

func TestIndexAllFilesWholeDrive(t *testing.T) {
	idx := seedIndex(t, "drv", "drv")

	got := relPaths(idx, t)
	want := []string{"A/B/f2.mp4", "A/f1.mp4"} // trashed f3 excluded
	if !equal(got, want) {
		t.Fatalf("AllFiles = %v; want %v", got, want)
	}
}

func TestIndexAllFilesScoped(t *testing.T) {
	// scope the source to folder A: paths are relative to A, not the drive root.
	idx := seedIndex(t, "drv", "fA")

	got := relPaths(idx, t)
	want := []string{"B/f2.mp4", "f1.mp4"}
	if !equal(got, want) {
		t.Fatalf("scoped AllFiles = %v; want %v", got, want)
	}
}

func TestIndexDelete(t *testing.T) {
	idx := seedIndex(t, "drv", "drv")
	if err := idx.Delete("i1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n, _ := idx.Count(); n != 1 {
		t.Fatalf("Count after delete = %d; want 1", n)
	}
	if _, ok, _ := idx.LookupPath("A/f1.mp4"); ok {
		t.Fatal("deleted file still resolvable")
	}
}

func TestIndexChildrenAndListed(t *testing.T) {
	idx := seedIndex(t, "drv", "drv")

	kids, err := idx.Children("fA")
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(kids) != 2 { // fB, i1 (trashed i3 excluded)
		t.Fatalf("Children(fA) = %d; want 2", len(kids))
	}

	if listed, _ := idx.IsListed("fA"); listed {
		t.Fatal("fA listed before MarkAllFoldersListed")
	}
	if err := idx.MarkAllFoldersListed(); err != nil {
		t.Fatalf("MarkAllFoldersListed: %v", err)
	}
	for _, id := range []string{"fA", "fB", "drv"} {
		if listed, _ := idx.IsListed(id); !listed {
			t.Fatalf("%s not listed after MarkAllFoldersListed", id)
		}
	}
}

func TestIndexPageTokenAndFlags(t *testing.T) {
	idx := seedIndex(t, "drv", "drv")

	if !idx.IsWholeDrive() || idx.RootID() != "drv" {
		t.Fatalf("whole-drive flags wrong: whole=%v root=%q", idx.IsWholeDrive(), idx.RootID())
	}
	if err := idx.SetPageToken("tok123"); err != nil {
		t.Fatalf("SetPageToken: %v", err)
	}
	if tok, _ := idx.PageToken(); tok != "tok123" {
		t.Fatalf("PageToken = %q; want tok123", tok)
	}
	if done, _ := idx.FullIndexDone(); done {
		t.Fatal("FullIndexDone true before set")
	}
	if err := idx.SetFullIndexDone(); err != nil {
		t.Fatalf("SetFullIndexDone: %v", err)
	}
	if done, _ := idx.FullIndexDone(); !done {
		t.Fatal("FullIndexDone false after set")
	}
}

func relPaths(idx *Index, t *testing.T) []string {
	t.Helper()
	files, err := idx.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	var out []string
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
