package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/eventsig"
)

// defaultSigningKeyFile is the sidecar filename written into the cortex
// directory when keygen is not told otherwise. It mirrors the convention of
// keeping per-cortex secrets beside cortex.md.
const defaultSigningKeyFile = "noema-signing.key"

func keygenCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate this Cortex's Ed25519 federation signing key",
		Long: `Generate the Ed25519 keypair this Cortex uses to sign federated events.

Signing authenticates the cortex that produced an event, so peers can refuse
forged or tampered events instead of trusting any holder of the shared
federation key.

What it does:
  1. Reads cortex.md and requires a stable cortex id (run
     'noema migrate cortex-id' first if missing).
  2. Generates a fresh Ed25519 keypair.
  3. Writes the private seed to a 0600 sidecar file (default
     ` + defaultSigningKeyFile + `) beside cortex.md. The seed is never printed.
  4. Records the public key and the sidecar path in cortex.md under 'signing'.

Peers learn the public key automatically on their next sync via the
cortex_identity handshake and pin it on first contact (trust-on-first-use).

Use --force to rotate: it overwrites the existing key. Rotation invalidates
every signature this cortex emitted under the old key, so peers that pinned the
old key will reject this cortex until they re-pin — coordinate a rotation the
same way you would a shared-key change.`,
		Example: "  noema keygen\n  noema keygen --cortex research\n  noema keygen --force",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			name := cortexFlag
			if name == "" {
				name = os.Getenv("NOEMA_CORTEX")
			}
			if name == "" {
				name = cfg.Default
			}
			if name == "" {
				return fmt.Errorf("no cortex specified: use --cortex, set NOEMA_CORTEX, or run `noema use <name>`")
			}
			entry, ok := cfg.Cortexes[name]
			if !ok {
				return fmt.Errorf("unknown cortex %q", name)
			}
			return runKeygen(cmd.OutOrStdout(), name, entry.Path, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing signing key (rotation); peers must re-pin")
	_ = cmd.RegisterFlagCompletionFunc("cortex", cortexNameCompletions)
	return cmd
}

// runKeygen is the testable core: it operates on a cortex directory, generates
// and persists the signing key, and updates cortex.md. It deliberately never
// writes the private seed to out — only the public key and file paths.
func runKeygen(out io.Writer, name, dir string, force bool) error {
	m, err := cortex.ReadManifest(dir)
	if err != nil {
		return fmt.Errorf("reading cortex.md: %w", err)
	}
	if m.ID == "" {
		return fmt.Errorf(
			"cortex %q has no stable id yet; run `noema migrate cortex-id --cortex %s` before generating a signing key",
			name, name,
		)
	}

	// Idempotency: a cortex that already has a working signing key is left
	// alone unless --force is given, so re-running keygen is safe.
	if !force && m.Signing != nil && m.Signing.PublicKey != "" {
		if _, err := cortex.LoadSigningKey(dir, m.Signing); err == nil {
			fmt.Fprintf(out, "Cortex %q already has a signing key:\n  public key: %s\nUse --force to rotate.\n", name, m.Signing.PublicKey)
			return nil
		}
		// Configured but unreadable (missing/corrupt sidecar) — fall through
		// and regenerate so the operator isn't stuck, but say why.
		fmt.Fprintf(out, "Cortex %q has a configured signing key that could not be loaded; regenerating.\n", name)
	}

	keyFile := defaultSigningKeyFile
	if m.Signing != nil && m.Signing.PrivateKeyFile != "" {
		keyFile = m.Signing.PrivateKeyFile
	}
	keyPath := keyFile
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(dir, keyFile)
	}

	_, pub, seed, err := eventsig.Generate()
	if err != nil {
		return fmt.Errorf("generating signing key: %w", err)
	}

	// Write the seed with owner-only permissions. O_TRUNC so a rotation
	// cleanly replaces the old seed; an explicit Chmod guards the case where
	// the file already existed with looser bits.
	if err := writeSecretFile(keyPath, seed+"\n"); err != nil {
		return fmt.Errorf("writing signing key file: %w", err)
	}

	m.Signing = &cortex.SigningConfig{PublicKey: pub, PrivateKeyFile: keyFile}
	if err := cortex.WriteManifest(dir, m); err != nil {
		return fmt.Errorf("writing cortex.md: %w", err)
	}

	fmt.Fprintf(out, "Generated signing key for cortex %q.\n", name)
	fmt.Fprintf(out, "  public key:  %s\n", pub)
	fmt.Fprintf(out, "  private key: %s (mode 0600 — keep it secret, never commit it)\n", keyPath)
	fmt.Fprintf(out, "  cortex.md:   signing block updated\n")
	fmt.Fprintln(out, "\nPeers pin this public key on their next sync (trust-on-first-use).")
	return nil
}

// writeSecretFile writes content to path with 0600 permissions, truncating any
// existing file and forcing the mode even if the file pre-existed with looser
// bits. Used for the signing-key seed sidecar.
func writeSecretFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
