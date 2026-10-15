package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDispatchResolvesNestedPath(t *testing.T) {
	var ranArgs []string
	root := &Command{
		Name:  "ipcod",
		Short: "root",
		Sub: []*Command{
			{
				Name:  "agent",
				Short: "agent cmd",
				Sub: []*Command{
					{
						Name:  "check",
						Short: "check cmd",
						Run: func(cmd *Command, args []string) error {
							ranArgs = args
							return nil
						},
					},
				},
			},
		},
	}

	var errw bytes.Buffer
	code := Execute(root, []string{"agent", "check", "-token", "abc"}, &errw)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errw.String())
	}
	if ranArgs == nil {
		t.Fatal("leaf Run was not invoked")
	}
	// The dispatcher must pass the flag args through to the leaf untouched.
	if len(ranArgs) != 2 || ranArgs[0] != "-token" || ranArgs[1] != "abc" {
		t.Fatalf("ranArgs = %v, want [-token abc]", ranArgs)
	}
}

func TestDispatchRequiresSubcommandWhenNotDefault(t *testing.T) {
	root := &Command{
		Name:            "ipcod",
		Short:           "root",
		DefaultDisabled: true,
		Sub: []*Command{
			{Name: "run", Short: "run", Run: func(*Command, []string) error { return nil }},
		},
	}
	var errw bytes.Buffer
	code := Execute(root, nil, &errw)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "requires a subcommand") {
		t.Fatalf("stderr = %q, want 'requires a subcommand'", errw.String())
	}
}

func TestDispatchDefaultRunWhenNoSubcommand(t *testing.T) {
	ran := false
	root := &Command{
		Name:  "ipcod",
		Short: "root",
		Run: func(*Command, []string) error {
			ran = true
			return nil
		},
		Sub: []*Command{
			{Name: "sub", Short: "sub", Run: func(*Command, []string) error { return nil }},
		},
	}
	var errw bytes.Buffer
	if code := Execute(root, nil, &errw); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !ran {
		t.Fatal("default Run not invoked")
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	root := &Command{
		Name:  "ipcod",
		Short: "root",
		Sub:   []*Command{{Name: "sub", Short: "s", Run: func(*Command, []string) error { return nil }}},
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var errw bytes.Buffer
		if code := Execute(root, args, &errw); code != 0 {
			t.Fatalf("args %v exit = %d, want 0", args, code)
		}
	}
}

func TestUnknownCommandExitsNonZero(t *testing.T) {
	root := &Command{
		Name:            "ipcod",
		Short:           "root",
		DefaultDisabled: true,
		Sub: []*Command{
			{Name: "run", Short: "run", Run: func(*Command, []string) error { return nil }},
		},
	}
	var errw bytes.Buffer
	if code := Execute(root, []string{"bogus"}, &errw); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "unknown command") {
		t.Fatalf("stderr = %q, want 'unknown command'", errw.String())
	}
}

func TestHelpShowsFullInvocationPath(t *testing.T) {
	root := &Command{
		Name:  "ipcod",
		Short: "root",
		Sub: []*Command{
			{
				Name:  "agent",
				Short: "agent",
				Sub: []*Command{
					{Name: "check", Short: "check", Run: func(*Command, []string) error { return nil }},
				},
			},
		},
	}
	var errw bytes.Buffer
	Execute(root, []string{"agent", "check", "--help"}, &errw)
	if !strings.Contains(errw.String(), "ipcod agent check") {
		t.Fatalf("help should show full path; got:\n%s", errw.String())
	}
}

func TestRunErrorExitsOne(t *testing.T) {
	root := &Command{
		Name:  "ipcod",
		Short: "root",
		Run: func(*Command, []string) error {
			return errors.New("boom")
		},
	}
	var errw bytes.Buffer
	if code := Execute(root, nil, &errw); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}
