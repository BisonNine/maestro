/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"

	"go.bison9.com/maestro/global"
)

// billingHolder boxes the host's billing function so it can be swapped
// atomically.
type billingHolder struct{ fn func(context.Context) string }

// SetHostBilling sets the function that returns the host's current billing
// code for the context of a request (nil: none). An embedding host calls it on
// every registration, possibly while a run is in progress, hence the atomic
// store.
func (r *Runner) SetHostBilling(fn func(context.Context) string) {
	if fn == nil {
		r.hostBilling.Store(nil)
		return
	}
	r.hostBilling.Store(&billingHolder{fn: fn})
}

// hostBillingFor returns the host's billing code for ctx, or "" without a host
// function. It must be called on the request's own context, while the request
// is being made: execution later continues on a detached context, by when the
// host's answer may have changed.
func (r *Runner) hostBillingFor(ctx context.Context) string {
	if h := r.hostBilling.Load(); h != nil && ctx != nil {
		return h.fn(ctx)
	}
	return ""
}

// runBilling is the billing a run was requested with: the code given
// explicitly, and the host's code captured when the run was requested.
type runBilling struct {
	explicit string
	host     string
}

type runBillingKey struct{}

// withRunBilling returns ctx carrying the billing of the run being started.
func withRunBilling(ctx context.Context, b runBilling) context.Context {
	return context.WithValue(ctx, runBillingKey{}, b)
}

// resolveBilling returns the billing code for task: the code the run was
// requested with explicitly, else the code stored on the task, else the host's
// code captured when the run was requested. A code that differs from the
// stored one is stamped on the task, so a later run, retry, QA or revision
// keeps it.
func (r *Runner) resolveBilling(ctx context.Context, project string, task *global.Task) string {
	b, _ := ctx.Value(runBillingKey{}).(runBilling)
	code := task.Work.Billing
	switch {
	case b.explicit != "":
		code = b.explicit
	case code == "":
		code = b.host
	}
	if code == task.Work.Billing {
		return code
	}
	task.Work.Billing = code
	updates := map[string]interface{}{"work": map[string]interface{}{"billing": code}}
	if _, err := r.tasks.UpdateTask(project, task.UUID, updates); err != nil {
		r.logger.Warnf("Task %d: Failed to save billing code: %v", task.ID, err)
	}
	return code
}
