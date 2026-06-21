package main

import "time"

// deviceName returns the user label if set, otherwise the stable device ID.
func deviceName(d DeviceView) string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

// fmtTime formats a timestamp for display; the zero time renders empty.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}
