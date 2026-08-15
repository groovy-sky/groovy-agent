package coreutils

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
)

const maxGrepInputSize = 16 << 20

func init() {
	register(Command{"grep", "Search input lines using a regular expression or literal string", runGrep})
}

func runGrep(ctx context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	lineNumbers, invert, literal := false, false, false
	for len(args) > 0 {
		switch args[0] {
		case "-n":
			lineNumbers, args = true, args[1:]
		case "-v":
			invert, args = true, args[1:]
		case "-F":
			literal, args = true, args[1:]
		case "-E":
			args = args[1:]
		case "--":
			args = args[1:]
			goto parsed
		default:
			goto parsed
		}
	}
parsed:
	if len(args) == 0 {
		return fmt.Errorf("grep: missing pattern")
	}
	pattern := args[0]
	if literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("grep: invalid pattern: %w", err)
	}
	return eachInput(args[1:], stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(&grepLimitReader{reader: input, remaining: maxGrepInputSize})
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for line := 1; scanner.Scan(); line++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			matched := matcher.MatchString(scanner.Text())
			if matched == invert {
				continue
			}
			if lineNumbers {
				if _, err := fmt.Fprintf(out, "%d:", line); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(out, scanner.Text()); err != nil {
				return err
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return nil
	})
}

type grepLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (reader *grepLimitReader) Read(data []byte) (int, error) {
	if reader.remaining == 0 {
		var extra [1]byte
		count, err := reader.reader.Read(extra[:])
		if count > 0 {
			return 0, fmt.Errorf("grep: input exceeds %d MiB limit", maxGrepInputSize>>20)
		}
		return 0, err
	}
	if int64(len(data)) > reader.remaining {
		data = data[:reader.remaining]
	}
	count, err := reader.reader.Read(data)
	reader.remaining -= int64(count)
	return count, err
}
