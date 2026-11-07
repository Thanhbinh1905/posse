package app

import (
	"fmt"

	"github.com/thanhbinh1905/posse/internal/axi"
	"github.com/thanhbinh1905/posse/internal/config"
)

// homeCommand serves install.sh: it prints the posse home's path, source and
// pointer file, one per line, and with --set saves a new data folder.
func (s *Service) homeCommand(ctx *axi.Context, args []string) error {
	parsed, err := parseArgs("_home", args, map[string]flagSpec{"set": {}})
	if err != nil {
		return err
	}
	if len(parsed.Positionals) != 0 {
		return axi.Usage("_home accepts only --set <folder>")
	}
	var home config.Home
	if choice, found := parsed.Flags["set"]; found {
		home, err = config.SetHomePointer(config.ProcessLookup, choice)
	} else {
		home, err = config.ResolveHome(config.ProcessLookup)
	}
	if err != nil {
		fmt.Fprintln(ctx.ErrOut, err)
		return axi.Reported(axi.Failure("home_invalid", err.Error(), false))
	}
	_, err = fmt.Fprintf(ctx.Out, "%s\n%s\n%s\n", home.Path, home.Source, home.Pointer)
	return err
}
