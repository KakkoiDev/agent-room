package room

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Room is one conversation.
type Room struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Message is one line of a room's transcript. Agent replies, progress notes and
// request state changes are messages too, so the transcript is the whole state.
type Message struct {
	Requests         []Request `json:"assignments,omitempty"`
	AssignmentID     string    `json:"assignment_id,omitempty"`
	AssignmentStatus string    `json:"assignment_status,omitempty"`
	OperationID      string    `json:"operation_id,omitempty"`
	ClientID         string    `json:"client_id,omitempty"`
	Mentions         []string  `json:"mentions,omitempty"`
	Author           string    `json:"author,omitempty"`
	Kind             string    `json:"kind,omitempty"`
	Text             string    `json:"text"`
	Attachment       string    `json:"attachment,omitempty"`
	At               time.Time `json:"at"`
}

// Request is work for one agent, created by an @mention of that agent.
// The JSON names match the helper's protocol ("assignment").
type Request struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Prompt     string `json:"prompt,omitempty"`
	Attachment string `json:"attachment,omitempty"`
	Status     string `json:"status,omitempty"`
	Source     int    `json:"source"`
	Task       int    `json:"task,omitempty"` // legacy shared tasks only
}

// Member is a person or agent session in a room. Sessions are keyed by a hash
// of their bearer token.
type Member struct {
	Waiting    int          `json:"-"` // agents in wait
	watching   int          // people in wait
	LastPoll   time.Time    `json:"-"`
	Connection string       `json:"connection,omitempty"`
	Listening  bool         `json:"listening"`
	ReadItems  map[int]bool `json:"read_items,omitempty"`
	Hue        int          `json:"hue"`
	Name       string       `json:"username"`
	Role       string       `json:"role"`
	Seen       time.Time    `json:"seen"`
	Read       int          `json:"read"`
	Online     bool         `json:"online"`
	Stopped    bool         `json:"stopped,omitempty"`
}

// Members maps room ID to session key to member.
type Members map[string]map[string]*Member

// Store keeps rooms, transcripts and sessions. The server serializes calls with
// its own lock, so implementations need not be safe for concurrent use.
type Store interface {
	Rooms() ([]Room, error)
	CreateRoom(title string) (Room, error)
	Messages(room string) ([]Message, error)
	Append(room string, m Message) error
	LoadMembers() (Members, error)
	SaveMembers(Members) error
}

// FileStore keeps everything in one directory: one JSONL transcript per room,
// a .room file per room title, and members.json for sessions.
type FileStore struct{ root *os.Root }

// NewFileStore opens (and creates) dir for storage.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FileStore{root: root}, nil
}

// Close releases the directory.
func (s *FileStore) Close() error { return s.root.Close() }

func validID(id string) bool {
	return id != "" && len(id) <= 64 && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

func (s *FileStore) Rooms() ([]Room, error) {
	rooms := []Room{{"general", "General"}}
	items, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, e := range items {
		id, ok := strings.CutSuffix(e.Name(), ".room")
		if !ok || !e.Type().IsRegular() || !validID(id) || id == "general" {
			continue
		}
		if b, err := s.root.ReadFile(e.Name()); err == nil {
			rooms = append(rooms, Room{id, string(b)})
		}
	}
	sort.SliceStable(rooms[1:], func(i, j int) bool { return rooms[i+1].Title < rooms[j+1].Title })
	return rooms, nil
}

func (s *FileStore) CreateRoom(title string) (Room, error) {
	r := Room{ID: rand.Text(), Title: title}
	return r, s.root.WriteFile(r.ID+".room", []byte(title), 0o600)
}

func (s *FileStore) Messages(room string) ([]Message, error) {
	if !validID(room) {
		return nil, fs.ErrNotExist
	}
	f, err := s.root.Open(room + ".jsonl")
	if errors.Is(err, fs.ErrNotExist) {
		return []Message{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := []Message{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 256<<10)
	for scan.Scan() {
		var m Message
		if json.Unmarshal(scan.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out, scan.Err()
}

func (s *FileStore) Append(room string, m Message) error {
	if !validID(room) {
		return fs.ErrNotExist
	}
	f, err := s.root.OpenFile(room+".jsonl", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(m)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

const membersFile = "members.json"

func (s *FileStore) LoadMembers() (Members, error) {
	m := Members{}
	data, err := s.root.ReadFile(membersFile)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Members{}, err
	}
	return m, nil
}

func (s *FileStore) SaveMembers(m Members) error {
	tmp := membersFile + "-" + rand.Text()
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	err = json.NewEncoder(f).Encode(m)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return s.root.Rename(tmp, membersFile)
}

// MemoryStore keeps everything in memory; useful for tests and demos.
type MemoryStore struct {
	mu       sync.Mutex
	rooms    []Room
	messages map[string][]Message
	members  []byte
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rooms: []Room{{"general", "General"}}, messages: map[string][]Message{}}
}

func (s *MemoryStore) Rooms() ([]Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Room{}, s.rooms...), nil
}

func (s *MemoryStore) CreateRoom(title string) (Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Room{ID: rand.Text(), Title: title}
	s.rooms = append(s.rooms, r)
	return r, nil
}

func (s *MemoryStore) Messages(room string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message{}, s.messages[room]...), nil
}

func (s *MemoryStore) Append(room string, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[room] = append(s.messages[room], m)
	return nil
}

func (s *MemoryStore) LoadMembers() (Members, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Members{}
	if s.members != nil {
		return m, json.Unmarshal(s.members, &m)
	}
	return m, nil
}

func (s *MemoryStore) SaveMembers(m Members) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members = b
	return nil
}
