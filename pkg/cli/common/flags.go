package common

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type FlagKind int

const (
	String FlagKind = iota
	Bool
	Int
	Slice
	// OptionalValue takes a value only when joined with "="
	// (--target-hub=h); bare, it never takes the next token, so in
	// `tapOn --target-hub Login` "Login" stays the tapOn text.
	OptionalValue
)

// Flag[T] is one entry of the shared flag table.
type Flag[T any] struct {
	Name    string
	Aliases []string
	Kind    FlagKind
	Env     []string
	Usage   string
	Value   string // default, shown in help; not applied
	// apply receives the string form ("true"/"false" for bools) once per
	// occurrence (slices) or once (everything else).
	Apply func(o *T, v string) error
}

func StringFlag[T any](name, usage string, env []string, apply func(o *T, v string), aliases ...string) Flag[T] {
	return Flag[T]{Name: name, Aliases: aliases, Kind: String, Env: env, Usage: usage,
		Apply: func(o *T, v string) error { apply(o, v); return nil }}
}

func BoolFlag[T any](name, usage string, env []string, apply func(o *T, v bool), aliases ...string) Flag[T] {
	return Flag[T]{Name: name, Aliases: aliases, Kind: Bool, Env: env, Usage: usage,
		Apply: func(o *T, v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return daemon.Errorf(daemon.CodeUsage, "--%s expects true or false, got %q", name, v)
			}
			apply(o, b)
			return nil
		}}
}

func IntFlag[T any](name, usage string, env []string, apply func(o *T, v int), aliases ...string) Flag[T] {
	return Flag[T]{Name: name, Aliases: aliases, Kind: Int, Env: env, Usage: usage,
		Apply: func(o *T, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return daemon.Errorf(daemon.CodeUsage, "--%s expects an integer, got %q", name, v)
			}
			apply(o, n)
			return nil
		}}
}

// FlagSet joins groups into one ordered list; a flag in two groups (the
// token is a spawn flag and a target flag) is listed once.
func FlagSet[T any](groups ...[]Flag[T]) []Flag[T] {
	var out []Flag[T]
	seen := map[string]bool{}
	for _, g := range groups {
		for _, f := range g {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	return out
}

func One[T any](f Flag[T]) []Flag[T] { return []Flag[T]{f} }

func FlagUsage[T any](flags []Flag[T]) string {
	var b strings.Builder
	for _, f := range flags {
		names := "--" + f.Name
		for _, a := range f.Aliases {
			if len(a) == 1 {
				names = "-" + a + ", " + names
			} else {
				names += ", --" + a
			}
		}
		switch f.Kind {
		case String, Slice:
			names += " value"
		case Int:
			names += " N"
		case OptionalValue:
			names += "[=HOST[:PORT]]"
		}
		fmt.Fprintf(&b, "   %-32s %s\n", names, f.Usage)
	}
	return b.String()
}

// InheritedArgs re-encodes upstream global flags given before the command
// name (`maestro-d --device X --team-id T tapOn Login`) as leading
// arguments, so they act as defaults for the command's own flags.
func InheritedArgs[T any](c *cli.Context, flags []Flag[T]) []string {
	var out []string
	lineage := c.Lineage()
	if len(lineage) < 2 {
		return nil
	}
	for _, parent := range lineage[1:] {
		for _, f := range flags {
			if !parent.IsSet(f.Name) {
				continue
			}
			switch f.Kind {
			case Bool:
				out = append(out, fmt.Sprintf("--%s=%t", f.Name, parent.Bool(f.Name)))
			case Int:
				out = append(out, fmt.Sprintf("--%s=%d", f.Name, parent.Int(f.Name)))
			case Slice:
				for _, v := range parent.StringSlice(f.Name) {
					out = append(out, "--"+f.Name+"="+v)
				}
			default:
				out = append(out, "--"+f.Name+"="+parent.String(f.Name))
			}
		}
	}
	return out
}

// FlagHelp appends the flag table to a command description.
func FlagHelp[T any](desc string, flags []Flag[T]) string {
	if desc != "" {
		desc += "\n\n"
	}
	return desc + "Flags (anywhere on the line):\n" + FlagUsage(flags)
}
