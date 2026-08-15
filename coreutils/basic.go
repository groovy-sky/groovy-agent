package coreutils

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	register(Command{"pwd", "Print the current working directory", runPwd})
	register(Command{"basename", "Strip directory and suffix from a path", runBasename})
	register(Command{"dirname", "Strip the last component from a path", runDirname})
}

func noArgs(name string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s: unexpected operand %q", name, args[0])
	}
	return nil
}

func runPwd(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	if err := noArgs("pwd", args); err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, dir)
	return err
}

func runBasename(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("basename: expected PATH [SUFFIX]")
	}
	path := strings.TrimRight(args[0], string(filepath.Separator))
	if path == "" {
		path = string(filepath.Separator)
	}
	name := filepath.Base(path)
	if len(args) == 2 && args[1] != name {
		name = strings.TrimSuffix(name, args[1])
	}
	_, err := fmt.Fprintln(out, name)
	return err
}

func runDirname(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("dirname: expected PATH")
	}
	_, err := fmt.Fprintln(out, filepath.Dir(args[0]))
	return err
}
