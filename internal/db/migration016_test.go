package db_test

import (
	"testing"

	"github.com/Fail-Safe/Noema/internal/db"
)

// ---- Migration 016: trace_embeddings ----

func TestMigration016_ColumnsAndSchema(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query(`PRAGMA table_info(trace_embeddings)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]struct {
		notnull int
		pk      int
	}{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		cols[name] = struct {
			notnull int
			pk      int
		}{notnull, pk}
	}
	for _, c := range []string{"trace_id", "embedding_model", "dim", "embedding", "source_hash", "updated_at"} {
		if _, ok := cols[c]; !ok {
			t.Errorf("trace_embeddings.%s missing after migration 016", c)
		}
	}
	if cols["trace_id"].pk != 1 {
		t.Error("trace_id should be the primary key")
	}
	if cols["embedding"].notnull != 1 {
		t.Error("embedding should be NOT NULL")
	}
}

// TestMigration016_EmbeddingFKCascade locks in ON DELETE CASCADE: deleting a
// trace removes its embedding row (foreign_keys is enabled per the DSN).
func TestMigration016_EmbeddingFKCascade(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		`INSERT INTO traces (id, title, type, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"20260603-emb", "embed me", "note", "2026-06-03T00:00:00Z", "2026-06-03T00:00:00Z",
	); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO trace_embeddings (trace_id, embedding_model, dim, embedding, source_hash, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"20260603-emb", "nomic-embed-text", 3, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}, "sha256:abc", "2026-06-03T00:00:00Z",
	); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	if _, err := conn.Exec(`DELETE FROM traces WHERE id = ?`, "20260603-emb"); err != nil {
		t.Fatalf("delete trace: %v", err)
	}

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM trace_embeddings WHERE trace_id = ?`, "20260603-emb").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("embedding row not cascade-deleted; %d remain", n)
	}
}

// TestMigration016_RejectsNullEmbedding confirms the NOT NULL constraint on
// the embedding BLOB column.
func TestMigration016_RejectsNullEmbedding(t *testing.T) {
	conn, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Exec(
		`INSERT INTO traces (id, title, type, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"20260603-x", "x", "note", "2026-06-03T00:00:00Z", "2026-06-03T00:00:00Z",
	); err != nil {
		t.Fatalf("seed trace: %v", err)
	}
	_, err = conn.Exec(
		`INSERT INTO trace_embeddings (trace_id, embedding_model, dim, embedding, source_hash, updated_at) VALUES (?, ?, ?, NULL, ?, ?)`,
		"20260603-x", "m", 3, "sha256:x", "2026-06-03T00:00:00Z",
	)
	if err == nil {
		t.Error("NULL embedding should be rejected by NOT NULL")
	}
}
