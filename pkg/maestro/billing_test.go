// Maestro
// License: MIT

package maestro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/maestro/config"
	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/llm"
	"go.bison9.com/maestro/logging"
	"go.bison9.com/maestro/reference"
)

// billingRecorder is a host dispatcher that records the billing code of every
// call.
type billingRecorder struct {
	mu    sync.Mutex
	codes []string
}

func (d *billingRecorder) Dispatch(_ context.Context, req *llm.DispatchRequest) (*llm.DispatchResult, error) {
	d.mu.Lock()
	d.codes = append(d.codes, req.Billing)
	d.mu.Unlock()
	return &llm.DispatchResult{ExitCode: 0, Stdout: "ok", Text: "ok", ResponseParsed: true, NormalTermination: true, Success: true}, nil
}
func (d *billingRecorder) GetLLM(id string) *config.LLM        { return &config.LLM{ID: id} }
func (d *billingRecorder) GetExecInfo(string) *llm.LLMExecInfo { return &llm.LLMExecInfo{} }
func (d *billingRecorder) TestLLM(string) (bool, error)        { return true, nil }

func (d *billingRecorder) sent() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.codes...)
}

// newBillingProvider registers a host-dispatched provider whose host billing
// code is "host-code", and creates project "kp" with task set "main".
func newBillingProvider(t *testing.T, d *billingRecorder) *Provider {
	t.Helper()
	p := &Provider{}
	p.RegisterTools(toolspec.Deps{Cfg: newPreparedConfig(t), Host: HostDeps{
		Dispatcher: d,
		Billing:    func(context.Context) string { return "host-code" },
	}})
	if _, err := p.projects.Create("kp", "KP", "", "", "", "none"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := p.tasks.CreateTaskSet("kp", "main", "Main", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("create task set: %v", err)
	}
	return p
}

// TestBillingParam_OnlyUnderHostDispatch: the parameter is offered on exactly
// task_run and task_dispatch under host dispatch, and nowhere standalone,
// where no definition mentions it.
func TestBillingParam_OnlyUnderHostDispatch(t *testing.T) {
	want := []string{global.ToolTaskDispatch, global.ToolTaskRun}
	for name, host := range map[string]any{
		"host":              HostDeps{Dispatcher: stubDispatcher{}},
		"host with billing": HostDeps{Dispatcher: stubDispatcher{}, Billing: func(context.Context) string { return "x" }},
	} {
		if got := paramNames(registerProvider(t, host), "billing"); !reflect.DeepEqual(sortedCopy(got), sortedCopy(want)) {
			t.Errorf("%s: billing offered on %v, want %v", name, got, want)
		}
	}

	for name, host := range map[string]any{
		"standalone":         nil,
		"host, own dispatch": HostDeps{Billing: func(context.Context) string { return "x" }},
	} {
		defs := registerProvider(t, host)
		if got := paramNames(defs, "billing"); len(got) != 0 {
			t.Errorf("%s: billing offered on %v, want none", name, got)
		}
		if s := defsJSON(t, defs); strings.Contains(strings.ToLower(s), "billing") {
			t.Errorf("%s: tool definitions mention billing", name)
		}
	}
}

// TestBillingParam_RunKeepsStoredCode: an explicit code reaches the host and
// is stored; a second task_run without the argument keeps it rather than
// taking the host's current code.
func TestBillingParam_RunKeepsStoredCode(t *testing.T) {
	d := &billingRecorder{}
	p := newBillingProvider(t, d)
	task, err := p.tasks.CreateTask("kp", "main", "Task", "", &global.WorkExecution{Prompt: "Do it."}, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run := func(args map[string]any) {
		t.Helper()
		res, err := p.handleTaskRun(call(args))
		if err != nil || res.IsError {
			t.Fatalf("task_run: %v %+v", err, res)
		}
		p.runner.Wait()
	}
	run(map[string]any{"project": "kp", "billing": "alice-audit"})

	// The task runs again, as after a reset or an interrupted run.
	if _, err := p.tasks.UpdateTask("kp", task.UUID, map[string]any{"work": map[string]any{"status": global.ExecutionStatusWaiting}}); err != nil {
		t.Fatalf("reset task: %v", err)
	}
	if err := os.Remove(filepath.Join(p.tasks.GetResultsDir("kp"), task.UUID+".json")); err != nil {
		t.Fatalf("delete result: %v", err)
	}
	run(map[string]any{"project": "kp"})

	if got, want := d.sent(), []string{"alice-audit", "alice-audit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("billing sent = %v, want %v", got, want)
	}
}

// TestBillingParam_HostDefault: without the argument a new task takes the
// host's code, for task_run and task_dispatch.
func TestBillingParam_HostDefault(t *testing.T) {
	d := &billingRecorder{}
	p := newBillingProvider(t, d)
	if _, err := p.tasks.CreateTask("kp", "main", "Task", "", &global.WorkExecution{Prompt: "Do it."}, nil); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if res, err := p.handleTaskRun(call(map[string]any{"project": "kp", "billing": ""})); err != nil || res.IsError {
		t.Fatalf("task_run: %v %+v", err, res)
	}
	p.runner.Wait()
	if res, err := p.handleTaskDispatch(call(map[string]any{"project": "kp", "prompt": "Do it."})); err != nil || res.IsError {
		t.Fatalf("task_dispatch: %v %+v", err, res)
	}
	p.runner.Wait()

	if got, want := d.sent(), []string{"host-code", "host-code"}; !reflect.DeepEqual(got, want) {
		t.Errorf("billing sent = %v, want %v", got, want)
	}
}

// TestBillingParam_Invalid: an invalid code is refused with one sentence, and
// nothing is run, created or stored.
func TestBillingParam_Invalid(t *testing.T) {
	for _, code := range []any{"has space", "bad/code", "é", strings.Repeat("a", 65), 7.0} {
		d := &billingRecorder{}
		p := newBillingProvider(t, d)
		task, err := p.tasks.CreateTask("kp", "main", "Task", "", &global.WorkExecution{Prompt: "Do it."}, nil)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}

		// task_run would run the waiting task; task_dispatch would create task set "other".
		for tool, c := range map[string]struct {
			handler func(*toolspec.ToolCall) (*toolspec.Result, error)
			args    map[string]any
		}{
			global.ToolTaskRun:      {p.handleTaskRun, map[string]any{"project": "kp", "billing": code}},
			global.ToolTaskDispatch: {p.handleTaskDispatch, map[string]any{"project": "kp", "prompt": "Do it.", "path": "other", "billing": code}},
		} {
			res, err := c.handler(call(c.args))
			if err != nil {
				t.Fatalf("%s %v: %v", tool, code, err)
			}
			if !res.IsError || res.ForLLM != errInvalidBilling {
				t.Errorf("%s %v: result %+v, want %q", tool, code, res, errInvalidBilling)
			}
		}
		p.runner.Wait()

		if n := len(d.sent()); n != 0 {
			t.Errorf("%v: %d model calls, want 0", code, n)
		}
		got, _, err := p.tasks.GetTask("kp", task.UUID)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if got.Work.Status != global.ExecutionStatusWaiting || got.Work.Billing != "" {
			t.Errorf("%v: task status=%q billing=%q, want waiting and none", code, got.Work.Status, got.Work.Billing)
		}
		if _, err := p.tasks.GetTaskSet("kp", "other"); err == nil {
			t.Errorf("%v: task_dispatch created a task set", code)
		}
	}
}

// TestBillingParam_ValidCodes: the edges of the character set are accepted.
func TestBillingParam_ValidCodes(t *testing.T) {
	p := &Provider{hostDispatched: true}
	for _, code := range []string{"a", "default", "Alice.audit_2026-Q4", strings.Repeat("Z", 64)} {
		got, msg := p.billingArg(map[string]any{"billing": code})
		if msg != "" || got != code {
			t.Errorf("billingArg(%q) = %q, %q; want the code", code, got, msg)
		}
	}
}

// TestHandleStartHere_Billing: the host start guide explains billing; the
// standalone one does not mention it.
func TestHandleStartHere_Billing(t *testing.T) {
	lg, err := logging.New(filepath.Join(t.TempDir(), "maestro.log"), global.DefaultDirMode, global.DefaultFileMode)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	defer func() { _ = lg.Close() }()
	ref := reference.NewService(reference.WithEmbeddedFS(EmbeddedReference), reference.WithLogger(lg))

	content := func(hostDispatched bool) string {
		t.Helper()
		p := &Provider{reference: ref, hostDispatched: hostDispatched}
		res, err := p.handleStartHere(&toolspec.ToolCall{})
		if err != nil {
			t.Fatalf("handleStartHere: %v", err)
		}
		var item struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(res.ForLLM), &item); err != nil {
			t.Fatalf("unmarshal start_here result: %v", err)
		}
		return item.Content
	}
	if !strings.Contains(content(true), "**Billing**: `task_run` and `task_dispatch` take an optional `billing` code") {
		t.Error("host start guide lacks the billing paragraph")
	}
	if strings.Contains(strings.ToLower(content(false)), "billing") {
		t.Error("standalone start guide mentions billing")
	}
}
