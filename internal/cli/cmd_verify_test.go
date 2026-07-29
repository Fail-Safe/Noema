package cli

import (
	"bytes"
	"testing"

	"github.com/Fail-Safe/Noema/internal/trace"
	"github.com/spf13/cobra"
)

func TestVerifyTracesBackfillPreservesUpdated(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "verify-backfill")
	cx := openCortexDir(t, "verify-backfill", dir)

	tr := trace.New("Preserve timestamp", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	path := cx.TraceFile(tr.ID, false)
	parsed, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	originalUpdated := parsed.Updated
	parsed.ContentHash = "sha256:stale"
	if err := parsed.WritePreservingUpdated(path); err != nil {
		t.Fatalf("seed stale hash: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runVerifyTraces(cmd, cx, true); err != nil {
		t.Fatalf("runVerifyTraces: %v", err)
	}

	repaired, err := trace.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile repaired: %v", err)
	}
	if repaired.ContentHash != trace.ContentHash(repaired.Body) {
		t.Errorf("ContentHash = %q, want body hash", repaired.ContentHash)
	}
	if repaired.Updated != originalUpdated {
		t.Errorf("Updated = %q, want preserved %q", repaired.Updated, originalUpdated)
	}
}

func TestRecoverBodyToTrashPreservesUpdated(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "verify-trash")
	cx := openCortexDir(t, "verify-trash", dir)

	tr := trace.New("Recover to trash", "note", "", nil, "body")
	if err := cx.Add(tr); err != nil {
		t.Fatalf("Add: %v", err)
	}
	row, err := cx.Get(tr.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := cx.IngestExternalDelete(tr.ID); err != nil {
		t.Fatalf("IngestExternalDelete: %v", err)
	}
	recovered, err := trace.ParseFile(cx.TrashFile(tr.ID))
	if err != nil {
		t.Fatalf("ParseFile trash: %v", err)
	}
	if recovered.Updated != row.UpdatedAt {
		t.Errorf("Updated = %q, want preserved %q", recovered.Updated, row.UpdatedAt)
	}
}
