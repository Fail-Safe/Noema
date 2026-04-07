package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/config"
	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/db"
)

func cortexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cortex",
		Short: "Manage cortexes",
	}
	cmd.AddCommand(
		cortexListCmd(),
		cortexRemoveCmd(),
		cortexBackupCmd(),
		cortexRestoreCmd(),
	)
	return cmd
}

func cortexListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all known cortexes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runCortexList(cmd.OutOrStdout(), cfg)
		},
	}
}

func runCortexList(out io.Writer, cfg *config.Config) error {
	if len(cfg.Cortexes) == 0 {
		fmt.Fprintln(out, "No cortexes configured. Run `noema init --name <name>` to create one.")
		return nil
	}

	// Sort names for stable output.
	names := make([]string, 0, len(cfg.Cortexes))
	for name := range cfg.Cortexes {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPATH\t")
	for _, name := range names {
		marker := " "
		if name == cfg.Default {
			marker = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, cfg.Cortexes[name].Path, marker)
	}
	w.Flush()

	// When there's no default set, the table above shows no `*`
	// marker and leaves the user guessing what to do. Print a hint
	// that names the exact `noema use` command and — in the single-
	// cortex case — notes that the next runtime command will auto-
	// promote it anyway, so the user knows both paths are valid.
	if cfg.Default == "" {
		fmt.Fprintln(out)
		if len(cfg.Cortexes) == 1 {
			fmt.Fprintf(out, "(No default cortex set. Run `noema use %s`, or it will be auto-promoted on next use.)\n", names[0])
		} else {
			fmt.Fprintln(out, "(No default cortex set. Run `noema use <name>` to pick one.)")
		}
	}
	return nil
}

// cortexRemoveCmd unregisters a cortex from the config, optionally deleting
// the on-disk directory as well. The default (no flags) is intentionally
// non-destructive: the cortex directory is left in place so an operator who
// second-guesses the decision can re-register it with a manual config edit.
// --purge is the destructive mode and requires interactive confirmation.
func cortexRemoveCmd() *cobra.Command {
	var purge, force bool

	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm", "delete"},
		Short:   "Unregister a cortex (use --purge to also delete its directory)",
		Args:    cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return cortexNameCompletions(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runCortexRemove(cmd.OutOrStdout(), cmd.InOrStdin(), cfg, args[0], purge, force)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the cortex directory from disk")
	cmd.Flags().BoolVar(&force, "force", false, "skip safety checks and confirmations")
	return cmd
}

func runCortexRemove(out io.Writer, in io.Reader, cfg *config.Config, name string, purge, force bool) error {
	entry, ok := cfg.Cortexes[name]
	if !ok {
		return fmt.Errorf("unknown cortex %q", name)
	}

	// Guard 1: the current default. Removing it would leave noema in a
	// state where every command without an explicit --cortex returns
	// "no cortex specified" — not broken, but surprising. Force the
	// operator to acknowledge the consequence.
	if cfg.Default == name && !force {
		return fmt.Errorf(
			"cortex %q is the current default — switch with `noema use <other>` first,\n"+
				"  or re-run with --force to clear the default as part of removal",
			name,
		)
	}

	// Guard 2: other locally-registered cortexes that list this one as a
	// federation peer. Removing it would leave dangling peer entries in
	// their cortex.md files. We warn rather than refuse by default so
	// operators can still clean up a broken cortex, but --force is
	// required to skip the check silently.
	var peerRefs []string
	for otherName, otherEntry := range cfg.Cortexes {
		if otherName == name {
			continue
		}
		m, err := cortex.ReadManifest(otherEntry.Path)
		if err != nil || m.Federation == nil {
			continue
		}
		for _, p := range m.Federation.Peers {
			if p.Name == name {
				peerRefs = append(peerRefs, otherName)
				break
			}
		}
	}
	if len(peerRefs) > 0 && !force {
		sort.Strings(peerRefs)
		return fmt.Errorf(
			"cortex %q is referenced as a federation peer in: %s\n"+
				"  Remove those peer entries from their cortex.md files first,\n"+
				"  or re-run with --force to leave the dangling references in place",
			name, strings.Join(peerRefs, ", "),
		)
	}

	// Interactive confirmation for --purge. Config-only removal is cheap
	// to reverse (the directory survives), so we don't prompt for it.
	if purge && !force {
		fmt.Fprintf(out, "This will permanently delete the cortex directory:\n")
		fmt.Fprintf(out, "  %s\n", entry.Path)
		fmt.Fprintf(out, "Traces, event log, and federation state will all be lost.\n")
		fmt.Fprint(out, "Proceed? [y/N]: ")
		var resp string
		_, _ = fmt.Fscanln(in, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted by user")
		}
	}

	wasDefault := cfg.Default == name
	delete(cfg.Cortexes, name)

	// If we just evicted the default, try to leave the user in a sane
	// state rather than a config with no default and no hint about what
	// to do next. When exactly one cortex remains it's the only possible
	// answer, so promote it automatically. With more than one we'd be
	// guessing, so leave the default empty and point at `noema use`
	// below. The user already accepted destructive consequences by
	// passing --force, so a silent promotion (with a clear notice) is
	// less pestering than a second prompt.
	var promoted string
	if wasDefault {
		cfg.Default = ""
		if len(cfg.Cortexes) == 1 {
			for onlyName := range cfg.Cortexes {
				cfg.Default = onlyName
				promoted = onlyName
			}
		}
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	if purge {
		if err := os.RemoveAll(entry.Path); err != nil {
			return fmt.Errorf("removing %s: %w", entry.Path, err)
		}
		fmt.Fprintf(out, "Cortex %q removed from config and deleted from %s\n", name, entry.Path)
	} else {
		fmt.Fprintf(out, "Cortex %q unregistered from config (directory at %s was not touched)\n", name, entry.Path)
	}

	// Tell the operator what happened to the default, but only if they
	// actually touched it. Removing a non-default cortex leaves the
	// default alone and doesn't need a status line.
	if promoted != "" {
		fmt.Fprintf(out, "Promoted %q as the new default cortex.\n", promoted)
	} else if wasDefault {
		if len(cfg.Cortexes) == 0 {
			fmt.Fprintln(out, "No cortexes remain. Run `noema init --name <name>` to create one.")
		} else {
			remaining := make([]string, 0, len(cfg.Cortexes))
			for n := range cfg.Cortexes {
				remaining = append(remaining, n)
			}
			sort.Strings(remaining)
			fmt.Fprintln(out, "No default cortex set. Use `noema use <name>` to pick one.")
			fmt.Fprintf(out, "Registered cortexes: %s\n", strings.Join(remaining, ", "))
		}
	}

	if len(peerRefs) > 0 {
		fmt.Fprintf(out, "warning: dangling peer references remain in: %s\n", strings.Join(peerRefs, ", "))
	}
	return nil
}

// cortexBackupCmd writes a gzipped tarball of the cortex directory after
// running a WAL checkpoint so the snapshot is consistent. The output file
// defaults to `<name>-YYYYMMDD-HHMMSSZ.tar.gz` in the current directory.
func cortexBackupCmd() *cobra.Command {
	var output string
	var force bool

	cmd := &cobra.Command{
		Use:   "backup <name>",
		Short: "Write a gzipped tarball of a cortex",
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveDefault
			}
			return cortexNameCompletions(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runCortexBackup(cmd.OutOrStdout(), cfg, args[0], output, force)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "tarball output path (default: ./<name>-<timestamp>.tar.gz)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite the output file if it exists")
	return cmd
}

func runCortexBackup(out io.Writer, cfg *config.Config, name, outputPath string, force bool) error {
	entry, ok := cfg.Cortexes[name]
	if !ok {
		return fmt.Errorf("unknown cortex %q", name)
	}
	if _, err := os.Stat(entry.Path); err != nil {
		return fmt.Errorf("cortex directory %s: %w", entry.Path, err)
	}

	if outputPath == "" {
		stamp := time.Now().UTC().Format("20060102-150405Z")
		outputPath = fmt.Sprintf("%s-%s.tar.gz", name, stamp)
	}
	if _, err := os.Stat(outputPath); err == nil && !force {
		return fmt.Errorf("output file %s already exists (use --force to overwrite)", outputPath)
	}

	// Best-effort WAL checkpoint. A failure here is non-fatal: the
	// tarball will still include the -wal sidecar, which is a valid
	// SQLite file to restore from, just fatter than necessary.
	if err := db.CheckpointWAL(entry.Path); err != nil {
		fmt.Fprintf(out, "warning: WAL checkpoint failed (%v) — backup will still proceed\n", err)
	}

	size, err := tarGzDir(entry.Path, outputPath)
	if err != nil {
		// Don't leave a half-written tarball behind.
		_ = os.Remove(outputPath)
		return fmt.Errorf("writing tarball: %w", err)
	}
	fmt.Fprintf(out, "Backed up cortex %q to %s (%s)\n", name, outputPath, humanBytes(size))
	if entry.ID != "" {
		fmt.Fprintf(out, "Cortex ID: %s\n", entry.ID)
	}
	return nil
}

// cortexRestoreCmd extracts a tarball produced by `cortex backup` and
// registers the cortex in config. By default it preserves the original
// name and ID — callers who want a clone (for testing, or to copy a
// cortex onto a new host while the original keeps running) should pass
// --name to relabel and then run `noema migrate cortex-id --reset` to
// assign a fresh identity. Without the reset, two live cortexes would
// share a ULID and break federation.
func cortexRestoreCmd() *cobra.Command {
	var overrideName, overridePath string
	var force bool

	cmd := &cobra.Command{
		Use:   "restore <tarball>",
		Short: "Restore a cortex from a backup tarball",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runCortexRestore(cmd.OutOrStdout(), cfg, args[0], overrideName, overridePath, force)
		},
	}
	cmd.Flags().StringVar(&overrideName, "name", "", "register the restored cortex under this name (default: name from cortex.md)")
	cmd.Flags().StringVar(&overridePath, "path", "", "parent directory for the restored cortex (default: ~/.noema)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing directory at the destination")
	return cmd
}

func runCortexRestore(out io.Writer, cfg *config.Config, tarballPath, overrideName, overridePath string, force bool) error {
	if _, err := os.Stat(tarballPath); err != nil {
		return fmt.Errorf("tarball %s: %w", tarballPath, err)
	}

	// Stage the extraction in a tempdir so we can validate the manifest
	// and run collision checks before touching the real cortex location.
	// Any failure during staging leaves the real filesystem untouched.
	stagingParent, err := os.MkdirTemp("", "noema-restore-")
	if err != nil {
		return fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(stagingParent)

	stagingCortex, err := untarGzTo(tarballPath, stagingParent)
	if err != nil {
		return fmt.Errorf("extracting tarball: %w", err)
	}

	m, err := cortex.ReadManifest(stagingCortex)
	if err != nil {
		return fmt.Errorf("reading cortex.md from tarball: %w", err)
	}
	if m.Name == "" {
		return fmt.Errorf("tarball cortex.md has empty name field — is this a valid Noema backup?")
	}

	finalName := m.Name
	if overrideName != "" {
		finalName = overrideName
	}

	// Name collision: another registered cortex already uses this label.
	// Point the operator at the exact fix rather than silently clobbering.
	if existing, dup := cfg.Cortexes[finalName]; dup {
		return fmt.Errorf(
			"cortex name %q is already registered at %s\n"+
				"  Re-run with --name <other> to restore under a different label,\n"+
				"  or remove the existing entry first with `noema cortex remove %s`",
			finalName, existing.Path, finalName,
		)
	}

	// ID collision: another registered cortex already has this ULID,
	// meaning the tarball is either a backup of that same cortex or a
	// clone of it. Either way, restoring as-is would produce two live
	// cortexes with the same federation identity — the exact scenario
	// `noema migrate cortex-id --reset` exists to fix.
	if m.ID != "" {
		for existingName, existingEntry := range cfg.Cortexes {
			if existingEntry.ID == m.ID {
				return fmt.Errorf(
					"cortex id %s is already registered as %q at %s\n"+
						"  The tarball is a backup (or clone) of that cortex. To restore as a\n"+
						"  clone with a fresh identity, restore under a new name with --name,\n"+
						"  then run `noema migrate cortex-id --reset --cortex <new-name>` to\n"+
						"  assign a new ULID. To replace the existing entry, remove it first\n"+
						"  with `noema cortex remove %s --purge`.",
					m.ID, existingName, existingEntry.Path, existingName,
				)
			}
		}
	}

	// Resolve the final destination. Mirror `noema init`'s default
	// (~/.noema) so restored cortexes land next to freshly-initialized
	// ones unless the caller explicitly asks for a different parent.
	finalParent := overridePath
	if finalParent == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("finding home dir: %w", err)
		}
		finalParent = filepath.Join(home, ".noema")
	}
	finalParent, err = filepath.Abs(finalParent)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(finalParent, finalName)

	if _, err := os.Stat(finalPath); err == nil {
		if !force {
			return fmt.Errorf("destination %s already exists (use --force to overwrite)", finalPath)
		}
		if err := os.RemoveAll(finalPath); err != nil {
			return fmt.Errorf("removing existing destination: %w", err)
		}
	}

	if err := os.MkdirAll(finalParent, 0o750); err != nil {
		return fmt.Errorf("creating parent dir: %w", err)
	}
	if err := moveDir(stagingCortex, finalPath); err != nil {
		return fmt.Errorf("moving restored cortex to %s: %w", finalPath, err)
	}

	// If the operator relabeled the restore, rewrite cortex.md so the
	// display name on disk matches the config entry. The ID is
	// deliberately left alone — relabeling is not the same as cloning.
	if overrideName != "" && overrideName != m.Name {
		m.Name = overrideName
		if err := cortex.WriteManifest(finalPath, m); err != nil {
			return fmt.Errorf("rewriting cortex.md with new name: %w", err)
		}
	}

	cfg.Cortexes[finalName] = config.CortexEntry{Path: finalPath, ID: m.ID}
	if cfg.Default == "" {
		cfg.Default = finalName
	}
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Fprintf(out, "Restored cortex %q to %s\n", finalName, finalPath)
	if m.ID != "" {
		fmt.Fprintf(out, "Cortex ID: %s\n", m.ID)
	}
	if cfg.Default == finalName {
		fmt.Fprintf(out, "Set as default cortex.\n")
	}
	return nil
}

// tarGzDir writes a gzipped tar archive of srcDir to outPath. The archive's
// entries are rooted at filepath.Base(srcDir) so `tar xf` extracts into a
// single top-level directory, matching the convention used by `tar czf
// backup.tar.gz -C <parent> <name>` on the command line. Returns the size
// of the written tarball in bytes.
func tarGzDir(srcDir, outPath string) (int64, error) {
	srcDir = filepath.Clean(srcDir)
	base := filepath.Base(srcDir)

	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		// Entry name in the archive: "<base>/<rel>" with forward slashes
		// regardless of the host OS, per tar convention.
		entryName := base
		if rel != "." {
			entryName = filepath.ToSlash(filepath.Join(base, rel))
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = entryName
		if info.IsDir() {
			hdr.Name += "/"
		}
		// Strip user/group IDs — they're meaningless on restore to a
		// different machine and leak the backing-up user's identity.
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		if _, err := io.Copy(tw, src); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	// Flush writers before stat'ing so we capture the final size.
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// untarGzTo extracts a gzipped tarball into destParent and returns the
// absolute path of the single top-level directory in the archive (which
// corresponds to the cortex root). Rejects entries whose cleaned paths
// escape destParent — this is the standard "zip slip" guard, critical
// here because restore is driven by a file the user supplied from
// outside the repo.
func untarGzTo(tarballPath, destParent string) (string, error) {
	f, err := os.Open(tarballPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("reading gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	destParent, err = filepath.Abs(destParent)
	if err != nil {
		return "", err
	}

	topLevels := make(map[string]struct{})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tar entry: %w", err)
		}

		// Zip-slip guard: reject absolute or escaping paths. filepath.Join
		// cleans "..", so we compare the joined result against destParent
		// with a trailing separator to catch the "/foo/bar/../../evil" case.
		cleaned := filepath.Clean(hdr.Name)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
			return "", fmt.Errorf("tar entry %q has unsafe path", hdr.Name)
		}
		target := filepath.Join(destParent, cleaned)
		rel, err := filepath.Rel(destParent, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}

		// Track the first path segment of every entry to identify the
		// archive's top-level directory. A valid backup has exactly one.
		if parts := strings.SplitN(filepath.ToSlash(cleaned), "/", 2); len(parts) > 0 && parts[0] != "" {
			topLevels[parts[0]] = struct{}{}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
		default:
			// Ignore symlinks, device files, etc. A Noema cortex is
			// plain files + directories; anything else in the tarball
			// is either irrelevant or actively suspicious.
			continue
		}
	}

	if len(topLevels) == 0 {
		return "", fmt.Errorf("tarball is empty")
	}
	if len(topLevels) > 1 {
		return "", fmt.Errorf("tarball has multiple top-level entries — expected exactly one cortex directory")
	}
	var top string
	for k := range topLevels {
		top = k
	}
	return filepath.Join(destParent, top), nil
}

// moveDir renames src onto dst, falling back to a recursive copy if the
// source and destination live on different filesystems (os.Rename returns
// EXDEV in that case). The staging tempdir from os.MkdirTemp is typically
// under /tmp, which on Linux often is a tmpfs — cross-device restores to
// ~/.noema hit the fallback every time.
func moveDir(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyDir(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// humanBytes formats a byte count as a short human-readable string. Used
// only in backup's success message, so precision beyond one decimal is
// not needed.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
