/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package runner

import (
	"context"
	"fmt"
	"strings"

	"go.bison9.com/maestro/global"
	"go.bison9.com/maestro/knowledge"
)

// knowledgeHolder boxes a knowledge.Source so it can be swapped atomically.
type knowledgeHolder struct{ src knowledge.Source }

// SetKnowledge sets the source of the knowledge that tasks may request (nil:
// none). An embedding host calls it on every registration, possibly while a
// run is in progress, hence the atomic store.
func (r *Runner) SetKnowledge(src knowledge.Source) {
	if src == nil {
		r.knowledge.Store(nil)
		return
	}
	r.knowledge.Store(&knowledgeHolder{src: src})
}

func (r *Runner) knowledgeSource() knowledge.Source {
	if h := r.knowledge.Load(); h != nil {
		return h.src
	}
	return nil
}

// knowledgeSection renders the knowledge the task requests, for insertion
// after the instructions and before the task prompt. A task without knowledge
// gets "". Any failure to provide every required record is an error, and the
// caller fails the task before calling a model.
//
// Each concept ID appears once. Listed records come first, each followed by
// its includes (one level deep, in declared order); then the query's passages
// in rank order while they fit in global.DefaultKnowledgeBytes, followed by a
// count of those left out.
func (r *Runner) knowledgeSection(ctx context.Context, task *global.Task) (string, error) {
	req := task.Work.Knowledge
	if req == nil {
		return "", nil
	}
	return renderKnowledge(ctx, r.knowledgeSource(), req, global.DefaultKnowledgeBytes)
}

func renderKnowledge(ctx context.Context, src knowledge.Source, req *global.KnowledgeRequest, budget int) (string, error) {
	if src == nil {
		return "", fmt.Errorf("knowledge bases are not available (%s)", describeKnowledge(req))
	}

	var sb strings.Builder
	sb.WriteString("=== KNOWLEDGE ===\n\n")
	fmt.Fprintf(&sb, "Knowledge base: %s\n\n", req.KB)

	seen := make(map[string]bool)
	for _, id := range req.Include {
		// A listed record already shown as an include is still fetched: its
		// own includes are required too.
		rec, err := src.Get(ctx, req.KB, id)
		if err != nil {
			return "", fmt.Errorf("knowledge base %q, concept %q: %w", req.KB, id, err)
		}
		if rec == nil {
			return "", fmt.Errorf("knowledge base %q, concept %q: not found", req.KB, id)
		}
		if len(rec.Missing) > 0 {
			return "", fmt.Errorf("knowledge base %q, concept %q: missing included concepts: %s",
				req.KB, id, strings.Join(rec.Missing, ", "))
		}
		if len(rec.Omitted) > 0 {
			return "", fmt.Errorf("knowledge base %q, concept %q: included concepts not returned: %s",
				req.KB, id, strings.Join(rec.Omitted, ", "))
		}
		recID := recordID(rec, id)
		if !seen[recID] {
			writeRecord(&sb, rec, recID, "")
		}
		seen[recID] = true
		seen[id] = true
		for i := range rec.Includes {
			inc := &rec.Includes[i]
			if inc.ID == "" {
				return "", fmt.Errorf("knowledge base %q, concept %q: an included record has no ID", req.KB, id)
			}
			incID := inc.ID
			if seen[incID] {
				continue
			}
			seen[incID] = true
			writeRecord(&sb, inc, incID, recID)
		}
	}

	// +1 for the newline that closes the section.
	if sb.Len()+1 > budget {
		return "", fmt.Errorf("knowledge base %q: the required concepts (%s) take %d bytes, more than the %d-byte limit",
			req.KB, strings.Join(req.Include, ", "), sb.Len()+1, budget)
	}

	if req.Query != "" {
		limit := req.Limit
		if limit == 0 {
			limit = global.DefaultKnowledgeLimit
		}
		hits, err := src.Search(ctx, req.KB, req.Query, limit)
		if err != nil {
			return "", fmt.Errorf("knowledge base %q, query %q: %w", req.KB, req.Query, err)
		}
		writePassages(&sb, req.Query, hits, seen, budget)
	}

	sb.WriteString("\n")
	return sb.String(), nil
}

// recordID is the record's own ID, or fallback when the source left it empty.
func recordID(rec *knowledge.Record, fallback string) string {
	if rec.ID != "" {
		return rec.ID
	}
	return fallback
}

func writeRecord(sb *strings.Builder, rec *knowledge.Record, id, includedBy string) {
	heading := id
	if rec.Title != "" {
		heading += ": " + rec.Title
	}
	if includedBy != "" {
		heading += " (included by " + includedBy + ")"
	}
	fmt.Fprintf(sb, "--- %s ---\n", heading)
	if len(rec.Sources) > 0 {
		fmt.Fprintf(sb, "Sources: %s\n", strings.Join(rec.Sources, "; "))
	}
	if body := strings.TrimRight(rec.Body, "\n"); body != "" {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

// writePassages appends the query's passages in rank order, skipping concepts
// already shown, while the section (with the count of passages left out and
// its closing newline) stays within budget. Whatever does not fit at all,
// even the heading, is left out.
func writePassages(sb *strings.Builder, query string, hits []knowledge.Hit, seen map[string]bool, budget int) {
	if len(hits) == 0 {
		if msg := fmt.Sprintf("No passages matched \"%s\".\n", query); sb.Len()+len(msg)+1 <= budget {
			sb.WriteString(msg)
		}
		return
	}

	var lines []string
	for _, h := range hits {
		if seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		cite := h.ID
		if h.Section != "" {
			cite += " § " + h.Section
		}
		text := h.Passage
		if h.Title != "" {
			text = h.Title + ": " + text
		}
		lines = append(lines, fmt.Sprintf("[%s] %s\n", cite, text))
	}
	if len(lines) == 0 {
		return
	}

	// Keep the longest rank-order prefix that fits together with the count of
	// the passages left out. The "\n" that closes the section counts too.
	limit := budget - 1 - sb.Len()
	header := fmt.Sprintf("Passages matching \"%s\":\n", query)
	for i := range lines {
		lines[i] = fmt.Sprintf("%d. %s", i+1, lines[i])
	}
	size := make([]int, len(lines)+1) // size[k]: header plus the first k lines
	size[0] = len(header)
	for i, line := range lines {
		size[i+1] = size[i] + len(line)
	}
	for k := len(lines); k >= 0; k-- {
		if size[k]+len(omittedNote(len(lines)-k)) <= limit {
			sb.WriteString(header)
			for _, line := range lines[:k] {
				sb.WriteString(line)
			}
			sb.WriteString(omittedNote(len(lines) - k))
			return
		}
	}
}

// omittedNote reports n passages left out for lack of room ("" for none).
func omittedNote(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 more passage omitted\n"
	default:
		return fmt.Sprintf("%d more passages omitted\n", n)
	}
}

// describeKnowledge names a request's knowledge base and concept IDs for
// error messages.
func describeKnowledge(req *global.KnowledgeRequest) string {
	s := fmt.Sprintf("kb %q", req.KB)
	if len(req.Include) > 0 {
		s += ", concepts " + strings.Join(req.Include, ", ")
	}
	if req.Query != "" {
		s += fmt.Sprintf(", query %q", req.Query)
	}
	return s
}
