// Package store — persistencia SQLite (modernc.org/sqlite, Go puro, sin cgo).
// Multi-owner (H8): una instancia sirve a TODOS los usuarios de OpenCloud;
// cada fila de assets/albums lleva su owner (oc_id) y TODOS los métodos de
// dominio toman `owner string` tras ctx y filtran por él. Regla IDOR: leer
// o escribir un id ajeno devuelve sql.ErrNoRows ("no encontrado" → 404 en la
// API), nunca un error que delate la existencia del recurso.
// Para 70k-500k assets por usuario SQLite va sobrado; backups = copiar el
// fichero .db.
//
// Apertura y migraciones delegadas en common/store (SPEC §5.2/§5.3): la BD
// se abre con los pragmas unificados y el esquema se gestiona con el runner
// de migraciones + baseline de adopción (ver migrations/001_baseline.sql y
// migrations/002_multiowner.sql). El backfill de filas de la era
// single-tenant (owner=”) no puede ser SQL (necesita Graph): lo hace el
// módulo con BackfillOwner tras resolver el oc_id (SPEC §5.3).
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

// Open abre la BD con common/store.Open (pragmas unificados, §5.2) y deja el
// esquema al día con la política de adopción de SPEC §5.3:
//   - BD nueva/vacía → Migrate aplica 001_baseline.sql (esquema ya
//     multi-owner) + 002_multiowner.sql (no-op efectivo sobre tablas vacías)
//     y queda en user_version=2.
//   - BD viva (esquema pre-H8 ya presente, user_version=0) → se verifica con
//     PRAGMA table_info, se adopta con Baseline(db, 1) y Migrate aplica 002
//     (rebuild de assets con owner + ALTER de albums).
//   - BD con tablas pero sin el esquema esperado completo → error (fail-loud;
//     desaparecen los ALTERs tolerantes a "duplicate column").
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

// tablas y columnas (en assets) que debe tener una memories.db viva (§5.3).
// Las columnas nuevas de 002 (owner) NO se exigen aquí: una BD viva pre-H8
// no las tiene y es precisamente 002 quien las añade.
var expectedTables = []string{"assets", "scan_state", "albums", "album_assets", "asset_tags", "geocode"}
var expectedAssetCols = []string{"is_archived", "phash"}

func (s *Store) adoptOrMigrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("leer user_version: %w", err)
	}
	if version == 0 {
		tables, err := s.existingTables()
		if err != nil {
			return err
		}
		switch {
		case len(tables) == 0:
			// BD nueva: Migrate aplica el baseline (y 002).
		case liveSchemaPresent(tables):
			cols, err := s.assetColumns()
			if err != nil {
				return err
			}
			for _, c := range expectedAssetCols {
				if !cols[c] {
					return fmt.Errorf("BD con esquema parcial: assets.%s no existe; migra la memories.db con una versión que aún aplique los ALTER históricos", c)
				}
			}
			// esquema vivo completo: adopción sin ejecutar SQL
			if err := commonstore.Baseline(s.db, 1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("BD no reconocible como memories.db de photos: tiene %d tablas pero faltan tablas esperadas del esquema (%v); no se adopta", len(tables), expectedTables)
		}
	}
	return commonstore.Migrate(s.db, migrationsFS, "migrations")
}

// existingTables devuelve el conjunto de tablas de usuario presentes.
func (s *Store) existingTables() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("listar tablas: %w", err)
	}
	defer rows.Close()
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

// assetColumns devuelve las columnas de assets vía PRAGMA table_info (§5.3).
func (s *Store) assetColumns() (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(assets)`)
	if err != nil {
		return nil, fmt.Errorf("table_info(assets): %w", err)
	}
	defer rows.Close()
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

// Ping verifica que la BD sigue viva (health del módulo, SPEC §4.5).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// --- backfill de la era single-tenant (H8, SPEC §5.3) ---

// OrphanCounts cuenta las filas de la era single-tenant (owner=”) que quedan
// por adoptar. Si ambos son 0 no hay backfill pendiente.
func (s *Store) OrphanCounts(ctx context.Context) (assets, albums int64, err error) {
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE owner=''`).Scan(&assets); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM albums WHERE owner=''`).Scan(&albums); err != nil {
		return 0, 0, err
	}
	return assets, albums, nil
}

// BackfillOwner asigna owner a todas las filas huérfanas (owner=”) de assets
// y albums. Devuelve cuántas filas adoptó de cada tabla. No puede ser una
// migración SQL: el oc_id hay que resolverlo contra Graph con el app-token
// (SPEC §5.3); lo invoca el módulo al arrancar si OCAPPS_PHOTOS_USER +
// OCAPPS_PHOTOS_APP_TOKEN están configurados.
func (s *Store) BackfillOwner(ctx context.Context, owner string) (assets, albums int64, err error) {
	if owner == "" {
		return 0, 0, fmt.Errorf("backfill con owner vacío")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE assets SET owner=? WHERE owner=''`, owner)
	if err != nil {
		return 0, 0, err
	}
	assets, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `UPDATE albums SET owner=? WHERE owner=''`, owner)
	if err != nil {
		return 0, 0, err
	}
	albums, _ = res.RowsAffected()
	return assets, albums, nil
}

// AssetOwner devuelve el owner de un asset por id. Uso EXCLUSIVO del
// streaming de vídeo firmado (capability URL sin sesión, H8 §6.2): la firma
// HMAC solo se genera tras verificar ownership en videoURL, así que resolver
// el owner desde el id no filtra nada que la capability no conceda ya.
// NO usar en handlers autenticados (allí el owner viene del contexto).
func (s *Store) AssetOwner(ctx context.Context, id int64) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM assets WHERE id=?`, id).Scan(&owner)
	return owner, err
}

// --- escritura (scanner) ---

// UpsertByETag inserta o actualiza si el etag cambió. Devuelve (id, changed).
// La unicidad es (owner, path): el mismo path puede coexistir en dos owners.
func (s *Store) UpsertByETag(ctx context.Context, owner, path, etag, filename, mediaType string, mtime time.Time, size int64) (int64, bool, error) {
	var id int64
	var curETag string
	err := s.db.QueryRowContext(ctx, `SELECT id, etag FROM assets WHERE owner = ? AND path = ?`, owner, path).Scan(&id, &curETag)
	switch {
	case err == sql.ErrNoRows:
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO assets (owner, path, etag, filename, media_type, taken_at, size) VALUES (?,?,?,?,?,?,?)`,
			owner, path, etag, filename, mediaType, mtime.Unix(), size)
		if err != nil {
			return 0, false, err
		}
		id, _ = res.LastInsertId()
		return id, true, nil
	case err != nil:
		return 0, false, err
	case curETag != etag:
		// fichero modificado: resetea EXIF para reprocesar, conserva favorito
		if _, err := s.db.ExecContext(ctx,
			`UPDATE assets SET etag=?, taken_at=?, size=?, exif_done=0, deleted_at=NULL WHERE id=?`,
			etag, mtime.Unix(), size, id); err != nil {
			return 0, false, err
		}
		return id, true, nil
	default:
		// sin cambios; asegúrate de resucitarlo si estaba soft-deleted
		_, err := s.db.ExecContext(ctx, `UPDATE assets SET deleted_at=NULL WHERE id=?`, id)
		return id, false, err
	}
}

// SoftDeleteExcept marca como borrados los assets vivos DEL OWNER cuyo etag
// no está en seen. Nunca toca filas de otros owners.
func (s *Store) SoftDeleteExcept(ctx context.Context, owner string, seen map[string]bool) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, etag FROM assets WHERE owner = ? AND deleted_at IS NULL`, owner)
	if err != nil {
		return 0, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var etag string
		if err := rows.Scan(&id, &etag); err != nil {
			rows.Close()
			return 0, err
		}
		if !seen[etag] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	now := time.Now().Unix()
	for _, id := range stale {
		if _, err := s.db.ExecContext(ctx, `UPDATE assets SET deleted_at=? WHERE id=?`, now, id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// PendingExif devuelve assets del owner sin EXIF procesado (lotes del worker).
func (s *Store) PendingExif(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path, media_type FROM assets WHERE owner = ? AND exif_done = 0 AND deleted_at IS NULL AND media_type='image' LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// SaveExif guarda el EXIF por id (los ids son globales; el worker solo
// procesa ids obtenidos de PendingExif(owner, ...), así que no hace falta
// repetir el filtro de owner aquí).
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

// --- lectura (API) ---

const assetCols = `id, path, filename, media_type, taken_at, width, height, camera, lens, iso, aperture, shutter, focal, lat, lon, size, is_favorite, is_archived, exif_done, deleted_at`

func scanAsset(rows interface{ Scan(...any) error }) (Asset, error) {
	var a Asset
	var fav, arch, exif int
	var lat, lon *float64
	var camera, lens, aperture, shutter, focal *string
	var iso, width, height *int
	err := rows.Scan(&a.ID, &a.Path, &a.Filename, &a.MediaType, &a.TakenAt, &width, &height,
		&camera, &lens, &iso, &aperture, &shutter, &focal, &lat, &lon, &a.Size, &fav, &arch, &exif, &a.DeletedAt)
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

// AssetsPage pagina por cursor (taken_at, id) descendente — scroll infinito
// estable. Solo devuelve assets del owner.
func (s *Store) AssetsPage(ctx context.Context, owner string, beforeTaken int64, beforeID int64, limit int, favoritesOnly, archivedOnly bool, query string) ([]Asset, error) {
	arch := 0
	if archivedOnly {
		arch = 1
	}
	q := `SELECT ` + assetCols + ` FROM assets WHERE owner = ? AND deleted_at IS NULL AND is_archived = ? AND (taken_at < ? OR (taken_at = ? AND id < ?))`
	args := []any{owner, arch, beforeTaken, beforeTaken, beforeID}
	if favoritesOnly {
		q += ` AND is_favorite = 1`
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
	defer rows.Close()
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

// AssetByID: un id de OTRO owner devuelve sql.ErrNoRows (regla IDOR, H8).
func (s *Store) AssetByID(ctx context.Context, owner string, id int64) (Asset, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id=? AND owner=?`, id, owner)
	return scanAsset(row)
}

// updateOwned ejecuta un UPDATE acotado al owner y devuelve sql.ErrNoRows si
// no tocó ninguna fila (id inexistente O ajeno: regla IDOR, H8).
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

// SetPHash guarda el hash perceptual (hex) de una foto.
func (s *Store) SetPHash(ctx context.Context, owner string, id int64, hash string) error {
	return updateOwned(s.db.ExecContext(ctx, `UPDATE assets SET phash=? WHERE id=? AND owner=?`, hash, id, owner))
}

// AssetsWithoutPHash: fotos vivas del owner sin hash perceptual.
func (s *Store) AssetsWithoutPHash(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND (phash IS NULL OR phash='') LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// AllPHashes: id -> hash perceptual de las fotos vivas del owner que ya lo tienen.
func (s *Store) AllPHashes(ctx context.Context, owner string) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, phash FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND phash IS NOT NULL AND phash <> ''`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// OnThisDay: fotos de este mes-día (o ±dayRange días) en años anteriores.
func (s *Store) OnThisDay(ctx context.Context, owner string, month, day, thisYear, dayRange int) ([]Asset, error) {
	// pares (mes, día) del rango, evitando duplicados
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
	defer rows.Close()
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

// OnThisMonth: fotos del mes actual en años anteriores (fallback de highlights).
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
	defer rows.Close()
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

// OldestAssets: las fotos más antiguas del owner (último recurso de highlights).
func (s *Store) OldestAssets(ctx context.Context, owner string, limit int) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE owner = ? AND deleted_at IS NULL ORDER BY taken_at ASC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// --- Álbumes ---

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

// albumOwned devuelve sql.ErrNoRows si el álbum no existe O es de otro owner
// (regla IDOR, H8): las operaciones sobre álbumes la usan como guardia.
func (s *Store) albumOwned(ctx context.Context, owner string, albumID int64) error {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id=? AND owner=?`, albumID, owner).Scan(&one)
	return err
}

// ListAlbums: álbumes del owner con recuento de fotos vivas y portada (la más
// reciente).
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
	defer rows.Close()
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

// AlbumAssets: fotos del álbum (solo si el álbum es del owner; IDOR → ErrNoRows).
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
	defer rows.Close()
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

// AddToAlbum añade assets a un álbum verificando que el álbum Y TODOS los
// assets son del owner; si alguno no lo es, sql.ErrNoRows (regla IDOR, H8:
// no se distingue "no existe" de "es ajeno").
func (s *Store) AddToAlbum(ctx context.Context, owner string, albumID int64, assetIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM albums WHERE id=? AND owner=?`, albumID, owner).Scan(&one); err != nil {
		return err // ErrNoRows: álbum inexistente o ajeno
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO album_assets (album_id, asset_id) VALUES (?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range assetIDs {
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, id, owner).Scan(&one); err != nil {
			return err // ErrNoRows: asset inexistente o ajeno
		}
		if _, err := stmt.ExecContext(ctx, albumID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveFromAlbum quita un asset de un álbum del owner (IDOR → ErrNoRows).
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

// --- Lugares ---

type Place struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Count   int     `json:"count"`
	CoverID int64   `json:"coverId"`
	Name    string  `json:"name,omitempty"`
}

// PlaceClusters agrupa las fotos con GPS del owner por coordenadas redondeadas
// a `decimals` decimales (~1 km con 2). Devuelve la portada (foto más
// reciente de cada sitio).
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
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// portadas FUERA del cursor: con MaxOpenConns(1) no se puede consultar mientras
	// el rows sigue abierto (deadlock).
	for i := range out {
		_ = s.db.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE owner = ? AND deleted_at IS NULL AND round(lat,?)=? AND round(lon,?)=?
			 ORDER BY taken_at DESC LIMIT 1`, owner, decimals, out[i].Lat, decimals, out[i].Lon).Scan(&out[i].CoverID)
	}
	return out, nil
}

// --- Etiquetas ---

type Tag struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// AddTag etiqueta un asset del owner (id inexistente/ajeno → ErrNoRows, H8).
func (s *Store) AddTag(ctx context.Context, owner string, assetID int64, tag string) error {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO asset_tags (asset_id, tag) VALUES (?,?)`, assetID, tag)
	return err
}

// RemoveTag quita una etiqueta de un asset del owner (IDOR → ErrNoRows).
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

// AssetTags: etiquetas de un asset del owner (IDOR → ErrNoRows).
func (s *Store) AssetTags(ctx context.Context, owner string, assetID int64) ([]string, error) {
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM assets WHERE id=? AND owner=?`, assetID, owner).Scan(&one); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM asset_tags WHERE asset_id=? ORDER BY tag COLLATE NOCASE`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// ListTags: etiquetas del owner con recuento de fotos vivas.
func (s *Store) ListTags(ctx context.Context, owner string) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tag, count(*) FROM asset_tags t JOIN assets a ON a.id=t.asset_id
		WHERE a.owner = ? AND a.deleted_at IS NULL GROUP BY t.tag ORDER BY t.tag COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	defer rows.Close()
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

// --- Carpetas (derivadas del índice; base = prefijo DAV del espacio + ruta) ---

type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// Subfolders: subcarpetas directas bajo `base` (con recuento de fotos, recursivo).
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
	defer rows.Close()
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

// FolderAssets: fotos directamente en `base` (sin bajar a subcarpetas).
func (s *Store) FolderAssets(ctx context.Context, owner, base string) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM assets
		WHERE owner = ? AND deleted_at IS NULL AND path LIKE ? || '%' AND path NOT LIKE ? || '%/%'
		ORDER BY taken_at DESC`, owner, base, base)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// --- caché de geocodificación (Lugares) ---
// SIN owner a propósito (H8): es una caché GLOBAL de nombres de lugares
// (dato público de Nominatim), no información personal del usuario.

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
	defer rows.Close()
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

// MonthCount: fotos de un mes (1-12) del año.
type MonthCount struct {
	Month int   `json:"month"`
	Count int64 `json:"count"`
}

// YearCount: fotos por año de captura, con desglose por mes (scrubber "Rewind").
type YearCount struct {
	Year   int          `json:"year"`
	Count  int64        `json:"count"`
	Months []MonthCount `json:"months"`
}

// Calendar devuelve los años (y meses) con fotos del owner, descendente.
// Excluye archivadas.
func (s *Store) Calendar(ctx context.Context, owner string) ([]YearCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT CAST(strftime('%Y', taken_at, 'unixepoch') AS INT) AS y,
		        CAST(strftime('%m', taken_at, 'unixepoch') AS INT) AS m, count(*)
		 FROM assets WHERE owner = ? AND deleted_at IS NULL AND is_archived = 0
		 GROUP BY y, m ORDER BY y DESC, m DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// Stats del owner (los endpoints de admin globales NO existen, H8).
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
