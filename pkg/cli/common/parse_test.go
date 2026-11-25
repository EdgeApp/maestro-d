package common

import (
	"reflect"
	"strings"
	"testing"
)

type testOptions struct {
	platform string
	verbose  bool
	count    int
	target   string
}

func TestSharedFlagGrammar(t *testing.T) {
	platform := StringFlag("platform", "platform", nil, func(o *testOptions, v string) { o.platform = v }, "p")
	verbose := BoolFlag("verbose", "verbose", []string{"COMMON_TEST_VERBOSE"}, func(o *testOptions, v bool) { o.verbose = v }, "v")
	count := IntFlag("count", "count", []string{"COMMON_TEST_COUNT"}, func(o *testOptions, v int) { o.count = v })
	target := Flag[testOptions]{Name: "target", Kind: OptionalValue, Apply: func(o *testOptions, v string) error { o.target = v; return nil }}
	flags := FlagSet([]Flag[testOptions]{platform, verbose, count, target}, One(verbose))
	t.Setenv("COMMON_TEST_VERBOSE", "true")
	t.Setenv("COMMON_TEST_COUNT", "7")

	opts, parsed, err := ParseArgs([]string{"-p", "ios", "--target", "-1", "--no-verbose", "--optional", "--timeout", "-3", "--label=x", "--clear", "app", "--thing", "-", "--", "--literal"}, flags, map[string]string{"clear": "*bool"})
	if err != nil {
		t.Fatal(err)
	}
	if *opts != (testOptions{platform: "ios", count: 7}) || parsed.PlatformGate != "ios" || !parsed.Explicit["verbose"] || parsed.Explicit["count"] {
		t.Fatalf("options %+v, flags %+v", opts, parsed)
	}
	if len(flags) != 4 || !reflect.DeepEqual(parsed.Positional, []string{"-1", "app", "--literal"}) || !reflect.DeepEqual(parsed.Step, []StepFlag{{Key: "clear", Bare: true}, {Key: "thing", Value: "-"}}) || !reflect.DeepEqual(parsed.Base, map[string]any{"optional": true, "timeout": -3, "label": "x"}) {
		t.Fatalf("parsed %+v", parsed)
	}
	if !strings.Contains(FlagHelp("cmd", flags), "--target[=HOST[:PORT]]") || !IsBaseStepFlag("label") {
		t.Fatal("help and step flags diverged")
	}

	for _, arg := range []string{"--count=not-an-integer", "--optional=nope", "--verbose=maybe", "--timeout=zero", "--timeout", "--label", "--platform"} {
		t.Run(arg, func(t *testing.T) {
			if _, _, err := ParseArgs([]string{arg}, flags, nil); err == nil {
				t.Fatal("accepted invalid argument")
			}
		})
	}
	t.Setenv("COMMON_TEST_COUNT", "invalid")
	if _, _, err := ParseArgs(nil, flags, nil); err == nil {
		t.Fatal("accepted an invalid environment value")
	}
	opts, _, err = ParseArgs([]string{"--count=0", "--target=mac"}, flags, nil)
	if err != nil || opts.target != "mac" || !opts.verbose {
		t.Fatalf("explicit flags and environment: %+v, %v", opts, err)
	}
}
