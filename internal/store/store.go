// Package store persists sessions and their event logs as plain files:
//
//	<dir>/sessions/<id>.json   current session record (atomically replaced)
//	<dir>/events/<id>.jsonl    append-only event log
//
// It has no dependencies and is good enough for a single control-plane
// instance. A DynamoDB/S3 implementation can replace it behind the same API.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	StatusStarting Status = "starting" // sandbox is being provisioned
	StatusRunning  Status = "running"  // agent turn in progress
	StatusIdle     Status = "idle"     // waiting for the next message, sandbox up
	StatusStopped  Status = "stopped"  // sandbox removed, workspace volume kept
	StatusFailed   Status = "failed"
	StatusDeleted  Status = "deleted" // workspace removed, log kept
)

type Session struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Repo            string    `json:"repo"`
	BaseBranch      string    `json:"baseBranch"`
	Branch          string    `json:"branch"`
	Agent           string    `json:"agent"`
	CreatedBy       string    `json:"createdBy"`
	Status          Status    `json:"status"`
	Error           string    `json:"error,omitempty"`
	PRURL           string    `json:"prUrl,omitempty"`
	ClaudeSessionID string    `json:"claudeSessionId"`
	Turns           int       `json:"turns"`
	CostUSD         float64   `json:"costUsd"`
	Source          string    `json:"source"` // web | slack | api
	SlackChannel    string    `json:"slackChannel,omitempty"`
	SlackThread     string    `json:"slackThread,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	LastActiveAt    time.Time `json:"lastActiveAt"`
}

type Event struct {
	Seq     int64           `json:"seq"`
	Time    time.Time       `json:"time"`
	Type    string          `json:"type"` // user | assistant | tool_use | tool_result | result | status | error | egress | system
	Text    string          `json:"text,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Session string          `json:"-"`
}

type Store struct {
	dir      string
	mu       sync.RWMutex
	sessions map[string]*Session
	seq      map[string]int64
	subs     map[string]map[chan Event]struct{}
}

func Open(dir string) (*Store, error) {
	for _, d := range []string{"sessions", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, sessions: map[string]*Session{}, seq: map[string]int64{}, subs: map[string]map[chan Event]struct{}{}}
	files, err := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var sess Session
		if err := json.Unmarshal(b, &sess); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		s.sessions[sess.ID] = &sess
		evs, err := s.readEvents(sess.ID)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			s.seq[sess.ID] = evs[len(evs)-1].Seq
		}
	}
	return s, nil
}

func (s *Store) Create(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[sess.ID]; ok {
		return fmt.Errorf("session %s exists", sess.ID)
	}
	now := time.Now().UTC()
	sess.CreatedAt, sess.UpdatedAt, sess.LastActiveAt = now, now, now
	cp := *sess
	s.sessions[sess.ID] = &cp
	return s.write(&cp)
}

func (s *Store) Get(id string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, false
	}
	return *sess, true
}

func (s *Store) List() []Session {
	s.mu.RLock()
	out := make([]Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, *v)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Update applies fn to the session under lock and persists it.
func (s *Store) Update(id string, fn func(*Session)) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, fmt.Errorf("session %s not found", id)
	}
	cp := *sess
	fn(&cp)
	cp.UpdatedAt = time.Now().UTC()
	if err := s.write(&cp); err != nil {
		return Session{}, err
	}
	*sess = cp
	return cp, nil
}

func (s *Store) write(sess *Session) error {
	b, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.dir, "sessions", sess.ID+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Append stores an event and fans it out to subscribers.
func (s *Store) Append(id string, ev Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ev, fmt.Errorf("session %s not found", id)
	}
	s.seq[id]++
	ev.Seq = s.seq[id]
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "events", id+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ev, err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	for ch := range s.subs[id] {
		select {
		case ch <- ev:
		default: // slow subscriber: drop; it will resync via ?after=
		}
	}
	return ev, err
}

// Events returns events with seq > after.
func (s *Store) Events(id string, after int64) ([]Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs, err := s.readEvents(id)
	if err != nil {
		return nil, err
	}
	i := sort.Search(len(evs), func(i int) bool { return evs[i].Seq > after })
	return evs[i:], nil
}

func (s *Store) readEvents(id string) ([]Event, error) {
	f, err := os.Open(filepath.Join(s.dir, "events", id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // tolerate a torn last line after a crash
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// Subscribe returns a channel of new events for a session. Call cancel when done.
func (s *Store) Subscribe(id string) (ch chan Event, cancel func()) {
	ch = make(chan Event, 256)
	s.mu.Lock()
	if s.subs[id] == nil {
		s.subs[id] = map[chan Event]struct{}{}
	}
	s.subs[id][ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs[id], ch)
		s.mu.Unlock()
	}
}
