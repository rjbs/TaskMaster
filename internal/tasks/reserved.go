package tasks

import (
	"slices"

	"github.com/rjbs/taskmaster/internal/jmap"
)

// JSCalendar bis (§1.7.3, §7.4.2.2, Appendix A.2.2) reserves these property
// names in the object types listed, and an object holding one is invalid.
// "extra" is reserved in every object.  "excluded" is reserved too, but may
// form an entire recurrence override patch; overrideInstances handles that
// case, so anywhere else it is reported here.  "useDefaultAlerts" is
// reserved for JMAP, so it is ours to use and not listed.
var (
	reservedInTask        = []string{"extra", "excluded", "localizations", "replyTo", "requestStatus"}
	reservedInParticipant = []string{"extra", "invitedBy", "participationComment", "scheduleAgent",
		"scheduleForceSend", "scheduleSequence", "scheduleStatus", "scheduleUpdated", "sendTo"}
	reservedInLocation = []string{"extra", "description"}
	reservedInOther    = []string{"extra"}
)

// reservedObjects maps each Task property holding an Id-keyed map of
// JSCalendar objects to the names reserved in those objects.
var reservedObjects = map[string][]string{
	"participants":     reservedInParticipant,
	"locations":        reservedInLocation,
	"virtualLocations": reservedInLocation,
	"alerts":           reservedInOther,
	"links":            reservedInOther,
	"relatedTo":        reservedInOther,
}

// reservedPaths returns the JSON Pointer paths, in order, of every reserved
// property set in obj or in the objects it contains.
func reservedPaths(obj map[string]any) []string {
	var paths []string
	check := func(prefix string, m map[string]any, names []string) {
		for _, name := range names {
			if _, ok := m[name]; ok {
				paths = append(paths, prefix+name)
			}
		}
	}

	check("", obj, reservedInTask)
	if rule, ok := obj["recurrenceRule"].(map[string]any); ok {
		check("recurrenceRule/", rule, reservedInOther)
	}
	for prop, names := range reservedObjects {
		coll, _ := obj[prop].(map[string]any)
		for id, v := range coll {
			if m, ok := v.(map[string]any); ok {
				check(prop+"/"+jmap.EscapeToken(id)+"/", m, names)
			}
		}
	}
	slices.Sort(paths)
	return paths
}

// reservedOverride returns the recurrence id of the first instance of a
// recurring task that has a reserved property set, or "" if there is none.
func reservedOverride(obj map[string]any) string {
	for rid, instance := range overrideInstances(obj) {
		if len(reservedPaths(instance)) > 0 {
			return rid
		}
	}
	return ""
}
