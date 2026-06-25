package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/soypat/picohub/flash"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketDevices  = []byte("devices")
	bucketSessions = []byte("sessions")
	bucketFlashes  = []byte("flashes")
)

// DeviceRecord is the persisted, user-editable metadata for a board. Live state
// (Port, Present) is not stored here; it comes from discovery at runtime.
type DeviceRecord struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`            // user label
	TargetOverride flash.Target `json:"target_override"` // TargetUnknown = use discovered
	FirstSeen      time.Time    `json:"first_seen"`
	LastSeen       time.Time    `json:"last_seen"`
	Notes          string       `json:"notes"`
}

// Session is one monitoring period for a device: from connect/flash until the
// next flash or disconnect. The raw serial bytes are stored in LogFile on disk.
type Session struct {
	ID            string    `json:"id"`
	DeviceID      string    `json:"device_id"`
	Seq           int       `json:"seq"` // 1-based, per device
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"` // zero while active
	LogFile       string    `json:"log_file"`
	ByteLen       int64     `json:"byte_len"`
	FirmwareName  string    `json:"firmware_name"` // firmware flashed at session start, if any
	FirmwareSHA   string    `json:"firmware_sha"`
	ContinuedFrom string    `json:"continued_from"` // prior session ID when this is a rollover continuation
}

// Active reports whether the session is still being written to.
func (s Session) Active() bool { return s.EndedAt.IsZero() }

// Continuation reports whether this session continues an earlier one whose log
// rolled over at the size limit.
func (s Session) Continuation() bool { return s.ContinuedFrom != "" }

// FlashRecord is one flash attempt (success or failure).
type FlashRecord struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"device_id"`
	At         time.Time `json:"at"`
	Firmware   string    `json:"firmware"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	OK         bool      `json:"ok"`
	Err        string    `json:"err"`
	DurationMs int64     `json:"duration_ms"`
}

// Store persists device metadata, sessions, and flash history in bbolt, and
// owns the on-disk log directory where raw serial output is appended.
type Store struct {
	db      *bolt.DB
	logsDir string
}

// OpenStore opens (creating if needed) the bbolt database and logs directory.
func OpenStore(dbPath, logsDir string) (*Store, error) {
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", dbPath, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketDevices, bucketSessions, bucketFlashes} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, logsDir: logsDir}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- generic JSON tx helpers (gsan-style) ----------------------------------

func txPut(tx *bolt.Tx, bucket []byte, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), data)
}

func txGet(tx *bolt.Tx, bucket []byte, key string, dst any) (bool, error) {
	data := tx.Bucket(bucket).Get([]byte(key))
	if data == nil {
		return false, nil
	}
	return true, json.Unmarshal(data, dst)
}

// --- devices ---------------------------------------------------------------

// DeviceSeen records that a device was observed, creating its record on first
// sight and bumping LastSeen otherwise. Returns the stored record.
func (s *Store) DeviceSeen(id string, now time.Time) (DeviceRecord, error) {
	var rec DeviceRecord
	err := s.db.Update(func(tx *bolt.Tx) error {
		found, err := txGet(tx, bucketDevices, id, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = DeviceRecord{ID: id, FirstSeen: now}
		}
		rec.LastSeen = now
		return txPut(tx, bucketDevices, id, rec)
	})
	return rec, err
}

func (s *Store) Device(id string) (DeviceRecord, bool, error) {
	var rec DeviceRecord
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		found, err = txGet(tx, bucketDevices, id, &rec)
		return err
	})
	return rec, found, err
}

func (s *Store) Devices() ([]DeviceRecord, error) {
	var out []DeviceRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDevices).ForEach(func(_, v []byte) error {
			var rec DeviceRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, rec)
			return nil
		})
	})
	return out, err
}

// SetDeviceName updates the user label for a device.
func (s *Store) SetDeviceName(id, name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var rec DeviceRecord
		found, err := txGet(tx, bucketDevices, id, &rec)
		if err != nil {
			return err
		}
		if !found {
			rec = DeviceRecord{ID: id, FirstSeen: time.Now()}
		}
		rec.Name = name
		return txPut(tx, bucketDevices, id, rec)
	})
}

// --- sessions --------------------------------------------------------------

// SessionStart creates a new active session for a device and returns it, with
// LogFile pointing at a fresh file path under logsDir (the caller opens it).
func (s *Store) SessionStart(deviceID string, now time.Time) (Session, error) {
	return s.startSession(Session{DeviceID: deviceID, StartedAt: now})
}

// SessionContinue starts a continuation session for a device whose previous log
// rolled over at the size limit. It carries forward the firmware context and
// links back to prev via ContinuedFrom.
func (s *Store) SessionContinue(prev Session, now time.Time) (Session, error) {
	return s.startSession(Session{
		DeviceID:      prev.DeviceID,
		StartedAt:     now,
		ContinuedFrom: prev.ID,
		FirmwareName:  prev.FirmwareName,
		FirmwareSHA:   prev.FirmwareSHA,
	})
}

// startSession assigns an ID, sequence, and log path to sess, persists it, and
// ensures its log directory exists. The caller opens the log file.
func (s *Store) startSession(sess Session) (Session, error) {
	sess.ID = newID()
	err := s.db.Update(func(tx *bolt.Tx) error {
		sess.Seq = s.nextSeq(tx, sess.DeviceID)
		sess.LogFile = s.logPath(sess.DeviceID, sess.ID)
		return txPut(tx, bucketSessions, sess.ID, sess)
	})
	if err != nil {
		return Session{}, err
	}
	if err := os.MkdirAll(filepath.Dir(sess.LogFile), 0o755); err != nil {
		return Session{}, err
	}
	return sess, nil
}

// EndStaleSessions marks any session still flagged active as ended. It is meant
// to run once at startup: a session active at boot can only be a leftover from
// a previous process that exited without ending it (a crash or kill). EndedAt
// is set to the log file's last-modified time — the last moment output arrived
// — and ByteLen is backfilled from the file size when it was never recorded.
// Returns the number of sessions repaired.
func (s *Store) EndStaleSessions() (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		var stale []Session
		_ = b.ForEach(func(_, v []byte) error {
			var sess Session
			if json.Unmarshal(v, &sess) == nil && sess.Active() {
				stale = append(stale, sess)
			}
			return nil
		})
		for _, sess := range stale {
			ended := sess.StartedAt
			if fi, err := os.Stat(sess.LogFile); err == nil {
				ended = fi.ModTime()
				if sess.ByteLen == 0 {
					sess.ByteLen = fi.Size()
				}
			}
			sess.EndedAt = ended
			if err := txPut(tx, bucketSessions, sess.ID, sess); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// nextSeq returns the next per-device session sequence number.
func (s *Store) nextSeq(tx *bolt.Tx, deviceID string) int {
	max := 0
	_ = tx.Bucket(bucketSessions).ForEach(func(_, v []byte) error {
		var sess Session
		if json.Unmarshal(v, &sess) == nil && sess.DeviceID == deviceID && sess.Seq > max {
			max = sess.Seq
		}
		return nil
	})
	return max + 1
}

// SessionUpdate applies a mutation to a stored session.
func (s *Store) SessionUpdate(id string, fn func(*Session)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var sess Session
		found, err := txGet(tx, bucketSessions, id, &sess)
		if err != nil || !found {
			return err
		}
		fn(&sess)
		return txPut(tx, bucketSessions, id, sess)
	})
}

// DeleteSession removes a session's record and its on-disk log file. A missing
// session is reported via the bool, not an error; a missing log file is ignored.
func (s *Store) DeleteSession(id string) (bool, error) {
	var sess Session
	var found bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		found, err = txGet(tx, bucketSessions, id, &sess)
		if err != nil || !found {
			return err
		}
		return tx.Bucket(bucketSessions).Delete([]byte(id))
	})
	if err != nil || !found {
		return found, err
	}
	if sess.LogFile != "" {
		if err := os.Remove(sess.LogFile); err != nil && !os.IsNotExist(err) {
			return true, err
		}
	}
	return true, nil
}

func (s *Store) Session(id string) (Session, bool, error) {
	var sess Session
	var found bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		found, err = txGet(tx, bucketSessions, id, &sess)
		return err
	})
	return sess, found, err
}

// Sessions returns a device's sessions, newest first.
func (s *Store) Sessions(deviceID string) ([]Session, error) {
	var out []Session
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSessions).ForEach(func(_, v []byte) error {
			var sess Session
			if err := json.Unmarshal(v, &sess); err != nil {
				return err
			}
			if sess.DeviceID == deviceID {
				out = append(out, sess)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out, err
}

// --- flashes ---------------------------------------------------------------

func (s *Store) FlashRecord(rec FlashRecord) error {
	if rec.ID == "" {
		rec.ID = newID()
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return txPut(tx, bucketFlashes, rec.ID, rec)
	})
}

// Flashes returns a device's flash attempts, newest first.
func (s *Store) Flashes(deviceID string) ([]FlashRecord, error) {
	var out []FlashRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFlashes).ForEach(func(_, v []byte) error {
			var rec FlashRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if rec.DeviceID == deviceID {
				out = append(out, rec)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, err
}

// --- paths / ids -----------------------------------------------------------

func (s *Store) logPath(deviceID, sessionID string) string {
	return filepath.Join(s.logsDir, safeFileName(deviceID), sessionID+".log")
}

// safeFileName makes a device ID safe to use as a directory name.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}

// newID returns a short random hex identifier.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
