package cortex

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/db"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// Manifest is the cortex.md file at the root of each Cortex.
type Manifest struct {
	Name    string `yaml:"name"`
	Purpose string `yaml:"purpose,omitempty"`
	Owner   string `yaml:"owner,omitempty"`
	Created string `yaml:"created"`
	Version int    `yaml:"version"`
}

type Cortex struct {
	Name string
	Dir  string
	DB   *db.DB
}

// Create initialises a new Cortex on disk and registers it.
// dir is the parent directory; the cortex is created as dir/<name>/.
func Create(name, dir string) error {
	root := filepath.Join(dir, name)
	for _, sub := range []string{"traces", "archive/traces", "db"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", sub, err)
		}
	}

	manifest := Manifest{
		Name:    name,
		Created: time.Now().UTC().Format("2006-01-02"),
		Version: 1,
	}
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "cortex.md"), data, 0o640); err != nil {
		return fmt.Errorf("writing cortex.md: %w", err)
	}

	// Open (and migrate) the DB to initialise the schema.
	conn, err := db.Open(root)
	if err != nil {
		return fmt.Errorf("initialising database: %w", err)
	}
	return conn.Close()
}

// Open opens an existing Cortex by directory path.
// It ensures required subdirectories exist and auto-purges expired trash.
func Open(name, dir string) (*Cortex, error) {
	for _, sub := range []string{"traces", "archive/traces", "trash/traces"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("ensuring %s: %w", sub, err)
		}
	}
	conn, err := db.Open(dir)
	if err != nil {
		return nil, err
	}
	cx := &Cortex{Name: name, Dir: dir, DB: conn}

	// Auto-purge expired trash. Best-effort: errors are silently ignored.
	cfg, _ := config.Load()
	days := 30
	if cfg != nil && cfg.TrashDays > 0 {
		days = cfg.TrashDays
	}
	_ = cx.Purge(days)

	return cx, nil
}

func (c *Cortex) Close() error {
	return c.DB.Close()
}

func (c *Cortex) TracesDir() string {
	return filepath.Join(c.Dir, "traces")
}

func (c *Cortex) ArchiveDir() string {
	return filepath.Join(c.Dir, "archive", "traces")
}

func (c *Cortex) TrashDir() string {
	return filepath.Join(c.Dir, "trash", "traces")
}

// TraceFile returns the absolute path to a trace's markdown file.
func (c *Cortex) TraceFile(id string, archived bool) string {
	if archived {
		return filepath.Join(c.ArchiveDir(), id+".md")
	}
	return filepath.Join(c.TracesDir(), id+".md")
}

// TrashFile returns the path for a trace in the trash.
func (c *Cortex) TrashFile(id string) string {
	return filepath.Join(c.TrashDir(), id+".md")
}

// filePath resolves the correct on-disk path for any row state.
func (c *Cortex) filePath(r *Row) string {
	if r.TrashedAt != "" {
		return c.TrashFile(r.ID)
	}
	return c.TraceFile(r.ID, r.ArchivedAt != "")
}

// Add writes a new Trace to disk and inserts it into the DB.
func (c *Cortex) Add(t *trace.Trace) error {
	path := c.TraceFile(t.ID, false)
	if err := t.Write(path); err != nil {
		return fmt.Errorf("writing trace file: %w", err)
	}
	if err := c.insertDB(t); err != nil {
		os.Remove(path)
		return fmt.Errorf("inserting into database: %w", err)
	}
	return nil
}

func (c *Cortex) insertDB(t *trace.Trace) error {
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO traces (id, title, type, author, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Type, t.Author, t.Created, t.Updated,
	)
	if err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, t.ID, tag); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO traces_fts (id, title, body) VALUES (?, ?, ?)`, t.ID, t.Title, t.Body)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Row is a DB row joined with tags, returned by list/search operations.
type Row struct {
	ID         string
	Title      string
	Type       string
	Author     string
	Tags       []string
	ArchivedAt string
	TrashedAt  string
	CreatedAt  string
	UpdatedAt  string
}

type ListOptions struct {
	Type     string
	Author   string
	Tag      string
	Archived bool // only archived (excludes trashed)
	Trashed  bool // only trashed
	All      bool // active + archived (excludes trashed)
}

func (c *Cortex) List(opts ListOptions) ([]Row, error) {
	q := `SELECT id, title, type, author, archived_at, trashed_at, created_at, updated_at FROM traces WHERE 1=1`
	var args []any

	switch {
	case opts.Trashed:
		q += ` AND trashed_at IS NOT NULL`
	case opts.All:
		q += ` AND trashed_at IS NULL`
	case opts.Archived:
		q += ` AND archived_at IS NOT NULL AND trashed_at IS NULL`
	default:
		q += ` AND archived_at IS NULL AND trashed_at IS NULL`
	}
	if opts.Type != "" {
		q += ` AND type = ?`
		args = append(args, opts.Type)
	}
	if opts.Author != "" {
		q += ` AND author = ?`
		args = append(args, opts.Author)
	}
	if opts.Tag != "" {
		q += ` AND id IN (SELECT trace_id FROM trace_tags WHERE tag = ?)`
		args = append(args, opts.Tag)
	}
	q += ` ORDER BY created_at DESC, rowid DESC`

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return c.scanRows(rows)
}

func (c *Cortex) Search(query string, opts ListOptions) ([]Row, error) {
	q := `
		SELECT t.id, t.title, t.type, t.author, t.archived_at, t.trashed_at, t.created_at, t.updated_at
		FROM traces t
		WHERE t.id IN (SELECT id FROM traces_fts WHERE traces_fts MATCH ?)`
	args := []any{query}

	switch {
	case opts.Trashed:
		q += ` AND t.trashed_at IS NOT NULL`
	case opts.All:
		q += ` AND t.trashed_at IS NULL`
	case opts.Archived:
		q += ` AND t.archived_at IS NOT NULL AND t.trashed_at IS NULL`
	default:
		q += ` AND t.archived_at IS NULL AND t.trashed_at IS NULL`
	}
	if opts.Type != "" {
		q += ` AND t.type = ?`
		args = append(args, opts.Type)
	}
	if opts.Author != "" {
		q += ` AND t.author = ?`
		args = append(args, opts.Author)
	}
	if opts.Tag != "" {
		q += ` AND t.id IN (SELECT trace_id FROM trace_tags WHERE tag = ?)`
		args = append(args, opts.Tag)
	}
	q += ` ORDER BY t.created_at DESC`

	rows, err := c.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return c.scanRows(rows)
}

func (c *Cortex) Get(id string) (*Row, error) {
	var r Row
	var archivedAt, trashedAt *string
	err := c.DB.QueryRow(
		`SELECT id, title, type, author, archived_at, trashed_at, created_at, updated_at FROM traces WHERE id = ?`, id,
	).Scan(&r.ID, &r.Title, &r.Type, &r.Author, &archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		r.ArchivedAt = *archivedAt
	}
	if trashedAt != nil {
		r.TrashedAt = *trashedAt
	}
	r.Tags, err = c.tagsFor(id)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Remove permanently deletes a trace from disk and the database.
// Use Trash for recoverable deletion.
func (c *Cortex) Remove(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if err := os.Remove(c.filePath(r)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing file: %w", err)
	}
	_, err = c.DB.Exec(`DELETE FROM traces WHERE id = ?`, id)
	return err
}

// Trash moves a trace to the trash directory for deferred deletion.
func (c *Cortex) Trash(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return fmt.Errorf("trace %s is already in trash", id)
	}
	src := c.filePath(r)
	dst := c.TrashFile(id)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file to trash: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = c.DB.Exec(`UPDATE traces SET trashed_at = ?, archived_at = NULL WHERE id = ?`, now, id)
	return err
}

// Recover moves a trace out of the trash and back to the active traces directory.
func (c *Cortex) Recover(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt == "" {
		return fmt.Errorf("trace %s is not in trash", id)
	}
	src := c.TrashFile(id)
	dst := c.TraceFile(id, false)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("recovering file from trash: %w", err)
	}
	_, err = c.DB.Exec(`UPDATE traces SET trashed_at = NULL WHERE id = ?`, id)
	return err
}

// Purge permanently deletes traces that have been in the trash for more than
// days days. A days value of 0 is treated as 30.
func (c *Cortex) Purge(days int) error {
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	rows, err := c.DB.Query(`SELECT id FROM traces WHERE trashed_at IS NOT NULL AND trashed_at < ?`, cutoff)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.Remove(c.TrashFile(id)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("purging %s: %w", id, err)
		}
		if _, err := c.DB.Exec(`DELETE FROM traces WHERE id = ?`, id); err != nil {
			return fmt.Errorf("purging %s from db: %w", id, err)
		}
	}
	return nil
}

func (c *Cortex) Archive(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.TrashedAt != "" {
		return fmt.Errorf("trace %s is in trash — recover it first", id)
	}
	if r.ArchivedAt != "" {
		return fmt.Errorf("trace %s is already archived", id)
	}
	src := c.TraceFile(id, false)
	dst := c.TraceFile(id, true)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = c.DB.Exec(`UPDATE traces SET archived_at = ? WHERE id = ?`, now, id)
	return err
}

func (c *Cortex) Unarchive(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	if r.ArchivedAt == "" {
		return fmt.Errorf("trace %s is not archived", id)
	}
	src := c.TraceFile(id, true)
	dst := c.TraceFile(id, false)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("moving file: %w", err)
	}
	_, err = c.DB.Exec(`UPDATE traces SET archived_at = NULL WHERE id = ?`, id)
	return err
}

// Update rewrites an existing trace's DB row and FTS entry from its (potentially
// edited) markdown file on disk.
func (c *Cortex) Update(id string) error {
	r, err := c.Get(id)
	if err != nil {
		return err
	}
	t, err := trace.ParseFile(c.filePath(r))
	if err != nil {
		return err
	}
	tx, err := c.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`UPDATE traces SET title=?, type=?, author=?, updated_at=? WHERE id=?`,
		t.Title, t.Type, t.Author, t.Updated, id,
	)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trace_tags WHERE trace_id = ?`, id); err != nil {
		return err
	}
	for _, tag := range t.Tags {
		if _, err := tx.Exec(`INSERT INTO trace_tags (trace_id, tag) VALUES (?, ?)`, id, tag); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM traces_fts WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO traces_fts (id, title, body) VALUES (?, ?, ?)`, id, t.Title, t.Body); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Cortex) scanRows(rows *sql.Rows) ([]Row, error) { //nolint:govet
	var result []Row
	for rows.Next() {
		var r Row
		var archivedAt, trashedAt *string
		if err := rows.Scan(&r.ID, &r.Title, &r.Type, &r.Author, &archivedAt, &trashedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if archivedAt != nil {
			r.ArchivedAt = *archivedAt
		}
		if trashedAt != nil {
			r.TrashedAt = *trashedAt
		}
		tags, err := c.tagsFor(r.ID)
		if err != nil {
			return nil, err
		}
		r.Tags = tags
		result = append(result, r)
	}
	return result, rows.Err()
}

func (c *Cortex) tagsFor(id string) ([]string, error) {
	rows, err := c.DB.Query(`SELECT tag FROM trace_tags WHERE trace_id = ? ORDER BY tag`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}

