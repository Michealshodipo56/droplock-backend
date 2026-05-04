package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"droplock-backend/internal/transfer"
)

const (
	maxUploadBytes     = int64(2 * 1024 * 1024 * 1024) // 2GB (P2P transfers)
	maxLockerFileBytes = int64(2 * 1024 * 1024 * 1024) // 2GB (locker file uploads)
	offlineAfter       = 45 * time.Second
	transferTTL        = 30 * time.Minute
	codeTransferTTL    = 5 * time.Minute
	defaultServeAddr   = ":8080"
)

type recoveryTokenEntry struct {
	lockerName string
	expiresAt  time.Time
}

type server struct {
	store          *transfer.Store
	mu             sync.Mutex
	recoveryTokens map[string]recoveryTokenEntry
}

type registerReq struct {
	SessionID  string `json:"sessionId"`
	DeviceName string `json:"deviceName"`
}

type onlineResp struct {
	Devices []transfer.Session `json:"devices"`
}

type transferFileResp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type inboxItemResp struct {
	TransferID string             `json:"transferId"`
	SenderName string             `json:"senderName"`
	SenderID   string             `json:"senderId"`
	TargetName string             `json:"targetName"`
	CreatedAt  time.Time          `json:"createdAt"`
	TotalBytes int64              `json:"totalBytes"`
	FileCount  int                `json:"fileCount"`
	Files      []transferFileResp `json:"files"`
}

type inboxResp struct {
	Transfers []inboxItemResp `json:"transfers"`
}

func main() {
	lockerDataPath := os.Getenv("DROPLOCK_DATA_FILE")
	if strings.TrimSpace(lockerDataPath) == "" {
		lockerDataPath = "droplock-data.json"
	}
	srv := &server{
		store:          transfer.NewStore(offlineAfter, transferTTL, codeTransferTTL, lockerDataPath),
		recoveryTokens: make(map[string]recoveryTokenEntry),
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/presence/register", srv.handleRegister)
	mux.HandleFunc("/api/presence/heartbeat", srv.handleHeartbeat)
	mux.HandleFunc("/api/presence/offline", srv.handleOffline)
	mux.HandleFunc("/api/devices/online", srv.handleOnlineDevices)
	mux.HandleFunc("/api/transfers", srv.handleTransfers)
	mux.HandleFunc("/api/transfers/inbox", srv.handleInbox)
	mux.HandleFunc("/api/transfers/", srv.handleTransferDownload)
	mux.HandleFunc("/api/transfers/encrypted/upload", srv.handleEncryptedUpload)
	mux.HandleFunc("/api/transfers/encrypted/check", srv.handleEncryptedCheck)
	mux.HandleFunc("/api/transfers/encrypted/download", srv.handleEncryptedDownload)
	mux.HandleFunc("/api/locker/check", srv.handleLockerCheck)
	mux.HandleFunc("/api/locker/create", srv.handleLockerCreate)
	mux.HandleFunc("/api/locker/open", srv.handleLockerOpen)
	mux.HandleFunc("/api/locker/notes", srv.handleLockerNotes)
	mux.HandleFunc("/api/locker/notes/delete", srv.handleLockerNoteDelete)
	mux.HandleFunc("/api/locker/files", srv.handleLockerFiles)
	mux.HandleFunc("/api/locker/files/upload", srv.handleLockerFileUpload)
	mux.HandleFunc("/api/locker/files/download", srv.handleLockerFileDownload)
	mux.HandleFunc("/api/locker/files/delete", srv.handleLockerFileDelete)
	mux.HandleFunc("/api/locker/delete", srv.handleLockerDelete)
	mux.HandleFunc("/api/locker/change-password", srv.handleLockerChangePassword)
	mux.HandleFunc("/api/locker/set-email", srv.handleLockerSetEmail)
	mux.HandleFunc("/api/locker/get-email", srv.handleLockerGetEmail)
	mux.HandleFunc("/api/locker/recover/send-code", srv.handleLockerRecoverSendCode)
	mux.HandleFunc("/api/locker/recover/verify-code", srv.handleLockerRecoverVerifyCode)
	mux.HandleFunc("/api/locker/recover/reset-password", srv.handleLockerRecoverResetPassword)

	// Serve frontend from the droplock/ directory
	frontendDir := "../../../droplock"
	if _, err := os.Stat(frontendDir); os.IsNotExist(err) {
		frontendDir = "."
	}
	staticFS := http.FileServer(http.Dir(frontendDir))
	mux.Handle("/", staticFS)

	addr := os.Getenv("PORT")
	if addr == "" {
		addr = defaultServeAddr
	} else if _, err := strconv.Atoi(addr); err == nil {
		addr = ":" + addr
	}

	h := withCORS(withRequestLog(mux))

	srv2 := &http.Server{Addr: addr, Handler: h}
	log.Printf("DropLock server listening on %s", addr)

	// Graceful shutdown on SIGTERM / SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Println("Shutdown signal received, flushing data...")
		srv.store.FlushNow()
		log.Println("Data flushed. Shutting down.")
		os.Exit(0)
	}()

	if err := srv2.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.DeviceName = strings.TrimSpace(req.DeviceName)
	if req.SessionID == "" || req.DeviceName == "" {
		http.Error(w, "sessionId and deviceName are required", http.StatusBadRequest)
		return
	}

	session := s.store.RegisterSession(req.SessionID, req.DeviceName)
	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.SessionID == "" {
		http.Error(w, "sessionId is required", http.StatusBadRequest)
		return
	}
	if !s.store.TouchSession(strings.TrimSpace(req.SessionID)) {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleOffline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.SessionID != "" {
		s.store.RemoveSession(strings.TrimSpace(req.SessionID))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleOnlineDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	exclude := strings.TrimSpace(r.URL.Query().Get("excludeSessionId"))
	devices := s.store.OnlineSessions(exclude)
	writeJSON(w, http.StatusOK, onlineResp{Devices: devices})
}

func (s *server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, fmt.Sprintf("invalid multipart payload: %v", err), http.StatusBadRequest)
		return
	}

	senderSession := strings.TrimSpace(r.FormValue("senderSessionId"))
	senderName := strings.TrimSpace(r.FormValue("senderName"))
	targetSession := strings.TrimSpace(r.FormValue("targetSessionId"))
	targetName := strings.TrimSpace(r.FormValue("targetName"))

	if senderSession == "" || senderName == "" || targetSession == "" || targetName == "" {
		http.Error(w, "sender and target metadata are required", http.StatusBadRequest)
		return
	}
	if !s.store.SessionExists(targetSession) {
		http.Error(w, "target device is offline", http.StatusGone)
		return
	}

	m := r.MultipartForm
	headers := m.File["files"]
	if len(headers) == 0 {
		http.Error(w, "at least one file is required", http.StatusBadRequest)
		return
	}

	stored := make([]transfer.StoredFile, 0, len(headers))
	for _, header := range headers {
		f, err := header.Open()
		if err != nil {
			http.Error(w, "unable to read uploaded file", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			http.Error(w, "unable to read uploaded file", http.StatusBadRequest)
			return
		}
		ctype := header.Header.Get("Content-Type")
		if ctype == "" {
			ctype = mime.TypeByExtension(path.Ext(header.Filename))
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		stored = append(stored, transfer.StoredFile{
			ID:          transferFileID(),
			Name:        header.Filename,
			ContentType: ctype,
			Size:        int64(len(data)),
			Content:     data,
		})
	}

	xfer := s.store.AddTransfer(senderSession, senderName, targetSession, targetName, stored)
	writeJSON(w, http.StatusCreated, map[string]any{
		"transferId": xfer.ID,
		"fileCount":  len(stored),
	})
}

func (s *server) handleInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if sessionID == "" {
		http.Error(w, "sessionId is required", http.StatusBadRequest)
		return
	}

	transfers := s.store.Inbox(sessionID)
	resp := inboxResp{Transfers: make([]inboxItemResp, 0, len(transfers))}
	for _, xfer := range transfers {
		item := inboxItemResp{
			TransferID: xfer.ID,
			SenderName: xfer.SenderName,
			SenderID:   xfer.SenderSession,
			TargetName: xfer.TargetName,
			CreatedAt:  xfer.CreatedAt,
			FileCount:  len(xfer.Files),
			Files:      make([]transferFileResp, 0, len(xfer.Files)),
		}
		for _, f := range xfer.Files {
			item.TotalBytes += f.Size
			item.Files = append(item.Files, transferFileResp{ID: f.ID, Name: f.Name, Size: f.Size})
		}
		resp.Transfers = append(resp.Transfers, item)
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Encrypted Transfer Handlers ---

func (s *server) handleEncryptedUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" || !s.store.SessionExists(sessionID) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if err := r.ParseMultipartForm(500 << 20); err != nil { // 500 MB limit
		http.Error(w, "unable to parse form", http.StatusBadRequest)
		return
	}

	senderSession := r.FormValue("senderSession")
	if senderSession != sessionID {
		http.Error(w, "session mismatch", http.StatusUnauthorized)
		return
	}

	s.store.TouchSession(sessionID)

	formFiles := r.MultipartForm.File["files"]
	if len(formFiles) == 0 {
		http.Error(w, "no files provided", http.StatusBadRequest)
		return
	}
	if len(formFiles) > 10 {
		http.Error(w, "too many files (max 10)", http.StatusBadRequest)
		return
	}

	var storedFiles []transfer.StoredFile
	for _, fh := range formFiles {
		f, err := fh.Open()
		if err != nil {
			http.Error(w, "unable to open file", http.StatusInternalServerError)
			return
		}
		content, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			http.Error(w, "unable to read file", http.StatusInternalServerError)
			return
		}

		contentType := fh.Header.Get("Content-Type")
		if contentType == "" {
			contentType = mime.TypeByExtension(path.Ext(fh.Filename))
			if contentType == "" {
				contentType = "application/octet-stream"
			}
		}
		b := make([]byte, 16)
		rand.Read(b)

		storedFiles = append(storedFiles, transfer.StoredFile{
			ID:          hex.EncodeToString(b),
			Name:        fh.Filename,
			ContentType: contentType,
			Size:        fh.Size,
			Content:     content,
		})
	}

	ct := s.store.AddCodeTransfer(sessionID, storedFiles)

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"code":    ct.Code,
		"files":   ct.FileMeta,
	})
}

func (s *server) handleEncryptedCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	ct, exists := s.store.GetCodeTransfer(code)
	if !exists {
		http.Error(w, "invalid or expired code", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code":      ct.Code,
		"senderId":  ct.SenderID,
		"files":     ct.FileMeta,
		"createdAt": ct.CreatedAt,
		"expiresAt": ct.ExpiresAt,
	})
}

func (s *server) handleEncryptedDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	fileID := strings.TrimSpace(r.URL.Query().Get("fileId"))
	if code == "" || fileID == "" {
		http.Error(w, "missing code or fileId", http.StatusBadRequest)
		return
	}

	f, ok := s.store.DownloadCodeTransferFile(code, fileID)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, f.Name))
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusOK)
	w.Write(f.Content)
}

func (s *server) handleTransferDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/transfers/"), "/")
	if len(parts) != 4 || parts[1] != "files" {
		http.NotFound(w, r)
		return
	}
	transferID := strings.TrimSpace(parts[0])
	fileID := strings.TrimSpace(parts[2])
	if parts[3] != "download" {
		http.NotFound(w, r)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if sessionID == "" {
		http.Error(w, "sessionId is required", http.StatusBadRequest)
		return
	}

	file, ok := s.store.DownloadFile(sessionID, transferID, fileID)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file.Name))
	_, _ = w.Write(file.Content)
}

func transferFileID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (s *server) handleLockerCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	exists := s.store.CheckLocker(req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"exists": exists})
}

func (s *server) handleLockerCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	if req.Name == "" || req.Password == "" {
		http.Error(w, "name and password are required", http.StatusBadRequest)
		return
	}
	if !s.store.CreateLocker(req.Name, req.Password) {
		http.Error(w, "locker already exists", http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"success": true})
}

func (s *server) handleLockerOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	if req.Name == "" || req.Password == "" {
		http.Error(w, "name and password are required", http.StatusBadRequest)
		return
	}
	if !s.store.VerifyLocker(req.Name, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// --- Notes ---

func (s *server) handleLockerNotes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		password := strings.TrimSpace(r.URL.Query().Get("password"))
		if name == "" || password == "" {
			http.Error(w, "name and password are required", http.StatusBadRequest)
			return
		}
		notes, ok := s.store.ListNotes(name, password)
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"notes": notes})

	case http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			NoteID   string `json:"noteId"`
			Title    string `json:"title"`
			Content  string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.Password = strings.TrimSpace(req.Password)
		req.NoteID = strings.TrimSpace(req.NoteID)
		if req.Name == "" || req.Password == "" || req.NoteID == "" {
			http.Error(w, "name, password, and noteId are required", http.StatusBadRequest)
			return
		}
		if req.Title == "" {
			req.Title = "Untitled"
		}
		note, ok := s.store.SaveNote(req.Name, req.Password, req.NoteID, req.Title, req.Content)
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"note": note})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleLockerNoteDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		NoteID   string `json:"noteId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	req.NoteID = strings.TrimSpace(req.NoteID)
	if req.Name == "" || req.Password == "" || req.NoteID == "" {
		http.Error(w, "name, password, and noteId are required", http.StatusBadRequest)
		return
	}
	if !s.store.DeleteNote(req.Name, req.Password, req.NoteID) {
		http.Error(w, "invalid credentials or note not found", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// --- Files ---

func (s *server) handleLockerFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	password := strings.TrimSpace(r.URL.Query().Get("password"))
	noteID := strings.TrimSpace(r.URL.Query().Get("noteId"))
	if name == "" || password == "" {
		http.Error(w, "name and password are required", http.StatusBadRequest)
		return
	}
	if noteID != "" {
		files, ok := s.store.ListLockerFilesByNote(name, password, noteID)
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
	} else {
		files, ok := s.store.ListLockerFiles(name, password)
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": files})
	}
}

func (s *server) handleLockerFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, fmt.Sprintf("invalid multipart payload: %v", err), http.StatusBadRequest)
		return
	}
	lockerName := strings.TrimSpace(r.FormValue("name"))
	password := strings.TrimSpace(r.FormValue("password"))
	noteID := strings.TrimSpace(r.FormValue("noteId"))
	if lockerName == "" || password == "" || noteID == "" {
		http.Error(w, "name, password, and noteId are required", http.StatusBadRequest)
		return
	}

	headers := r.MultipartForm.File["files"]
	if len(headers) == 0 {
		http.Error(w, "at least one file is required", http.StatusBadRequest)
		return
	}

	// Enforce per-file 2GB limit
	for _, header := range headers {
		if header.Size > maxLockerFileBytes {
			http.Error(w, fmt.Sprintf("file %q exceeds the 2 GB limit", header.Filename), http.StatusRequestEntityTooLarge)
			return
		}
	}

	var uploaded []map[string]any
	for _, header := range headers {
		f, err := header.Open()
		if err != nil {
			http.Error(w, "unable to read uploaded file", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			http.Error(w, "unable to read uploaded file", http.StatusBadRequest)
			return
		}
		ctype := header.Header.Get("Content-Type")
		if ctype == "" {
			ctype = mime.TypeByExtension(path.Ext(header.Filename))
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		lf, ok := s.store.UploadLockerFile(lockerName, password, noteID, header.Filename, ctype, int64(len(data)), data)
		if !ok {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		uploaded = append(uploaded, map[string]any{
			"id":   lf.ID,
			"name": lf.Name,
			"size": lf.Size,
		})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"files": uploaded})
}

func (s *server) handleLockerFileDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	password := strings.TrimSpace(r.URL.Query().Get("password"))
	fileID := strings.TrimSpace(r.URL.Query().Get("fileId"))
	if name == "" || password == "" || fileID == "" {
		http.Error(w, "name, password, and fileId are required", http.StatusBadRequest)
		return
	}
	lf, ok := s.store.DownloadLockerFile(name, password, fileID)
	if !ok {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", lf.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(lf.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", lf.Name))
	_, _ = w.Write(lf.Content)
}

func (s *server) handleLockerFileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		FileID   string `json:"fileId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	req.FileID = strings.TrimSpace(req.FileID)
	if req.Name == "" || req.Password == "" || req.FileID == "" {
		http.Error(w, "name, password, and fileId are required", http.StatusBadRequest)
		return
	}
	if !s.store.VerifyLocker(req.Name, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if !s.store.DeleteLockerFile(req.Name, req.Password, req.FileID) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *server) handleLockerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	if req.Name == "" || req.Password == "" {
		http.Error(w, "name and password are required", http.StatusBadRequest)
		return
	}
	if !s.store.DeleteLocker(req.Name, req.Password) {
		http.Error(w, "invalid credentials or locker not found", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *server) handleLockerChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name        string `json:"name"`
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.OldPassword = strings.TrimSpace(req.OldPassword)
	req.NewPassword = strings.TrimSpace(req.NewPassword)
	if req.Name == "" || req.OldPassword == "" || req.NewPassword == "" {
		http.Error(w, "name, oldPassword and newPassword are required", http.StatusBadRequest)
		return
	}
	if !s.store.ChangeLockerPassword(req.Name, req.OldPassword, req.NewPassword) {
		http.Error(w, "incorrect old password or locker not found", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": true})
}

// --- Recovery / Secure Account Handlers ---

func (s *server) handleLockerSetEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	req.Email = strings.TrimSpace(req.Email)
	if req.Name == "" || req.Password == "" || req.Email == "" {
		http.Error(w, "name, password and email are required", http.StatusBadRequest)
		return
	}
	if !s.store.SetLockerEmail(req.Name, req.Password, req.Email) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (s *server) handleLockerGetEmail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Password = strings.TrimSpace(req.Password)
	if req.Name == "" || req.Password == "" {
		http.Error(w, "name and password are required", http.StatusBadRequest)
		return
	}
	if !s.store.VerifyLocker(req.Name, req.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	email, _ := s.store.GetLockerEmail(req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"email": email})
}

func (s *server) handleLockerRecoverSendCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.TrimSpace(req.Email)
	if req.Name == "" || req.Email == "" {
		http.Error(w, "name and email are required", http.StatusBadRequest)
		return
	}

	// Check if locker exists
	if !s.store.CheckLocker(req.Name) {
		http.Error(w, "locker not found", http.StatusNotFound)
		return
	}

	// Check if email matches
	storedEmail, hasEmail := s.store.GetLockerEmail(req.Name)
	if !hasEmail || storedEmail != req.Email {
		http.Error(w, "email does not match the recovery email on file", http.StatusUnauthorized)
		return
	}

	// Generate OTP
	code, err := s.store.CreateRecoveryOTP(req.Name)
	if err != nil {
		http.Error(w, "failed to generate recovery code", http.StatusInternalServerError)
		return
	}

	// Attempt SMTP delivery
	smtpEmail := strings.TrimSpace(os.Getenv("SMTP_EMAIL"))
	smtpPassword := strings.TrimSpace(os.Getenv("SMTP_PASSWORD"))
	smtpHost := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	smtpPort := strings.TrimSpace(os.Getenv("SMTP_PORT"))
	if smtpHost == "" {
		smtpHost = "smtp.gmail.com"
	}
	if smtpPort == "" {
		smtpPort = "587"
	}

	if smtpEmail != "" && smtpPassword != "" {
		go func() {
			if err := sendRecoveryEmail(req.Email, code, smtpHost, smtpPort, smtpEmail, smtpPassword); err != nil {
				log.Printf("[SMTP] Failed to send recovery email to %s: %v", req.Email, err)
			} else {
				log.Printf("[SMTP] Recovery code sent to %s", req.Email)
			}
		}()
	} else {
		log.Printf("[DEV] Recovery OTP for locker %q → %s (SMTP not configured)", req.Name, code)
	}

	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

func (s *server) handleLockerRecoverVerifyCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Code = strings.TrimSpace(req.Code)
	if req.Name == "" || req.Code == "" {
		http.Error(w, "name and code are required", http.StatusBadRequest)
		return
	}

	if !s.store.VerifyRecoveryOTP(req.Name, req.Code) {
		http.Error(w, "invalid or expired code", http.StatusUnauthorized)
		return
	}

	// Generate a one-time recovery token
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(tokenBytes)

	s.mu.Lock()
	s.recoveryTokens[token] = recoveryTokenEntry{
		lockerName: req.Name,
		expiresAt:  time.Now().UTC().Add(10 * time.Minute),
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "token": token})
}

func (s *server) handleLockerRecoverResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token       string `json:"token"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	req.NewPassword = strings.TrimSpace(req.NewPassword)
	if req.Token == "" || req.NewPassword == "" {
		http.Error(w, "token and newPassword are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	entry, ok := s.recoveryTokens[req.Token]
	if !ok || time.Now().UTC().After(entry.expiresAt) {
		delete(s.recoveryTokens, req.Token)
		s.mu.Unlock()
		http.Error(w, "invalid or expired recovery token", http.StatusUnauthorized)
		return
	}
	lockerName := entry.lockerName
	delete(s.recoveryTokens, req.Token)
	s.mu.Unlock()

	if !s.store.ResetLockerPassword(lockerName, req.NewPassword) {
		http.Error(w, "locker not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func sendRecoveryEmail(toEmail, code, smtpHost, smtpPort, smtpEmail, smtpPassword string) error {
	auth := smtp.PlainAuth("", smtpEmail, smtpPassword, smtpHost)
	subject := "DropLock Recovery Code"
	body := fmt.Sprintf(
		"Your DropLock recovery verification code is:\n\n    %s\n\nThis code expires in 10 minutes.\nIf you did not request this, please ignore this email.",
		code,
	)
	msg := fmt.Sprintf(
		"From: DropLock <%s>\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		smtpEmail, toEmail, subject, body,
	)
	addr := fmt.Sprintf("%s:%s", smtpHost, smtpPort)
	return smtp.SendMail(addr, auth, smtpEmail, []string{toEmail}, []byte(msg))
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/api/presence/heartbeat") {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Truncate(time.Millisecond))
		}
	})
}
