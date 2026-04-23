package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"droplock-backend/internal/transfer"
)

const (
	maxUploadBytes   = int64(4 * 1024 * 1024 * 1024) // 4GB
	offlineAfter     = 45 * time.Second
	transferTTL      = 30 * time.Minute
	defaultServeAddr = ":8080"
)

type server struct {
	store *transfer.Store
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
	srv := &server{store: transfer.NewStore(offlineAfter, transferTTL, lockerDataPath)}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/presence/register", srv.handleRegister)
	mux.HandleFunc("/api/presence/heartbeat", srv.handleHeartbeat)
	mux.HandleFunc("/api/presence/offline", srv.handleOffline)
	mux.HandleFunc("/api/devices/online", srv.handleOnlineDevices)
	mux.HandleFunc("/api/transfers", srv.handleTransfers)
	mux.HandleFunc("/api/transfers/inbox", srv.handleInbox)
	mux.HandleFunc("/api/transfers/", srv.handleTransferDownload)
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
	log.Printf("DropLock server listening on %s", addr)
	if err := http.ListenAndServe(addr, h); err != nil {
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
