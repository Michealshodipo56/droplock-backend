package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
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

type Note struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type LockerFile struct {
	ID          string    `json:"id"`
	NoteID      string    `json:"noteId"`
	Name        string    `json:"name"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	Content     []byte    `json:"-"`
	UploadedAt  time.Time `json:"uploadedAt"`
}

type Locker struct {
	Name         string                 `json:"name"`
	PasswordHash string                 `json:"-"`
	CreatedAt    time.Time              `json:"createdAt"`
	Notes        map[string]*Note       `json:"-"`
	Files        map[string]*LockerFile `json:"-"`
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
	lockers        map[string]Locker
	offlineAfter   time.Duration
	transferTTL    time.Duration
	persistPath    string
}

func NewStore(offlineAfter, transferTTL time.Duration, persistPath string) *Store {
	s := &Store{
		sessions:       make(map[string]Session),
		transfers:      make(map[string]Transfer),
		transfersByTgt: make(map[string][]string),
		lockers:        make(map[string]Locker),
		offlineAfter:   offlineAfter,
		transferTTL:    transferTTL,
		persistPath:    persistPath,
	}
	s.loadLockerData()
	go s.cleanupLoop()
	return s
}

type persistedLocker struct {
	Name         string                 `json:"name"`
	PasswordHash string                 `json:"passwordHash"`
	CreatedAt    time.Time              `json:"createdAt"`
	Notes        map[string]*Note       `json:"notes"`
	Files        map[string]*LockerFile `json:"files"`
}

type lockerDataFile struct {
	Lockers map[string]persistedLocker `json:"lockers"`
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

func (s *Store) CheckLocker(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.lockers[name]
	return ok
}

func (s *Store) CreateLocker(name, passwordHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lockers[name]; ok {
		return false
	}
	s.lockers[name] = Locker{
		Name:         name,
		PasswordHash: passwordHash,
		CreatedAt:    time.Now().UTC(),
		Notes:        make(map[string]*Note),
		Files:        make(map[string]*LockerFile),
	}
	s.persistLockerDataLocked()
	return true
}

func (s *Store) VerifyLocker(name, passwordHash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locker, ok := s.lockers[name]
	if !ok {
		return false
	}
	return locker.PasswordHash == passwordHash
}

func (s *Store) DeleteLocker(name, passwordHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[name]
	if !ok || locker.PasswordHash != passwordHash {
		return false
	}
	delete(s.lockers, name)
	s.persistLockerDataLocked()
	return true
}

func (s *Store) ChangeLockerPassword(name, oldPasswordHash, newPasswordHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[name]
	if !ok || locker.PasswordHash != oldPasswordHash {
		return false
	}
	locker.PasswordHash = newPasswordHash
	s.lockers[name] = locker
	s.persistLockerDataLocked()
	return true
}

// --- Note CRUD ---

func (s *Store) SaveNote(lockerName, password, noteID, title, content string) (*Note, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	now := time.Now().UTC()
	if locker.Notes == nil {
		locker.Notes = make(map[string]*Note)
	}
	existing, exists := locker.Notes[noteID]
	if exists {
		existing.Title = title
		existing.Content = content
		existing.UpdatedAt = now
	} else {
		existing = &Note{
			ID:        noteID,
			Title:     title,
			Content:   content,
			CreatedAt: now,
			UpdatedAt: now,
		}
		locker.Notes[noteID] = existing
	}
	s.lockers[lockerName] = locker
	s.persistLockerDataLocked()
	return existing, true
}

func (s *Store) DeleteNote(lockerName, password, noteID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return false
	}
	if locker.Notes == nil {
		return false
	}
	delete(locker.Notes, noteID)
	// Also remove files attached to this note
	if locker.Files != nil {
		for fid, f := range locker.Files {
			if f.NoteID == noteID {
				delete(locker.Files, fid)
			}
		}
	}
	s.lockers[lockerName] = locker
	s.persistLockerDataLocked()
	return true
}

func (s *Store) ListNotes(lockerName, password string) ([]Note, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	out := make([]Note, 0, len(locker.Notes))
	for _, n := range locker.Notes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, true
}

// --- File CRUD ---

func (s *Store) UploadLockerFile(lockerName, password, noteID, fileName, contentType string, size int64, content []byte) (*LockerFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	if locker.Files == nil {
		locker.Files = make(map[string]*LockerFile)
	}
	f := &LockerFile{
		ID:          randomID(12),
		NoteID:      noteID,
		Name:        fileName,
		ContentType: contentType,
		Size:        size,
		Content:     content,
		UploadedAt:  time.Now().UTC(),
	}
	locker.Files[f.ID] = f
	s.lockers[lockerName] = locker
	s.persistLockerDataLocked()
	return f, true
}

func (s *Store) ListLockerFiles(lockerName, password string) ([]LockerFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	out := make([]LockerFile, 0, len(locker.Files))
	for _, f := range locker.Files {
		out = append(out, LockerFile{
			ID:          f.ID,
			NoteID:      f.NoteID,
			Name:        f.Name,
			ContentType: f.ContentType,
			Size:        f.Size,
			UploadedAt:  f.UploadedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UploadedAt.After(out[j].UploadedAt)
	})
	return out, true
}

func (s *Store) ListLockerFilesByNote(lockerName, password, noteID string) ([]LockerFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	var out []LockerFile
	for _, f := range locker.Files {
		if f.NoteID == noteID {
			out = append(out, LockerFile{
				ID:          f.ID,
				NoteID:      f.NoteID,
				Name:        f.Name,
				ContentType: f.ContentType,
				Size:        f.Size,
				UploadedAt:  f.UploadedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UploadedAt.After(out[j].UploadedAt)
	})
	return out, true
}

func (s *Store) DownloadLockerFile(lockerName, password, fileID string) (*LockerFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return nil, false
	}
	f, ok := locker.Files[fileID]
	if !ok {
		return nil, false
	}
	return f, true
}

func (s *Store) DeleteLockerFile(lockerName, password, fileID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	locker, ok := s.lockers[lockerName]
	if !ok || locker.PasswordHash != password {
		return false
	}
	if locker.Files == nil {
		return false
	}
	if _, exists := locker.Files[fileID]; !exists {
		return false
	}
	delete(locker.Files, fileID)
	s.lockers[lockerName] = locker
	s.persistLockerDataLocked()
	return true
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

func (s *Store) loadLockerData() {
	if s.persistPath == "" {
		return
	}
	data, err := os.ReadFile(s.persistPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("unable to read locker data file %q: %v", s.persistPath, err)
		}
		return
	}
	var state lockerDataFile
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("unable to parse locker data file %q: %v", s.persistPath, err)
		return
	}
	if state.Lockers == nil {
		state.Lockers = make(map[string]persistedLocker)
	}
	s.lockers = make(map[string]Locker, len(state.Lockers))
	for key, item := range state.Lockers {
		notes := item.Notes
		files := item.Files
		if notes == nil {
			notes = make(map[string]*Note)
		}
		if files == nil {
			files = make(map[string]*LockerFile)
		}
		s.lockers[key] = Locker{
			Name:         item.Name,
			PasswordHash: item.PasswordHash,
			CreatedAt:    item.CreatedAt,
			Notes:        notes,
			Files:        files,
		}
	}
}

func (s *Store) persistLockerDataLocked() {
	if s.persistPath == "" {
		return
	}
	lockers := make(map[string]persistedLocker, len(s.lockers))
	for key, item := range s.lockers {
		lockers[key] = persistedLocker{
			Name:         item.Name,
			PasswordHash: item.PasswordHash,
			CreatedAt:    item.CreatedAt,
			Notes:        item.Notes,
			Files:        item.Files,
		}
	}
	state := lockerDataFile{Lockers: lockers}
	payload, err := json.Marshal(state)
	if err != nil {
		log.Printf("unable to marshal locker data: %v", err)
		return
	}
	tmpPath := s.persistPath + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		log.Printf("unable to write locker data temp file %q: %v", tmpPath, err)
		return
	}
	if err := os.Rename(tmpPath, s.persistPath); err != nil {
		log.Printf("unable to swap locker data file %q: %v", s.persistPath, err)
	}
}
