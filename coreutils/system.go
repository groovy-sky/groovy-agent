package coreutils

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

func init() {
	register(Command{"cp", "Copy a regular file; use -f to replace an existing destination", runCP})
	register(Command{"date", "Print the current date and time", runDate})
}

func runCP(ctx context.Context, args []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
	force := false
	if len(args) > 0 && args[0] == "-f" {
		force, args = true, args[1:]
	}
	if len(args) != 2 {
		return fmt.Errorf("cp: expected SOURCE DESTINATION")
	}
	source, destination := args[0], args[1]
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cp: source is not a regular file: %s", source)
	}
	if destinationInfo, err := os.Stat(destination); err == nil {
		if os.SameFile(info, destinationInfo) {
			return fmt.Errorf("cp: source and destination are the same file")
		}
		if !force {
			return fmt.Errorf("cp: destination exists (use -f to replace it)")
		}
		if !destinationInfo.Mode().IsRegular() {
			return fmt.Errorf("cp: destination is not a regular file: %s", destination)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	output, err := os.OpenFile(destination, flags, info.Mode().Perm())
	if err != nil {
		return err
	}
	copyErr := copyWithContext(ctx, output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func runDate(_ context.Context, args []string, _ io.Reader, out, _ io.Writer) error {
	utc := false
	if len(args) > 0 && args[0] == "-u" {
		utc, args = true, args[1:]
	}
	format := "Mon Jan _2 15:04:05 MST 2006"
	if len(args) == 1 && strings.HasPrefix(args[0], "+") {
		var err error
		format, err = dateLayout(args[0][1:])
		if err != nil {
			return err
		}
	} else if len(args) != 0 {
		return fmt.Errorf("date: expected [-u] [+FORMAT]")
	}
	now := timeNow()
	if utc {
		now = now.UTC()
	}
	_, err := fmt.Fprintln(out, now.Format(format))
	return err
}

func dateLayout(format string) (string, error) {
	replacer := strings.NewReplacer(
		"%%", "%",
		"%Y", "2006",
		"%m", "01",
		"%d", "02",
		"%H", "15",
		"%M", "04",
		"%S", "05",
		"%z", "-0700",
		"%Z", "MST",
		"%F", "2006-01-02",
		"%T", "15:04:05",
	)
	for index := 0; index < len(format); index++ {
		if format[index] == '%' && (index+1 == len(format) || !strings.ContainsRune("%YmdHMSzZFT", rune(format[index+1]))) {
			return "", fmt.Errorf("date: unsupported format directive")
		}
	}
	return replacer.Replace(format), nil
}
