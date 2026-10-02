package tasks

import (
	"regexp"
	"strings"
	"time"
)

// utcLayout is how UTCDateTime values are stored and emitted.  Fixed-width
// fields mean the strings also compare correctly as text in SQL.
const utcLayout = "2006-01-02T15:04:05Z"

var localLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05.999999999",
}

// parseLocalDateTime parses a JSCalendar LocalDateTime: a date-time with no
// zone or offset.  The returned time is in UTC purely as a carrier for the
// wall-clock fields; use inZone to interpret it.
func parseLocalDateTime(s string) (time.Time, bool) {
	if strings.HasSuffix(s, "Z") || strings.ContainsAny(s[strings.IndexByte(s+"T", 'T'):], "+-") {
		return time.Time{}, false
	}
	for _, layout := range localLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// inZone reinterprets the wall-clock fields of a parsed LocalDateTime in loc
// and returns the resulting instant in UTC.
func inZone(local time.Time, loc *time.Location) time.Time {
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), local.Second(), local.Nanosecond(),
		loc,
	).UTC()
}

// parseUTCDateTime parses a JSCalendar UTCDateTime, which must end in "Z".
func parseUTCDateTime(s string) (time.Time, bool) {
	if !strings.HasSuffix(s, "Z") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), err == nil
}

func formatUTC(t time.Time) string {
	return t.UTC().Format(utcLayout)
}

// loadZone validates a TimeZoneId.  Go's LoadLocation accepts a few things
// that are not IANA names, which we exclude here.
func loadZone(name string) (*time.Location, bool) {
	if name == "" || name == "Local" {
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, false
	}
	return loc, true
}

var durationPattern = regexp.MustCompile(
	`^[+-]?P(?:\d+W)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`,
)

// validDuration checks a JSCalendar Duration.  signed allows a leading sign,
// which SignedDuration permits and Duration does not.
func validDuration(s string, signed bool) bool {
	if !durationPattern.MatchString(s) {
		return false
	}
	if !signed && (s[0] == '+' || s[0] == '-') {
		return false
	}
	trimmed := strings.TrimLeft(s, "+-")
	return trimmed != "P" && trimmed != "PT" && !strings.HasSuffix(trimmed, "T")
}
