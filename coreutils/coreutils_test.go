package coreutils

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execute(t *testing.T, name string, args []string, input string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), name, args, strings.NewReader(input), &output, &bytes.Buffer{})
	return output.String(), err
}

func TestCatAndHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := execute(t, "head", []string{"-n", "2", path}, "")
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestBase64RoundTrip(t *testing.T) {
	encoded, err := execute(t, "base64", nil, "coreutils")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := execute(t, "base64", []string{"-d"}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != "coreutils" {
		t.Fatalf("decoded = %q", decoded)
	}
}

func TestUnknownUtility(t *testing.T) {
	if _, err := execute(t, "missing", nil, ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCutAndSort(t *testing.T) {
	output, err := execute(t, "cut", []string{"-d", ":", "-f", "1,3"}, "b:two:2\na:one:1\n")
	if err != nil {
		t.Fatal(err)
	}
	output, err = execute(t, "sort", nil, output)
	if err != nil {
		t.Fatal(err)
	}
	if output != "a:1\nb:2\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestTr(t *testing.T) {
	output, err := execute(t, "tr", []string{"abc", "ABC"}, "cab\n")
	if err != nil {
		t.Fatal(err)
	}
	if output != "CAB\n" {
		t.Fatalf("output = %q", output)
	}
}
