package tasks

import (
	"maps"
	"slices"

	"github.com/rjbs/taskmaster/internal/jmap"
)

// unanchoredAlert returns the id of an alert in obj with an OffsetTrigger
// relative to a property obj lacks, or "" if there is none.  JSCalendar bis
// §3.5.1 forbids "start" (the default) on a Task without "start" and "end"
// on a Task without "due".  Ids are checked in order, so the answer is
// stable.
func unanchoredAlert(obj map[string]any) string {
	alerts, _ := obj["alerts"].(map[string]any)
	for _, id := range slices.Sorted(maps.Keys(alerts)) {
		alert, _ := alerts[id].(map[string]any)
		trigger, _ := alert["trigger"].(map[string]any)
		if trigger["@type"] != "OffsetTrigger" {
			continue
		}
		anchor := "start"
		if trigger["relativeTo"] == "end" {
			anchor = "due"
		} else if r, ok := trigger["relativeTo"]; ok && r != "start" {
			continue
		}
		if _, ok := obj[anchor]; !ok {
			return id
		}
	}
	return ""
}

// unanchoredOverride returns the recurrence id of the first instance of a
// recurring task that, once its override patch is applied, has an alert
// relative to a property the instance lacks, or "" if there is none.
//
// An instance inherits everything from the base task except "start", which
// is the recurrence id unless the patch sets it.  A patch can add or remove
// "start", "due", or alerts independently, so each instance must be built
// in full before it can be checked.  A patch that cannot be applied is left
// alone: that is a malformed override, not an unanchored alert.
func unanchoredOverride(obj map[string]any) string {
	overrides, _ := obj["recurrenceOverrides"].(map[string]any)
	for _, rid := range slices.Sorted(maps.Keys(overrides)) {
		patch, _ := overrides[rid].(map[string]any)
		if patch["excluded"] == true {
			continue
		}
		instance := deepCopy(obj).(map[string]any)
		delete(instance, "recurrenceOverrides")
		instance["start"] = rid
		if jmap.ApplyPatch(instance, deepCopy(patch).(map[string]any)) != nil {
			continue
		}
		if unanchoredAlert(instance) != "" {
			return rid
		}
	}
	return ""
}

// deepCopy copies a decoded JSON value, so that patching the copy cannot
// change the original.
func deepCopy(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}
