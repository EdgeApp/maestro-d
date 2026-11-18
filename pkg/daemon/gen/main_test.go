package main

import (
	"strings"
	"testing"
)

func sampleCommands(t *testing.T) []Command {
	t.Helper()
	cmds := []Command{
		{Name: "addMedia", Fields: []Field{{Key: "files", GoType: "[]string", TSType: "string[]"}}},
		{Name: "runScript", Scalar: "script", Fields: []Field{
			{Key: "script", GoType: "string", TSType: "string"}, {Key: "file", GoType: "string", TSType: "string", Doc: "Script filename"},
		}},
		{Name: "takeScreenshot", Scalar: "path", Fields: []Field{{Key: "path", GoType: "string", TSType: "string"}}},
		{Name: "tapOn", Scalar: "text", Fields: []Field{{Key: "text", GoType: "string", TSType: "string"}}},
		{Name: "retry", Fields: []Field{{Key: "file", GoType: "string", TSType: "string"}}},
	}
	for i := range cmds {
		if err := markFiles(&cmds[i]); err != nil {
			t.Fatal(err)
		}
	}
	return cmds
}

func TestMarkFiles(t *testing.T) {
	cmds := sampleCommands(t)
	if cmds[0].BareField != "files" || !cmds[0].Fields[0].Path {
		t.Fatalf("addMedia: %+v", cmds[0])
	}
	if cmds[1].BareField != "script" || cmds[1].Fields[0].PathSuffix != ".js" || cmds[1].Fields[1].PathSuffix != ".js" {
		t.Fatalf("runScript: %+v", cmds[1])
	}
	if !cmds[2].Fields[0].Output || cmds[3].Fields[0].Path || cmds[3].BareField != "text" {
		t.Fatalf("takeScreenshot/tapOn: %+v %+v", cmds[2], cmds[3])
	}
	if !cmds[4].Fields[0].Flow || cmds[4].Fields[0].Path || cmds[4].BareField != "" {
		t.Fatalf("retry: %+v", cmds[4])
	}
	// A marked field the command lacks is an error (an upstream rename).
	if err := markFiles(&Command{Name: "addMedia"}); err == nil {
		t.Fatal("expected an error for a missing marked field")
	}
}

func TestRenderFileMarks(t *testing.T) {
	cmds := sampleCommands(t)
	goSrc := string(renderGoSource(cmds))
	for _, want := range []string{
		`BareField: "files"`,
		`{Key: "files", Type: "[]string", Doc: "", Path: true}`,
		`Path: true, PathSuffix: ".js"`,
		`{Key: "path", Type: "string", Doc: "", Output: true}`,
		"\tBareField string\n",
		"\tPathSuffix string\n",
		`{Key: "file", Type: "string", Doc: "", Flow: true}`,
		"\tFlow bool\n",
	} {
		if !strings.Contains(goSrc, want) {
			t.Errorf("Go output lacks %q", want)
		}
	}
	ts := string(renderTS(cmds))
	for _, want := range []string{
		"bareField: 'files'",
		"files: { files: { kind: 'path' } }",
		"files: { script: { kind: 'path', suffix: '.js' }, file: { kind: 'path', suffix: '.js' } }",
		"files: { path: { kind: 'output' } }",
		"bareField: 'text', valueLess: false",
		"export interface FileMark",
		"files: { file: { kind: 'flow' } }",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("TS output lacks %q", want)
		}
	}
	md := string(renderMD(cmds))
	for _, want := range []string{
		"A file on the caller's machine: uploaded to a hub.",
		"Script filename A file when the value ends in `.js`: uploaded to a hub.",
		"Written on the device's host: fetched back from a hub.",
		"A flow file on the caller's machine: packed with its references and sent to a hub as a bundle.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown output lacks %q", want)
		}
	}
}
