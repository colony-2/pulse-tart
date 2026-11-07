package provider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/colony-2/pulse-tart/internal/ctxlock"
	"os"
	"path/filepath"
	"time"

	compute "github.com/colony-2/pulse-provider-api"
	"golang.org/x/sys/unix"
)

var ErrState = errors.New("provider state unavailable")

type Record struct {
	LaunchID    string         `json:"launch_id"`
	Fingerprint string         `json:"fingerprint"`
	NativeHash  string         `json:"native_hash"`
	Name        string         `json:"name"`
	UID         string         `json:"uid"`
	Job         compute.JobRef `json:"job"`
	Created     time.Time      `json:"created"`
	Outcome     compute.Status `json:"outcome"`
	Phase       string         `json:"phase"`
	PairID      string         `json:"pair_id,omitempty"`
	SlotID      string         `json:"slot_id,omitempty"`
}
type state struct {
	Version int                     `json:"version"`
	Scope   string                  `json:"scope"`
	Key     []byte                  `json:"key"`
	Records map[string]*Record      `json:"records"`
	Slots   map[string]*SlotRecord  `json:"slots,omitempty"`
	Pairs   map[string]*PairRecord  `json:"pairs,omitempty"`
	Images  map[string]*ImageRecord `json:"images,omitempty"`
}
type Store struct {
	mu     ctxlock.Mutex
	dir    string
	lock   *os.File
	data   state
	broken bool
}

func OpenStore(dir, scope string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, ErrState
	}
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, ErrState
	}
	f, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, ErrState
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, ErrState
	}
	s := &Store{dir: dir, lock: f}
	st, e = os.Lstat(filepath.Join(dir, "state.json"))
	if e == nil && (!st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0) {
		s.Close()
		return nil, ErrState
	}
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(e) {
		s.data = state{Version: 3, Scope: scope, Key: make([]byte, 32), Records: map[string]*Record{}}
		if _, e = rand.Read(s.data.Key); e == nil {
			e = s.save()
		}
	} else if e == nil {
		e = json.Unmarshal(b, &s.data)
	}
	if e != nil || s.data.Version != 3 || s.data.Scope != scope || len(s.data.Key) != 32 || s.data.Records == nil {
		s.Close()
		return nil, ErrState
	}
	for id, r := range s.data.Records {
		if r == nil || r.LaunchID != id || r.Name == "" || r.Fingerprint == "" || r.NativeHash == "" || (r.Outcome != compute.Accepted && r.Outcome != compute.Unknown) || !validPhase(r.Phase) {
			s.Close()
			return nil, ErrState
		}
	}
	if s.data.Pairs == nil {
		s.data.Pairs = map[string]*PairRecord{}
	}
	for id, p := range s.data.Pairs {
		if p == nil || p.ID != id || p.Primary.Name == "" || p.PrimaryHash == "" || p.SlotID == "" || !validPairPhase(p.Phase) {
			s.Close()
			return nil, ErrState
		}
	}
	for _, r := range s.data.Records {
		if r.PairID != "" && s.data.Pairs[r.PairID] == nil {
			s.Close()
			return nil, ErrState
		}
	}
	if s.data.Slots == nil {
		s.data.Slots = map[string]*SlotRecord{}
	}
	if s.data.Images == nil {
		s.data.Images = map[string]*ImageRecord{}
	}
	for name, image := range s.data.Images {
		if image == nil || name != image.Name || image.NativeHash == "" || image.Worker == "" || !validSlotPhase(image.Phase) {
			s.Close()
			return nil, ErrState
		}
	}
	for id, r := range s.data.Slots {
		if r == nil || id != r.ID || r.Name == "" || r.NativeHash == "" || r.ConfigHash == "" || !validSlotPhase(r.Phase) || !validSlotIdentity(r) {
			s.Close()
			return nil, ErrState
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.lock.Close() }
func (s *Store) hash(v any) string {
	b, _ := json.Marshal(v)
	h := hmac.New(sha256.New, s.data.Key)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *Store) save() error {
	if s.broken {
		return ErrState
	}
	b, e := json.Marshal(s.data)
	if e != nil {
		return ErrState
	}
	f, e := os.CreateTemp(s.dir, ".state-")
	if e != nil {
		s.broken = true
		return ErrState
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, filepath.Join(s.dir, "state.json"))
	}
	if e == nil {
		d, de := os.Open(s.dir)
		if de == nil {
			e = d.Sync()
			d.Close()
		} else {
			e = de
		}
	}
	if e != nil {
		s.broken = true
		return ErrState
	}
	return nil
}

func validPhase(phase string) bool {
	switch phase {
	case "dispatch", "active", "deleting", "quarantine", "done":
		return true
	}
	return false
}

func validPairPhase(s string) bool {
	switch s {
	case "reserved", "engine-dispatch", "primary-dispatch", "configuring", "ready", "handoff-intent", "running", "reclaiming", "quarantine", "gone":
		return true
	}
	return false
}
