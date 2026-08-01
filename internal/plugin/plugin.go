package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

type Definition struct {
	Name         string
	Description  string
	Files        fs.FS
	ManagedFiles []string
}

type State string

const (
	StateUpToDate     State = "up to date"
	StateDrift        State = "drift detected"
	StateNotInstalled State = "not installed"
)

type FileState string

const (
	FileMatch     FileState = "match"
	FileMissing   FileState = "missing"
	FileChanged   FileState = "changed"
	FileIrregular FileState = "not a regular file"
)

type FileReport struct {
	Path          string
	State         FileState
	EmbeddedHash  string
	InstalledHash string
}

type StatusReport struct {
	Plugin string
	Target string
	State  State
	Files  []FileReport
}

type InstallAction string

const (
	ActionInstalled    InstallAction = "installed"
	ActionReplaced     InstallAction = "replaced"
	ActionUnchanged    InstallAction = "unchanged"
	ActionRefused      InstallAction = "refused"
	ActionWouldInstall InstallAction = "would install"
	ActionWouldReplace InstallAction = "would replace"
)

type InstallFileReport struct {
	Path   string
	Action InstallAction
}

type InstallReport struct {
	Plugin string
	Target string
	Files  []InstallFileReport
}

func (r InstallReport) Pending() bool {
	for _, file := range r.Files {
		if file.Action == ActionWouldInstall || file.Action == ActionWouldReplace {
			return true
		}
	}
	return false
}

func (r InstallReport) Refused() bool {
	for _, file := range r.Files {
		if file.Action == ActionRefused {
			return true
		}
	}
	return false
}

func Inspect(def Definition, target string) (StatusReport, error) {
	managed, err := loadManaged(def)
	if err != nil {
		return StatusReport{}, err
	}
	target, err = normalizeTarget(target)
	if err != nil {
		return StatusReport{}, err
	}

	report := StatusReport{Plugin: def.Name, Target: target}
	info, err := os.Stat(report.Target)
	if os.IsNotExist(err) {
		report.State = StateNotInstalled
		return report, nil
	}
	if err != nil {
		return StatusReport{}, fmt.Errorf("inspecting plugin target %s: %w", report.Target, err)
	}
	if !info.IsDir() {
		return StatusReport{}, fmt.Errorf("plugin target %s is not a directory", report.Target)
	}

	report.State = StateUpToDate
	for _, file := range managed {
		installedPath := filepath.Join(report.Target, filepath.FromSlash(file.path))
		fileReport := FileReport{Path: file.path, EmbeddedHash: hashBytes(file.data)}
		installedInfo, statErr := os.Lstat(installedPath)
		switch {
		case os.IsNotExist(statErr):
			fileReport.State = FileMissing
			report.State = StateDrift
		case statErr != nil:
			return StatusReport{}, fmt.Errorf("inspecting managed file %s: %w", installedPath, statErr)
		case !installedInfo.Mode().IsRegular():
			fileReport.State = FileIrregular
			report.State = StateDrift
		default:
			installed, readErr := os.ReadFile(installedPath)
			if readErr != nil {
				return StatusReport{}, fmt.Errorf("reading managed file %s: %w", installedPath, readErr)
			}
			fileReport.InstalledHash = hashBytes(installed)
			if fileReport.InstalledHash == fileReport.EmbeddedHash {
				fileReport.State = FileMatch
			} else {
				fileReport.State = FileChanged
				report.State = StateDrift
			}
		}
		report.Files = append(report.Files, fileReport)
	}
	return report, nil
}

type InstallOptions struct {
	Check bool
	Force bool
}

func Install(def Definition, target string, opts InstallOptions) (InstallReport, error) {
	return install(def, target, opts, atomicFileOps{replace: replaceFile})
}

func install(def Definition, target string, opts InstallOptions, ops atomicFileOps) (InstallReport, error) {
	managed, err := loadManaged(def)
	if err != nil {
		return InstallReport{}, err
	}
	target, err = normalizeTarget(target)
	if err != nil {
		return InstallReport{}, err
	}

	report := InstallReport{Plugin: def.Name, Target: target}
	info, err := os.Stat(report.Target)
	switch {
	case os.IsNotExist(err):
		if !opts.Check {
			if mkdirErr := os.MkdirAll(report.Target, 0o755); mkdirErr != nil {
				return InstallReport{}, fmt.Errorf("creating plugin directory %s: %w", report.Target, mkdirErr)
			}
		}
	case err != nil:
		return InstallReport{}, fmt.Errorf("inspecting plugin target %s: %w", report.Target, err)
	case !info.IsDir():
		return InstallReport{}, fmt.Errorf("plugin target %s is not a directory", report.Target)
	}

	for _, file := range managed {
		destination := filepath.Join(report.Target, filepath.FromSlash(file.path))
		installedInfo, statErr := os.Lstat(destination)
		switch {
		case os.IsNotExist(statErr):
			action := ActionWouldInstall
			if !opts.Check {
				if writeErr := writeAtomicFile(destination, file.data, ops); writeErr != nil {
					return report, fmt.Errorf("installing managed file %s: %w", destination, writeErr)
				}
				action = ActionInstalled
			}
			report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: action})
		case statErr != nil:
			return report, fmt.Errorf("inspecting managed file %s: %w", destination, statErr)
		case installedInfo.Mode().IsRegular():
			installed, readErr := os.ReadFile(destination)
			if readErr != nil {
				return report, fmt.Errorf("reading managed file %s: %w", destination, readErr)
			}
			if hashBytes(installed) == hashBytes(file.data) {
				report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: ActionUnchanged})
				continue
			}
			if !opts.Force {
				report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: ActionRefused})
				continue
			}
			action := ActionWouldReplace
			if !opts.Check {
				if writeErr := writeAtomicFile(destination, file.data, ops); writeErr != nil {
					return report, fmt.Errorf("replacing managed file %s: %w", destination, writeErr)
				}
				action = ActionReplaced
			}
			report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: action})
		case installedInfo.Mode()&os.ModeSymlink != 0:
			if !opts.Force {
				report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: ActionRefused})
				continue
			}
			action := ActionWouldReplace
			if !opts.Check {
				if writeErr := writeAtomicFile(destination, file.data, ops); writeErr != nil {
					return report, fmt.Errorf("replacing managed symlink %s: %w", destination, writeErr)
				}
				action = ActionReplaced
			}
			report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: action})
		default:
			report.Files = append(report.Files, InstallFileReport{Path: file.path, Action: ActionRefused})
		}
	}
	return report, nil
}

type managedFile struct {
	path string
	data []byte
}

func loadManaged(def Definition) ([]managedFile, error) {
	if def.Name == "" {
		return nil, fmt.Errorf("plugin definition has no name")
	}
	if def.Files == nil {
		return nil, fmt.Errorf("plugin %s has no embedded filesystem", def.Name)
	}
	if len(def.ManagedFiles) == 0 {
		return nil, fmt.Errorf("plugin %s has no managed files", def.Name)
	}

	seen := make(map[string]struct{}, len(def.ManagedFiles))
	managed := make([]managedFile, 0, len(def.ManagedFiles))
	for _, name := range def.ManagedFiles {
		if !fs.ValidPath(name) || name == "." {
			return nil, fmt.Errorf("plugin %s has unsafe managed path %q", def.Name, name)
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("plugin %s repeats managed path %q", def.Name, name)
		}
		seen[name] = struct{}{}
		info, err := fs.Stat(def.Files, name)
		if err != nil {
			return nil, fmt.Errorf("plugin %s managed file %q: %w", def.Name, name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("plugin %s managed path %q is not a regular file", def.Name, name)
		}
		data, err := fs.ReadFile(def.Files, name)
		if err != nil {
			return nil, fmt.Errorf("plugin %s reading managed file %q: %w", def.Name, name, err)
		}
		managed = append(managed, managedFile{path: name, data: data})
	}

	// Definition order is the output contract. This check catches callers that
	// accidentally build the manifest from a map and makes the nondeterminism
	// visible during tests rather than in user-facing status output.
	if !sort.StringsAreSorted(def.ManagedFiles) {
		return nil, fmt.Errorf("plugin %s managed files must be sorted", def.Name)
	}
	return managed, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("plugin target is empty")
	}
	target = filepath.Clean(target)
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("plugin target must be absolute: %s", target)
	}
	return target, nil
}
