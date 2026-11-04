package models

import (
	"encoding/json"
	"strings"
)

// JSONStringList decodes a JSON array column into a slice, tolerating the empty
// string, `null` and a malformed payload by answering no entries. Every JSON
// text column in the schema is read through this, so a column written by an
// older release cannot panic a reader.
func JSONStringList(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// JSONStringListToJSON encodes a string slice for a JSON text column — the writer
// that pairs with JSONStringList. A nil slice becomes `[]` rather than `null`: every
// reader tolerates both, and `[]` is what a column written by this release looks
// like, which keeps a dump of the database readable by eye.
func JSONStringListToJSON(items []string) string {
	if items == nil {
		items = []string{}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// JSONStringMap decodes a JSON object column into a map, same tolerance.
func JSONStringMap(raw string) map[string]string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// JSONObjects decodes an event's box column with the same tolerance: a column
// that is empty, `null`, not JSON, or the wrong JSON shape answers no boxes.
// The console draws boxes on the snapshot from this, and an event list must stay
// readable when one row's column was written by an older release.
func JSONObjects(raw string) []EventObject {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var out []EventObject
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// AppendUnique appends items missing from `have`, preserving the order of
// `have`. The event row's class/zone/rule unions use it, so the accumulated
// facts keep the order they arrived in and never grow duplicates.
func AppendUnique(have, items []string) []string {
	seen := make(map[string]bool, len(have)+len(items))
	out := make([]string, 0, len(have)+len(items))
	for _, v := range have {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range items {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
