package cli

// oneshot.go registers every YAML command (tapOn, assertVisible, launchApp,
// …) as a CLI command. The arguments are turned into the same YAML value a
// flow file would hold and sent to the daemon, which parses and executes it
// with the unmodified upstream step code:
//
//	maestro-d tapOn Login                → - tapOn: Login
//	maestro-d tapOn --id btn --index 2   → - tapOn: {id: btn, index: 2}
//	maestro-d tapOn Login --optional     → - tapOn: {text: Login, optional: true}
//	maestro-d swipe --start.x 50% …      → - swipe: {start: {x: 50%}, …}
//	maestro-d repeat --yaml loop.yaml    → - repeat: <file contents>
//	maestro-d back                       → - back

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// oneShotFlags are the table flags every one-shot accepts.
var oneShotFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), common.One(waitFlag), attachFlags, targetFlags)

// oneShotCommands builds one cli.Command per known step type.
func oneShotCommands() []*cli.Command {
	types := flow.StepTypes()
	cmds := make([]*cli.Command, 0, len(types))
	for _, t := range types {
		name := string(t)
		spec, ok := daemon.CommandSpecs[name]
		if !ok {
			spec = daemon.CommandSpec{Name: name}
		}
		cmds = append(cmds, &cli.Command{
			Name:            name,
			Usage:           spec.Doc,
			ArgsUsage:       oneShotArgsUsage(spec),
			Description:     commandHelp(spec) + "\nCommon flags:\n" + common.FlagUsage(oneShotFlags),
			Category:        "YAML commands",
			SkipFlagParsing: true,
			HideHelpCommand: true,
			Action: func(c *cli.Context) error {
				return runOneShot(spec, append(common.InheritedArgs(c, oneShotFlags), c.Args().Slice()...))
			},
		})
	}
	return cmds
}

// valueOptional lists the commands that are meaningful with no value at all
// (`- launchApp` in YAML): the scalar has a default — the attach --app-id,
// the report's screenshot path, scrolling down, erasing 50 characters.
// Every other command needs a value or a field, so a bare `maestro-d
// tapOn` is a usage error rather than a step that fails on the device.
var valueOptional = map[string]bool{
	"launchApp": true, "stopApp": true, "killApp": true, "clearState": true,
	"takeScreenshot": true, "scroll": true, "eraseText": true,
	"waitForAnimationToEnd": true, "startRecording": true, "stopRecording": true,
	"setTrafficShape":       true, // every field defaults: no flags sends the unshaped shape
	"assertNoDefectsWithAI": true, // maestro-d: its assertion is built in, and its options default
}

func oneShotArgsUsage(spec daemon.CommandSpec) string {
	switch {
	case spec.ValueLess:
		return " "
	case spec.Compound:
		return "[VALUE] [--field value …] [--yaml FILE|-]"
	case spec.Scalar != "":
		return "[" + strings.ToUpper(spec.Scalar) + "] [--field value …]"
	}
	return "[--field value …]"
}

// commandHelp renders the field table for a command (used by --help and
// `maestro-d commands <name>`).
func commandHelp(spec daemon.CommandSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", spec.Doc)
	switch {
	case spec.ValueLess:
		fmt.Fprintf(&b, "Takes no value. YAML: - %s\n", spec.Name)
		return b.String()
	case spec.Scalar != "":
		fmt.Fprintf(&b, "A bare value sets %q: maestro-d %s VALUE  ≡  - %s: VALUE\n", spec.Scalar, spec.Name, spec.Name)
	}
	if valueOptional[spec.Name] {
		fmt.Fprintf(&b, "The value may be omitted: maestro-d %s  ≡  - %s\n", spec.Name, spec.Name)
	}
	if spec.Compound {
		fmt.Fprintf(&b, "Nested steps are passed with --yaml FILE (or - for stdin) holding the command's YAML value.\n")
	}
	var fields []daemon.FieldSpec
	for _, f := range spec.Fields {
		if !common.IsBaseStepFlag(f.Key) && f.Key != "platform" {
			fields = append(fields, f)
		}
	}
	if len(fields) > 0 {
		fmt.Fprintf(&b, "\nFields (--<field> value; nested fields as --a.b value; JSON/YAML for lists and maps):\n")
		for _, f := range fields {
			doc := f.Doc
			if doc == "" {
				doc = f.Type
			} else {
				doc = fmt.Sprintf("%s (%s)", doc, f.Type)
			}
			fmt.Fprintf(&b, "   --%-28s %s\n", f.Key, doc)
		}
	}
	if !spec.ValueLess {
		fmt.Fprintf(&b, "\nEvery command also takes --optional, --label TEXT, --timeout MS and\n--platform android|ios|web (skip the step on other platforms; on the first\ncommand for a device this also selects the platform to attach with).\n")
	}
	return b.String()
}

// runOneShot is the action for every YAML command.
func runOneShot(spec daemon.CommandSpec, args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Printf("USAGE:\n   maestro-d %s %s\n\n%s\nCommon flags:\n%s", spec.Name, oneShotArgsUsage(spec), commandHelp(spec), common.FlagUsage(oneShotFlags))
			return nil
		}
	}
	stepKeys := map[string]string{}
	for _, f := range spec.Fields {
		stepKeys[f.Key] = f.Type
	}
	o, p, err := parseDaemonArgs(args, oneShotFlags, stepKeys)
	if err != nil {
		return common.FinishErr(err, false)
	}
	ctx, cancel := common.SignalContext()
	defer cancel()
	return common.FinishErr(execOneShot(ctx, spec, o, p), o.JSON)
}

func execOneShot(ctx context.Context, spec daemon.CommandSpec, o *daemonOpts, p *common.ParsedArgs) error {
	value, err := buildStepValue(spec, p)
	if err != nil {
		return err
	}
	// Relative paths resolve against the caller's directory — on this
	// machine. A targeted daemon is another machine, and a remote caller
	// may not name a directory there, so no cwd is sent.
	cwd := ""
	if o.Target == "" {
		cwd, _ = os.Getwd()
	}
	client, id, err := session(ctx, o)
	if err != nil {
		return err
	}
	var files *hubFiles
	if o.Target != "" {
		// The daemon is another machine: upload the input files and name
		// the outputs ourselves, then fetch those back.
		files = newHubFiles(ctx, client, id, o.Attach.Env, o.JSON)
		if value, err = files.rewrite(spec, value); err != nil {
			return err
		}
	}
	res, err := client.CommandCwd(ctx, id, spec.Name, value, cwd, !o.NoWait)
	if files != nil && res != nil {
		if ferr := files.fetchOutputs(res.Outputs); ferr != nil && err == nil {
			err = ferr
		}
	}
	return emitStep(o, res, err)
}

// buildStepValue turns the parsed positional/flag arguments into the YAML
// value of the step.
func buildStepValue(spec daemon.CommandSpec, p *common.ParsedArgs) (any, error) {
	usage := func(format string, a ...any) error { return daemon.Errorf(daemon.CodeUsage, format, a...) }

	// --yaml FILE|- supplies the whole value (compound steps, or anything
	// too awkward for flags).
	var value any
	haveValue := false
	var fields []common.StepFlag
	for _, f := range p.Step {
		if f.Key != "yaml" {
			fields = append(fields, f)
			continue
		}
		if f.Bare {
			return nil, usage("--yaml needs a file (or - for stdin)")
		}
		var data []byte
		var err error
		if f.Value == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(f.Value)
		}
		if err != nil {
			return nil, usage("--yaml: %v", err)
		}
		if err := yaml.Unmarshal(data, &value); err != nil {
			return nil, usage("--yaml: %v", err)
		}
		haveValue = true
	}

	if spec.ValueLess {
		if len(p.Positional) > 0 || len(fields) > 0 || haveValue {
			return nil, usage("%s takes no value", spec.Name)
		}
		if len(p.Base) > 0 || p.PlatformGate != "" {
			common.Progress("warning: %s ignores --optional/--label/--timeout/--platform (it takes no value)", spec.Name)
		}
		return nil, nil
	}

	var scalar any
	haveScalar := false
	if len(p.Positional) > 0 {
		s, err := typedScalar(strings.Join(p.Positional, " "))
		if err != nil {
			return nil, err
		}
		if m, ok := s.(map[string]any); ok {
			// `tapOn '{"id": "x"}'` — a whole value as JSON/YAML.
			if haveValue {
				return nil, usage("give the value either as an argument or with --yaml, not both")
			}
			value, haveValue = m, true
		} else if _, ok := s.([]any); ok {
			if haveValue {
				return nil, usage("give the value either as an argument or with --yaml, not both")
			}
			value, haveValue = s, true
		} else {
			scalar, haveScalar = s, true
		}
	}

	needMap := len(fields) > 0 || len(p.Base) > 0 || p.PlatformGate != ""
	switch {
	case haveValue:
		m, isMap := value.(map[string]any)
		if haveScalar || (needMap && !isMap) {
			return nil, usage("%s: cannot combine a --yaml/JSON value with other arguments", spec.Name)
		}
		if !needMap {
			return value, nil
		}
		value = m
	case haveScalar && !needMap:
		return scalar, nil
	case haveScalar:
		if spec.Scalar == "" {
			return nil, usage("%s does not take a bare value; use --<field> flags", spec.Name)
		}
		m := map[string]any{spec.Scalar: scalar}
		value = m
	default:
		if !needMap {
			if valueOptional[spec.Name] {
				return nil, nil
			}
			return nil, usage("%s needs a value: %s", spec.Name, oneShotArgsUsage(spec))
		}
		value = map[string]any{}
	}

	m := value.(map[string]any)
	types := map[string]string{}
	for _, f := range spec.Fields {
		types[f.Key] = f.Type
	}
	for _, f := range fields {
		if top, _, _ := strings.Cut(f.Key, "."); len(types) > 0 && types[top] == "" {
			return nil, usage("%s has no field %q (see `maestro-d %s --help`)", spec.Name, top, spec.Name)
		}
		var v any = true
		if !f.Bare {
			var err error
			if v, err = typedScalar(f.Value); err != nil {
				return nil, err
			}
		}
		// map fields accept KEY=VALUE pairs: `runFlow x.yaml --env USER=bob`.
		if strings.HasPrefix(types[f.Key], "map[") && !f.Bare {
			if k, val, ok := strings.Cut(f.Value, "="); ok {
				if _, isMap := v.(map[string]any); !isMap {
					if err := setPath(m, f.Key+"."+k, val); err != nil {
						return nil, err
					}
					continue
				}
			}
		}
		if err := setPath(m, f.Key, v); err != nil {
			return nil, err
		}
	}
	for k, v := range p.Base {
		m[k] = v
	}
	if p.PlatformGate != "" {
		m["platform"] = p.PlatformGate
	}
	return m, nil
}
