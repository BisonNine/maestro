/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package global

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Task knowledge limits. Knowledge is available only when an embedding host
// supplies a knowledge source.
const (
	// DefaultKnowledgeBytes is the most a task's rendered knowledge section
	// may take in a prompt.
	DefaultKnowledgeBytes = 256 * 1024
	// DefaultKnowledgeLimit is the number of passages a query returns when
	// the task does not set limit.
	DefaultKnowledgeLimit = 5
	// MaxKnowledgeLimit is the largest limit a task may set.
	MaxKnowledgeLimit = 20
	// MaxKnowledgeIncludes is the most concept IDs a task may include.
	MaxKnowledgeIncludes = 16
	// MaxKnowledgeQueryRunes is the longest query a task may set.
	MaxKnowledgeQueryRunes = 256

	// ErrorCodeKnowledgeFailed marks a task that failed before any model call
	// because its knowledge could not be loaded.
	ErrorCodeKnowledgeFailed = "knowledge_failed"
)

// KnowledgeRequest names the knowledge a task's prompts must carry: records
// from one knowledge base by concept ID, ranked passages for a query, or both.
type KnowledgeRequest struct {
	KB      string   `json:"kb"`
	Include []string `json:"include,omitempty"`
	Query   string   `json:"query,omitempty"`
	Limit   int      `json:"limit,omitempty"` // 0 means DefaultKnowledgeLimit
}

// Validate checks the request's shape. It does not check that the knowledge
// base or the records exist: that happens when the task runs.
func (k *KnowledgeRequest) Validate() error {
	if strings.TrimSpace(k.KB) == "" {
		return fmt.Errorf("knowledge.kb is required")
	}
	if len(k.Include) == 0 && k.Query == "" {
		return fmt.Errorf("knowledge needs include, query or both")
	}
	if len(k.Include) > MaxKnowledgeIncludes {
		return fmt.Errorf("knowledge.include must list 1 to %d concept IDs", MaxKnowledgeIncludes)
	}
	seen := make(map[string]bool, len(k.Include))
	for _, id := range k.Include {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("knowledge.include has an empty concept ID")
		}
		if seen[id] {
			return fmt.Errorf("knowledge.include lists %q twice", id)
		}
		seen[id] = true
	}
	if k.Query != "" && (strings.TrimSpace(k.Query) == "" || utf8.RuneCountInString(k.Query) > MaxKnowledgeQueryRunes) {
		return fmt.Errorf("knowledge.query must be 1 to %d characters", MaxKnowledgeQueryRunes)
	}
	if k.Limit != 0 {
		if k.Query == "" {
			return fmt.Errorf("knowledge.limit needs a query")
		}
		if k.Limit < 1 || k.Limit > MaxKnowledgeLimit {
			return fmt.Errorf("knowledge.limit must be a whole number from 1 to %d", MaxKnowledgeLimit)
		}
	}
	return nil
}
