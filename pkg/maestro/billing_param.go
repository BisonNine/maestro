/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package maestro

import (
	"go.bison9.com/agent/accounting"
)

// Descriptions of the billing parameter. It is offered only under host
// dispatch: standalone Maestro records no accounting.
const (
	billingRunDescription      = "Optional billing code the host records these tasks' model calls under: 1 to 64 letters, digits, '-', '_' or '.'. Without it, a task keeps the code it already has, else takes the host's current code."
	billingDispatchDescription = "Optional billing code the host records this task's model calls under: 1 to 64 letters, digits, '-', '_' or '.'. Without it, the host's current code is used."
)

// errInvalidBilling is returned for a billing code accounting.CheckBillingCode
// refuses.
const errInvalidBilling = "Invalid billing code: use 1 to 64 letters, digits, '-', '_' or '.'."

// billingArg reads the billing argument of task_run and task_dispatch. It is
// read only under host dispatch; standalone it is ignored, as any argument the
// tool does not offer. An empty string counts as not given. A code that is
// not valid is returned as a message for the model.
func (p *Provider) billingArg(args map[string]any) (code, errMsg string) {
	if !p.hostDispatched {
		return "", ""
	}
	raw, ok := args["billing"]
	if !ok || raw == nil {
		return "", ""
	}
	code, ok = raw.(string)
	if !ok {
		return "", errInvalidBilling
	}
	if code == "" {
		return "", ""
	}
	if err := accounting.CheckBillingCode(code); err != nil {
		return "", errInvalidBilling
	}
	return code, ""
}
