package coreutils

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

func init() {
	register(Command{"cut", "Remove selected fields from each line", runCut})
	register(Command{"paste", "Merge corresponding lines of files", runPaste})
	register(Command{"sort", "Sort lines of text", runSort})
	register(Command{"tr", "Translate or delete characters", runTr})
}

func runCut(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	delimiter := "\t"
	fieldSpec := ""
	for len(args) > 0 {
		switch args[0] {
		case "-d":
			if len(args) < 2 || len([]rune(args[1])) != 1 {
				return fmt.Errorf("cut: delimiter must be one character")
			}
			delimiter, args = args[1], args[2:]
		case "-f":
			if len(args) < 2 {
				return fmt.Errorf("cut: option -f requires a field list")
			}
			fieldSpec, args = args[1], args[2:]
		default:
			goto parsed
		}
	}
parsed:
	fields, err := parseFieldList(fieldSpec)
	if err != nil {
		return err
	}
	return eachInput(args, stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			parts := strings.Split(scanner.Text(), delimiter)
			selected := make([]string, 0, len(fields))
			for _, field := range fields {
				if field <= len(parts) {
					selected = append(selected, parts[field-1])
				}
			}
			if _, err := fmt.Fprintln(out, strings.Join(selected, delimiter)); err != nil {
				return err
			}
		}
		return scanner.Err()
	})
}

func parseFieldList(spec string) ([]int, error) {
	if spec == "" {
		return nil, fmt.Errorf("cut: a field list is required")
	}
	seen := map[int]bool{}
	var fields []int
	for _, item := range strings.Split(spec, ",") {
		bounds := strings.SplitN(item, "-", 2)
		first, err := strconv.Atoi(bounds[0])
		if err != nil || first < 1 {
			return nil, fmt.Errorf("cut: invalid field list %q", spec)
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil || last < first || last-first > 10_000 {
				return nil, fmt.Errorf("cut: invalid field list %q", spec)
			}
		}
		for field := first; field <= last; field++ {
			if !seen[field] {
				fields, seen[field] = append(fields, field), true
			}
		}
	}
	sort.Ints(fields)
	return fields, nil
}

func runPaste(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	delimiter := "\t"
	if len(args) >= 2 && args[0] == "-d" {
		delimiter, args = args[1], args[2:]
	}
	if len(args) == 0 {
		args = []string{"-"}
	}
	scanners := make([]*bufio.Scanner, 0, len(args))
	files := make([]*os.File, 0, len(args))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, name := range args {
		var reader io.Reader = stdin
		if name != "-" {
			file, err := os.Open(name)
			if err != nil {
				return err
			}
			files, reader = append(files, file), file
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		scanners = append(scanners, scanner)
	}
	for {
		values := make([]string, len(scanners))
		hasValue := false
		for i, scanner := range scanners {
			if scanner.Scan() {
				values[i], hasValue = scanner.Text(), true
			} else if err := scanner.Err(); err != nil {
				return err
			}
		}
		if !hasValue {
			return nil
		}
		if _, err := fmt.Fprintln(out, strings.Join(values, delimiter)); err != nil {
			return err
		}
	}
}

func runSort(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	reverse := false
	if len(args) > 0 && args[0] == "-r" {
		reverse, args = true, args[1:]
	}
	var lines []string
	err := eachInput(args, stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			if len(lines) >= 1_000_000 {
				return fmt.Errorf("sort: line limit exceeded")
			}
			lines = append(lines, scanner.Text())
		}
		return scanner.Err()
	})
	if err != nil {
		return err
	}
	sort.Strings(lines)
	if reverse {
		for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
			lines[left], lines[right] = lines[right], lines[left]
		}
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func runTr(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	deleteMode := false
	if len(args) > 0 && args[0] == "-d" {
		deleteMode, args = true, args[1:]
	}
	expected := 2
	if deleteMode {
		expected = 1
	}
	if len(args) != expected {
		return fmt.Errorf("tr: expected %d character set operand(s)", expected)
	}
	set1 := []rune(args[0])
	if len(set1) == 0 {
		return fmt.Errorf("tr: empty character set")
	}
	replacements := map[rune]rune{}
	deletions := map[rune]bool{}
	if deleteMode {
		for _, character := range set1 {
			deletions[character] = true
		}
	} else {
		set2 := []rune(args[1])
		if len(set2) == 0 {
			return fmt.Errorf("tr: empty replacement set")
		}
		for i, character := range set1 {
			replacement := set2[len(set2)-1]
			if i < len(set2) {
				replacement = set2[i]
			}
			replacements[character] = replacement
		}
	}
	reader := bufio.NewReader(stdin)
	for {
		character, _, err := reader.ReadRune()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if deletions[character] {
			continue
		}
		if replacement, ok := replacements[character]; ok {
			character = replacement
		}
		if _, err := fmt.Fprint(out, string(character)); err != nil {
			return err
		}
	}
}
