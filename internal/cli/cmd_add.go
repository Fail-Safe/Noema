package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Fail-Safe/Noema/internal/trace"
)

func addCmd() *cobra.Command {
	var (
		title    string
		traceType string
		author   string
		tags     []string
		body     string
	)

	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add a new Trace",
		RunE: func(cmd *cobra.Command, args []string) error {
			cx, err := resolveCortex()
			if err != nil {
				return err
			}
			defer cx.Close()

			// Interactive prompts for any missing required fields.
			if title == "" {
				title, err = prompt("Title")
				if err != nil {
					return err
				}
				if title == "" {
					return fmt.Errorf("title is required")
				}
			}

			if traceType == "" {
				types := make([]string, len(trace.ValidTypes))
				for i, t := range trace.ValidTypes {
					types[i] = string(t)
				}
				fmt.Printf("Type [%s] (note): ", strings.Join(types, "/"))
				traceType, err = readLine()
				if err != nil {
					return err
				}
				if traceType == "" {
					traceType = "note"
				}
			}
			if !trace.IsValidType(traceType) {
				return fmt.Errorf("invalid type %q", traceType)
			}

			if author == "" && !cmd.Flags().Changed("author") {
				a, err := prompt("Author (optional)")
				if err != nil {
					return err
				}
				author = a
			}

			if len(tags) == 0 && !cmd.Flags().Changed("tag") {
				raw, err := prompt("Tags (comma-separated, optional)")
				if err != nil {
					return err
				}
				if raw != "" {
					for _, t := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' }) {
						if tag := strings.TrimSpace(t); tag != "" {
							tags = append(tags, tag)
						}
					}
				}
			}

			if body == "" && !cmd.Flags().Changed("body") {
				fi, _ := os.Stdin.Stat()
				if (fi.Mode() & os.ModeCharDevice) == 0 {
					// stdin is a pipe — read body directly, no prompt.
					data, err := readBodyFromStdin()
					if err != nil {
						return err
					}
					body = data
				} else {
					fmt.Println("Body (Ctrl+D to save, Ctrl+C to cancel):")
					data, err := readBodyFromStdin()
					if err != nil {
						return err
					}
					body = data
				}
			}

			t := trace.New(title, traceType, author, tags, body)
			if err := cx.Add(t); err != nil {
				return err
			}
			fmt.Printf("Trace added: %s\n", t.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "trace title")
	cmd.Flags().StringVar(&traceType, "type", "", "trace type")
	cmd.Flags().StringVar(&author, "author", "", "author name or agent identifier")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag (repeatable)")
	cmd.Flags().StringVar(&body, "body", "", "trace body content")
	return cmd
}

