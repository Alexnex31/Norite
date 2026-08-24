package instanceadmin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// lineReader reads one visible line at a time from in.
//
// A buffered line read rather than fmt.Fscanln, which is wrong twice over: on an empty line it fails with
// "unexpected newline" instead of returning one, so pressing Enter at the prompt produces a scanner error
// rather than the command's own "a username is required"; and it stops at the first space, so a mistyped
// answer silently becomes its first word. The same reader `norite login` and the instance wizard use.
func lineReader(in io.Reader, out io.Writer) func(string) (string, error) {
	buffered := bufio.NewReader(in)
	return func(prompt string) (string, error) {
		_, _ = fmt.Fprint(out, prompt)

		line, err := buffered.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		// A final line with no newline is still an answer, which is what EOF means here rather than a
		// failure — a heredoc without a trailing newline is the ordinary case.
		return strings.TrimRight(line, "\r\n"), nil
	}
}
