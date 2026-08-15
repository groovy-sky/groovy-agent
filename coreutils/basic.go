package coreutils

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func init() {
	register(Command{"echo", "Display a line of text", runEcho})
	register(Command{"pwd", "Print the current working directory", runPwd})
	register(Command{"basename", "Strip directory and suffix from a path", runBasename})
	register(Command{"dirname", "Strip the last component from a path", runDirname})
	register(Command{"whoami", "Print the current user name", runWhoami})
	register(Command{"uname", "Print system information", runUname})
	register(Command{"env", "Print an optionally modified environment", runEnv})
	register(Command{"seq", "Print a sequence of numbers", runSeq})
	register(Command{"sleep", "Pause for a duration", runSleep})
	register(Command{"true", "Return successfully", runTrue})
}

func noArgs(name string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("%s: unexpected operand %q", name, args[0])
	}
	return nil
}

func runEcho(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	newline := true
	if len(args) > 0 && args[0] == "-n" {
		newline = false
		args = args[1:]
	}
	_, err := io.WriteString(out, strings.Join(args, " "))
	if err == nil && newline {
		_, err = io.WriteString(out, "\n")
	}
	return err
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

func runWhoami(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	if err := noArgs("whoami", args); err != nil {
		return err
	}
	current, err := user.Current()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, current.Username)
	return err
}

func runUname(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	all := len(args) == 1 && args[0] == "-a"
	if len(args) > 0 && !all && args[0] != "-s" && args[0] != "-m" {
		return fmt.Errorf("uname: supported options are -a, -s, and -m")
	}
	if len(args) > 1 {
		return fmt.Errorf("uname: too many options")
	}
	value := runtime.GOOS
	if len(args) == 1 && args[0] == "-m" {
		value = runtime.GOARCH
	} else if all {
		host, _ := os.Hostname()
		value = strings.Join([]string{runtime.GOOS, host, runtime.GOARCH, runtime.Version()}, " ")
	}
	_, err := fmt.Fprintln(out, value)
	return err
}

func runEnv(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	environment := append([]string(nil), os.Environ()...)
	for _, arg := range args {
		if !strings.Contains(arg, "=") {
			return fmt.Errorf("env: only NAME=VALUE operands are supported")
		}
		name := strings.SplitN(arg, "=", 2)[0]
		prefix := name + "="
		replaced := false
		for i := range environment {
			if strings.HasPrefix(environment[i], prefix) {
				environment[i], replaced = arg, true
				break
			}
		}
		if !replaced {
			environment = append(environment, arg)
		}
	}
	for _, item := range environment {
		if _, err := fmt.Fprintln(out, item); err != nil {
			return err
		}
	}
	return nil
}

func runSeq(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	if len(args) < 1 || len(args) > 3 {
		return fmt.Errorf("seq: expected LAST, FIRST LAST, or FIRST STEP LAST")
	}
	values := make([]float64, len(args))
	for i, arg := range args {
		value, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return fmt.Errorf("seq: invalid number %q", arg)
		}
		values[i] = value
	}
	first, step, last := 1.0, 1.0, values[0]
	if len(values) >= 2 {
		first, last = values[0], values[1]
	}
	if len(values) == 3 {
		first, step, last = values[0], values[1], values[2]
	}
	if step == 0 {
		return fmt.Errorf("seq: zero increment")
	}
	for value, count := first, 0; (step > 0 && value <= last) || (step < 0 && value >= last); value, count = value+step, count+1 {
		if count >= 1_000_000 {
			return fmt.Errorf("seq: output limit exceeded")
		}
		if _, err := fmt.Fprintln(out, strconv.FormatFloat(value, 'g', -1, 64)); err != nil {
			return err
		}
	}
	return nil
}

func runSleep(ctx context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("sleep: expected DURATION (for example 1s or 250ms)")
	}
	duration, err := time.ParseDuration(args[0])
	if err != nil || duration < 0 {
		return fmt.Errorf("sleep: invalid duration %q", args[0])
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runTrue(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	return noArgs("true", args)
}
