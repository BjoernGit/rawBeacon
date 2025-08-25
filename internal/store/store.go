package store

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Peer beschreibt einen bekannten Beacon (Sidecar).
type Peer struct {
	UID      string
	Name     string // früher: Tag (Primär-Anzeigename)
	IP       string
	RecvPort int
	LastSeen time.Time
}

// PeerStore verwaltet Peers thread-safe.
type PeerStore struct {
	mu    sync.Mutex
	peers map[string]Peer // key: UID (hex)
}

func NewPeerStore() *PeerStore {
	return &PeerStore{peers: make(map[string]Peer)}
}

// Upsert fügt einen Peer ein oder aktualisiert ihn.
func (s *PeerStore) Upsert(p Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, ok := s.peers[p.UID]
	if ok {
		if strings.TrimSpace(p.Name) != "" {
			prev.Name = p.Name
		}
		if strings.TrimSpace(p.IP) != "" {
			prev.IP = p.IP
		}
		if p.RecvPort != 0 {
			prev.RecvPort = p.RecvPort
		}
		if !p.LastSeen.IsZero() {
			prev.LastSeen = p.LastSeen
		}
		s.peers[p.UID] = prev
		return
	}
	s.peers[p.UID] = p
}

// SetRecvPort setzt (oder aktualisiert) den bekannten Receive-Port eines Peers.
func (s *PeerStore) SetRecvPort(uid string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[uid]
	p.RecvPort = port
	s.peers[uid] = p
}

// Snapshot liefert eine sortierte Kopie (Name, dann UID).
func (s *PeerStore) Snapshot() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// Prune entfernt Peers, die länger als d nicht gesehen wurden.
func (s *PeerStore) Prune(d time.Duration) int {
	cutoff := time.Now().Add(-d)
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for uid, p := range s.peers {
		if p.LastSeen.Before(cutoff) {
			delete(s.peers, uid)
			removed++
		}
	}
	return removed
}

// ---- Lokale Tags dieses Beacons ----

type TagStore struct {
	mu   sync.Mutex
	tags []string // immer sortiert
}

func NewTagStore() *TagStore {
	return &TagStore{}
}

func sanitizeTag(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b = append(b, r)
		}
	}
	return string(b)
}

func (s *TagStore) Add(t string) bool {
	t = sanitizeTag(strings.TrimSpace(t))
	if t == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.tags {
		if strings.EqualFold(ex, t) {
			return false
		}
	}
	s.tags = append(s.tags, t)
	sort.Slice(s.tags, func(i, j int) bool { return strings.ToLower(s.tags[i]) < strings.ToLower(s.tags[j]) })
	return true
}

func (s *TagStore) Remove(t string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := len(s.tags)
	dst := s.tags[:0]
	for _, v := range s.tags {
		if !strings.EqualFold(v, t) {
			dst = append(dst, v)
		}
	}
	s.tags = dst
	return len(s.tags) != old
}

func (s *TagStore) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tags))
	copy(out, s.tags)
	return out
}
