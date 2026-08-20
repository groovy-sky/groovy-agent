package main

import (
	"context"
	"testing"
)

func TestIsPolicyDenied(t *testing.T) {
	if !isPolicyDenied(assertErr("approval_required_non_interactive")) {
		t.Fatal("expected policy denied")
	}
	if !isPolicyDenied(assertErr("plan_mode_denied")) {
		t.Fatal("expected plan mode denied")
	}
	if isPolicyDenied(assertErr("other")) {
		t.Fatal("did not expect denied")
	}
}

type textErr string

func (errorText textErr) Error() string { return string(errorText) }

func assertErr(text string) error { return textErr(text) }

func TestRunHeadlessInvalidOutputFlag(t *testing.T) {
	status := runHeadless(context.Background(), []string{"-p", "hello", "--output", "xml"})
	if status != exitInvalidConfig {
		t.Fatalf("status = %d", status)
	}
}

func TestOutputDirDefaultUsesEnvVar(t *testing.T) {
	t.Setenv("AGENT_OUTPUT_DIR", "/custom/output")
	if got := outputDirDefault(); got != "/custom/output" {
		t.Fatalf("outputDirDefault = %q, want /custom/output", got)
	}
}

func TestOutputDirDefaultFallsBackToPackageDefault(t *testing.T) {
	t.Setenv("AGENT_OUTPUT_DIR", "")
	if got := outputDirDefault(); got != "output" {
		t.Fatalf("outputDirDefault = %q, want output", got)
	}
}
