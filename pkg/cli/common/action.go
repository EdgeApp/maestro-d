package common

import (
	"context"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type ArgParser[T any] func([]string, []Flag[T], map[string]string) (*T, *ParsedArgs, error)

func Action[T any](flags []Flag[T], parse ArgParser[T], fn func(context.Context, *T, []string) error, jsonOutput func(*T) bool) cli.ActionFunc {
	return func(c *cli.Context) error {
		args := c.Args().Slice()
		for _, a := range args {
			if a == "-h" || a == "--help" {
				cli.HelpPrinter(c.App.Writer, cli.CommandHelpTemplate, c.Command)
				return nil
			}
		}
		o, p, err := parse(append(InheritedArgs(c, flags), args...), flags, nil)
		if err != nil {
			return FinishErr(err, false)
		}
		if len(p.Step) > 0 {
			return FinishErr(daemon.Errorf(daemon.CodeUsage, "unknown flag --%s (see `%s %s --help`)", p.Step[0].Key, c.App.Name, c.Command.Name), false)
		}
		if len(p.Base) > 0 {
			for k := range p.Base {
				return FinishErr(daemon.Errorf(daemon.CodeUsage, "unknown flag --%s (see `%s %s --help`)", k, c.App.Name, c.Command.Name), false)
			}
		}
		ctx, cancel := SignalContext()
		defer cancel()
		return FinishErr(fn(ctx, o, p.Positional), jsonOutput(o))
	}
}
