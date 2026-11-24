/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/maestro/config"
	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/llm"
)

// scriptedDispatcher records every prompt it is sent. A QA prompt (one that
// carries the work result to review) is answered with the next verdict from
// qaVerdicts ("pass" once they run out); any other prompt with "ok".
type scriptedDispatcher struct {
	mu         sync.Mutex
	prompts    []string
	qaVerdicts []string
}

func (d *scriptedDispatcher) Dispatch(_ context.Context, req *llm.DispatchRequest) (*llm.DispatchResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prompts = append(d.prompts, req.Prompt)
	text := "ok"
	if strings.Contains(req.Prompt, "=== WORK RESULT TO REVIEW ===") {
		verdict := "pass"
		if len(d.qaVerdicts) > 0 {
			verdict, d.qaVerdicts = d.qaVerdicts[0], d.qaVerdicts[1:]
		}
		text = `{"verdict":"` + verdict + `"}`
	}
	return &llm.DispatchResult{
		ExitCode: 0, Stdout: text, Text: text,
		ResponseParsed: true, NormalTermination: true, Success: true,
	}, nil
}

func (d *scriptedDispatcher) GetLLM(id string) *config.LLM        { return &config.LLM{ID: id} }
func (d *scriptedDispatcher) GetExecInfo(string) *llm.LLMExecInfo { return &llm.LLMExecInfo{} }
func (d *scriptedDispatcher) TestLLM(string) (bool, error)        { return true, nil }

func (d *scriptedDispatcher) sent() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.prompts...)
}

// newScriptedRunner builds a host-dispatched runner with project "kp" and an
// unvalidated task set "main".
func newScriptedRunner(t *testing.T, d *scriptedDispatcher) *testRunner {
	t.Helper()
	tr, tmpDir := setupTestRunner(t)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	tr.llm = d
	tr.SetHostDispatched(true)
	if _, err := tr.projects.Create("kp", "Knowledge Project", "prompt tests", "", "", "none"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := tr.tasks.CreateTaskSet("kp", "main", "Main", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("create task set: %v", err)
	}
	return tr
}

const projectContextBlock = "=== PROJECT CONTEXT ===\n\n" +
	"Project: kp\n" +
	"IMPORTANT: Use this project name (with source=project) for ALL file operations (file_list, file_get, file_search).\n\n"

// TestPrompts_WithoutKnowledge_Layout pins the worker, QA and revision prompts
// of a task without knowledge, so adding knowledge cannot change them.
func TestPrompts_WithoutKnowledge_Layout(t *testing.T) {
	d := &scriptedDispatcher{qaVerdicts: []string{"fail", "pass"}}
	tr := newScriptedRunner(t, d)

	work := &global.WorkExecution{InstructionsText: "Follow the rules.", Prompt: "Do the work."}
	qa := &global.QAExecution{Enabled: true, InstructionsText: "Check the rules.", Prompt: "Review the work."}
	if _, err := tr.tasks.CreateTask("kp", "main", "Task", "", work, qa); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tr.Run(context.Background(), &global.RunRequest{Project: "kp"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr.Wait()

	worker := projectContextBlock + "Follow the rules.\n\n=== TASK PROMPT ===\n\nDo the work.\n\n"
	qaPrompt := projectContextBlock + "Check the rules.\n\n=== QA TASK PROMPT ===\n\nReview the work.\n\n=== WORK RESULT TO REVIEW ===\n\nok"
	revise := projectContextBlock + "Follow the rules.\n\n=== TASK PROMPT ===\n\nDo the work.\n\n" +
		"=== QA FEEDBACK ===\n\nThe previous attempt was reviewed by QA and received verdict: fail\n\nFull QA response:\n{\"verdict\":\"fail\"}"
	want := []string{worker, qaPrompt, revise, qaPrompt}

	got := d.sent()
	if len(got) != len(want) {
		t.Fatalf("dispatched %d prompts, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("prompt %d:\ngot  %q\nwant %q", i, got[i], want[i])
		}
	}
}
