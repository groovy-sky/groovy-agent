package coreutils

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"strings"
)

func init() {
	register(Command{"base32", "Encode or decode base32 data", runBase32})
	register(Command{"base64", "Encode or decode base64 data", runBase64})
	register(Command{"md5sum", "Compute MD5 checksums", checksum(md5.New)})
	register(Command{"sha1sum", "Compute SHA-1 checksums", checksum(sha1.New)})
	register(Command{"sha224sum", "Compute SHA-224 checksums", checksum(sha256.New224)})
	register(Command{"sha256sum", "Compute SHA-256 checksums", checksum(sha256.New)})
	register(Command{"sha384sum", "Compute SHA-384 checksums", checksum(sha512.New384)})
	register(Command{"sha512sum", "Compute SHA-512 checksums", checksum(sha512.New)})
	register(Command{"uniq", "Report or omit adjacent repeated lines", runUniq})
	register(Command{"wc", "Print newline, word, and byte counts", runWC})
	register(Command{"tac", "Concatenate and print lines in reverse", runTac})
}

func baseArgs(name string, args []string) (decode bool, files []string, err error) {
	if len(args) > 0 && (args[0] == "-d" || args[0] == "--decode") {
		decode, args = true, args[1:]
	}
	if len(args) > 1 {
		return false, nil, fmt.Errorf("%s: expected at most one file", name)
	}
	return decode, args, nil
}

func runBase32(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	decode, files, err := baseArgs("base32", args)
	if err != nil {
		return err
	}
	return eachInput(files, stdin, func(_ string, input io.Reader) error {
		if decode {
			_, err := io.Copy(out, base32.NewDecoder(base32.StdEncoding, input))
			return err
		}
		encoder := base32.NewEncoder(base32.StdEncoding, out)
		if _, err := io.Copy(encoder, input); err != nil {
			return err
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out)
		return err
	})
}

func runBase64(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	decode, files, err := baseArgs("base64", args)
	if err != nil {
		return err
	}
	return eachInput(files, stdin, func(_ string, input io.Reader) error {
		if decode {
			_, err := io.Copy(out, base64.NewDecoder(base64.StdEncoding, input))
			return err
		}
		encoder := base64.NewEncoder(base64.StdEncoding, out)
		if _, err := io.Copy(encoder, input); err != nil {
			return err
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out)
		return err
	})
}

func checksum(newHash func() hash.Hash) func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
	return func(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
		return eachInput(args, stdin, func(name string, input io.Reader) error {
			digest := newHash()
			if _, err := io.Copy(digest, input); err != nil {
				return err
			}
			_, err := fmt.Fprintf(out, "%x  %s\n", digest.Sum(nil), name)
			return err
		})
	}
}

func runUniq(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	count := false
	if len(args) > 0 && args[0] == "-c" {
		count, args = true, args[1:]
	}
	if len(args) > 1 {
		return fmt.Errorf("uniq: expected at most one file")
	}
	return eachInput(args, stdin, func(_ string, input io.Reader) error {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var previous string
		repeats := 0
		flush := func() error {
			if repeats == 0 {
				return nil
			}
			if count {
				_, err := fmt.Fprintf(out, "%7d %s\n", repeats, previous)
				return err
			}
			_, err := fmt.Fprintln(out, previous)
			return err
		}
		for scanner.Scan() {
			line := scanner.Text()
			if repeats > 0 && line != previous {
				if err := flush(); err != nil {
					return err
				}
				repeats = 0
			}
			previous, repeats = line, repeats+1
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return flush()
	})
}

func runWC(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	return eachInput(args, stdin, func(name string, input io.Reader) error {
		data, err := readAllLimited(input)
		if err != nil {
			return err
		}
		lines := bytes.Count(data, []byte{'\n'})
		words := len(strings.Fields(string(data)))
		_, err = fmt.Fprintf(out, "%d %d %d %s\n", lines, words, len(data), name)
		return err
	})
}

func runTac(_ context.Context, args []string, stdin io.Reader, out, _ io.Writer) error {
	return eachInput(args, stdin, func(_ string, input io.Reader) error {
		data, err := readAllLimited(input)
		if err != nil {
			return err
		}
		trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if _, err := io.WriteString(out, lines[i]); err != nil {
				return err
			}
			if i > 0 || trailingNewline {
				if _, err := io.WriteString(out, "\n"); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
