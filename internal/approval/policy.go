package approval

import "fmt"

type Policy struct {
	PlanMode    bool
	Yolo        bool
	Interactive bool
}

type Decision struct {
	Allowed        bool
	NeedsApproval  bool
	DeniedReason   string
	StructuredCode string
}

func (policy Policy) EvaluateMutation(toolName string) Decision {
	if policy.PlanMode {
		return Decision{
			Allowed:        false,
			DeniedReason:   fmt.Sprintf("plan mode is enabled; mutation tool %q was not executed", toolName),
			StructuredCode: "plan_mode_denied",
		}
	}
	if policy.Yolo {
		return Decision{Allowed: true}
	}
	if !policy.Interactive {
		return Decision{
			Allowed:        false,
			DeniedReason:   fmt.Sprintf("mutation tool %q requires approval; non-interactive mode cannot prompt. rerun with --yolo or --plan", toolName),
			StructuredCode: "approval_required_non_interactive",
		}
	}
	return Decision{Allowed: false, NeedsApproval: true}
}
