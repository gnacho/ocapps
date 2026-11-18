// Package store - SQLite persistence (modernc.org/sqlite, pure Go, no cgo).
// Multi-owner (H8): one instance serves ALL the OpenCloud users, every row of
// assets/albums carries its owner (oc_id) and ALL the domain methods take
// `owner string` after ctx and filter by it. IDOR rule: reading or writing a
// foreign id returns sql.ErrNoRows ("not found" -> 404 in the API), never an
// error that leaks the existence of the resource.
// For 70k-500k assets per user SQLite is more than enough, backups = copying the
// .db file.
//
// Opening and migrations delegated to common/store (SPEC §5.2/§5.3): the
// database is opened with the unified pragmas and the schema is managed with the
// migration runner plus the adoption baseline (see migrations/001_baseline.sql
// and migrations/002_multiowner.sql). The backfill of the rows of the
// single-tenant era (owner empty) cannot be SQL (it needs Graph): the module does it
// with BackfillOwner after resolving the oc_id (SPEC §5.3).
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	commonstore "github.com/gnacho/ocapps/internal/common/store"
)

//go:embed migrations
var migrationsFS embed.FS

type Asset struct {
	ID         int64    `json:"id"`
	Path       string   `json:"path"`
	Filename   string   `json:"filename"`
	MediaType  string   `json:"mediaType"`
	TakenAt    int64    `json:"takenAt"`
	SpaceID    string   `json:"spaceId,omitempty"`
	SpaceName  string   `json:"spaceName,omitempty"`
	SpaceType  string   `json:"spaceType,omitempty"`
	Width      int      `json:"width"`
	Height     int      `json:"height"`
	Camera     string   `json:"camera,omitempty"`
	Lens       string   `json:"lens,omitempty"`
	ISO        int      `json:"iso,omitempty"`
	Aperture   string   `json:"aperture,omitempty"`
	Shutter    string   `json:"shutter,omitempty"`
	Focal      string   `json:"focal,omitempty"`
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	Size       int64    `json:"size"`
	IsFavorite bool     `json:"favorite"`
	IsArchived bool     `json:"archived"`
	ExifDone   bool     `json:"-"`
	DeletedAt  *int64   `json:"-"`
}

type DayBucket struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

type Store struct {
	db *sql.DB
}

// Open opens the database with common/store.Open (unified pragmas, §5.2) and
// brings the schema up to date with the adoption policy of SPEC §5.3:
//   - new/empty database -> Migrate applies 001_baseline.sql (already
//     multi-owner schema) + 002_multiowner.sql (effective no-op on empty tables)
//     and it ends up at user_version=2.
//   - live database (pre-H8 schema already present, user_version=0) -> it is
//     checked with PRAGMA table_info, adopted with Baseline(db, 1) and Migrate
//     applies 002 (rebuild of assets with owner + ALTER of albums).
//   - database with tables but without the full expected schema -> error
//     (fail-loud, the tolerant "duplicate column" ALTERs are gone).
func Open(path string) (*Store, error) {
	db, err := commonstore.Open(path)
	if err != nil {
		return nil, err
	}
	st := &Store{db: db}
	if err := st.adoptOrMigrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

// tables and columns (on assets) a live memories.db must have (§5.3). The new
// columns of 002 (owner) are NOT required here: a live pre-H8 database does not
// have them and it is precisely 002 that adds them.
var expectedTables = []string{"assets", "scan_state", "albums", "album_assets", "asset_tags", "geocode"}
var expectedAssetCols = []string{"is_archived", "phash"}

func (s *Store) adoptOrMigrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version == 0 {
		tables, err := s.existingTables()
		if err != nil {
			return err
		}
		switch {
		case len(tables) == 0:
			// New database: Migrate applies the baseline (and 002).
		case liveSchemaPresent(tables):
			cols, err := s.assetColumns()
			if err != nil {
				return err
			}
			for _, c := range expectedAssetCols {
				if !cols[c] {
					return fmt.Errorf("database with a partial schema: assets.%s does not exist, migrate the memories.db with a version that still applies the historical ALTERs", c)
				}
			}
			// complete live schema: adoption without running SQL
			if err := commonstore.Baseline(s.db, 1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("database not recognisable as a photos memories.db: it has %d tables but expected tables of the schema are missing (%v), it is not adopted", len(tables), expectedTables)
		}
	}
	return commonstore.Migrate(s.db, migrationsFS, "migrations")
}

// existingTables returns the set of user tables present.
func (s *Store) existingTables() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func liveSchemaPresent(tables map[string]bool) bool {
	for _, t := range expectedTables {
		if !tables[t] {
			return false
		}
	}
	return true
}

// assetColumns returns the columns of assets via PRAGMA table_info (§5.3).
func (s *Store) assetColumns() (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(assets)`)
	if err != nil {
		return nil, fmt.Errorf("table_info(assets): %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database is still alive (module health, SPEC §4.5).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// --- backfill of the single-tenant era (H8, SPEC §5.3) ---

// OrphanCounts counts the rows of the single-tenant era (owner empty) still
// pending adoption. If both are 0 there is no backfill pending.
func (s *Store) OrphanCounts(ctx context.Context) (assets, albums int64, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE owner=''`).Scan(&assets); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM albums WHERE owner=''`).Scan(&albums); err != nil {
		return 0, 0, err
	}
	return assets, albums, nil
}

// BackfillOwner assigns an owner to the orphan rows (owner empty) of assets and
// albums. It returns (adopted, removed) per table. It cannot be an SQL
// migration: the oc_id has to be resolved against Graph with the app token
// (SPEC §5.3), the module calls it at startup if OCAPPS_PHOTOS_USER +
// OCAPPS_PHOTOS_APP_TOKEN are configured.
//
// Tolerant to collisions with UNIQUE(owner, path) (H8 review): if the backfill
// was skipped on one start (OpenCloud down) and the user rescanned, (empty path)
// and (owner,path) can coexist. A blind UPDATE would blow up entirely with error
// 2067 and the legacy rows would stay invisible forever. That is why the
// conflicting orphans are DELETED first (the new rows of the owner already
// represent them, their album_assets/asset_tags die through the ON DELETE
// CASCADE) and only the non-conflicting ones are adopted. Albums (no UNIQUE):
// same criterion by (owner, name). Idempotent.
func (s *Store) BackfillOwner(ctx context.Context, owner string) (adoptedAssets, adoptedAlbums, removedAssets, removedAlbums int64, err error) {
	if owner == "" {
		return 0, 0, 0, 0, fmt.Errorf("backfill with an empty owner")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM assets WHERE owner='' AND path IN (SELECT path FROM assets WHERE owner=?)`, owner)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	removedAssets, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `UPDATE assets SET owner=? WHERE owner=''`, owner)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	adoptedAssets, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM albums WHERE owner='' AND name IN (SELECT name FROM albums WHERE owner=?)`, owner)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	removedAlbums, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `UPDATE albums SET owner=? WHERE owner=''`, owner)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	adoptedAlbums, _ = res.RowsAffected()
	return adoptedAssets, adoptedAlbums, removedAssets, removedAlbums, nil
}

// AssetOwner returns the owner of an asset by id. For EXCLUSIVE use of the
// signed video streaming (capability URL without a session, H8 §6.2): the HMAC
// signature is only generated after verifying ownership in videoURL, so
// resolving the owner from the id leaks nothing that the capability does not
// grant already. Do NOT use it in authenticated handlers (there the owner comes
// from the context).
func (s *Store) AssetOwner(ctx context.Context, id int64) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM assets WHERE id=?`, id).Scan(&owner)
	return owner, err
}

// --- writing (scanner) ---

// Space is a drive of the owner (issue #145): the id after "$" in
// /dav/spaces/<storage>$<space>, the display name and the OpenCloud driveType
// (personal, project, share...). The API lists the spaces present in the index
// so the UI can filter by them.
type Space struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Assets int64  `json:"count"`
}

// UpsertByETag inserts or updates an asset whose space is not known by the
// caller (tests, one-off tooling). It is UpsertInSpace with an empty space.
func (s *Store) UpsertByETag(ctx context.Context, owner, path, etag, filename, mediaType string, mtime time.Time, size int64) (int64, bool, error) {
	return s.UpsertInSpace(ctx, owner, path, etag, filename, mediaType, mtime, size, Space{})
}

// UpsertInSpace is UpsertByETag with the drive the asset was found in: the
// scanner walks every space of the owner, so the space is part of the index.
// It returns (id, changed). The uniqueness is (owner, path): the same physical
// path can coexist under two owners, and two spaces never share a path because
// the path carries the space UUID.
// space is stored so the API can filter by space and show its name (issue
// #145). A rename of the space does not change the path, so the name and the
// type are refreshed when they differ.
func (s *Store) UpsertInSpace(ctx context.Context, owner, path, etag, filename, mediaType string, mtime time.Time, size int64, space Space) (int64, bool, error) {
	var id int64
	var curETag, curSpaceID, curSpaceName, curSpaceType string
	err := s.db.QueryRowContext(ctx, `SELECT id, etag, space_id, space_name, space_type FROM assets WHERE owner = ? AND path = ?`, owner, path).Scan(&id, &curETag, &curSpaceID, &curSpaceName, &curSpaceType)
	switch {
	case err == sql.ErrNoRows:
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO assets (owner, path, etag, filename, media_type, taken_at, size, space_id, space_name, space_type) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			owner, path, etag, filename, mediaType, mtime.Unix(), size, space.ID, space.Name, space.Type)
		if err != nil {
			return 0, false, err
		}
		id, _ = res.LastInsertId()
		return id, true, nil
	case err != nil:
		return 0, false, err
	case curETag != etag:
		// modified file (or first scan after 004, when space_id is still
		// empty): reset the EXIF to reprocess it, keep the favourite flag
		if _, err := s.db.ExecContext(ctx,
			`UPDATE assets SET etag=?, taken_at=?, size=?, exif_done=0, deleted_at=NULL, space_id=?, space_name=?, space_type=? WHERE id=?`,
			etag, mtime.Unix(), size, space.ID, space.Name, space.Type, id); err != nil {
			return 0, false, err
		}
		return id, true, nil
	case curSpaceID != space.ID || curSpaceName != space.Name || curSpaceType != space.Type:
		// a rename of the space (its path does not change) or the first scan
		// after migration 004 (space_id still empty)
		_, err := s.db.ExecContext(ctx,
			`UPDATE assets SET deleted_at=NULL, space_id=?, space_name=?, space_type=? WHERE id=?`,
			space.ID, space.Name, space.Type, id)
		return id, false, err
	default:
		// no changes, make sure it is revived if it was soft-deleted
		_, err := s.db.ExecContext(ctx, `UPDATE assets SET deleted_at=NULL WHERE id=?`, id)
		return id, false, err
	}
}

// Spaces lists the spaces (drives) with live assets of the owner, with their
// count, ordered by name. Rows indexed before migration 004 have no space yet:
// they are left out until the next scan fills them in.
func (s *Store) Spaces(ctx context.Context, owner string) ([]Space, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT space_id, space_name, space_type, COUNT(*) FROM assets
		 WHERE owner = ? AND deleted_at IS NULL AND is_archived = 0 AND space_id <> ''
		 GROUP BY space_id, space_name, space_type ORDER BY space_name, space_id`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Space{}
	for rows.Next() {
		var sp Space
		if err := rows.Scan(&sp.ID, &sp.Name, &sp.Type, &sp.Assets); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SoftDeleteExcept marks as deleted the live assets OF THE OWNER that fall under
// prefix and whose etag is not in seen. It never touches rows of other owners nor
// of other paths of the same owner: a scan that walks one folder cannot delete
// what lives outside it (issue #36: a non-existent root left the owner index at
// zero because the soft-delete was global to the owner).
func (s *Store) SoftDeleteExcept(ctx context.Context, owner, prefix string, seen map[string]bool) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, etag FROM assets WHERE owner = ? AND deleted_at IS NULL`, owner)
	if err != nil {
		return 0, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var path, etag string
		if err := rows.Scan(&id, &path, &etag); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		if !seen[etag] {
			stale = append(stale, id)
		}
	}
	_ = rows.Close()
	now := time.Now().Unix()
	for _, id := range stale {
		if _, err := s.db.ExecContext(ctx, `UPDATE assets SET deleted_at=? WHERE id=?`, now, id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// PendingExif returns assets of the owner without processed EXIF (worker
// batches).
func (s *Store) PendingExif(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, media_type FROM assets WHERE owner = ? AND exif_done = 0 AND deleted_at IS NULL AND media_type='image' LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.Path, &a.MediaType); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type ExifResult struct {
	TakenAt                                *time.Time
	Camera, Lens, Aperture, Shutter, Focal string
	ISO                                    int
	Width, Height                          int
	Lat, Lon                               *float64
}

// SaveExif stores the EXIF by id (ids are global, the worker only processes ids
// obtained from PendingExif(owner, ...), so there is no need to repeat the owner
// filter here).
func (s *Store) SaveExif(ctx context.Context, id int64, r ExifResult) error {
	q := `UPDATE assets SET exif_done=1, camera=?, lens=?, iso=?, aperture=?, shutter=?, focal=?, width=?, height=?, lat=?, lon=?`
	args := []any{r.Camera, r.Lens, r.ISO, r.Aperture, r.Shutter, r.Focal, r.Width, r.Height, r.Lat, r.Lon}
	if r.TakenAt != nil {
		q += `, taken_at=?`
		args = append(args, r.TakenAt.Unix())
	}
	q += ` WHERE id=?`
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// --- reading (API) ---

const assetCols = `id, path, filename, media_type, taken_at, width, height, camera, lens, iso, aperture, shutter, focal, lat, lon, size, is_favorite, is_archived, exif_done, deleted_at, space_id, space_name, space_type`

func scanAsset(rows interface{ Scan(...any) error }) (Asset, error) {
	var a Asset
	var fav, arch, exif int
	var lat, lon *float64
	var camera, lens, aperture, shutter, focal *string
	var iso, width, height *int
	err := rows.Scan(&a.ID, &a.Path, &a.Filename, &a.MediaType, &a.TakenAt, &width, &height,
		&camera, &lens, &iso, &aperture, &shutter, &focal, &lat, &lon, &a.Size, &fav, &arch, &exif, &a.DeletedAt,
		&a.SpaceID, &a.SpaceName, &a.SpaceType)
	if err != nil {
		return a, err
	}
	a.IsFavorite = fav == 1
	a.IsArchived = arch == 1
	a.ExifDone = exif == 1
	a.Lat, a.Lon = lat, lon
	if camera != nil {
		a.Camera = *camera
	}
	if lens != nil {
		a.Lens = *lens
	}
	if aperture != nil {
		a.Aperture = *aperture
	}
	if shutter != nil {
		a.Shutter = *shutter
	}
	if focal != nil {
		a.Focal = *focal
	}
	if iso != nil {
		a.ISO = *iso
	}
	if width != nil {
		a.Width = *width
	}
	if height != nil {
		a.Height = *height
	}
	return a, nil
}

// AssetsPage paginates by cursor (taken_at, id) descending - stable infinite
// scroll. It only returns assets of the owner.
func (s *Store) AssetsPage(ctx context.Context, owner string, beforeTaken int64, beforeID int64, limit int, favoritesOnly, archivedOnly bool, query, spaceID string) ([]Asset, error) {
	arch := 0
	if archivedOnly {
		arch = 1
	}
	q := `SELECT ` + assetCols + ` FROM assets WHERE owner = ? AND deleted_at IS NULL AND is_archived = ? AND (taken_at < ? OR (taken_at = ? AND id < ?))`
	args := []any{owner, arch, beforeTaken, beforeTaken, beforeID}
	if favoritesOnly {
		q += ` AND is_favorite = 1`
	}
	if spaceID != "" {
		q += ` AND space_id = ?`
		args = append(args, spaceID)
	}
	if query != "" {
		q += ` AND (filename LIKE ? OR camera LIKE ? OR path LIKE ?)`
		like := "%" + query + "%"
		args = append(args, like, like, like)
	}
	q += ` ORDER BY taken_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AssetByID: an id of ANOTHER owner returns sql.ErrNoRows (IDOR rule, H8).
func (s *Store) AssetByID(ctx context.Context, owner string, id int64) (Asset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id=? AND owner=?`, id, owner)
	return scanAsset(row)
}

// AssetByPath looks for an asset by its absolute DAV href (the indexed path).
// It is the basis of the /api/assets/resolve endpoint ("Open with" from Files):
// the host sends driveAliasAndItem (<alias><path>) and the indexed path is
// "/dav/spaces/" + driveAliasAndItem. It filters deleted_at: a deleted file must
// not be resolved. sql.ErrNoRows = not indexed or foreign (same answer, IDOR rule
// H8).
func (s *Store) AssetByPath(ctx context.Context, owner, davPath string) (Asset, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE owner=? AND path=? AND deleted_at IS NULL`,
		owner, davPath)
	return scanAsset(row)
}

// AssetBySpaceItem is the fallback of AssetByPath for when the host sends
// driveAliasAndItem in the form <type>/<name>/<path> (observed in OpenCloud
// 8.0.1) instead of <type>$<spaceId>/<path>: the alias cannot be built directly,
// but fileId carries the spaceId (storageId$spaceId!opaqueId) and the indexed
// path always contains "$<spaceId>/<path>". The LIKE anchors the prefix
// "$<spaceId>/" so as not to cross spaceIds that share a suffix.
func (s *Store) AssetBySpaceItem(ctx context.Context, owner, spaceID, itemPath string) (Asset, error) {
	pattern := "%$" + likeEscape(spaceID) + "/" + likeEscape(itemPath)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE owner=? AND deleted_at IS NULL AND path LIKE ? ESCAPE '\'`,
		owner, pattern)
	return scanAsset(row)
}

// likeEscape escapes the LIKE wildcards (and the backslash itself) to compare
// literals with ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// updateOwned runs an UPDATE scoped to the owner and returns sql.ErrNoRows if it
// touched no row (id that does not exist OR foreign: IDOR rule, H8).
func updateOwned(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SetFavorite(ctx context.Context, owner string, id int64, fav bool) error {
	v := 0
	if fav {
		v = 1
	}
	return updateOwned(s.db.ExecContext(ctx, `UPDATE assets SET is_favorite=? WHERE id=? AND owner=?`, v, id, owner))
}

func (s *Store) SetArchived(ctx context.Context, owner string, id int64, arch bool) error {
	v := 0
	if arch {
		v = 1
	}
	return updateOwned(s.db.ExecContext(ctx, `UPDATE assets SET is_archived=? WHERE id=? AND owner=?`, v, id, owner))
}

// SetPHash stores the perceptual hash (hex) of a photo.
func (s *Store) SetPHash(ctx context.Context, owner string, id int64, hash string) error {
	return updateOwned(s.db.ExecContext(ctx, `UPDATE assets SET phash=? WHERE id=? AND owner=?`, hash, id, owner))
}

// AssetsWithoutPHash: live photos of the owner without a perceptual hash.
func (s *Store) AssetsWithoutPHash(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND (phash IS NULL OR phash='') LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllPHashes: id -> perceptual hash of the live photos of the owner that already
// have one.
func (s *Store) AllPHashes(ctx context.Context, owner string) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, phash FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND phash IS NOT NULL AND phash <> ''`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// OnThisDay: photos of this month-day (or +-dayRange days) in previous years.
func (s *Store) OnThisDay(ctx context.Context, owner string, month, day, thisYear, dayRange int) ([]Asset, error) {
	// (month, day) pairs of the range, skipping duplicates
	base := time.Date(2000, time.Month(month), day, 12, 0, 0, 0, time.UTC)
	seen := map[[2]int]bool{}
	var conds []string
	args := []any{owner}
	for j := -dayRange; j <= dayRange; j++ {
		t := base.AddDate(0, 0, j)
		k := [2]int{int(t.Month()), t.Day()}
		if seen[k] {
			continue
		}
		seen[k] = true
		conds = append(conds, `(CAST(strftime('%m', taken_at, 'unixepoch') AS INT) = ? AND CAST(strftime('%d', taken_at, 'unixepoch') AS INT) = ?)`)
		args = append(args, k[0], k[1])
	}
	args = append(args, thisYear)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetCols+` FROM assets
		 WHERE owner = ? AND deleted_at IS NULL
		   AND (`+strings.Join(conds, " OR ")+`)
		   AND CAST(strftime('%Y', taken_at, 'unixepoch') AS INT) < ?
		 ORDER BY taken_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OnThisMonth: photos of the current month in previous years (highlights
// fallback).
func (s *Store) OnThisMonth(ctx context.Context, owner string, month, thisYear, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetCols+` FROM assets
		 WHERE owner = ? AND deleted_at IS NULL
		   AND CAST(strftime('%m', taken_at, 'unixepoch') AS INT) = ?
		   AND CAST(strftime('%Y', taken_at, 'unixepoch') AS INT) < ?
		 ORDER BY taken_at DESC LIMIT ?`, owner, month, thisYear, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// OldestAssets: the oldest photos of the owner (last resort of highlights).
func (s *Store) OldestAssets(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE owner = ? AND deleted_at IS NULL ORDER BY taken_at ASC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Albums ---

type Album struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Count     int    `json:"count"`
	CoverID   int64  `json:"coverId,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Store) CreateAlbum(ctx context.Context, owner, name string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO albums (owner, name) VALUES (?,?)`, owner, name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) RenameAlbum(ctx context.Context, owner string, id int64, name string) error {
	return updateOwned(s.db.ExecContext(ctx, `UPDATE albums SET name=? WHERE id=? AND owner=?`, name, id, owner))
}

func (s *Store) DeleteAlbum(ctx context.Context, owner string, id int64) error {
	return updateOwned(s.db.ExecContext(ctx, `DELETE FROM albums WHERE id=? AND owner=?`, id, owner))
}

// albumOwned returns sql.ErrNoRows if the album does not exist OR belongs to
// another owner (IDOR rule, H8): the operations on albums use it as a guard.
func (s *Store) albumOwned(ctx context.Context, owner string, albumID int64) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id=? AND owner=?`, albumID, owner).Scan(&one)
	return err
}

// ListAlbums: albums of the owner with the count of live photos and a cover (the
// most recent one).
func (s *Store) ListAlbums(ctx context.Context, owner string) ([]Album, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.created_at,
		  (SELECT count(*) FROM album_assets aa JOIN assets s ON s.id=aa.asset_id
		     WHERE aa.album_id=a.id AND s.deleted_at IS NULL) AS cnt,
		  COALESCE((SELECT aa.asset_id FROM album_assets aa JOIN assets s ON s.id=aa.asset_id
		     WHERE aa.album_id=a.id AND s.deleted_at IS NULL
		     ORDER BY s.taken_at DESC LIMIT 1), 0) AS cover
		FROM albums a WHERE a.owner = ? ORDER BY a.name COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Album{}
	for rows.Next() {
		var al Album
		if err := rows.Scan(&al.ID, &al.Name, &al.CreatedAt, &al.Count, &al.CoverID); err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, rows.Err()
}

// AlbumAssets: photos of the album (only if the album belongs to the owner, IDOR
// -> ErrNoRows).
func (s *Store) AlbumAssets(ctx context.Context, owner string, albumID int64) ([]Asset, error) {
	if err := s.albumOwned(ctx, owner, albumID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND id IN (SELECT asset_id FROM album_assets WHERE album_id=?)
		ORDER BY taken_at DESC`, owner, albumID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddToAlbum adds assets to an album checking that the album AND ALL the assets
// belong to the owner, if any of them does not, sql.ErrNoRows (IDOR rule, H8:
// "does not exist" and "is foreign" are not distinguished).
func (s *Store) AddToAlbum(ctx context.Context, owner string, albumID int64, assetIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id=? AND owner=?`, albumID, owner).Scan(&one); err != nil {
		return err // ErrNoRows: album that does not exist or is foreign
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO album_assets (album_id, asset_id) VALUES (?,?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, id := range assetIDs {
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, id, owner).Scan(&one); err != nil {
			return err // ErrNoRows: asset that does not exist or is foreign
		}
		if _, err := stmt.ExecContext(ctx, albumID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveFromAlbum removes an asset from an album of the owner (IDOR ->
// ErrNoRows).
func (s *Store) RemoveFromAlbum(ctx context.Context, owner string, albumID, assetID int64) error {
	if err := s.albumOwned(ctx, owner, albumID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM album_assets WHERE album_id=? AND asset_id=?`, albumID, assetID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Places ---

type Place struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Count   int     `json:"count"`
	CoverID int64   `json:"coverId"`
	Name    string  `json:"name,omitempty"`
}

// PlaceClusters groups the photos with GPS of the owner by coordinates rounded to
// `decimals` decimals (~1 km with 2). It returns the cover (the most recent photo
// of each place).
func (s *Store) PlaceClusters(ctx context.Context, owner string, decimals int) ([]Place, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT round(lat, ?) AS rlat, round(lon, ?) AS rlon, count(*) AS cnt
		FROM assets WHERE owner = ? AND deleted_at IS NULL AND lat IS NOT NULL
		GROUP BY rlat, rlon ORDER BY cnt DESC`, decimals, decimals, owner)
	if err != nil {
		return nil, err
	}
	out := []Place{}
	for rows.Next() {
		var p Place
		if err := rows.Scan(&p.Lat, &p.Lon, &p.Count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// covers OUTSIDE the cursor: with MaxOpenConns(1) it is not possible to query
	// while rows is still open (deadlock).
	for i := range out {
		_ = s.db.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE owner = ? AND deleted_at IS NULL AND round(lat,?)=? AND round(lon,?)=?
			 ORDER BY taken_at DESC LIMIT 1`, owner, decimals, out[i].Lat, decimals, out[i].Lon).Scan(&out[i].CoverID)
	}
	return out, nil
}

// --- Tags ---

type Tag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// AddTag tags an asset of the owner (id that does not exist or is foreign ->
// ErrNoRows, H8).
func (s *Store) AddTag(ctx context.Context, owner string, assetID int64, tag string) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO asset_tags (asset_id, tag) VALUES (?,?)`, assetID, tag)
	return err
}

// RemoveTag removes a tag from an asset of the owner (IDOR -> ErrNoRows).
func (s *Store) RemoveTag(ctx context.Context, owner string, assetID int64, tag string) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM asset_tags WHERE asset_id=? AND tag=?`, assetID, tag)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AssetTags: tags of an asset of the owner (IDOR -> ErrNoRows).
func (s *Store) AssetTags(ctx context.Context, owner string, assetID int64) ([]string, error) {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM asset_tags WHERE asset_id=? ORDER BY tag COLLATE NOCASE`, assetID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTags replaces the whole set of tags of an asset of the owner in a single
// transaction. The scanner uses it when projecting an XMP sidecar: without the
// replacement a tag deleted outside the web would stay alive in the index. IDOR
// -> ErrNoRows.
func (s *Store) SetTags(ctx context.Context, owner string, assetID int64, tags []string) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_tags WHERE asset_id=?`, assetID); err != nil {
		return err
	}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO asset_tags (asset_id, tag) VALUES (?,?)`, assetID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SidecarETag returns the etag of the last XMP sidecar applied to an asset (empty
// = never applied). IDOR -> ErrNoRows.
func (s *Store) SidecarETag(ctx context.Context, owner string, assetID int64) (string, error) {
	var etag string
	err := s.db.QueryRowContext(ctx, `SELECT sidecar_etag FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&etag)
	return etag, err
}

// SetSidecarETag marks the applied sidecar so as not to download it again.
func (s *Store) SetSidecarETag(ctx context.Context, owner string, assetID int64, etag string) error {
	return updateOwned(s.db.ExecContext(ctx, `UPDATE assets SET sidecar_etag=? WHERE id=? AND owner=?`, etag, assetID, owner))
}

// ListTags: tags of the owner with the count of live photos.
func (s *Store) ListTags(ctx context.Context, owner string) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tag, count(*) FROM asset_tags t JOIN assets a ON a.id=t.asset_id
		WHERE a.owner = ? AND a.deleted_at IS NULL GROUP BY t.tag ORDER BY t.tag COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Tag{}
	for rows.Next() {
		var tg Tag
		if err := rows.Scan(&tg.Name, &tg.Count); err != nil {
			return nil, err
		}
		out = append(out, tg)
	}
	return out, rows.Err()
}

func (s *Store) AssetsByTag(ctx context.Context, owner, tag string) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND id IN (SELECT asset_id FROM asset_tags WHERE tag=?)
		ORDER BY taken_at DESC`, owner, tag)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Folders (derived from the index, base = DAV prefix of the space + path) ---

type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// Subfolders: direct subfolders under `base` (with the photo count, recursive).
func (s *Store) Subfolders(ctx context.Context, owner, base string) ([]FolderEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(path, ?+1, instr(substr(path, ?+1), '/')-1) AS name, count(*)
		FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND path LIKE ? || '%' AND instr(substr(path, ?+1), '/') > 0
		GROUP BY name ORDER BY name COLLATE NOCASE`,
		len(base), len(base), owner, base, len(base))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []FolderEntry{}
	for rows.Next() {
		var fe FolderEntry
		if err := rows.Scan(&fe.Name, &fe.Count); err != nil {
			return nil, err
		}
		out = append(out, fe)
	}
	return out, rows.Err()
}

// FolderAssets: photos directly in `base` (without going down into subfolders).
func (s *Store) FolderAssets(ctx context.Context, owner, base string) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND path LIKE ? || '%' AND path NOT LIKE ? || '%/%'
		ORDER BY taken_at DESC`, owner, base, base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- geocoding cache (Places) ---
// WITHOUT owner on purpose (H8): it is a GLOBAL cache of place names (public
// Nominatim data), not personal information of the user.

func (s *Store) GetGeocode(ctx context.Context, latKey, lonKey float64) (string, bool, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM geocode WHERE lat_key=? AND lon_key=?`, latKey, lonKey).Scan(&name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

func (s *Store) SaveGeocode(ctx context.Context, latKey, lonKey float64, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO geocode (lat_key, lon_key, name, ts) VALUES (?,?,?,unixepoch())`,
		latKey, lonKey, name)
	return err
}

func (s *Store) GeoAssets(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE owner = ? AND deleted_at IS NULL AND lat IS NOT NULL ORDER BY taken_at DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Asset{}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MonthCount: photos of one month (1-12) of the year.
type MonthCount struct {
	Month int   `json:"month"`
	Count int64 `json:"count"`
}

// YearCount: photos per capture year, with a breakdown per month (Rewind
// scrubber).
type YearCount struct {
	Year   int          `json:"year"`
	Count  int64        `json:"count"`
	Months []MonthCount `json:"months"`
}

// Calendar returns the years (and months) with photos of the owner, descending.
// It excludes archived ones.
func (s *Store) Calendar(ctx context.Context, owner string) ([]YearCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT CAST(strftime('%Y', taken_at, 'unixepoch') AS INT) AS y,
		        CAST(strftime('%m', taken_at, 'unixepoch') AS INT) AS m, count(*)
		 FROM assets WHERE owner = ? AND deleted_at IS NULL AND is_archived = 0
		 GROUP BY y, m ORDER BY y DESC, m DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byYear := map[int]*YearCount{}
	order := []int{}
	for rows.Next() {
		var y, m int
		var c int64
		if err := rows.Scan(&y, &m, &c); err != nil {
			return nil, err
		}
		if byYear[y] == nil {
			byYear[y] = &YearCount{Year: y, Months: []MonthCount{}}
			order = append(order, y)
		}
		byYear[y].Count += c
		byYear[y].Months = append(byYear[y].Months, MonthCount{Month: m, Count: c})
	}
	out := []YearCount{}
	for _, y := range order {
		out = append(out, *byYear[y])
	}
	return out, rows.Err()
}

// Stats of the owner (the global admin endpoints DO NOT exist, H8).
func (s *Store) Stats(ctx context.Context, owner string) (map[string]any, error) {
	var total, favs, geo, exifPending, videos int64
	row := s.db.QueryRowContext(ctx, `SELECT
		count(*),
		COALESCE(sum(is_favorite),0),
		COALESCE(sum(lat IS NOT NULL),0),
		COALESCE(sum(exif_done=0 AND media_type='image'),0),
		COALESCE(sum(media_type='video'),0)
	  FROM assets WHERE owner = ? AND deleted_at IS NULL`, owner)
	if err := row.Scan(&total, &favs, &geo, &exifPending, &videos); err != nil {
		return nil, err
	}
	return map[string]any{
		"assets": total, "favorites": favs, "geo": geo,
		"exifPending": exifPending, "videos": videos,
	}, nil
}
