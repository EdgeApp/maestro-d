package common

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

var negNumRe = regexp.MustCompile(`^-[0-9]+(\.[0-9]+)?$`)

// IsBaseStepFlag reports whether a one-shot flag applies to every Maestro step.
func IsBaseStepFlag(name string) bool { _, ok := baseStepFlags[name]; return ok }

// ParsedArgs is the output of ParseArgs for SkipFlagParsing commands.
type ParsedArgs struct {
	Explicit map[string]bool
	// Positional arguments in order.
	Positional []string
	// Step holds flags the table does not know, in order: they become the
	// command's YAML value (`--id foo` → {id: foo}).
	Step []StepFlag
	// Base holds BaseStep flags (optional, label, timeout) if given.
	Base map[string]any
	// PlatformGate is --platform, which doubles as the step's platform gate.
	PlatformGate string
}

type StepFlag struct {
	Key   string // as written, without dashes; may be dotted
	Value string
	Bare  bool // given without a value (→ true)
}

// isBoolType reports whether a command field's Go type is a bool.
func isBoolType(goType string) bool {
	return goType == "bool" || goType == "*bool"
}

// baseStepFlags are accepted by every one-shot command and merged into the
// value map; "platform" is shared with the attach flag of the same name.
var baseStepFlags = map[string]FlagKind{"optional": Bool, "label": String, "timeout": Int}

// ParseArgs implements the one-shot grammar:
//
//	<cmd> [VALUE …] [--flag value | --flag=value | --bool] [-e K=V] [--] [VALUE …]
//
// Known flags (from the table) fill opts; unknown flags are collected as
// step fields. A flag without a following value is a boolean true; the
// following token is taken as the value unless it starts with "--" (a bare
// "-" or a negative number is still a value). stepKeys maps the command's
// field names to their Go types so that a bool field never swallows the
// token after it (`launchApp --clearState co.example`).
func ParseArgs[T any](args []string, flags []Flag[T], stepKeys map[string]string) (*T, *ParsedArgs, error) {
	byName := map[string]Flag[T]{}
	for _, f := range flags {
		byName[f.Name] = f
		for _, a := range f.Aliases {
			byName[a] = f
		}
	}
	o := new(T)
	p := &ParsedArgs{Base: map[string]any{}}
	explicit := map[string]bool{}

	usage := func(format string, a ...any) error { return daemon.Errorf(daemon.CodeUsage, format, a...) }
	takesValue := func(next []string) (string, bool) {
		if len(next) == 0 {
			return "", false
		}
		v := next[0]
		if v == "-" || !strings.HasPrefix(v, "-") || negNumRe.MatchString(v) {
			return v, true
		}
		return "", false
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.Positional = append(p.Positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" || negNumRe.MatchString(a) {
			p.Positional = append(p.Positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value, hasValue := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, value, hasValue = k, v, true
		}
		// --no-<bool> negates a known bool flag.
		negated := false
		if f, ok := byName[strings.TrimPrefix(name, "no-")]; !hasValue && strings.HasPrefix(name, "no-") && ok && f.Kind == Bool {
			if _, direct := byName[name]; !direct {
				name, negated = strings.TrimPrefix(name, "no-"), true
			}
		}
		// A field of the command wins over a table flag of the same name
		// (`openBrowser --url`, `getConsoleLogs --output`); --platform is
		// both, and -e/--env feeds the step's env map when it has one.
		f, known := byName[name]
		if known && name != "platform" && (stepKeys[name] != "" || (f.Name == "env" && stepKeys["env"] != "")) {
			known = false
			if f.Name == "env" {
				name = "env"
			}
		}
		if known {
			switch {
			case f.Kind == Bool:
				if negated {
					value = "false"
				} else if !hasValue {
					value = "true"
				}
			case f.Kind == OptionalValue:
				// Bare: the flag's own default; never the next token.
			case !hasValue:
				if v, ok := takesValue(args[i+1:]); ok {
					value, i = v, i+1
				} else {
					return nil, nil, usage("--%s needs a value", name)
				}
			}
			if err := f.Apply(o, value); err != nil {
				return nil, nil, err
			}
			explicit[f.Name] = true
			if f.Name == "platform" {
				p.PlatformGate = value
			}
			continue
		}
		if kind, ok := baseStepFlags[name]; ok {
			switch kind {
			case Bool:
				if !hasValue {
					value = "true"
				}
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, nil, usage("--%s expects true or false", name)
				}
				p.Base[name] = b
			case Int:
				if !hasValue {
					if v, ok := takesValue(args[i+1:]); ok {
						value, i = v, i+1
					} else {
						return nil, nil, usage("--%s needs a value", name)
					}
				}
				n, err := strconv.Atoi(value)
				if err != nil {
					return nil, nil, usage("--%s expects an integer, got %q", name, value)
				}
				p.Base[name] = n
			default:
				if !hasValue {
					if v, ok := takesValue(args[i+1:]); ok {
						value, i = v, i+1
					} else {
						return nil, nil, usage("--%s needs a value", name)
					}
				}
				p.Base[name] = value
			}
			continue
		}
		// Unknown: a step field. Bool fields never take the next token.
		if !hasValue && !isBoolType(stepKeys[name]) {
			if v, ok := takesValue(args[i+1:]); ok {
				value, i, hasValue = v, i+1, true
			}
		}
		if !hasValue {
			p.Step = append(p.Step, StepFlag{Key: name, Bare: true})
			continue
		}
		p.Step = append(p.Step, StepFlag{Key: name, Value: value})
	}

	// Env-var fallbacks for known flags that were not given.
	for _, f := range flags {
		if explicit[f.Name] {
			continue
		}
		for _, e := range f.Env {
			if v := os.Getenv(e); v != "" {
				if f.Kind == Slice {
					continue
				}
				if err := f.Apply(o, v); err != nil {
					return nil, nil, err
				}
				break
			}
		}
	}
	p.Explicit = explicit
	return o, p, nil
}
