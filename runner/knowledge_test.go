/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/knowledge"
)

// fakeSource serves records and search hits from maps. failGetAfter, when
// positive, makes every Get after that many calls fail.
type fakeSource struct {
	mu           sync.Mutex
	kb           string
	records      map[string]*knowledge.Record
	hits         map[string][]knowledge.Hit
	searchErr    error
	getCalls     int
	failGetAfter int
	lastLimit    int
}

func (s *fakeSource) Get(_ context.Context, kb, id string) (*knowledge.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	if s.failGetAfter > 0 && s.getCalls > s.failGetAfter {
		return nil, errors.New("store unavailable")
	}
	if kb != s.kb {
		return nil, fmt.Errorf("unknown knowledge base %q", kb)
	}
	rec, ok := s.records[id]
	if !ok {
		return nil, fmt.Errorf("concept %q not found", id)
	}
	cp := *rec
	return &cp, nil
}

func (s *fakeSource) Search(_ context.Context, kb, query string, limit int) ([]knowledge.Hit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastLimit = limit
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if kb != s.kb {
		return nil, fmt.Errorf("unknown knowledge base %q", kb)
	}
	hits := s.hits[query]
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// standardsSource is a small knowledge base: FCS_COP.1 includes FCS_CKM.1.
func standardsSource() *fakeSource {
	ckm := knowledge.Record{ID: "SFR/FCS_CKM.1", Title: "Key generation", Body: "Generate keys of the stated sizes.\n", Sources: []string{"cc-part2.pdf#ckm"}}
	return &fakeSource{
		kb: "standards",
		records: map[string]*knowledge.Record{
			"SFR/FCS_COP.1": {
				ID: "SFR/FCS_COP.1", Title: "Cryptographic operation", Body: "Perform operations with approved algorithms.",
				Sources:  []string{"cc-part2.pdf#cop", "notes.md"},
				Includes: []knowledge.Record{ckm},
			},
			"SFR/FCS_CKM.1": &ckm,
			"SFR/FCS_RBG.1": {ID: "SFR/FCS_RBG.1", Title: "Random bit generation", Body: "Seed from an approved source."},
		},
		hits: map[string][]knowledge.Hit{
			"key sizes": {
				{ID: "SFR/FCS_COP.1", Title: "Cryptographic operation", Section: "Application note", Passage: "already shown"},
				{ID: "SFR/FCS_CKM.2", Title: "Key establishment", Section: "Sizes", Passage: "Use 2048-bit RSA or larger."},
				{ID: "SFR/FCS_CKM.2", Title: "Key establishment", Section: "Other", Passage: "second passage, same concept"},
				{ID: "SFR/FCS_CKM.4", Passage: "Destroy keys after use."},
			},
		},
	}
}

func newKnowledgeRunner(t *testing.T, src knowledge.Source, d *scriptedDispatcher) *testRunner {
	t.Helper()
	tr := newScriptedRunner(t, d)
	if src != nil {
		tr.SetKnowledge(src)
	}
	return tr
}

// runOne creates one task in kp/main and runs it to completion.
func runOne(t *testing.T, tr *testRunner, work *global.WorkExecution, qa *global.QAExecution) *global.Task {
	t.Helper()
	task, err := tr.tasks.CreateTask("kp", "main", "Task", "", work, qa)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tr.Run(context.Background(), &global.RunRequest{Project: "kp"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tr.Wait()
	got, _, err := tr.tasks.GetTask("kp", task.UUID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	return got
}

const wantKnowledgeSection = "=== KNOWLEDGE ===\n\n" +
	"Knowledge base: standards\n\n" +
	"--- SFR/FCS_COP.1: Cryptographic operation ---\n" +
	"Sources: cc-part2.pdf#cop; notes.md\n" +
	"Perform operations with approved algorithms.\n\n" +
	"--- SFR/FCS_CKM.1: Key generation (included by SFR/FCS_COP.1) ---\n" +
	"Sources: cc-part2.pdf#ckm\n" +
	"Generate keys of the stated sizes.\n\n" +
	"Passages matching \"key sizes\":\n" +
	"1. [SFR/FCS_CKM.2 § Sizes] Key establishment: Use 2048-bit RSA or larger.\n" +
	"2. [SFR/FCS_CKM.4] Destroy keys after use.\n" +
	"\n"

// TestKnowledge_WorkerPromptLayout: the section sits between the instructions
// and the task prompt, records first with includes, then passages; concepts
// already shown are not repeated.
func TestKnowledge_WorkerPromptLayout(t *testing.T) {
	src := standardsSource()
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, src, d)

	task := runOne(t, tr, &global.WorkExecution{
		InstructionsText: "Follow the rules.", Prompt: "Do the work.",
		Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}, Query: "key sizes"},
	}, nil)

	if task.Work.Status != global.ExecutionStatusDone {
		t.Fatalf("status = %q (%s), want done", task.Work.Status, task.Work.Error)
	}
	want := projectContextBlock + "Follow the rules.\n\n" + wantKnowledgeSection + "=== TASK PROMPT ===\n\nDo the work.\n\n"
	got := d.sent()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("worker prompt:\ngot  %q\nwant %q", got, want)
	}
	if src.lastLimit != global.DefaultKnowledgeLimit {
		t.Errorf("search limit = %d, want default %d", src.lastLimit, global.DefaultKnowledgeLimit)
	}
}

// TestKnowledge_Dedupe: a concept listed directly and also included, or found
// again by the query, appears once; a listed record already shown as an
// include still brings its own includes.
func TestKnowledge_Dedupe(t *testing.T) {
	src := standardsSource()
	// FCS_CKM.1 now includes FCS_RBG.1.
	ckm := src.records["SFR/FCS_CKM.1"]
	ckm.Includes = []knowledge.Record{*src.records["SFR/FCS_RBG.1"]}
	src.hits["rbg"] = []knowledge.Hit{
		{ID: "SFR/FCS_RBG.1", Passage: "dup"},
		{ID: "SFR/FCS_CKM.1", Passage: "dup"},
	}

	out, err := renderKnowledge(context.Background(), src, &global.KnowledgeRequest{
		KB: "standards", Include: []string{"SFR/FCS_COP.1", "SFR/FCS_CKM.1"}, Query: "rbg",
	}, global.DefaultKnowledgeBytes)
	if err != nil {
		t.Fatalf("renderKnowledge: %v", err)
	}
	for _, id := range []string{"SFR/FCS_COP.1", "SFR/FCS_CKM.1", "SFR/FCS_RBG.1"} {
		if n := strings.Count(out, "--- "+id); n != 1 {
			t.Errorf("%s rendered %d times, want 1:\n%s", id, n, out)
		}
	}
	if !strings.Contains(out, "--- SFR/FCS_RBG.1: Random bit generation (included by SFR/FCS_CKM.1) ---") {
		t.Errorf("include of a record already shown is missing:\n%s", out)
	}
	if strings.Contains(out, "Passages matching") || strings.Contains(out, "dup") {
		t.Errorf("passages for concepts already shown must be dropped:\n%s", out)
	}
	if i, j := strings.Index(out, "--- SFR/FCS_COP.1"), strings.Index(out, "--- SFR/FCS_CKM.1"); i > j {
		t.Errorf("records out of listed order:\n%s", out)
	}
}

// TestKnowledge_FailuresBeforeModelCall: every way the knowledge cannot be
// provided fails the task with knowledge_failed, naming the kb and concepts,
// with no model call and no invocation counted.
func TestKnowledge_FailuresBeforeModelCall(t *testing.T) {
	big := strings.Repeat("x", global.DefaultKnowledgeBytes)
	tests := []struct {
		name    string
		src     func() knowledge.Source
		req     global.KnowledgeRequest
		wantMsg []string
	}{
		{
			name:    "no source",
			src:     func() knowledge.Source { return nil },
			req:     global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}},
			wantMsg: []string{"not available", "standards", "SFR/FCS_COP.1"},
		},
		{
			name:    "unknown kb",
			src:     func() knowledge.Source { return standardsSource() },
			req:     global.KnowledgeRequest{KB: "nope", Include: []string{"SFR/FCS_COP.1"}},
			wantMsg: []string{`"nope"`, "SFR/FCS_COP.1"},
		},
		{
			name:    "record not found",
			src:     func() knowledge.Source { return standardsSource() },
			req:     global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1", "SFR/NOPE.1"}},
			wantMsg: []string{"standards", "SFR/NOPE.1", "not found"},
		},
		{
			name: "missing include",
			src: func() knowledge.Source {
				s := standardsSource()
				s.records["SFR/FCS_COP.1"].Missing = []string{"SFR/GONE.1"}
				return s
			},
			req:     global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}},
			wantMsg: []string{"standards", "SFR/FCS_COP.1", "SFR/GONE.1"},
		},
		{
			name: "omitted include",
			src: func() knowledge.Source {
				s := standardsSource()
				s.records["SFR/FCS_COP.1"].Omitted = []string{"SFR/CUT.1"}
				return s
			},
			req:     global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}},
			wantMsg: []string{"standards", "SFR/FCS_COP.1", "SFR/CUT.1"},
		},
		{
			name: "search error",
			src: func() knowledge.Source {
				s := standardsSource()
				s.searchErr = errors.New("index unavailable")
				return s
			},
			req:     global.KnowledgeRequest{KB: "standards", Query: "key sizes"},
			wantMsg: []string{"standards", "key sizes", "index unavailable"},
		},
		{
			name: "required set over budget",
			src: func() knowledge.Source {
				s := standardsSource()
				s.records["SFR/FCS_RBG.1"].Body = big
				return s
			},
			req:     global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1", "SFR/FCS_RBG.1"}},
			wantMsg: []string{"standards", "SFR/FCS_COP.1", "SFR/FCS_RBG.1", "limit"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &scriptedDispatcher{}
			tr := newKnowledgeRunner(t, tt.src(), d)
			req := tt.req
			task := runOne(t, tr, &global.WorkExecution{Prompt: "Do the work.", Knowledge: &req}, nil)

			if task.Work.Status != global.ExecutionStatusFailed {
				t.Errorf("status = %q, want failed", task.Work.Status)
			}
			if task.Work.ErrorCode != global.ErrorCodeKnowledgeFailed {
				t.Errorf("error_code = %q, want %q", task.Work.ErrorCode, global.ErrorCodeKnowledgeFailed)
			}
			for _, w := range tt.wantMsg {
				if !strings.Contains(task.Work.Error, w) {
					t.Errorf("error %q lacks %q", task.Work.Error, w)
				}
			}
			if n := len(d.sent()); n != 0 {
				t.Errorf("model calls = %d, want 0", n)
			}
			if task.Work.Invocations != 0 {
				t.Errorf("invocations = %d, want 0", task.Work.Invocations)
			}
		})
	}
}

// TestKnowledge_FailureNotRetried: a knowledge failure is final; a later run
// does not pick the task up again.
func TestKnowledge_FailureNotRetried(t *testing.T) {
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, standardsSource(), d)
	task := runOne(t, tr, &global.WorkExecution{Prompt: "p", Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/NOPE.1"}}}, nil)
	if _, err := tr.Run(context.Background(), &global.RunRequest{Project: "kp"}, nil); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	tr.Wait()
	again, _, _ := tr.tasks.GetTask("kp", task.UUID)
	if again.Work.Status != global.ExecutionStatusFailed || len(d.sent()) != 0 || again.Work.Invocations != 0 {
		t.Errorf("after second run: status=%q calls=%d invocations=%d, want failed/0/0",
			again.Work.Status, len(d.sent()), again.Work.Invocations)
	}
}

// TestKnowledge_DispatchFailure: task_dispatch's single-shot path fails the
// same way and keeps the error code.
func TestKnowledge_DispatchFailure(t *testing.T) {
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, nil, d)
	res, err := tr.RunDispatch(context.Background(), &DispatchRequest{
		Project: "kp", Prompt: "p",
		Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}},
	}, nil)
	if err != nil {
		t.Fatalf("RunDispatch: %v", err)
	}
	tr.Wait()
	task, _, _ := tr.tasks.GetTask("kp", res.UUID)
	if task.Work.Status != global.ExecutionStatusFailed || task.Work.ErrorCode != global.ErrorCodeKnowledgeFailed {
		t.Errorf("status=%q code=%q, want failed/%s", task.Work.Status, task.Work.ErrorCode, global.ErrorCodeKnowledgeFailed)
	}
	if n := len(d.sent()); n != 0 {
		t.Errorf("model calls = %d, want 0", n)
	}
}

// TestKnowledge_EmptyQueryResult: a query with no hits is not an error.
func TestKnowledge_EmptyQueryResult(t *testing.T) {
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, standardsSource(), d)
	task := runOne(t, tr, &global.WorkExecution{
		Prompt:    "Do the work.",
		Knowledge: &global.KnowledgeRequest{KB: "standards", Query: "nothing here", Limit: 3},
	}, nil)
	if task.Work.Status != global.ExecutionStatusDone {
		t.Fatalf("status = %q (%s), want done", task.Work.Status, task.Work.Error)
	}
	want := projectContextBlock +
		"=== KNOWLEDGE ===\n\nKnowledge base: standards\n\nNo passages matched \"nothing here\".\n\n" +
		"=== TASK PROMPT ===\n\nDo the work.\n\n"
	if got := d.sent(); len(got) != 1 || got[0] != want {
		t.Errorf("prompt:\ngot  %q\nwant %q", got, want)
	}
}

// TestKnowledge_PassagesTrimmedToBudget: passages are kept in rank order while
// the section fits, and the rest are counted.
func TestKnowledge_PassagesTrimmedToBudget(t *testing.T) {
	src := standardsSource()
	var hits []knowledge.Hit
	for i := 1; i <= 20; i++ {
		hits = append(hits, knowledge.Hit{ID: fmt.Sprintf("P/%02d", i), Passage: strings.Repeat("y", 20*1024)})
	}
	src.hits["long"] = hits

	out, err := renderKnowledge(context.Background(), src, &global.KnowledgeRequest{
		KB: "standards", Include: []string{"SFR/FCS_RBG.1"}, Query: "long", Limit: 20,
	}, global.DefaultKnowledgeBytes)
	if err != nil {
		t.Fatalf("renderKnowledge: %v", err)
	}
	if len(out) > global.DefaultKnowledgeBytes {
		t.Errorf("section is %d bytes, over the %d-byte budget", len(out), global.DefaultKnowledgeBytes)
	}
	// Each passage line is a little over 20 KiB, so 12 fit after the record.
	if !strings.Contains(out, "12. [P/12] ") || strings.Contains(out, "[P/13]") {
		t.Errorf("expected passages 1-12 only:\n%.300s", out)
	}
	if !strings.HasSuffix(out, "8 more passages omitted\n\n") {
		t.Errorf("section must end with the omitted count, ends with %q", out[len(out)-40:])
	}
	if !strings.Contains(out, "--- SFR/FCS_RBG.1") {
		t.Error("required record missing")
	}
}

// TestKnowledge_BudgetEdges: exact fits at the budget, for the required set
// and for the passages, including the count of those left out.
func TestKnowledge_BudgetEdges(t *testing.T) {
	src := standardsSource()
	src.hits["q"] = []knowledge.Hit{{ID: "A", Passage: "aaaa"}, {ID: "B", Passage: "bbbb"}}
	req := &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_RBG.1"}}
	exact, err := renderKnowledge(context.Background(), src, req, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renderKnowledge(context.Background(), src, req, len(exact)); err != nil {
		t.Errorf("required set of exactly the budget must fit: %v", err)
	}
	if _, err := renderKnowledge(context.Background(), src, req, len(exact)-1); err == nil {
		t.Error("required set one byte over the budget must fail")
	}

	qreq := &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_RBG.1"}, Query: "q"}
	full, err := renderKnowledge(context.Background(), src, qreq, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for budget := len(exact); budget <= len(full); budget++ {
		out, err := renderKnowledge(context.Background(), src, qreq, budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if len(out) > budget {
			t.Errorf("budget %d: section is %d bytes", budget, len(out))
		}
		if strings.Contains(out, "Passages matching") && !strings.Contains(out, "2. [B]") && !strings.Contains(out, "omitted") {
			t.Errorf("budget %d: passages dropped without a count:\n%s", budget, out)
		}
	}
	if out, _ := renderKnowledge(context.Background(), src, qreq, len(full)); out != full {
		t.Errorf("at the full size every passage must fit:\n%s", out)
	}
}

// TestKnowledge_QAAndRevisePrompts: QA and revision prompts carry the same
// section, before their task prompts.
func TestKnowledge_QAAndRevisePrompts(t *testing.T) {
	d := &scriptedDispatcher{qaVerdicts: []string{"fail", "pass"}}
	tr := newKnowledgeRunner(t, standardsSource(), d)
	task := runOne(t, tr,
		&global.WorkExecution{InstructionsText: "Follow the rules.", Prompt: "Do the work.",
			Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}, Query: "key sizes"}},
		&global.QAExecution{Enabled: true, InstructionsText: "Check the rules.", Prompt: "Review the work."})
	if task.QA.Verdict != global.QAVerdictPass {
		t.Fatalf("QA verdict = %q, want pass", task.QA.Verdict)
	}

	got := d.sent()
	if len(got) != 4 {
		t.Fatalf("dispatched %d prompts, want 4 (worker, QA, revise, QA)", len(got))
	}
	wantQA := projectContextBlock + "Check the rules.\n\n" + wantKnowledgeSection + "=== QA TASK PROMPT ===\n\nReview the work.\n\n"
	wantRevise := projectContextBlock + "Follow the rules.\n\n" + wantKnowledgeSection + "=== TASK PROMPT ===\n\nDo the work.\n\n=== QA FEEDBACK ===\n\n"
	for i, want := range []string{"", wantQA, wantRevise, wantQA} {
		if want != "" && !strings.HasPrefix(got[i], want) {
			t.Errorf("prompt %d:\ngot  %q\nwant prefix %q", i, got[i], want)
		}
	}
}

// TestKnowledge_QAFailsBeforeDispatch: when the knowledge cannot be loaded for
// QA, QA fails without a model call.
func TestKnowledge_QAFailsBeforeDispatch(t *testing.T) {
	src := standardsSource()
	src.failGetAfter = 1 // the worker's Get succeeds, QA's fails
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, src, d)
	task := runOne(t, tr,
		&global.WorkExecution{Prompt: "Do the work.", Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}}},
		&global.QAExecution{Enabled: true, Prompt: "Review."})

	if n := len(d.sent()); n != 1 {
		t.Errorf("model calls = %d, want 1 (worker only)", n)
	}
	if task.QA.Status != global.ExecutionStatusFailed || task.QA.Invocations != 0 {
		t.Errorf("QA status=%q invocations=%d, want failed/0", task.QA.Status, task.QA.Invocations)
	}
}

// TestSetKnowledge_Replace: a nil source removes knowledge; tasks that need
// it then fail.
func TestSetKnowledge_Replace(t *testing.T) {
	d := &scriptedDispatcher{}
	tr := newKnowledgeRunner(t, standardsSource(), d)
	tr.SetKnowledge(nil)
	task := runOne(t, tr, &global.WorkExecution{Prompt: "p", Knowledge: &global.KnowledgeRequest{KB: "standards", Include: []string{"SFR/FCS_COP.1"}}}, nil)
	if task.Work.ErrorCode != global.ErrorCodeKnowledgeFailed {
		t.Errorf("error_code = %q, want %q", task.Work.ErrorCode, global.ErrorCodeKnowledgeFailed)
	}
}
