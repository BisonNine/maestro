/******************************************************************************
 * Copyright (c) 2025-2026 Tenebris Technologies Inc.                         *
 * Please see the LICENSE file for details                                    *
 ******************************************************************************/

package maestro

import (
	"fmt"
	"math"
	"sort"

	"go.bison9.com/maestro/global"
)

// Descriptions of the knowledge parameter. It is offered only when the host
// supplies a knowledge source (HostDeps.Knowledge).
const (
	knowledgeParamDescription  = `Optional knowledge for the prompts: {"kb": "<knowledge base>", "include": ["<concept ID>", ...], "query": "<search text>", "limit": <1-20, default 5>}. Needs kb and include, query or both. Included records and the records they include are inserted in full; a query adds ranked passages. A record that cannot be loaded fails the task without retry.`
	knowledgeUpdateDescription = knowledgeParamDescription + ` Replaces the stored value; {} removes it.`
	knowledgeListDescription   = knowledgeParamDescription + ` {{id}} and {{title}} in kb, include and query are replaced by each item's values.`
)

// errKnowledgeUnavailable is returned when a call carries knowledge but the
// host supplied no knowledge source.
const errKnowledgeUnavailable = "knowledge bases are not available"

// knowledgeKeys are the keys a knowledge object may carry.
var knowledgeKeys = map[string]bool{"kb": true, "include": true, "query": true, "limit": true}

// parseKnowledgeArg reads the knowledge argument. present reports whether the
// call carried it; an empty object yields (nil, true, nil). The shape is
// checked here (types, unknown keys, an empty include or query); the values
// are checked by KnowledgeRequest.Validate, after any substitution.
func parseKnowledgeArg(args map[string]any) (req *global.KnowledgeRequest, present bool, err error) {
	raw, ok := args["knowledge"]
	if !ok || raw == nil {
		return nil, false, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, true, fmt.Errorf("knowledge must be an object")
	}
	if len(obj) == 0 {
		return nil, true, nil
	}

	var unknown []string
	for k := range obj {
		if !knowledgeKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, true, fmt.Errorf("knowledge has an unknown key: %s", unknown[0])
	}

	req = &global.KnowledgeRequest{}
	if v, ok := obj["kb"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, true, fmt.Errorf("knowledge.kb must be a string")
		}
		req.KB = s
	}
	if v, ok := obj["include"]; ok {
		list, ok := v.([]any)
		if !ok {
			return nil, true, fmt.Errorf("knowledge.include must be a list of concept IDs")
		}
		if len(list) == 0 {
			return nil, true, fmt.Errorf("knowledge.include must list 1 to %d concept IDs", global.MaxKnowledgeIncludes)
		}
		for _, e := range list {
			s, ok := e.(string)
			if !ok {
				return nil, true, fmt.Errorf("knowledge.include must be a list of concept IDs")
			}
			req.Include = append(req.Include, s)
		}
	}
	if v, ok := obj["query"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, true, fmt.Errorf("knowledge.query must be 1 to %d characters", global.MaxKnowledgeQueryRunes)
		}
		req.Query = s
	}
	if v, ok := obj["limit"]; ok {
		f, ok := v.(float64)
		if i, isInt := v.(int); isInt {
			f, ok = float64(i), true
		}
		if !ok || f != math.Trunc(f) || f < 1 || f > global.MaxKnowledgeLimit {
			return nil, true, fmt.Errorf("knowledge.limit must be a whole number from 1 to %d", global.MaxKnowledgeLimit)
		}
		req.Limit = int(f)
	}
	return req, true, nil
}

// knowledgeArg reads and validates the knowledge argument of task_create,
// task_dispatch and task_update. With allowClear (task_update), an empty
// object is accepted and returned as (nil, true). Any problem is returned as
// a message for the model.
func (p *Provider) knowledgeArg(args map[string]any, allowClear bool) (*global.KnowledgeRequest, bool, string) {
	req, present, err := parseKnowledgeArg(args)
	if !present {
		return nil, false, ""
	}
	if !p.knowledge {
		return nil, true, errKnowledgeUnavailable
	}
	if err != nil {
		return nil, true, err.Error()
	}
	if req == nil {
		if allowClear {
			return nil, true, ""
		}
		return nil, true, "knowledge.kb is required"
	}
	if err := req.Validate(); err != nil {
		return nil, true, err.Error()
	}
	return req, true, ""
}
