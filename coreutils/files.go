package coreutils

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

func init() {
	register(Command{"cat", "Concatenate files or standard input", runCat})
	register(Command{"head", "Print the first lines of input", runHead})
	register(Command{"tail", "Print the last lines of input", runTail})
	register(Command{"tee", "Copy standard input to files and standard output", runTee})
	register(Command{"touch", "Create files or update their timestamps", runTouch})
	register(Command{"mkdir", "Create directories", runMkdir})
	register(Command{"rmdir", "Remove empty directories", runRmdir})
	register(Command{"unlink", "Remove a file", runUnlink})
	register(Command{"link", "Create a hard link", runLink})
}

func eachInput(args []string, stdin io.Reader, fn func(string, io.Reader) error) error {
	if len(args) == 0 {
		return fn("-", stdin)
	}
	for _, name := range args {
		if name == "-" {
			if err := fn(name, stdin); err != nil {
				return err
			}
			continue
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		err = fn(name, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func runCat(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	return eachInput(args, stdin, func(_ string, input io.Reader) error {
		_, err := io.Copy(out, input)
		return err
	})
}

func lineCountArgs(name string, args []string) (int, []string, error) {
	count := 10
	if len(args) >= 2 && args[0] == "-n" {
		value, err := strconv.Atoi(args[1])
		if err != nil || value < 0 {
			return 0, nil, fmt.Errorf("%s: invalid line count %q", name, args[1])
		}
		count, args = value, args[2:]
	}
	return count, args, nil
}

func runHead(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	count, files, err := lineCountArgs("head", args)
	if err != nil {
		return err
	}
	return eachInput(files, stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(input)
		buffer := make([]byte, 64*1024)
		scanner.Buffer(buffer, 1024*1024)
		for i := 0; i < count && scanner.Scan(); i++ {
			if _, err := fmt.Fprintln(out, scanner.Text()); err != nil {
				return err
			}
		}
		return scanner.Err()
	})
}

func runTail(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	count, files, err := lineCountArgs("tail", args)
	if err != nil {
		return err
	}
	return eachInput(files, stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		lines := make([]string, 0, count)
		for scanner.Scan() {
			if count == 0 {
				continue
			}
			if len(lines) == count {
				copy(lines, lines[1:])
				lines[count-1] = scanner.Text()
			} else {
				lines = append(lines, scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		for _, line := range lines {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
		return nil
	})
}

func runTee(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	appendMode := false
	if len(args) > 0 && args[0] == "-a" {
		appendMode, args = true, args[1:]
	}
	writers := []io.Writer{out}
	files := make([]*os.File, 0, len(args))
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	for _, name := range args {
		file, err := os.OpenFile(name, flags, 0o666)
		if err != nil {
			return err
		}
		files, writers = append(files, file), append(writers, file)
	}
	_, copyErr := io.Copy(io.MultiWriter(writers...), stdin)
	var closeErr error
	for _, file := range files {
		if err := file.Close(); closeErr == nil {
			closeErr = err
		}
	}
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func runTouch(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("touch: missing file operand")
	}
	for _, name := range args {
		file, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Chtimes(name, timeNow(), timeNow()); err != nil {
			return err
		}
	}
	return nil
}

var timeNow = func() (nowTime time.Time) { return time.Now() }

func runMkdir(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	parents := false
	if len(args) > 0 && args[0] == "-p" {
		parents, args = true, args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("mkdir: missing directory operand")
	}
	for _, name := range args {
		var err error
		if parents {
			err = os.MkdirAll(name, 0o777)
		} else {
			err = os.Mkdir(name, 0o777)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func runRmdir(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("rmdir: missing directory operand")
	}
	for _, name := range args {
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func runUnlink(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("unlink: expected FILE")
	}
	return os.Remove(args[0])
}

func runLink(_ context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("link: expected SOURCE DESTINATION")
	}
	return os.Link(args[0], args[1])
}

func readAllLimited(reader io.Reader) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := io.Copy(&buffer, io.LimitReader(reader, 16<<20))
	return buffer.Bytes(), err
}
