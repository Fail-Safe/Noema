package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

func TestVerifyDrift_UsesCortexIdentityNotDisplayOrigin(t *testing.T) {
	dir, _ := newSandboxedCortex(t, "shared-name")
	cx, err := cortex.Open("shared-name", dir)
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	defer cx.Close()

	foreign := trace.New("foreign same-name", "note", "", nil, "publisher body")
	foreign.Origin = cx.Name
	foreign.SourceHash = trace.ContentHash(foreign.Body)
	if err := cx.Add(foreign); err != nil {
		t.Fatalf("Add foreign: %v", err)
	}
	if _, err := cx.DB.Exec(
		`UPDATE traces SET cortex_id = ? WHERE id = ?`,
		"01JFOREIGNCORTEX0000000000", foreign.ID,
	); err != nil {
		t.Fatalf("mark foreign owner: %v", err)
	}
	parsed, err := trace.ParseFile(cx.TraceFile(foreign.ID, false))
	if err != nil {
		t.Fatalf("ParseFile foreign: %v", err)
	}
	parsed.Body = "locally drifted body"
	if err := parsed.WritePreservingUpdated(cx.TraceFile(foreign.ID, false)); err != nil {
		t.Fatalf("write drifted foreign trace: %v", err)
	}

	local := trace.New("local different-name", "note", "", nil, "local body")
	local.Origin = "old-display-name"
	local.SourceHash = trace.ContentHash(local.Body)
	if err := cx.Add(local); err != nil {
		t.Fatalf("Add local: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runVerifyDrift(cmd, cx); err != nil {
		t.Fatalf("runVerifyDrift: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "DRIFTED  "+foreign.ID) {
		t.Errorf("same-name foreign trace was not checked:\n%s", got)
	}
	if strings.Contains(got, local.ID) {
		t.Errorf("local-owned trace with an old display origin was checked:\n%s", got)
	}
	if !strings.Contains(got, "Checked 1 federated trace(s).") {
		t.Errorf("checked count did not use cortex identity:\n%s", got)
	}
}
