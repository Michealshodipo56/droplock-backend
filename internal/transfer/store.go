package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

type Session struct {
	SessionID   string    `json:"sessionId"`
	DeviceName  string    `json:"deviceName"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	ConnectedAt time.Time `json:"connectedAt"`
}

type StoredFile struct {
	ID          string
	Name        string
	ContentType string
	Size        int64
	Content     []byte
}

type Transfer struct {
	ID            string
	SenderSession string
	SenderName    string
	TargetSession string
	TargetName    string
	Files         []StoredFile
	CreatedAt     time.Time
}

type Store struct {
	mu             sync.RWMutex
	sessions       map[string]Session
	transfers      map[string]Transfer
	transfersByTgt map[string][]string
	offlineAfter   time.Duration
	transferTTL    time.Duration
}

func NewStore(offlineAfter, transferTTL time.Duration) *Store {
	s := &Store{
		sessions:       make(map[string]Session),
		transfers:      make(map[string]Transfer),
		transfersByTgt: make(map[string][]string),
		offlineAfter:   offlineAfter,
		transferTTL:    transferTTL,
	}
	go s.cleanupLoop()
	return s
}

func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.Cleanup()
	}
}

func (s *Store) Cleanup() {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, session := range s.sessions {
		if now.Sub(session.LastSeenAt) > s.offlineAfter {
			delete(s.sessions, id)
		}
	}

	for id, transfer := range s.transfers {
		if now.Sub(transfer.CreatedAt) > s.transferTTL {
			delete(s.transfers, id)
			ids := s.transfersByTgt[transfer.TargetSession]
			s.transfersByTgt[transfer.TargetSession] = removeID(ids, id)
		}
	}
}

func (s *Store) RegisterSession(sessionID, deviceName string) Session {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.sessions[sessionID]
	if !ok {
		existing = Session{SessionID: sessionID, ConnectedAt: now}
	}
	existing.DeviceName = deviceName
	existing.LastSeenAt = now
	if existing.ConnectedAt.IsZero() {
		existing.ConnectedAt = now
	}
	s.sessions[sessionID] = existing
	return existing
}

func (s *Store) TouchSession(sessionID string) bool {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return false
	}
	session.LastSeenAt = now
	s.sessions[sessionID] = session
	return true
}

func (s *Store) RemoveSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *Store) OnlineSessions(excludeSessionID string) []Session {
	now := time.Now().UTC()
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Session, 0, len(s.sessions))
	for id, session := range s.sessions {
		if id == excludeSessionID {
			continue
		}
		if now.Sub(session.LastSeenAt) > s.offlineAfter {
			continue
		}
		out = append(out, session)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].DeviceName == out[j].DeviceName {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].DeviceName < out[j].DeviceName
	})
	return out
}

func (s *Store) SessionExists(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.sessions[sessionID]
	return ok
}

func (s *Store) AddTransfer(senderSession, senderName, targetSession, targetName string, files []StoredFile) Transfer {
	now := time.Now().UTC()
	transfer := Transfer{
		ID:            randomID(12),
		SenderSession: senderSession,
		SenderName:    senderName,
		TargetSession: targetSession,
		TargetName:    targetName,
		Files:         files,
		CreatedAt:     now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfers[transfer.ID] = transfer
	s.transfersByTgt[targetSession] = append(s.transfersByTgt[targetSession], transfer.ID)
	return transfer
}

func (s *Store) Inbox(sessionID string) []Transfer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.transfersByTgt[sessionID]
	out := make([]Transfer, 0, len(ids))
	for _, id := range ids {
		if transfer, ok := s.transfers[id]; ok {
			out = append(out, transfer)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *Store) DownloadFile(sessionID, transferID, fileID string) (StoredFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	transfer, ok := s.transfers[transferID]
	if !ok || transfer.TargetSession != sessionID {
		return StoredFile{}, false
	}
	for _, file := range transfer.Files {
		if file.ID == fileID {
			return file, true
		}
	}
	return StoredFile{}, false
}

func randomID(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().UTC().Format("20060102150405")
	}
	return hex.EncodeToString(buf)
}

func removeID(ids []string, value string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := ids[:0]
	for _, id := range ids {
		if id != value {
			out = append(out, id)
		}
	}
	return out
}
