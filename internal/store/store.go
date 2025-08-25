package store

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- Peer store ----------

type Peer struct {
	UID      string
	Tag      string
	IP       string
	RecvPort int
	LastSeen time.Time
}

type PeerStore struct {
	mu    sync.Mutex
	peers map[string]Peer // key = UID hex
	ttl   time.Duration   // e.g. 5s
}

func NewPeerStore(ttl time.Duration) *PeerStore {
	return &PeerStore{
		peers: make(map[string]Peer),
		ttl:   ttl,
	}
}

// Upsert inserts or updates a peer entry.
func (s *PeerStore) Upsert(p Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers[p.UID] = p
}

// Snapshot returns a copy of all peers, sorted by Tag.
func (s *PeerStore) Snapshot() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Tag < out[j].Tag
	})
	return out
}

// Prune removes peers older than ttl, returns number removed.
func (s *PeerStore) Prune(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	cutoff := now.Add(-s.ttl)
	for uid, p := range s.peers {
		if p.LastSeen.Before(cutoff) {
			delete(s.peers, uid)
			removed++
		}
	}
	return removed
}

// ---------- Tag store ----------

type TagStore struct {
	mu   sync.Mutex
	tags []string // always kept sorted
}

func NewTagStore() *TagStore {
	return &TagStore{tags: []string{}}
}

// sanitizeTag removes all non [A-Za-z0-9] runes.
func sanitizeTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Add inserts a tag if valid (letters/digits) and not duplicate.
func (t *TagStore) Add(raw string) bool {
	tag := sanitizeTag(raw)
	if tag == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, existing := range t.tags {
		if strings.EqualFold(existing, tag) {
			return false
		}
	}
	t.tags = append(t.tags, tag)
	sort.Strings(t.tags)
	return true
}

// Remove deletes a tag (case-insensitive match).
func (t *TagStore) Remove(raw string) bool {
	tag := sanitizeTag(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	orig := len(t.tags)
	out := t.tags[:0]
	for _, v := range t.tags {
		if !strings.EqualFold(v, tag) {
			out = append(out, v)
		}
	}
	t.tags = out
	return len(t.tags) != orig
}

// List returns a copy of all tags.
func (t *TagStore) List() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.tags))
	copy(out, t.tags)
	return out
}
