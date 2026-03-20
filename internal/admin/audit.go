package admin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AuditEvent records a backend admin operation.
type AuditEvent struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Object    string    `json:"object,omitempty"`
	Result    string    `json:"result"`
	Detail    string    `json:"detail,omitempty"`
}

// AuditStore persists audit events in JSONL.
type AuditStore struct {
	mu   sync.Mutex
	path string
}

// NewAuditStore constructs a new audit store.
func NewAuditStore(path string) *AuditStore {
	return &AuditStore{path: strings.TrimSpace(path)}
}

// Path returns the current file path.
func (s *AuditStore) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// SetPath updates the file path.
func (s *AuditStore) SetPath(path string) {
	s.mu.Lock()
	s.path = strings.TrimSpace(path)
	s.mu.Unlock()
}

// Record appends one audit event.
func (s *AuditStore) Record(event AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := strings.TrimSpace(s.path)
	if path == "" {
		return errors.New("audit file path is empty")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	event.ID = strings.TrimSpace(event.ID)
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	event.Actor = strings.TrimSpace(event.Actor)
	event.Action = strings.TrimSpace(event.Action)
	event.Object = strings.TrimSpace(event.Object)
	event.Result = strings.TrimSpace(event.Result)
	event.Detail = strings.TrimSpace(event.Detail)

	if err := ensureAuditFile(path); err != nil {
		return err
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open audit file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

// List returns latest audit events.
func (s *AuditStore) List(limit int) ([]AuditEvent, error) {
	items, err := s.readAll()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Timestamp.After(items[j].Timestamp) })
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *AuditStore) readAll() ([]AuditEvent, error) {
	s.mu.Lock()
	path := strings.TrimSpace(s.path)
	s.mu.Unlock()
	if path == "" {
		return nil, errors.New("audit file path is empty")
	}
	if err := ensureAuditFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	items := make([]AuditEvent, 0, 64)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		items = append(items, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan audit file: %w", err)
	}
	return items, nil
}

func ensureAuditFile(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create audit dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		return err
	}
	return nil
}
