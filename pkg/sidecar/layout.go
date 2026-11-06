package sidecar

import (
	"fmt"
	"regexp"
	"time"

	"github.com/oklog/ulid/v2"
)

// StatusKey is the object key of the relay status document (Contract §7.5).
const StatusKey = "status/relay.json"

// NameRE matches final clip object names (Contract §7.2).
var NameRE = regexp.MustCompile(`^[0-9]{9}_[0-9A-HJKMNP-TV-Z]{26}\.(zrec|json)$`)

// ClipKeySet holds the storage keys of one clip, all under Dir.
type ClipKeySet struct {
	Video          string // <Dir>/<HHMMSSmmm>_<ULID>.zrec
	Sidecar        string // <Dir>/<HHMMSSmmm>_<ULID>.json
	StagingSidecar string // <Dir>/.<HHMMSSmmm>_<ULID>.json.part
	Dir            string // clips/<cam>/YYYY/MM/DD
}

// ObjectName returns "HHMMSSmmm_<ULID>.<ext>" with the time of day in UTC.
func ObjectName(start time.Time, id ulid.ULID, ext string) string {
	u := start.UTC()
	return fmt.Sprintf("%02d%02d%02d%03d_%s.%s",
		u.Hour(), u.Minute(), u.Second(), u.Nanosecond()/int(time.Millisecond), id, ext)
}

// StagingName returns the staging name ".<name>.part" (Contract §7.2).
func StagingName(name string) string {
	return "." + name + ".part"
}

// DayPrefix returns "clips/<cam>/YYYY/MM/DD" for the UTC day of day. The
// camera ID is assumed to be valid (Contract §4.1 pattern).
func DayPrefix(camera string, day time.Time) string {
	u := day.UTC()
	return fmt.Sprintf("clips/%s/%04d/%02d/%02d", camera, u.Year(), int(u.Month()), u.Day())
}

// ClipKeys returns the keys of a clip. Every key derives from the segment's
// own start time in UTC, so a segment starting after midnight lands in the
// next day's prefix.
func ClipKeys(camera string, start time.Time, id ulid.ULID) ClipKeySet {
	dir := DayPrefix(camera, start)
	sidecar := ObjectName(start, id, "json")
	return ClipKeySet{
		Video:          dir + "/" + ObjectName(start, id, "zrec"),
		Sidecar:        dir + "/" + sidecar,
		StagingSidecar: dir + "/" + StagingName(sidecar),
		Dir:            dir,
	}
}

// CoverageKey returns "status/coverage/<cam>/YYYY/MM/DD/HH.json" for the
// UTC hour containing hour.
func CoverageKey(camera string, hour time.Time) string {
	u := hour.UTC()
	return fmt.Sprintf("status/coverage/%s/%04d/%02d/%02d/%02d.json",
		camera, u.Year(), int(u.Month()), u.Day(), u.Hour())
}
