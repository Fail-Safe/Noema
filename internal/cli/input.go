package cli

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// stdin is a single shared reader over os.Stdin.
// Using one reader avoids the classic multi-Scanner bug where each new
// bufio.Scanner buffers a chunk of stdin and discards it when it goes out
// of scope, silently losing subsequent input.
var stdin = bufio.NewReader(os.Stdin)

func prompt(label string) (string, error) {
	print(label + ": ")
	line, err := stdin.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readLine reads a single line from stdin without printing a prompt label.
func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readBodyFromStdin reads until EOF (Ctrl+D), returning all lines joined.
func readBodyFromStdin() (string, error) {
	var sb strings.Builder
	for {
		line, err := stdin.ReadString('\n')
		if len(line) > 0 {
			sb.WriteString(line)
		}
		if err == io.EOF {
			return strings.TrimRight(sb.String(), "\n"), nil
		}
		if err != nil {
			return "", err
		}
	}
}
