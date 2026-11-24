// Maestro
// License: MIT

package maestro

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolspec"

	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/knowledge"
	"go.bison9.com/maestro/runner"
)

// nullSource is a knowledge source with nothing in it; the handler tests only
// need one to be present.
type nullSource struct{}

func (nullSource) Get(context.Context, string, string) (*knowledge.Record, error) {
	return nil, nil
}
func (nullSource) Search(context.Context, string, string, int) ([]knowledge.Hit, error) {
	return nil, nil
}

var knowledgeTools = []string{global.ToolTaskCreate, global.ToolTaskUpdate, global.ToolTaskDispatch, global.ToolListCreateTasks}

// newKnowledgeProvider registers a provider (with a knowledge source when
// withKnowledge) and creates project "kp" with task set "main".
func newKnowledgeProvider(t *testing.T, withKnowledge bool) *Provider {
	t.Helper()
	host := HostDeps{Dispatcher: stubDispatcher{}}
	if withKnowledge {
		host.Knowledge = nullSource{}
	}
	p := &Provider{}
	p.RegisterTools(toolspec.Deps{Cfg: newPreparedConfig(t), Host: host})
	if _, err := p.projects.Create("kp", "KP", "", "", "", "none"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := p.tasks.CreateTaskSet("kp", "main", "Main", "", nil, false, global.Limits{}, true, ""); err != nil {
		t.Fatalf("create task set: %v", err)
	}
	return p
}

func call(args map[string]any) *toolspec.ToolCall {
	return &toolspec.ToolCall{Ctx: context.Background(), Args: args}
}

// decodeArgs turns a JSON object into tool arguments as an MCP client sends them.
func decodeArgs(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return m
}

func paramNames(defs []toolspec.ToolDefinition, param string) []string {
	var out []string
	for _, d := range defs {
		for _, prm := range d.Parameters {
			if prm.Name == param {
				out = append(out, d.Name)
			}
		}
	}
	return out
}

// TestKnowledgeParam_OnlyWithSource: the parameter is offered on exactly the
// four task-creating tools when a source is set, and the definitions are
// otherwise identical to those without one (standalone or host).
func TestKnowledgeParam_OnlyWithSource(t *testing.T) {
	with := registerProvider(t, HostDeps{Dispatcher: stubDispatcher{}, Knowledge: nullSource{}})
	if got := paramNames(with, "knowledge"); !reflect.DeepEqual(sortedCopy(got), sortedCopy(knowledgeTools)) {
		t.Errorf("knowledge offered on %v, want %v", got, knowledgeTools)
	}

	for name, host := range map[string]any{
		"standalone":         nil,
		"host":               HostDeps{Dispatcher: stubDispatcher{}},
		"host, own dispatch": HostDeps{},
	} {
		without := registerProvider(t, host)
		if got := paramNames(without, "knowledge"); len(got) != 0 {
			t.Errorf("%s: knowledge offered on %v, want none", name, got)
		}
	}

	stripped := withoutParam(with, "knowledge")
	without := registerProvider(t, HostDeps{Dispatcher: stubDispatcher{}})
	if a, b := defsJSON(t, stripped), defsJSON(t, without); a != b {
		t.Error("tool definitions differ beyond the knowledge parameter")
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func defsJSON(t *testing.T, defs []toolspec.ToolDefinition) string {
	t.Helper()
	type def struct {
		Name, Description string
		Parameters        []toolspec.Parameter
		Hints             *toolspec.ToolHints
		Async             bool
	}
	var out []def
	for _, d := range defs {
		out = append(out, def{d.Name, d.Description, d.Parameters, d.Hints, d.Async})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestKnowledgeParam_Validation: each shape rule, through task_create.
func TestKnowledgeParam_Validation(t *testing.T) {
	long := strings.Repeat("é", global.MaxKnowledgeQueryRunes)
	var many []string
	for i := 0; i <= global.MaxKnowledgeIncludes; i++ {
		many = append(many, `"C`+string(rune('a'+i))+`"`)
	}
	tests := []struct {
		name, knowledge, wantErr string
	}{
		{"include", `{"kb":"s","include":["A","B"]}`, ""},
		{"query with limit", `{"kb":"s","query":"key sizes","limit":20}`, ""},
		{"both", `{"kb":"s","include":["A"],"query":"q"}`, ""},
		{"query at max runes", `{"kb":"s","query":"` + long + `"}`, ""},
		{"not an object", `"s"`, "must be an object"},
		{"empty object", `{}`, "kb is required"},
		{"no kb", `{"include":["A"]}`, "kb is required"},
		{"blank kb", `{"kb":" ","include":["A"]}`, "kb is required"},
		{"kb not a string", `{"kb":1,"include":["A"]}`, "kb must be a string"},
		{"neither include nor query", `{"kb":"s"}`, "include, query or both"},
		{"empty include", `{"kb":"s","include":[]}`, "1 to 16"},
		{"too many includes", `{"kb":"s","include":[` + strings.Join(many, ",") + `]}`, "1 to 16"},
		{"include not strings", `{"kb":"s","include":[1]}`, "list of concept IDs"},
		{"include empty entry", `{"kb":"s","include":["A",""]}`, "empty concept ID"},
		{"include duplicate", `{"kb":"s","include":["A","A"]}`, "twice"},
		{"empty query", `{"kb":"s","query":""}`, "1 to 256"},
		{"query too long", `{"kb":"s","query":"` + long + `x"}`, "1 to 256"},
		{"limit without query", `{"kb":"s","include":["A"],"limit":3}`, "needs a query"},
		{"limit zero", `{"kb":"s","query":"q","limit":0}`, "1 to 20"},
		{"limit too big", `{"kb":"s","query":"q","limit":21}`, "1 to 20"},
		{"limit fractional", `{"kb":"s","query":"q","limit":2.5}`, "1 to 20"},
		{"unknown key", `{"kb":"s","include":["A"],"extra":true}`, "unknown key: extra"},
	}
	p := newKnowledgeProvider(t, true)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := decodeArgs(t, `{"project":"kp","path":"main","title":"T","prompt":"p","knowledge":`+tt.knowledge+`}`)
			res, err := p.handleTaskCreate(call(args))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if tt.wantErr == "" {
				if res.IsError {
					t.Fatalf("unexpected error result: %s", res.ForLLM)
				}
				var task global.Task
				if err := json.Unmarshal([]byte(res.ForLLM), &task); err != nil {
					t.Fatal(err)
				}
				if task.Work.Knowledge == nil || task.Work.Knowledge.KB != "s" {
					t.Errorf("knowledge not stored: %+v", task.Work.Knowledge)
				}
				return
			}
			if !res.IsError || !strings.Contains(res.ForLLM, tt.wantErr) {
				t.Errorf("result %q (error=%v), want error containing %q", res.ForLLM, res.IsError, tt.wantErr)
			}
		})
	}
}

// TestKnowledgeParam_RefusedWithoutSource: a handler that receives knowledge
// although none is offered refuses it, on every tool that could carry it.
func TestKnowledgeParam_RefusedWithoutSource(t *testing.T) {
	p := newKnowledgeProvider(t, false)
	k := map[string]any{"kb": "s", "include": []any{"A"}}
	calls := map[string]func(*toolspec.ToolCall) (*toolspec.Result, error){
		global.ToolTaskCreate:      p.handleTaskCreate,
		global.ToolTaskDispatch:    p.handleTaskDispatch,
		global.ToolTaskUpdate:      p.handleTaskUpdate,
		global.ToolListCreateTasks: p.handleListCreateTasks,
	}
	for tool, h := range calls {
		args := map[string]any{"project": "kp", "path": "main", "title": "T", "prompt": "p", "uuid": "x",
			"list": "l", "type": "t", "knowledge": k}
		res, err := h(call(args))
		if err != nil {
			t.Fatalf("%s: handler error: %v", tool, err)
		}
		if !res.IsError || res.ForLLM != errKnowledgeUnavailable {
			t.Errorf("%s: result %q, want %q", tool, res.ForLLM, errKnowledgeUnavailable)
		}
	}
}

// TestKnowledgeParam_UpdateReplacesAndClears: task_update replaces the stored
// request, leaves it alone when absent, and {} clears it.
func TestKnowledgeParam_UpdateReplacesAndClears(t *testing.T) {
	p := newKnowledgeProvider(t, true)
	res, _ := p.handleTaskCreate(call(decodeArgs(t, `{"project":"kp","path":"main","title":"T","prompt":"p","knowledge":{"kb":"s","include":["A"]}}`)))
	var task global.Task
	if err := json.Unmarshal([]byte(res.ForLLM), &task); err != nil {
		t.Fatal(err)
	}

	update := func(extra string) *global.Task {
		t.Helper()
		res, err := p.handleTaskUpdate(call(decodeArgs(t, `{"project":"kp","uuid":"`+task.UUID+`"`+extra+`}`)))
		if err != nil || res.IsError {
			t.Fatalf("update %s: %v %s", extra, err, res.ForLLM)
		}
		got, _, err := p.tasks.GetTask("kp", task.UUID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := update(`,"knowledge":{"kb":"t","query":"q","limit":2}`)
	want := &global.KnowledgeRequest{KB: "t", Query: "q", Limit: 2}
	if !reflect.DeepEqual(got.Work.Knowledge, want) {
		t.Errorf("after replace: %+v, want %+v", got.Work.Knowledge, want)
	}
	if got = update(`,"title":"T2"`); !reflect.DeepEqual(got.Work.Knowledge, want) {
		t.Errorf("an update without knowledge changed it: %+v", got.Work.Knowledge)
	}
	if got = update(`,"knowledge":{}`); got.Work.Knowledge != nil {
		t.Errorf("after {}: %+v, want none", got.Work.Knowledge)
	}
	res, _ = p.handleTaskUpdate(call(decodeArgs(t, `{"project":"kp","uuid":"`+task.UUID+`","knowledge":{"kb":"t"}}`)))
	if !res.IsError {
		t.Error("an invalid replacement must be refused")
	}
}

// TestKnowledgeParam_Dispatch: task_dispatch stores the request on its task.
func TestKnowledgeParam_Dispatch(t *testing.T) {
	p := newKnowledgeProvider(t, true)
	res, err := p.handleTaskDispatch(call(decodeArgs(t, `{"project":"kp","prompt":"p","knowledge":{"kb":"s","query":"q"}}`)))
	if err != nil || res.IsError {
		t.Fatalf("dispatch: %v %s", err, res.ForLLM)
	}
	p.runner.Wait()
	var out struct{ UUID string }
	if err := json.Unmarshal([]byte(res.ForLLM), &out); err != nil {
		t.Fatal(err)
	}
	task, _, err := p.tasks.GetTask("kp", out.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (&global.KnowledgeRequest{KB: "s", Query: "q"}); !reflect.DeepEqual(task.Work.Knowledge, want) {
		t.Errorf("stored %+v, want %+v", task.Work.Knowledge, want)
	}
}

// TestKnowledgeParam_ListCreateTasks: {{id}} and {{title}} are substituted per
// item in kb, include and query, and each result is validated before any task
// is created.
func TestKnowledgeParam_ListCreateTasks(t *testing.T) {
	p := newKnowledgeProvider(t, true)
	items := []global.ListItem{{ID: "FCS_COP.1", Title: "Crypto", Content: "c"}, {ID: "FCS_CKM.1", Title: "Keys", Content: "k"}}
	if err := p.lists.Create("project", "kp", "", "sfrs", "SFRs", "", items); err != nil {
		t.Fatalf("create list: %v", err)
	}

	res, err := p.handleListCreateTasks(call(decodeArgs(t, `{"list_source":"project","list_project":"kp","list":"sfrs",
		"project":"kp","path":"sfr","type":"sfr","prompt":"Check.",
		"knowledge":{"kb":"std","include":["SFR/{{id}}"],"query":"{{title}} sizes","limit":3}}`)))
	if err != nil || res.IsError {
		t.Fatalf("list_create_tasks: %v %s", err, res.ForLLM)
	}
	list, err := p.tasks.ListTasks("kp", "sfr", "", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*global.KnowledgeRequest{}
	for _, task := range list.Tasks {
		got[task.Title] = task.Work.Knowledge
	}
	want := map[string]*global.KnowledgeRequest{
		"Crypto": {KB: "std", Include: []string{"SFR/FCS_COP.1"}, Query: "Crypto sizes", Limit: 3},
		"Keys":   {KB: "std", Include: []string{"SFR/FCS_CKM.1"}, Query: "Keys sizes", Limit: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tasks' knowledge = %+v, want %+v", got, want)
	}

	// Substitution that yields an invalid request (the same concept twice for
	// one item) creates nothing.
	res, err = p.handleListCreateTasks(call(decodeArgs(t, `{"list_source":"project","list_project":"kp","list":"sfrs",
		"project":"kp","path":"bad","type":"sfr","prompt":"Check.",
		"knowledge":{"kb":"std","include":["{{id}}","FCS_CKM.1"]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.ForLLM, "FCS_CKM.1") || !strings.Contains(res.ForLLM, "twice") {
		t.Errorf("result %q, want an error naming the item", res.ForLLM)
	}
	if _, err := p.tasks.GetTaskSet("kp", "bad"); err == nil {
		t.Error("a task set was created although an item's knowledge is invalid")
	}
}

// TestKnowledge_ReRegistrationRefreshesRunner: a host keeps its runner across
// reloads; registering again without a source removes it from that runner.
func TestKnowledge_ReRegistrationRefreshesRunner(t *testing.T) {
	cfg := newPreparedConfig(t)
	first := &Provider{}
	first.RegisterTools(toolspec.Deps{Cfg: cfg, Host: HostDeps{Dispatcher: stubDispatcher{}, Knowledge: nullSource{}}})
	kept := first.runner

	second := &Provider{}
	second.RegisterTools(toolspec.Deps{Cfg: cfg, Host: HostDeps{Dispatcher: stubDispatcher{}, Runner: kept}})
	if _, err := second.projects.Create("kp", "KP", "", "", "", "none"); err != nil {
		t.Fatal(err)
	}

	res, err := kept.RunDispatch(context.Background(), &runner.DispatchRequest{Project: "kp", Prompt: "p",
		Knowledge: &global.KnowledgeRequest{KB: "s", Include: []string{"A"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kept.Wait()
	task, _, err := second.tasks.GetTask("kp", res.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Work.ErrorCode != global.ErrorCodeKnowledgeFailed || !strings.Contains(task.Work.Error, "not available") {
		t.Errorf("status=%q code=%q error=%q, want the source removed", task.Work.Status, task.Work.ErrorCode, task.Work.Error)
	}
}

// TestHandleStartHere_Knowledge: the knowledge section is appended only when a
// source is set.
func TestHandleStartHere_Knowledge(t *testing.T) {
	content := func(p *Provider) string {
		t.Helper()
		res, err := p.handleStartHere(&toolspec.ToolCall{})
		if err != nil || res.IsError {
			t.Fatalf("start_here: %v", err)
		}
		var item struct{ Content string }
		if err := json.Unmarshal([]byte(res.ForLLM), &item); err != nil {
			t.Fatal(err)
		}
		return item.Content
	}
	with := content(newKnowledgeProvider(t, true))
	without := content(newKnowledgeProvider(t, false))
	if strings.Contains(without, "### Knowledge bases") {
		t.Error("start guide mentions knowledge bases without a source")
	}
	if !strings.HasSuffix(with, hostKnowledgeSection) || !strings.HasPrefix(with, strings.TrimRight(without, "\n")) {
		t.Error("with a source the guide must be the same text plus the knowledge section")
	}
}
