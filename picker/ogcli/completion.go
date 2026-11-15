package ogcli

import (
	"io"
	"os"
	"slices"

	"github.com/alecthomas/kong"
)

func environ() []string { return os.Environ() }

// runCompletion serves the hidden `og completion <shell>` command. `fish`
// alone prints the static script; everything else (bash, zsh, `fish -c`, help)
// is kong-completion's own command.
func runCompletion(parser *kong.Kong, args []string, stdout, stderr io.Writer) int {
	if slices.Equal(args, []string{completionCommand, "fish"}) {
		fprintf(stdout, "%s", fishScript(parser.Model))
		return 0
	}
	ctx, err := parser.Parse(args)
	if err != nil {
		fprintf(stderr, "og: %v\n", err)
		return 2
	}
	if err := ctx.Run(); err != nil {
		fprintf(stderr, "og: %v\n", err)
		return 1
	}
	return 0
}
