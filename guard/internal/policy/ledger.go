package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one amount the guard let through.
type Entry struct {
	Key  string    `json:"key"`
	Sat  int64     `json:"sat"`
	At   time.Time `json:"at"`
	Done bool      `json:"done"` // an on-chain send: never again
}

// Ledger is the guard's memory, kept on disk so a restart forgets nothing:
// what it let through in the last days (the daily limit, lockups sent once)
// and the hold invoices the backend created (the only ones it may cancel or
// settle).
type Ledger struct {
	mu      sync.Mutex
	path    string
	Entries map[string]Entry     `json:"entries"`
	Holds   map[string]time.Time `json:"holds"`
	now     func() time.Time
}

// keep is how long entries and holds are remembered.
const keep = 30 * 24 * time.Hour

// OpenLedger reads the ledger at path ("" keeps it in memory, for tests).
func OpenLedger(path string, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	l := &Ledger{path: path, Entries: map[string]Entry{}, Holds: map[string]time.Time{}, now: now}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, l.save()
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, err
	}
	if l.Entries == nil {
		l.Entries = map[string]Entry{}
	}
	if l.Holds == nil {
		l.Holds = map[string]time.Time{}
	}
	return l, nil
}

func (l *Ledger) save() error {
	if l.path == "" {
		return nil
	}
	cut := l.now().Add(-keep)
	for k, e := range l.Entries {
		if e.At.Before(cut) {
			delete(l.Entries, k)
		}
	}
	for h, at := range l.Holds {
		if at.Before(cut) {
			delete(l.Holds, h)
		}
	}
	b, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(l.path))
	if err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func (l *Ledger) Get(key string) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.Entries[key]
	return e, ok
}

func (l *Ledger) Put(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Entries[e.Key] = e
	return l.save()
}

func (l *Ledger) Delete(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.Entries, key)
	return l.save()
}

// Since sums what went through after t.
func (l *Ledger) Since(t time.Time) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s int64
	for _, e := range l.Entries {
		if e.At.After(t) {
			s += e.Sat
		}
	}
	return s
}

func (l *Ledger) AddHold(hash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Holds[hash] = l.now()
	return l.save()
}

func (l *Ledger) HasHold(hash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.Holds[hash]
	return ok
}
