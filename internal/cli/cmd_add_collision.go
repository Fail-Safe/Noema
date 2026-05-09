package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Fail-Safe/Noema/internal/cortex"
	"github.com/Fail-Safe/Noema/internal/trace"
)

// resolveCollisionInteractive runs the post-collision recovery flow for
// `noema add`. The user has already produced body content; we don't want
// to throw it away just because the deterministic id slot is held by an
// archived/trashed/purged sibling. Offers exactly the options that are
// valid for the existing row's state and reattempts the add when the
// caller picks one that frees the slot.
//
// `t` is the trace the caller tried to add; we mutate its title (and id
// via trace.New) on a vary-title retry rather than constructing a fresh
// Trace, so author/tags/body/derived_from carry through unchanged.
func resolveCollisionInteractive(cx *cortex.Cortex, t *trace.Trace, collision *cortex.ErrTraceIDExists, traceType, author string, tags []string, body string) error {
	fmt.Fprintln(os.Stderr, collision.Error())
	for {
		choice, err := promptCollisionChoice(collision.State)
		if err != nil {
			return err
		}
		switch choice {
		case "v":
			return retryWithNewTitle(cx, traceType, author, tags, body)
		case "r":
			if err := cx.Recover(collision.ID); err != nil {
				return fmt.Errorf("recover failed: %w", err)
			}
			fmt.Printf("Recovered %s. New content was discarded — edit the recovered trace if you want to update it.\n", collision.ID)
			return nil
		case "u":
			if err := cx.Unarchive(collision.ID); err != nil {
				return fmt.Errorf("unarchive failed: %w", err)
			}
			fmt.Printf("Unarchived %s. New content was discarded — edit the unarchived trace if you want to update it.\n", collision.ID)
			return nil
		case "p":
			if err := purgeAndRetry(cx, t, collision); err != nil {
				return err
			}
			fmt.Printf("Trace added: %s\n", t.ID)
			return nil
		case "q":
			return collision
		}
	}
}

// promptCollisionChoice loops until the user picks an option valid for
// the existing row's state. State-dependent letters keep the prompt
// honest — offering (R)ecover for an archived row would just confuse.
// EOF on stdin (closed pipe, no terminal) is treated as Q-quit so the
// caller surfaces the original collision instead of looping forever
// against an empty buffer — important when this resolver runs in a
// non-interactive context (script piping into `noema add`, automated
// test, etc.).
func promptCollisionChoice(state string) (string, error) {
	var menu string
	var valid map[string]struct{}
	switch state {
	case "trashed":
		menu = "(R)ecover trashed / (P)urge & retry / (V)ary title / (Q)uit"
		valid = map[string]struct{}{"r": {}, "p": {}, "v": {}, "q": {}}
	case "archived":
		menu = "(U)narchive / (P)urge & retry / (V)ary title / (Q)uit"
		valid = map[string]struct{}{"u": {}, "p": {}, "v": {}, "q": {}}
	case "purged":
		menu = "(V)ary title / (Q)uit  (the slot can only be freed via `noema memory purge --hard`)"
		valid = map[string]struct{}{"v": {}, "q": {}}
	default:
		menu = "(V)ary title / (Q)uit  (an active trace already holds this id)"
		valid = map[string]struct{}{"v": {}, "q": {}}
	}
	for {
		raw, eof, err := readChoiceLine(menu)
		if err != nil {
			return "", err
		}
		if eof {
			return "q", nil
		}
		raw = strings.ToLower(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}
		key := raw[:1]
		if _, ok := valid[key]; ok {
			return key, nil
		}
		fmt.Fprintln(os.Stderr, "  unrecognised option")
	}
}

// readChoiceLine reads one line from the package-level stdin, returning
// (line, eof, err). Unlike prompt() in input.go which silently swallows
// io.EOF as an empty string, this distinguishes a closed stream from a
// bare-Enter so the collision loop can bail rather than spin.
func readChoiceLine(label string) (string, bool, error) {
	fmt.Print(label + ": ")
	line, err := stdin.ReadString('\n')
	switch {
	case errors.Is(err, io.EOF):
		// EOF mid-line still counts as the line, but if no bytes
		// arrived at all we treat the stream as closed.
		return strings.TrimSpace(line), strings.TrimSpace(line) == "", nil
	case err != nil:
		return "", false, err
	}
	return strings.TrimSpace(line), false, nil
}

// retryWithNewTitle prompts for a fresh title, mints a new id from it,
// and attempts the add again. Recursion is bounded by the user's
// patience — if the new title also collides we re-enter the same
// resolver, but since each iteration requires a human keypress this
// can't loop unbounded.
func retryWithNewTitle(cx *cortex.Cortex, traceType, author string, tags []string, body string) error {
	for {
		newTitle, eof, err := readChoiceLine("New title")
		if err != nil {
			return err
		}
		if eof {
			return fmt.Errorf("input closed before a new title was supplied")
		}
		if newTitle == "" {
			fmt.Fprintln(os.Stderr, "  title cannot be empty")
			continue
		}
		t := trace.New(newTitle, traceType, author, tags, body)
		err = cx.Add(t)
		var collision *cortex.ErrTraceIDExists
		if asCollision(err, &collision) {
			fmt.Fprintln(os.Stderr, collision.Error())
			return resolveCollisionInteractive(cx, t, collision, traceType, author, tags, body)
		}
		if err != nil {
			return err
		}
		fmt.Printf("Trace added: %s\n", t.ID)
		return nil
	}
}

// purgeAndRetry tombstones the colliding row at its current tier (read
// straight off the live row to avoid the AdminPurge tier-mismatch
// safety rail tripping over a stale assumption) then retries the add.
// AdminPurge soft-deletes by default — the row is wiped to a tombstone
// that still occupies the id slot. So we follow up with --hard to
// actually free the slot for our retry, scoped to the trashed/archived
// case where the user has already committed to discarding the prior
// row.
func purgeAndRetry(cx *cortex.Cortex, t *trace.Trace, collision *cortex.ErrTraceIDExists) error {
	row, err := cx.Get(collision.ID)
	if err != nil {
		return fmt.Errorf("looking up colliding row before purge: %w", err)
	}
	const reason = "freed by interactive `noema add` to recreate id"
	if err := cx.AdminPurge(collision.ID, reason, row.Tier, true, cortex.ActorHuman); err != nil {
		return fmt.Errorf("hard purge failed: %w", err)
	}
	if err := cx.Add(t); err != nil {
		return err
	}
	return nil
}

// asCollision is errors.As specialised on *ErrTraceIDExists so the
// retry flow stays readable. Same pattern as the import-side caller.
func asCollision(err error, target **cortex.ErrTraceIDExists) bool {
	if err == nil {
		return false
	}
	c, ok := err.(*cortex.ErrTraceIDExists)
	if ok {
		*target = c
		return true
	}
	return false
}
