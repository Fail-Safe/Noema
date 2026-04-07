package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/Fail-Safe/Noema/internal/cortex"
)

// --------- renderInstructions (pure helper) ---------

// TestRenderInstructions_IncludesNoemaVersion pins the most important
// outcome of plumbing the version through: the agent reference guide
// must show the build version on the very line agents read first. The
// whole reason for this change was that operators couldn't tell which
// noema binary an MCP client was talking to — this assertion is what
// keeps that fix from regressing.
func TestRenderInstructions_IncludesNoemaVersion(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "agentbrain",
		Version: 2,
	}, "v0.2.5-test")

	want := "Version:  noema v0.2.5-test (manifest v2)"
	if !strings.Contains(out, want) {
		t.Errorf("instructions missing version line %q\nfull output:\n%s", want, out)
	}
}

// TestRenderInstructions_HandlesMissingOptionalFields pins the empty
// purpose/owner branches. The original code only emitted those lines
// when the manifest set them; the refactor must preserve that —
// otherwise empty cortex.md files would render with stray "Purpose: \n"
// noise that looks like a bug.
func TestRenderInstructions_HandlesMissingOptionalFields(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "minimal",
		Version: 2,
	}, "dev")

	if strings.Contains(out, "Purpose:") {
		t.Errorf("instructions should omit Purpose line when manifest has none:\n%s", out)
	}
	if strings.Contains(out, "Owner:") {
		t.Errorf("instructions should omit Owner line when manifest has none:\n%s", out)
	}
	// And the cortex name still has to appear in both spots that
	// reference it (Name: header and the Terminology line).
	if strings.Count(out, "minimal") < 2 {
		t.Errorf("expected cortex name in both header and terminology section:\n%s", out)
	}
}

// TestRenderInstructions_RendersPurposeAndOwner pins the populated
// branches: when the manifest carries Purpose/Owner, both lines must
// appear with the same column alignment as Name/Version. Misaligned
// fields would make the reference guide look broken to humans reading
// it via `noema serve --print-config` or in agent transcripts.
func TestRenderInstructions_RendersPurposeAndOwner(t *testing.T) {
	out := renderInstructions(cortex.Manifest{
		Name:    "research",
		Purpose: "Primary research cortex",
		Owner:   "mark",
		Version: 2,
	}, "v0.2.5")

	for _, want := range []string{
		"Name:     research",
		"Version:  noema v0.2.5 (manifest v2)",
		"Purpose:  Primary research cortex",
		"Owner:    mark",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions missing line %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderInstructions_ManifestVersionReflectsInput pins that the
// manifest version comes from the manifest field, not a hardcoded
// constant. When the cortex schema bumps, an old cortex still on
// manifest v1 will be visible as such — and that distinction matters
// for federation peers who need to know whether the remote end has
// migrated.
func TestRenderInstructions_ManifestVersionReflectsInput(t *testing.T) {
	for _, v := range []int{1, 2, 99} {
		out := renderInstructions(cortex.Manifest{Name: "test", Version: v}, "dev")
		want := "(manifest v" + itoa(v) + ")"
		if !strings.Contains(out, want) {
			t.Errorf("manifest v%d not reflected:\nlooking for %q\nfull:\n%s", v, want, out)
		}
	}
}

// itoa avoids pulling in strconv for a single call. Tests are the place
// for tiny helpers like this — keeps the imports minimal and signals
// "this isn't a hot path".
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --------- NewServer protocol-level wiring ---------

// newTestCortex spins up a real cortex in a temp directory. The MCP
// server needs a working cortex to back its tools, and the cortex
// constructor expects a real on-disk layout (db, traces dir, manifest),
// so there's no faking this.
func newTestCortex(t *testing.T) *cortex.Cortex {
	t.Helper()
	dir := t.TempDir()
	if _, err := cortex.Create("test", dir); err != nil {
		t.Fatalf("cortex.Create: %v", err)
	}
	cx, err := cortex.Open("test", filepath.Join(dir, "test"))
	if err != nil {
		t.Fatalf("cortex.Open: %v", err)
	}
	t.Cleanup(func() { cx.Close() })
	return cx
}

// initializeServer drives the MCP `initialize` handshake against a
// freshly built server and returns the parsed result. Any failure here
// is a fatal: a server that can't initialize is a server that no client
// can ever talk to, so a failed handshake is never a "soft" condition.
func initializeServer(t *testing.T, cx *cortex.Cortex, version string) mcp.InitializeResult {
	t.Helper()
	s := NewServer(cx, version)

	initReq := mcp.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(1)),
		Request: mcp.Request{Method: "initialize"},
		Params: struct {
			ProtocolVersion string                 `json:"protocolVersion"`
			ClientInfo      mcp.Implementation     `json:"clientInfo"`
			Capabilities    mcp.ClientCapabilities `json:"capabilities"`
		}{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "noema-test-client",
				Version: "1.0.0",
			},
		},
	}

	body, err := json.Marshal(initReq)
	if err != nil {
		t.Fatalf("marshal initialize request: %v", err)
	}

	resp := s.HandleMessage(context.Background(), body)
	if resp == nil {
		t.Fatal("HandleMessage returned nil response")
	}
	jr, ok := resp.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("expected JSONRPCResponse, got %T", resp)
	}
	result, ok := jr.Result.(mcp.InitializeResult)
	if !ok {
		t.Fatalf("expected InitializeResult, got %T", jr.Result)
	}
	return result
}

// TestNewServer_ServerInfoCarriesVersion is the protocol-level
// counterpart to TestRenderInstructions_IncludesNoemaVersion. Even an
// MCP client that never calls get_instructions (e.g. one that only
// inspects the initialize handshake) must be able to identify the
// noema build. This test pins serverInfo.Name and serverInfo.Version
// against the same value the build script injects via -ldflags.
func TestNewServer_ServerInfoCarriesVersion(t *testing.T) {
	cx := newTestCortex(t)
	result := initializeServer(t, cx, "v0.2.5-protocol-test")

	if result.ServerInfo.Name != "noema" {
		t.Errorf("ServerInfo.Name = %q, want %q", result.ServerInfo.Name, "noema")
	}
	if result.ServerInfo.Version != "v0.2.5-protocol-test" {
		t.Errorf("ServerInfo.Version = %q, want %q", result.ServerInfo.Version, "v0.2.5-protocol-test")
	}
}

// TestNewServer_EmptyVersionNormalizesToDev pins the empty-string
// guard. An MCP server that advertised an empty version would render
// as `noema ` in client UIs — which looks like a bug and breaks any
// client that key-by-version. We normalize to "dev" so the field is
// always non-empty AND signals "you're talking to a development
// build, expect surprises".
func TestNewServer_EmptyVersionNormalizesToDev(t *testing.T) {
	cx := newTestCortex(t)
	result := initializeServer(t, cx, "")

	if result.ServerInfo.Version != "dev" {
		t.Errorf("ServerInfo.Version = %q, want %q (empty should normalize)", result.ServerInfo.Version, "dev")
	}
}
