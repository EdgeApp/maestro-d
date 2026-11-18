// Command gen derives the per-command tables the daemon surfaces need from
// upstream's pkg/flow sources, so a rebase that adds a command or a field
// only requires re-running it:
//
//	go run ./pkg/daemon/gen            # rewrite outputs
//	go run ./pkg/daemon/gen -check     # exit 1 if any output is stale
//
// It parses (go/ast, no type checking) decodeStep in pkg/flow/parser.go
// for the `case StepX: var s XStep` clauses and the scalar-shorthand
// assignment inside `if valueNode.Kind == yaml.ScalarNode`, and the step
// structs in pkg/flow for their yaml tags. Outputs:
//
//	pkg/daemon/commands_gen.go             Go: CommandSpecs
//	npm/maestro-d/src/generated/commands.ts   TS: params types + table
//	docs/daemon/commands.md                Markdown reference
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// scalarOverrides covers clauses whose scalar branch doesn't assign a
// tagged field directly (a switch on the value instead).
var scalarOverrides = map[string]string{
	"setAirplaneMode": "enabled",
	"setDarkMode":     "enabled",
}

// commandOverrides patch clauses the AST walk can't describe: the value
// of defineVariables is the variable map itself, not an `env:` key.
var commandOverrides = map[string]func(*Command){
	"defineVariables": func(c *Command) {
		c.ValueLess = false
		c.Scalar = ""
		c.Doc = "Defines session variables; the value is a map of NAME: value."
		c.Fields = nil
	},
	// Nested steps are tagged `yaml:"-"` and read by hand in the parser.
	"repeat": func(c *Command) {
		c.Fields = append(c.Fields, nestedField("commands", "Steps to repeat"))
	},
	"retry": func(c *Command) {
		c.Fields = append(c.Fields, nestedField("commands", "Steps to retry (alternative to file)"))
	},
	"runFlow": func(c *Command) {
		c.Fields = append(c.Fields,
			nestedField("commands", "Inline steps (alternative to file)"),
			Field{Key: "else", GoType: "string|[]Step", TSType: "string | Step[]", Doc: "Fallback flow file or inline steps when `when` is false"},
			nestedField("elseCommands", "Inline fallback steps when `when` is false"),
		)
	},
}

func nestedField(key, doc string) Field {
	return Field{Key: key, GoType: "[]Step", TSType: "Step[]", Doc: doc}
}

// fileMark marks a field that names a file on the caller's machine.
type fileMark struct {
	// Path: an input file the client uploads to a hub and names by a
	// blob: ref. Suffix, when set, limits that to values ending in it
	// (runScript's script and file are paths only when they end in .js).
	Path   bool
	Suffix string
	// Output: a file the command writes on the device's host, which the
	// client names itself and fetches back from a hub.
	Output bool
	// Flow: a flow file, packed with its own references (`pack --flow`)
	// and sent to a hub as a bundle: ref.
	Flow bool
}

// fileFields are the fields that name files, by command and yaml key. The
// executor's caller policy walks the same set (executor.PolicyFields); a
// test in pkg/daemon keeps the two in step.
var fileFields = map[string]map[string]fileMark{
	"addMedia":         {"files": {Path: true}},
	"runScript":        {"script": {Path: true, Suffix: ".js"}, "file": {Path: true, Suffix: ".js"}},
	"assertScreenshot": {"path": {Path: true}},
	"runBrowserScript": {"file": {Path: true}},
	"runWebViewScript": {"file": {Path: true}},
	"uploadFile":       {"path": {Path: true}, "paths": {Path: true}},
	"loadAuthState":    {"path": {Path: true}},
	"runFlow":          {"file": {Flow: true}, "else": {Flow: true}},
	"retry":            {"file": {Flow: true}},
	"takeScreenshot":   {"path": {Output: true}},
	"saveAuthState":    {"path": {Output: true}},
}

// bareFieldOverrides name the field a bare value fills for commands with
// no Scalar: addMedia's bare scalar or sequence is its files.
var bareFieldOverrides = map[string]string{"addMedia": "files"}

// markFiles applies fileFields and the bare field to a command; a marked
// field the command does not have is an error, so an upstream rename is
// caught here.
func markFiles(c *Command) error {
	c.BareField = c.Scalar
	if v, ok := bareFieldOverrides[c.Name]; ok {
		c.BareField = v
	}
	marks := fileFields[c.Name]
	found := 0
	for i := range c.Fields {
		if m, ok := marks[c.Fields[i].Key]; ok {
			c.Fields[i].Path, c.Fields[i].Output, c.Fields[i].Flow, c.Fields[i].PathSuffix = m.Path, m.Output, m.Flow, m.Suffix
			found++
		}
	}
	if found != len(marks) {
		return fmt.Errorf("%s: fileFields names %d fields, the command has %d of them", c.Name, len(marks), found)
	}
	return nil
}

// compoundHelpers maps parse helpers used by compound steps to their struct.
var compoundHelpers = map[string]string{
	"parseRepeatStep":  "RepeatStep",
	"parseRetryStep":   "RetryStep",
	"parseRunFlowStep": "RunFlowStep",
}

// Field is one YAML key of a command's map form.
type Field struct {
	Key    string // yaml key
	GoName string
	GoType string
	TSType string
	Doc    string
	// Inline is the embedded struct this field came from ("" for own).
	Inline string
	// Path, Output, Flow and PathSuffix come from fileFields.
	Path       bool
	Output     bool
	Flow       bool
	PathSuffix string
}

// Command is one YAML command.
type Command struct {
	Name      string
	Struct    string
	Doc       string
	Scalar    string // yaml key the scalar shorthand sets ("" = none)
	ScalarGo  string // Go type of that field
	ValueLess bool   // command ignores its value (`- back`)
	Compound  bool   // repeat / retry / runFlow (nested steps)
	// BareField is the field a bare value fills: Scalar, or files for
	// addMedia.
	BareField string
	Fields    []Field
}

// freeForm reports whether a command's value is a map of arbitrary keys
// rather than the fields declared on its struct — defineVariables, whose
// value is the variable map itself. Base fields are added at render time,
// so a command with none of its own is free-form.
func freeForm(c Command) bool { return len(c.Fields) == 0 && !c.ValueLess }

type structInfo struct {
	name   string
	doc    string
	fields []*ast.Field
}

func main() {
	check := flag.Bool("check", false, "verify outputs are up to date instead of writing")
	flag.Parse()

	root, err := repoRoot()
	must(err)
	flowDir := filepath.Join(root, "pkg", "flow")

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, flowDir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	must(err)
	pkg := pkgs["flow"]
	if pkg == nil {
		must(fmt.Errorf("package flow not found in %s", flowDir))
	}

	structs := map[string]*structInfo{}
	consts := map[string]string{} // StepTapOn → "tapOn"
	var decode *ast.FuncDecl
	helpers := map[string]*ast.FuncDecl{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				for _, sp := range d.Specs {
					switch sp := sp.(type) {
					case *ast.TypeSpec:
						if st, ok := sp.Type.(*ast.StructType); ok {
							doc := ""
							if sp.Doc != nil {
								doc = sp.Doc.Text()
							} else if d.Doc != nil {
								doc = d.Doc.Text()
							}
							structs[sp.Name.Name] = &structInfo{name: sp.Name.Name, doc: doc, fields: st.Fields.List}
						}
					case *ast.ValueSpec:
						if len(sp.Names) == 1 && len(sp.Values) == 1 {
							if lit, ok := sp.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.HasPrefix(sp.Names[0].Name, "Step") {
								consts[sp.Names[0].Name] = strings.Trim(lit.Value, `"`)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name.Name == "decodeStep" {
					decode = d
				}
				if _, ok := compoundHelpers[d.Name.Name]; ok {
					helpers[d.Name.Name] = d
				}
			}
		}
	}
	if decode == nil {
		must(fmt.Errorf("decodeStep not found"))
	}

	var cmds []Command
	ast.Inspect(decode.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			if cc.List == nil {
				continue
			}
			for _, e := range cc.List {
				id, ok := e.(*ast.Ident)
				if !ok {
					continue
				}
				name, ok := consts[id.Name]
				if !ok {
					continue
				}
				cmd := Command{Name: name}
				analyseClause(&cmd, cc.Body, helpers)
				if cmd.Struct == "" {
					fmt.Fprintf(os.Stderr, "warning: no struct for %s\n", name)
					continue
				}
				si := structs[cmd.Struct]
				if si == nil {
					fmt.Fprintf(os.Stderr, "warning: struct %s not found for %s\n", cmd.Struct, name)
					continue
				}
				cmd.Doc = cleanDoc(si.doc, cmd.Struct)
				cmd.Fields = collectFields(si, structs, "")
				if ov, ok := scalarOverrides[name]; ok {
					cmd.Scalar = ov
				} else if cmd.Scalar != "" {
					cmd.Scalar = resolvePath(cmd.Scalar, si, structs)
				}
				if cmd.Scalar != "" {
					for _, f := range cmd.Fields {
						if f.Key == cmd.Scalar {
							cmd.ScalarGo = f.GoType
						}
					}
				}
				if ov := commandOverrides[name]; ov != nil {
					ov(&cmd)
				}
				must(markFiles(&cmd))
				cmds = append(cmds, cmd)
			}
		}
		return false
	})
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })

	outputs := map[string][]byte{
		filepath.Join(root, "pkg", "daemon", "commands_gen.go"):                    renderGo(cmds),
		filepath.Join(root, "npm", "maestro-d", "src", "generated", "commands.ts"): renderTS(cmds),
		filepath.Join(root, "docs", "daemon", "commands.md"):                       renderMD(cmds),
	}
	stale := false
	for path, data := range outputs {
		if *check {
			cur, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(cur, data) {
				fmt.Fprintf(os.Stderr, "stale: %s\n", path)
				stale = true
			}
			continue
		}
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, data, 0o644))
		fmt.Println("wrote", path)
	}
	if stale {
		fmt.Fprintln(os.Stderr, "run `go run ./pkg/daemon/gen` to regenerate")
		os.Exit(1)
	}
}

// analyseClause finds the step struct and the scalar-shorthand assignment.
func analyseClause(cmd *Command, body []ast.Stmt, helpers map[string]*ast.FuncDecl) {
	hasDecodeIntoS := false
	for _, st := range body {
		ast.Inspect(st, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.DeclStmt:
				if gd, ok := n.Decl.(*ast.GenDecl); ok {
					for _, sp := range gd.Specs {
						if vs, ok := sp.(*ast.ValueSpec); ok && len(vs.Names) == 1 && vs.Names[0].Name == "s" {
							if id, ok := vs.Type.(*ast.Ident); ok {
								cmd.Struct = id.Name
							}
						}
					}
				}
			case *ast.ReturnStmt:
				for _, r := range n.Results {
					if ue, ok := r.(*ast.UnaryExpr); ok {
						if cl, ok := ue.X.(*ast.CompositeLit); ok {
							if id, ok := cl.Type.(*ast.Ident); ok && cmd.Struct == "" {
								cmd.Struct = id.Name
								cmd.ValueLess = true
							}
						}
					}
					if call, ok := r.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok {
							if sn, ok := compoundHelpers[id.Name]; ok {
								cmd.Struct = sn
								cmd.Compound = true
								if h := helpers[id.Name]; h != nil {
									analyseClause(cmd, h.Body.List, nil)
									cmd.Compound = true
									cmd.ValueLess = false
								}
							}
						}
					}
				}
			case *ast.IfStmt:
				if exprHas(n.Cond, "ScalarNode") {
					if p := firstAssignToS(n.Body); p != "" && cmd.Scalar == "" {
						cmd.Scalar = p
					}
				}
			case *ast.CallExpr:
				// valueNode.Decode(&s) means the map form is honoured.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Decode" {
					for _, a := range n.Args {
						if ue, ok := a.(*ast.UnaryExpr); ok {
							if id, ok := ue.X.(*ast.Ident); ok && id.Name == "s" {
								hasDecodeIntoS = true
							}
						}
					}
				}
			}
			return true
		})
	}
	if cmd.Struct != "" && !cmd.ValueLess && !hasDecodeIntoS && !cmd.Compound && cmd.Scalar == "" {
		// `var s X; s.StepType = …; return &s` without decoding: value-less.
		cmd.ValueLess = true
	}
}

// firstAssignToS returns the "A.B" path of the first `s.A.B = …` in block.
func firstAssignToS(block *ast.BlockStmt) string {
	var path string
	ast.Inspect(block, func(n ast.Node) bool {
		if path != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		var parts []string
		e := as.Lhs[0]
		for {
			sel, ok := e.(*ast.SelectorExpr)
			if !ok {
				break
			}
			parts = append([]string{sel.Sel.Name}, parts...)
			e = sel.X
		}
		if id, ok := e.(*ast.Ident); ok && id.Name == "s" && len(parts) > 0 {
			path = strings.Join(parts, ".")
		}
		return true
	})
	return path
}

func exprHas(e ast.Expr, ident string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == ident {
			found = true
		}
		return !found
	})
	return found
}

// resolvePath maps a Go field path (Selector.Text) to its yaml key.
func resolvePath(path string, si *structInfo, structs map[string]*structInfo) string {
	parts := strings.Split(path, ".")
	cur := si
	for i, p := range parts {
		var match *ast.Field
		for _, f := range cur.fields {
			for _, n := range f.Names {
				if n.Name == p {
					match = f
				}
			}
			if len(f.Names) == 0 && typeName(f.Type) == p {
				match = f
			}
		}
		if match == nil {
			return ""
		}
		tag := yamlTag(match)
		if i == len(parts)-1 {
			if tag == "-" {
				return ""
			}
			if tag == "" {
				return lowerFirst(p)
			}
			return tag
		}
		next := structs[typeName(match.Type)]
		if next == nil {
			return ""
		}
		if tag != ",inline" && tag != "" && tag != "-" {
			// Nested (non-inline) struct: key path a.b; the CLI merges maps.
			rest := resolvePath(strings.Join(parts[i+1:], "."), next, structs)
			if rest == "" {
				return ""
			}
			return tag + "." + rest
		}
		cur = next
	}
	return ""
}

// collectFields flattens a step struct's yaml-visible fields.
func collectFields(si *structInfo, structs map[string]*structInfo, inline string) []Field {
	var out []Field
	for _, f := range si.fields {
		tag := yamlTag(f)
		if tag == "-" {
			continue
		}
		if len(f.Names) == 0 || tag == ",inline" {
			// Embedded / inline struct.
			tn := typeName(f.Type)
			if next := structs[tn]; next != nil {
				out = append(out, collectFields(next, structs, tn)...)
			}
			continue
		}
		key := tag
		if key == "" {
			key = lowerFirst(f.Names[0].Name)
		}
		key = strings.Split(key, ",")[0]
		if key == "" {
			key = lowerFirst(f.Names[0].Name)
		}
		doc := ""
		if f.Doc != nil {
			doc = strings.TrimSpace(f.Doc.Text())
		} else if f.Comment != nil {
			doc = strings.TrimSpace(f.Comment.Text())
		}
		doc = strings.Join(strings.Fields(doc), " ")
		gt := exprString(f.Type)
		out = append(out, Field{Key: key, GoName: f.Names[0].Name, GoType: gt, TSType: tsType(gt), Doc: doc, Inline: inline})
	}
	// Later duplicates (e.g. Selector.Optional after BaseStep.Optional)
	// are dropped: first wins.
	seen := map[string]bool{}
	dedup := out[:0]
	for _, f := range out {
		if seen[f.Key] {
			continue
		}
		seen[f.Key] = true
		dedup = append(dedup, f)
	}
	return dedup
}

func yamlTag(f *ast.Field) string {
	if f.Tag == nil {
		return ""
	}
	tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
	return tag.Get("yaml")
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.InterfaceType:
		return "any"
	}
	return fmt.Sprintf("%T", e)
}

func tsType(goType string) string {
	switch goType {
	case "string":
		return "string"
	case "int", "int64", "float64":
		return "number"
	case "bool", "*bool":
		return "boolean"
	case "any", "interface{}":
		return "unknown"
	case "Selector", "*Selector":
		return "Selector"
	case "[]*Selector", "[]Selector":
		return "Selector[]"
	case "Condition", "*Condition":
		return "Condition"
	case "map[string]string":
		return "Record<string, string>"
	case "[]string":
		return "string[]"
	case "[]Step":
		return "Step[]"
	}
	if strings.HasPrefix(goType, "*") {
		return tsType(goType[1:])
	}
	return "unknown"
}

func cleanDoc(doc, structName string) string {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return ""
	}
	first := strings.SplitN(doc, "\n\n", 2)[0]
	first = strings.Join(strings.Fields(first), " ")
	first = strings.TrimPrefix(first, structName+" ")
	if first != "" {
		first = strings.ToUpper(first[:1]) + first[1:]
	}
	return first
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Renderers

const header = "Code generated by go run ./pkg/daemon/gen from pkg/flow; DO NOT EDIT."

func renderGo(cmds []Command) []byte {
	src := renderGoSource(cmds)
	out, err := format.Source(src)
	must(err)
	return out
}

func renderGoSource(cmds []Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// %s\n\npackage daemon\n\n", header)
	b.WriteString("// CommandSpec describes one YAML command's shape for the CLI and docs.\n")
	b.WriteString("type CommandSpec struct {\n\tName string\n\t// Scalar is the yaml key a bare scalar value maps to (\"\" = none).\n\tScalar string\n\t// BareField is the field a bare value fills: Scalar when set, and\n\t// files for addMedia (\"\" = none). Clients apply that field's Path,\n\t// Flow or Output mark to a bare value, element by element for a sequence.\n\tBareField string\n\t// ValueLess commands ignore their value (`- back`).\n\tValueLess bool\n\t// Compound commands carry nested steps (repeat, retry, runFlow).\n\tCompound bool\n\t// FreeForm commands take arbitrary keys rather than the fields below\n\t// (defineVariables: NAME -> value), so their keys are not validated.\n\tFreeForm bool\n\t// Doc is the one-line description from the Go struct comment.\n\tDoc string\n\t// Fields are the yaml keys of the map form.\n\tFields []FieldSpec\n}\n\n")
	b.WriteString("// FieldSpec is one yaml key of a command's map form.\ntype FieldSpec struct {\n\tKey  string\n\tType string // Go type\n\tDoc  string\n\t// Path marks an input file on the caller's machine: a client that\n\t// targets a hub uploads it and sends a blob: ref instead. PathSuffix,\n\t// when set, limits that to values ending in it.\n\tPath       bool\n\tPathSuffix string\n\t// Output marks a file the command writes on the device's host: a\n\t// client that targets a hub sends a name of its own and fetches the\n\t// file back.\n\tOutput bool\n\t// Flow marks a flow file on the caller's machine: a client that\n\t// targets a hub packs it with its own references (pack --flow) and\n\t// sends a bundle: ref instead.\n\tFlow bool\n}\n\n")
	b.WriteString("// CommandSpecs indexes every command by name.\nvar CommandSpecs = map[string]CommandSpec{\n")
	for _, c := range cmds {
		bare := ""
		if c.BareField != "" {
			bare = fmt.Sprintf(" BareField: %q,", c.BareField)
		}
		fmt.Fprintf(&b, "\t%q: {Name: %q, Scalar: %q,%s ValueLess: %v, Compound: %v, FreeForm: %v, Doc: %q, Fields: []FieldSpec{\n", c.Name, c.Name, c.Scalar, bare, c.ValueLess, c.Compound, freeForm(c), c.Doc)
		for _, f := range c.Fields {
			marks := ""
			if f.Path {
				marks += ", Path: true"
			}
			if f.PathSuffix != "" {
				marks += fmt.Sprintf(", PathSuffix: %q", f.PathSuffix)
			}
			if f.Output {
				marks += ", Output: true"
			}
			if f.Flow {
				marks += ", Flow: true"
			}
			fmt.Fprintf(&b, "\t\t{Key: %q, Type: %q, Doc: %q%s},\n", f.Key, f.GoType, f.Doc, marks)
		}
		b.WriteString("\t}},\n")
	}
	b.WriteString("}\n")
	return b.Bytes()
}

func renderTS(cmds []Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// %s\n\n", header)
	b.WriteString("import type { Condition, Selector, Step } from '../types'\n\n")
	for _, c := range cmds {
		tn := paramsTypeName(c.Name)
		if c.Doc != "" {
			fmt.Fprintf(&b, "/** %s */\n", c.Doc)
		}
		if freeForm(c) {
			// A free-form map (defineVariables): NAME → value.
			fmt.Fprintf(&b, "export type %s = Record<string, string>\n\n", tn)
			continue
		}
		fmt.Fprintf(&b, "export interface %s {\n", tn)
		for _, f := range c.Fields {
			if f.Doc != "" {
				fmt.Fprintf(&b, "  /** %s */\n", strings.ReplaceAll(f.Doc, "*/", "* /"))
			}
			fmt.Fprintf(&b, "  %s?: %s\n", tsKey(f.Key), f.TSType)
		}
		b.WriteString("}\n\n")
	}
	b.WriteString("/** Shape of every command: scalar shorthand key, value-less, compound. */\n")
	b.WriteString("/** How a field names a file: an input the client uploads to a hub, a flow it packs as a bundle, or an output it fetches back. */\n")
	b.WriteString("export interface FileMark {\n  kind: 'path' | 'output' | 'flow'\n  /** The field is a path only when its value ends in this. */\n  suffix?: string\n}\n\n")
	b.WriteString("export interface CommandSpec {\n  name: string\n  scalar: string | null\n  /** The field a bare value fills: scalar, or files for addMedia. */\n  bareField: string | null\n  valueLess: boolean\n  compound: boolean\n  doc: string\n  /** The fields that name files, by key. */\n  files?: Record<string, FileMark>\n}\n\n")
	b.WriteString("export const COMMANDS = {\n")
	for _, c := range cmds {
		scalar := "null"
		if c.Scalar != "" {
			scalar = fmt.Sprintf("'%s'", c.Scalar)
		}
		bare := "null"
		if c.BareField != "" {
			bare = fmt.Sprintf("'%s'", c.BareField)
		}
		files := ""
		for _, f := range c.Fields {
			if !f.Path && !f.Output && !f.Flow {
				continue
			}
			kind := "path"
			switch {
			case f.Output:
				kind = "output"
			case f.Flow:
				kind = "flow"
			}
			suffix := ""
			if f.PathSuffix != "" {
				suffix = fmt.Sprintf(", suffix: '%s'", f.PathSuffix)
			}
			if files != "" {
				files += ", "
			}
			files += fmt.Sprintf("%s: { kind: '%s'%s }", tsKey(f.Key), kind, suffix)
		}
		if files != "" {
			files = ", files: { " + files + " }"
		}
		fmt.Fprintf(&b, "  %s: { name: '%s', scalar: %s, bareField: %s, valueLess: %v, compound: %v, doc: %s%s },\n",
			tsKey(c.Name), c.Name, scalar, bare, c.ValueLess, c.Compound, tsString(c.Doc), files)
	}
	b.WriteString("} as const satisfies Record<string, CommandSpec>\n\n")
	b.WriteString("export type CommandName = keyof typeof COMMANDS\n\n")
	b.WriteString("/** Params type per command name. */\nexport interface CommandParams {\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "  %s: %s\n", tsKey(c.Name), paramsTypeName(c.Name))
	}
	b.WriteString("}\n")
	return b.Bytes()
}

func paramsTypeName(name string) string {
	return strings.ToUpper(name[:1]) + name[1:] + "Params"
}

func tsKey(k string) string {
	for _, r := range k {
		if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return fmt.Sprintf("'%s'", k)
		}
	}
	return k
}

func tsString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}

func renderMD(cmds []Command) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "<!-- %s -->\n\n", header)
	b.WriteString("# Command reference\n\n")
	b.WriteString("Every YAML command maestro-runner understands is available as a one-shot CLI command,\n")
	b.WriteString("a REST call (`POST /v1/devices/{id}/commands/<name>` with the value as the JSON body)\n")
	b.WriteString("and a JavaScript method of the same name. **Scalar** is the key a bare value maps to\n")
	b.WriteString("(`tapOn Login` ≡ `tapOn --text Login`); commands without one take only the map form or\n")
	b.WriteString("no value at all. The common keys `optional`, `label`, `timeout` and `platform` are\n")
	b.WriteString("accepted by every command and passed as CLI flags / JS `opts`.\n\n")
	b.WriteString("| Command | Scalar | Description |\n| --- | --- | --- |\n")
	for _, c := range cmds {
		sc := "—"
		if c.Scalar != "" {
			sc = "`" + c.Scalar + "`"
		} else if c.ValueLess {
			sc = "_none_"
		}
		fmt.Fprintf(&b, "| [`%s`](#%s) | %s | %s |\n", c.Name, strings.ToLower(c.Name), sc, c.Doc)
	}
	b.WriteString("\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "## `%s`\n\n", c.Name)
		if c.Doc != "" {
			b.WriteString(c.Doc + "\n\n")
		}
		switch {
		case c.ValueLess:
			fmt.Fprintf(&b, "```sh\nmaestro-d %s\n```\n```js\nawait m.%s()\n```\n\n", c.Name, c.Name)
		case c.Scalar != "":
			fmt.Fprintf(&b, "```sh\nmaestro-d %s <%s>\nmaestro-d %s --%s <%s> [--key value …]\n```\n```js\nawait m.%s('<%s>')\nawait m.%s({ %s: '<%s>', … })\n```\n\n",
				c.Name, c.Scalar, c.Name, c.Scalar, c.Scalar, c.Name, c.Scalar, c.Name, c.Scalar, c.Scalar)
		default:
			fmt.Fprintf(&b, "```sh\nmaestro-d %s --key value […]\n```\n```js\nawait m.%s({ … })\n```\n\n", c.Name, c.Name)
		}
		if c.Compound {
			b.WriteString("Nested steps (`commands:` / `steps:`) are given as a YAML list or JSON array; the CLI reads them from `--yaml <file|->`.\n\n")
		}
		if len(c.Fields) > 0 {
			b.WriteString("| Key | Type | Description |\n| --- | --- | --- |\n")
			for _, f := range c.Fields {
				doc := strings.ReplaceAll(f.Doc, "|", "\\|")
				switch {
				case f.Path && f.PathSuffix != "":
					doc = strings.TrimSpace(doc + fmt.Sprintf(" A file when the value ends in `%s`: uploaded to a hub.", f.PathSuffix))
				case f.Path:
					doc = strings.TrimSpace(doc + " A file on the caller's machine: uploaded to a hub.")
				case f.Output:
					doc = strings.TrimSpace(doc + " Written on the device's host: fetched back from a hub.")
				case f.Flow:
					doc = strings.TrimSpace(doc + " A flow file on the caller's machine: packed with its references and sent to a hub as a bundle.")
				}
				fmt.Fprintf(&b, "| `%s` | `%s` | %s |\n", f.Key, f.TSType, doc)
			}
			b.WriteString("\n")
		}
	}
	return b.Bytes()
}
