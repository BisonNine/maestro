/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/maestro/config"
	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/llm"
)

// billingCall is one model call as a host dispatcher saw it.
type billingCall struct {
	kind    string // worker, qa, revise or probe
	billing string
}

// billingDispatcher records the kind and billing code of every call. QA calls
// are answered with the next verdict from qaVerdicts ("pass" once they run
// out). With failFirstWorker the first worker call fails, and with recovery
// the model has a recovery configuration that probes at once.
type billingDispatcher struct {
	mu              sync.Mutex
	calls           []billingCall
	qaVerdicts      []string
	failFirstWorker bool
	recovery        bool
}

func (d *billingDispatcher) Dispatch(_ context.Context, req *llm.DispatchRequest) (*llm.DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kind := "worker"
	switch {
	case req.Prompt == "probe":
		kind = "probe"
	case strings.Contains(req.Prompt, "=== WORK RESULT TO REVIEW ==="):
		kind = "qa"
	case strings.Contains(req.Prompt, "=== QA FEEDBACK ==="):
		kind = "revise"
	}
	d.calls = append(d.calls, billingCall{kind: kind, billing: req.Billing})

	text := "ok"
	if kind == "qa" {
		verdict := "pass"
		if len(d.qaVerdicts) > 0 {
			verdict, d.qaVerdicts = d.qaVerdicts[0], d.qaVerdicts[1:]
		}
		text = `{"verdict":"` + verdict + `"}`
	}
	if kind == "worker" && d.failFirstWorker {
		d.failFirstWorker = false
		return &llm.DispatchResult{ExitCode: 1, Stderr: "unavailable"}, nil
	}
	return &llm.DispatchResult{
		ExitCode: 0, Stdout: text, Text: text,
		ResponseParsed: true, NormalTermination: true, Success: true,
	}, nil
}

func (d *billingDispatcher) GetLLM(id string) *config.LLM {
	c := &config.LLM{ID: id}
	if d.recovery {
		c.RecoveryConfig = &config.LLMRecoveryConfig{TestPrompt: "probe", TestScheduleSeconds: []int{0}}
	}
	return c
}
func (d *billingDispatcher) GetExecInfo(string) *llm.LLMExecInfo { return &llm.LLMExecInfo{} }
func (d *billingDispatcher) TestLLM(string) (bool, error)        { return true, nil }

func (d *billingDispatcher) sent() []billingCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]billingCall(nil), d.calls...)
}

// changingHost is a host billing function whose answer changes after its
// first call, so a code read late is told apart from one captured on request.
type changingHost struct{ n atomic.Int32 }

func (h *changingHost) billing(context.Context) string {
	if h.n.Add(1) == 1 {
		return "code-1"
	}
	return "code-2"
}

// newBillingRunner builds a host-dispatched runner with project "bp" and an
// unvalidated task set "main".
func newBillingRunner(t *testing.T, d *billingDispatcher, host func(context.Context) string) *testRunner {
	t.Helper()
	tr, tmpDir := setupTestRunner(t)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	tr.llm = d
	tr.SetHostDispatched(true)
	tr.SetHostBilling(host)
	if _, err := tr.projects.Create("bp", "Billing Project", "billing tests", "", "", "none"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := tr.tasks.CreateTaskSet("bp", "main", "Main", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("create task set: %v", err)
	}
	return tr
}

// runBilled runs every eligible task in "bp" with the given explicit code and
// waits for the run to finish.
func runBilled(t *testing.T, tr *testRunner, billing string) {
	t.Helper()
	if _, err := tr.Run(context.Background(), &global.RunRequest{Project: "bp", Billing: billing}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr.Wait()
}

func getTask(t *testing.T, tr *testRunner, uuid string) *global.Task {
	t.Helper()
	task, _, err := tr.tasks.GetTask("bp", uuid)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	return task
}

func wantCalls(t *testing.T, got []billingCall, want ...billingCall) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// TestBilling_ExplicitReachesWorkerQARevise: a code given to the run is sent
// on the worker, QA and revision calls and stored on the task; the host is
// not asked.
func TestBilling_ExplicitReachesWorkerQARevise(t *testing.T) {
	d := &billingDispatcher{qaVerdicts: []string{"fail", "pass"}}
	host := &changingHost{}
	tr := newBillingRunner(t, d, host.billing)
	task, err := tr.tasks.CreateTask("bp", "main", "Task", "", &global.WorkExecution{Prompt: "Do the work."},
		&global.QAExecution{Enabled: true, Prompt: "Review the work."})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	runBilled(t, tr, "alice-audit")

	wantCalls(t, d.sent(),
		billingCall{"worker", "alice-audit"}, billingCall{"qa", "alice-audit"},
		billingCall{"revise", "alice-audit"}, billingCall{"qa", "alice-audit"})
	if got := getTask(t, tr, task.UUID).Work.Billing; got != "alice-audit" {
		t.Errorf("stored billing = %q, want alice-audit", got)
	}
	if n := host.n.Load(); n != 0 {
		t.Errorf("host billing called %d times, want 0", n)
	}
}

// TestBilling_HostCodeCapturedOnRequest: without an explicit or stored code,
// the host's code is the one in force when the run was requested, even
// though the run executes later on a detached context.
func TestBilling_HostCodeCapturedOnRequest(t *testing.T) {
	d := &billingDispatcher{qaVerdicts: []string{"fail", "pass"}}
	host := &changingHost{}
	tr := newBillingRunner(t, d, host.billing)
	task, err := tr.tasks.CreateTask("bp", "main", "Task", "", &global.WorkExecution{Prompt: "Do the work."},
		&global.QAExecution{Enabled: true, Prompt: "Review the work."})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	runBilled(t, tr, "")

	wantCalls(t, d.sent(),
		billingCall{"worker", "code-1"}, billingCall{"qa", "code-1"},
		billingCall{"revise", "code-1"}, billingCall{"qa", "code-1"})
	if got := getTask(t, tr, task.UUID).Work.Billing; got != "code-1" {
		t.Errorf("stored billing = %q, want code-1", got)
	}
	if n := host.n.Load(); n != 1 {
		t.Errorf("host billing called %d times, want 1 (on request only)", n)
	}
}

// TestBilling_StoredCode: a task that already ran under a code (and was then
// interrupted, so it is waiting again) keeps it on a later run without an
// explicit code, whatever the host's current code; an explicit code replaces
// it.
func TestBilling_StoredCode(t *testing.T) {
	tests := []struct {
		name, explicit, want string
	}{
		{"kept without explicit", "", "alice-audit"},
		{"replaced by explicit", "bob-review", "bob-review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &billingDispatcher{}
			tr := newBillingRunner(t, d, func(context.Context) string { return "host-now" })
			task, err := tr.tasks.CreateTask("bp", "main", "Task", "",
				&global.WorkExecution{Prompt: "Do the work.", Billing: "alice-audit"}, nil)
			if err != nil {
				t.Fatalf("create task: %v", err)
			}

			runBilled(t, tr, tt.explicit)

			wantCalls(t, d.sent(), billingCall{"worker", tt.want})
			if got := getTask(t, tr, task.UUID).Work.Billing; got != tt.want {
				t.Errorf("stored billing = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBilling_RetryAndProbeKeepCode: a worker call that fails is retried, and
// the recovery probe made in between is sent, under the code captured on
// request, although the host's answer has since changed.
func TestBilling_RetryAndProbeKeepCode(t *testing.T) {
	d := &billingDispatcher{failFirstWorker: true, recovery: true}
	host := &changingHost{}
	tr := newBillingRunner(t, d, host.billing)
	if _, err := tr.tasks.CreateTask("bp", "main", "Task", "", &global.WorkExecution{Prompt: "Do the work."}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runBilled(t, tr, "")

	wantCalls(t, d.sent(),
		billingCall{"worker", "code-1"}, billingCall{"probe", "code-1"}, billingCall{"worker", "code-1"})
}

// TestBilling_RunDispatch: a dispatched task takes the explicit code, else the
// host's code when dispatched, and stores it.
func TestBilling_RunDispatch(t *testing.T) {
	tests := []struct {
		name, explicit, want string
		hostCalls            int32
	}{
		{"explicit", "alice-audit", "alice-audit", 0},
		{"host", "", "code-1", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &billingDispatcher{}
			host := &changingHost{}
			tr := newBillingRunner(t, d, host.billing)

			res, err := tr.RunDispatch(context.Background(), &DispatchRequest{Project: "bp", Prompt: "Do it.", Billing: tt.explicit}, nil)
			if err != nil {
				t.Fatalf("RunDispatch: %v", err)
			}
			tr.Wait()

			wantCalls(t, d.sent(), billingCall{"worker", tt.want})
			if got := getTask(t, tr, res.UUID).Work.Billing; got != tt.want {
				t.Errorf("stored billing = %q, want %q", got, tt.want)
			}
			if n := host.n.Load(); n != tt.hostCalls {
				t.Errorf("host billing called %d times, want %d", n, tt.hostCalls)
			}
		})
	}
}

// TestBilling_None: with no host function and no explicit code the calls carry
// no code and the task set file gains no billing field.
func TestBilling_None(t *testing.T) {
	d := &billingDispatcher{qaVerdicts: []string{"fail", "pass"}}
	tr := newBillingRunner(t, d, nil)
	if _, err := tr.tasks.CreateTask("bp", "main", "Task", "", &global.WorkExecution{Prompt: "Do the work."},
		&global.QAExecution{Enabled: true, Prompt: "Review the work."}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	runBilled(t, tr, "")

	wantCalls(t, d.sent(),
		billingCall{"worker", ""}, billingCall{"qa", ""}, billingCall{"revise", ""}, billingCall{"qa", ""})
	files := 0
	err := filepath.WalkDir(filepath.Join(tr.config.ProjectsDir(), "bp"), func(path string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Ext(path) != ".json" {
			return err
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), `"billing"`) {
			t.Errorf("%s contains a billing field", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no project files found")
	}
}

// TestSetHostBilling_Replace: a later registration replaces the function, and
// nil removes it.
func TestSetHostBilling_Replace(t *testing.T) {
	d := &billingDispatcher{}
	tr := newBillingRunner(t, d, func(context.Context) string { return "first" })
	tr.SetHostBilling(func(context.Context) string { return "second" })
	if got := tr.hostBillingFor(context.Background()); got != "second" {
		t.Errorf("host billing = %q, want second", got)
	}
	tr.SetHostBilling(nil)
	if got := tr.hostBillingFor(context.Background()); got != "" {
		t.Errorf("host billing = %q, want empty", got)
	}
}
