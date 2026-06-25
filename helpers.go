package main

import (
	"fmt"
	"time"
)

// deviceName returns the user label if set, otherwise the stable device ID.
func deviceName(d DeviceView) string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

// logFileName is the single source of truth for a session log's download
// filename, used by both the in-page download button and the /raw endpoint so
// the two never disagree. It keys off the stable device ID rather than the
// user label, which may contain spaces or characters unsafe for filenames.
func logFileName(deviceID string, sess Session) string {
	return fmt.Sprintf("%s-session-%d.log", deviceID, sess.Seq)
}

// fmtTime formats a timestamp for display; the zero time renders empty.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
