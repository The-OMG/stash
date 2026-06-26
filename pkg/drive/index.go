package drive

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

// Index is a persistent (SQLite) mirror of a shared drive's file tree. It maps
// Drive's id-based model onto the path-based model stash expects, and stores
// the change page token so scans can resume incrementally.
type Index struct {
	db      *sqlx.DB
	driveID string
	rootID  string // tree root: the scoped folder id, or the drive id for a whole-drive source
}

const indexSchema = `
CREATE TABLE IF NOT EXISTS items (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    parent      TEXT NOT NULL,
    size        INTEGER NOT NULL,
    mime        TEXT NOT NULL,
    modtime     INTEGER NOT NULL,
    md5         TEXT NOT NULL,
    is_folder   INTEGER NOT NULL,
    trashed     INTEGER NOT NULL,
    width       INTEGER NOT NULL DEFAULT 0,
    height      INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    has_thumb   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent);
CREATE INDEX IF NOT EXISTS idx_items_parent_name ON items(parent, name);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS listed_folders (
    id TEXT PRIMARY KEY
);
`

// OpenIndex opens (creating if needed) the index database at dbPath for the
// given shared drive. rootID is the tree root the source is scoped to (a folder
// id); pass "" to scope to the whole drive (driveID).
func OpenIndex(dbPath, driveID, rootID string) (*Index, error) {
	db, err := sqlx.Connect("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(indexSchema); err != nil {
		db.Close()
		return nil, err
	}
	// Idempotently add columns for indexes created by older versions.
	for _, col := range []string{
		"ALTER TABLE items ADD COLUMN width INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE items ADD COLUMN height INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE items ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE items ADD COLUMN has_thumb INTEGER NOT NULL DEFAULT 0",
	} {
		db.Exec(col) // ignore "duplicate column" on existing indexes
	}
	if rootID == "" {
		rootID = driveID
	}
	idx := &Index{db: db, driveID: driveID, rootID: rootID}
	if err := idx.SetMeta("drive_id", driveID); err != nil {
		db.Close()
		return nil, err
	}
	return idx, nil
}

func (i *Index) Close() error { return i.db.Close() }

// dbItem is the on-disk row form of an Item.
type dbItem struct {
	ID         string `db:"id"`
	Name       string `db:"name"`
	Parent     string `db:"parent"`
	Size       int64  `db:"size"`
	Mime       string `db:"mime"`
	ModTime    int64  `db:"modtime"`
	MD5        string `db:"md5"`
	IsFolder   bool   `db:"is_folder"`
	Trashed    bool   `db:"trashed"`
	Width      int64  `db:"width"`
	Height     int64  `db:"height"`
	DurationMS int64  `db:"duration_ms"`
	HasThumb   bool   `db:"has_thumb"`
}

func toDB(it Item) dbItem {
	return dbItem{
		ID: it.ID, Name: it.Name, Parent: it.Parent, Size: it.Size,
		Mime: it.MimeType, ModTime: it.ModTime.Unix(), MD5: it.MD5,
		IsFolder: it.IsFolder, Trashed: it.Trashed,
		Width: it.Width, Height: it.Height, DurationMS: it.DurationMS, HasThumb: it.HasThumbnail,
	}
}

func fromDB(d dbItem) Item {
	return Item{
		ID: d.ID, Name: d.Name, Parent: d.Parent, Size: d.Size,
		MimeType: d.Mime, ModTime: time.Unix(d.ModTime, 0), MD5: d.MD5,
		IsFolder: d.IsFolder, Trashed: d.Trashed,
		Width: d.Width, Height: d.Height, DurationMS: d.DurationMS, HasThumbnail: d.HasThumb,
	}
}

// Upsert inserts or replaces a batch of items in a single transaction.
func (i *Index) Upsert(items []Item) error {
	tx, err := i.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	const q = `INSERT INTO items (id,name,parent,size,mime,modtime,md5,is_folder,trashed,width,height,duration_ms,has_thumb)
		VALUES (:id,:name,:parent,:size,:mime,:modtime,:md5,:is_folder,:trashed,:width,:height,:duration_ms,:has_thumb)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, parent=excluded.parent, size=excluded.size,
			mime=excluded.mime, modtime=excluded.modtime, md5=excluded.md5,
			is_folder=excluded.is_folder, trashed=excluded.trashed,
			width=excluded.width, height=excluded.height,
			duration_ms=excluded.duration_ms, has_thumb=excluded.has_thumb`
	for _, it := range items {
		if _, err := tx.NamedExec(q, toDB(it)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Delete removes an item (e.g. on a Changes removal/trash event).
func (i *Index) Delete(id string) error {
	_, err := i.db.Exec(`DELETE FROM items WHERE id = ?`, id)
	return err
}

// Count returns the number of non-folder, non-trashed files in the index.
func (i *Index) Count() (int, error) {
	var n int
	err := i.db.Get(&n, `SELECT COUNT(*) FROM items WHERE is_folder = 0 AND trashed = 0`)
	return n, err
}

// Get returns an item by id.
func (i *Index) Get(id string) (Item, bool, error) {
	var d dbItem
	err := i.db.Get(&d, `SELECT * FROM items WHERE id = ?`, id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Item{}, false, nil
		}
		return Item{}, false, err
	}
	return fromDB(d), true, nil
}

// Children returns the direct children of a folder id.
func (i *Index) Children(parentID string) ([]Item, error) {
	var rows []dbItem
	if err := i.db.Select(&rows, `SELECT * FROM items WHERE parent = ? AND trashed = 0`, parentID); err != nil {
		return nil, err
	}
	items := make([]Item, len(rows))
	for j, d := range rows {
		items[j] = fromDB(d)
	}
	return items, nil
}

// childByName resolves a single path segment under a parent.
func (i *Index) childByName(parentID, name string) (Item, bool, error) {
	var d dbItem
	err := i.db.Get(&d, `SELECT * FROM items WHERE parent = ? AND name = ? AND trashed = 0 LIMIT 1`, parentID, name)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return Item{}, false, nil
		}
		return Item{}, false, err
	}
	return fromDB(d), true, nil
}

// LookupPath resolves a slash-separated path (relative to the drive root) to
// an item by walking name segments. An empty path returns the synthetic root.
func (i *Index) LookupPath(p string) (Item, bool, error) {
	p = strings.Trim(path.Clean("/"+p), "/")
	cur := Item{ID: i.rootID, Name: "", IsFolder: true}
	if p == "" {
		return cur, true, nil
	}
	for _, seg := range strings.Split(p, "/") {
		child, ok, err := i.childByName(cur.ID, seg)
		if err != nil {
			return Item{}, false, err
		}
		if !ok {
			return Item{}, false, nil
		}
		cur = child
	}
	return cur, true, nil
}

// ResolvePath builds the slash-separated path of an item by walking its parent
// chain up to the drive root.
func (i *Index) ResolvePath(id string) (string, error) {
	var segs []string
	cur := id
	for cur != "" && cur != i.rootID {
		it, ok, err := i.Get(cur)
		if err != nil {
			return "", err
		}
		if !ok {
			// orphaned (parent not yet indexed); stop here.
			break
		}
		segs = append([]string{it.Name}, segs...)
		cur = it.Parent
	}
	return strings.Join(segs, "/"), nil
}

// IsListed reports whether a folder's children have already been fetched from
// the Drive API (so lazy listing skips a re-fetch).
func (i *Index) IsListed(folderID string) (bool, error) {
	var n int
	err := i.db.Get(&n, `SELECT COUNT(*) FROM listed_folders WHERE id = ?`, folderID)
	return n > 0, err
}

// MarkListed records that a folder's children have been fetched and indexed.
func (i *Index) MarkListed(folderID string) error {
	_, err := i.db.Exec(`INSERT OR IGNORE INTO listed_folders (id) VALUES (?)`, folderID)
	return err
}

// SetMeta stores a metadata key (e.g. the change page token).
func (i *Index) SetMeta(key, value string) error {
	_, err := i.db.Exec(
		`INSERT INTO meta (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, value)
	return err
}

// GetMeta reads a metadata value; ok is false if unset.
func (i *Index) GetMeta(key string) (string, bool, error) {
	var v string
	err := i.db.Get(&v, `SELECT value FROM meta WHERE key = ?`, key)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

const pageTokenKey = "change_page_token"

// PageToken returns the stored change page token (empty if a full index has
// not yet completed).
func (i *Index) PageToken() (string, error) {
	v, _, err := i.GetMeta(pageTokenKey)
	return v, err
}

// SetPageToken persists the change page token for the next incremental scan.
func (i *Index) SetPageToken(token string) error {
	return i.SetMeta(pageTokenKey, token)
}

// String summarises the index for logging.
func (i *Index) String() string {
	n, _ := i.Count()
	return fmt.Sprintf("drive %s: %d files indexed", i.driveID, n)
}
