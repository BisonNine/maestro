/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

// Package knowledge defines the seam through which an embedding host lends
// Maestro read access to its knowledge bases. Maestro inserts the records a
// task names, and the passages its query finds, into the task's prompts.
// Standalone Maestro has no Source and the feature is absent.
package knowledge

import "context"

// Source reads records from the host's knowledge bases. Implementations must
// be safe for concurrent use: tasks run in parallel.
type Source interface {
	// Get returns the record with concept ID id from knowledge base kb, with
	// the records it declares as includes one level deep. An unknown kb or a
	// missing record is an error.
	Get(ctx context.Context, kb, id string) (*Record, error)
	// Search returns up to limit ranked passages from knowledge base kb that
	// match query. An empty result is not an error.
	Search(ctx context.Context, kb, query string, limit int) ([]Hit, error)
}

// Record is one concept from a knowledge base.
type Record struct {
	ID      string
	Title   string
	Body    string
	Sources []string // source resources the record cites
	// Includes are the records this one declares as required, one level deep,
	// in the order declared.
	Includes []Record
	// Missing lists declared includes that do not exist.
	Missing []string
	// Omitted lists declared includes the Source did not return.
	Omitted []string
}

// Hit is one ranked search passage.
type Hit struct {
	ID      string // concept ID of the record the passage comes from
	Title   string
	Section string
	Passage string
}
