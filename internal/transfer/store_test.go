package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockerDataPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	persistPath := filepath.Join(t.TempDir(), "lockers.json")
	const (
		lockerName    = "alpha"
		passwordHash  = "hash-1"
		recoveryEmail = "alpha@example.com"
		noteID        = "note-1"
		noteTitle     = "First note"
		noteContent   = "persist me"
		fileName      = "memo.txt"
		fileType      = "text/plain"
		fileContent   = "file payload"
	)

	store := NewStore(time.Minute, time.Minute, 5*time.Minute, persistPath)
	if ok := store.CreateLocker(lockerName, passwordHash); !ok {
		t.Fatal("expected locker creation to succeed")
	}
	if ok := store.SetLockerEmail(lockerName, passwordHash, recoveryEmail); !ok {
		t.Fatal("expected locker recovery email to be saved")
	}
	if _, ok := store.SaveNote(lockerName, passwordHash, noteID, noteTitle, noteContent); !ok {
		t.Fatal("expected note save to succeed")
	}
	uploaded, ok := store.UploadLockerFile(lockerName, passwordHash, noteID, fileName, fileType, int64(len(fileContent)), []byte(fileContent))
	if !ok {
		t.Fatal("expected file upload to succeed")
	}

	data, err := os.ReadFile(persistPath)
	if err != nil {
		t.Fatalf("read persisted locker data: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected persisted locker data to be non-empty")
	}

	restarted := NewStore(time.Minute, time.Minute, 5*time.Minute, persistPath)
	if ok := restarted.VerifyLocker(lockerName, passwordHash); !ok {
		t.Fatal("expected password hash to persist across restart")
	}
	if email, ok := restarted.GetLockerEmail(lockerName); !ok || email != recoveryEmail {
		t.Fatalf("expected recovery email %q after restart, got %q, ok=%v", recoveryEmail, email, ok)
	}

	notes, ok := restarted.ListNotes(lockerName, passwordHash)
	if !ok {
		t.Fatal("expected notes lookup to succeed after restart")
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1 note after restart, got %d", len(notes))
	}
	if notes[0].ID != noteID || notes[0].Title != noteTitle || notes[0].Content != noteContent {
		t.Fatalf("unexpected note after restart: %+v", notes[0])
	}

	files, ok := restarted.ListLockerFiles(lockerName, passwordHash)
	if !ok {
		t.Fatal("expected file listing to succeed after restart")
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file after restart, got %d", len(files))
	}
	if files[0].ID != uploaded.ID || files[0].NoteID != noteID || files[0].Name != fileName || files[0].ContentType != fileType || files[0].Size != int64(len(fileContent)) {
		t.Fatalf("unexpected file metadata after restart: %+v", files[0])
	}

	downloaded, ok := restarted.DownloadLockerFile(lockerName, passwordHash, uploaded.ID)
	if !ok {
		t.Fatal("expected file download to succeed after restart")
	}
	if string(downloaded.Content) != fileContent {
		t.Fatalf("expected file content %q after restart, got %q", fileContent, string(downloaded.Content))
	}
}

func TestCodeTransferExpiresAfterConfiguredTTL(t *testing.T) {
	t.Parallel()

	const codeTTL = 5 * time.Minute
	store := NewStore(time.Minute, time.Minute, codeTTL, "")
	before := time.Now().UTC()

	ct := store.AddCodeTransfer("sender-1", []StoredFile{{
		ID:          "file-1",
		Name:        "memo.txt",
		ContentType: "text/plain",
		Size:        4,
		Content:     []byte("test"),
	}})

	ttl := ct.ExpiresAt.Sub(ct.CreatedAt)
	if ttl != codeTTL {
		t.Fatalf("expected code transfer ttl %s, got %s", codeTTL, ttl)
	}
	if ct.CreatedAt.Before(before) {
		t.Fatalf("expected createdAt to be >= %s, got %s", before, ct.CreatedAt)
	}
}
