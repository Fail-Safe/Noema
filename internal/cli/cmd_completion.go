package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func completionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate or install shell completions",
	}
	cmd.AddCommand(
		completionPrintCmd("bash"),
		completionPrintCmd("zsh"),
		completionPrintCmd("fish"),
		completionInstallCmd(),
	)
	return cmd
}

func completionPrintCmd(shell string) *cobra.Command {
	return &cobra.Command{
		Use:   shell,
		Short: "Print " + shell + " completion script to stdout",
		Example: fmt.Sprintf("  noema completion %s > ~/.%src_completions/noema", shell, shell),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch shell {
			case "bash":
				return rootCmd.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return rootCmd.GenZshCompletion(os.Stdout)
			case "fish":
				return rootCmd.GenFishCompletion(os.Stdout, true)
			}
			return nil
		},
	}
}

func completionInstallCmd() *cobra.Command {
	var shellFlag string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install completions into your shell config",
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := shellFlag
			if shell == "" {
				shell = detectShell()
			}
			if shell == "" {
				return fmt.Errorf("could not detect shell; use --shell bash|zsh|fish")
			}
			return installCompletion(shell)
		},
	}
	cmd.Flags().StringVar(&shellFlag, "shell", "", "shell to install for (bash, zsh, fish); detected from $SHELL if omitted")
	return cmd
}

func detectShell() string {
	base := filepath.Base(os.Getenv("SHELL"))
	switch base {
	case "bash", "zsh", "fish":
		return base
	}
	return ""
}

func installCompletion(shell string) error {
	switch shell {
	case "bash":
		return installBash()
	case "zsh":
		return installZsh()
	case "fish":
		return installFish()
	default:
		return fmt.Errorf("unsupported shell %q — supported: bash, zsh, fish", shell)
	}
}

func installBash() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".bash_completion.d")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, "noema")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := rootCmd.GenBashCompletionV2(f, true); err != nil {
		return err
	}
	fmt.Printf("Installed to %s\n\n", path)
	fmt.Println("Add to ~/.bashrc if not already sourced:")
	fmt.Printf("  [[ -f %s ]] && source %s\n", path, path)
	return nil
}

func installZsh() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".zfunc")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, "_noema")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := rootCmd.GenZshCompletion(f); err != nil {
		return err
	}
	fmt.Printf("Installed to %s\n\n", path)
	fmt.Println("Add to ~/.zshrc if not already present:")
	fmt.Println("  fpath+=(~/.zfunc)")
	fmt.Println("  autoload -Uz compinit && compinit")
	return nil
}

func installFish() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "fish", "completions")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, "noema.fish")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := rootCmd.GenFishCompletion(f, true); err != nil {
		return err
	}
	fmt.Printf("Installed to %s\n", path)
	fmt.Println("Completions will be active in new fish sessions.")
	return nil
}
