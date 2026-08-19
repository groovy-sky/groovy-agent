package approval

import "testing"

func TestEvaluateMutation(t *testing.T) {
	cases := []struct {
		name     string
		policy   Policy
		allowed  bool
		needsAsk bool
		code     string
	}{
		{name: "plan mode", policy: Policy{PlanMode: true, Interactive: true}, code: "plan_mode_denied"},
		{name: "yolo", policy: Policy{Yolo: true}, allowed: true},
		{name: "non-interactive", policy: Policy{}, code: "approval_required_non_interactive"},
		{name: "interactive prompt", policy: Policy{Interactive: true}, needsAsk: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decision := testCase.policy.EvaluateMutation("write_file")
			if decision.Allowed != testCase.allowed {
				t.Fatalf("allowed = %v", decision.Allowed)
			}
			if decision.NeedsApproval != testCase.needsAsk {
				t.Fatalf("needs approval = %v", decision.NeedsApproval)
			}
			if testCase.code != "" && decision.StructuredCode != testCase.code {
				t.Fatalf("code = %q", decision.StructuredCode)
			}
		})
	}
}
