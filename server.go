package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/*
var webFS embed.FS

type APIServer struct {
	switcher      *Switcher
	outputManager *OutputManager
	sysStats      *SysStatsMonitor
	preview       *PreviewManager
	audioMeter    *AudioMeter
	recorder      *Recorder
	bboxManager   *BboxManager
	dataDir       string
	port          int
	webUser       string
	webPass       string

	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]bool
	clientsMu sync.Mutex
}

func NewAPIServer(switcher *Switcher, outputManager *OutputManager, sysStats *SysStatsMonitor, preview *PreviewManager, audioMeter *AudioMeter, recorder *Recorder, bboxManager *BboxManager, dataDir string, port int, webUser string, webPass string) *APIServer {
	return &APIServer{
		switcher:      switcher,
		outputManager: outputManager,
		sysStats:      sysStats,
		preview:       preview,
		audioMeter:    audioMeter,
		recorder:      recorder,
		bboxManager:   bboxManager,
		dataDir:       dataDir,
		port:          port,
		webUser:       webUser,
		webPass:       webPass,
		upgrader:      websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		clients:       make(map[*websocket.Conn]bool),
	}
}

func (s *APIServer) basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.webUser == "" && s.webPass == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.webUser || pass != s.webPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type StatusResponse struct {
	System    SystemStats     `json:"system"`
	Switcher  SwitcherStats   `json:"switcher"`
	Outputs   []OutputStats   `json:"outputs"`
	Audio     AudioLevels     `json:"audio"`
	Recording RecordingStatus `json:"recording"`
}

func (s *APIServer) Run() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/outputs", s.handleOutputs)
	mux.HandleFunc("/api/outputs/", s.handleOutputAction)
	mux.HandleFunc("/api/upload/fallback", s.handleUploadFallback)
	mux.HandleFunc("/api/upload/watermark", s.handleUploadWatermark)
	mux.HandleFunc("/api/preview/frame", s.handlePreviewFrame)
	mux.HandleFunc("/api/preview/settings", s.handlePreviewSettings)
	mux.HandleFunc("/api/actions/", s.handleQuickAction)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/recording", s.handleRecording)
	mux.HandleFunc("/api/recordings", s.handleRecordings)
	mux.HandleFunc("/api/recordings/", s.handleRecordingAction)
	
	// Bbox routes
	mux.HandleFunc("/api/bbox/status", s.handleBboxStatus)
	mux.HandleFunc("/api/bbox/action", s.handleBboxAction)
	mux.HandleFunc("/api/bbox/logs", s.handleBboxLogs)
	mux.HandleFunc("/api/bbox/config", s.handleBboxConfig)
	mux.HandleFunc("/api/bbox/compose", s.handleBboxCompose)

	mux.HandleFunc("/ws", s.handleWebSocket)

	webContent, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("embed fs: %w", err)
	}
	fileServer := http.FileServer(http.FS(webContent))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		fileServer.ServeHTTP(w, r)
	}))

	go s.broadcastLoop()

	addr := fmt.Sprintf(":%d", s.port)
	if s.webUser != "" {
		log.Printf("[server] Web UI at http://0.0.0.0%s (Protected with Basic Auth)", addr)
	} else {
		log.Printf("[server] Web UI at http://0.0.0.0%s (WARNING: Open access, no auth!)", addr)
	}
	return http.ListenAndServe(addr, s.basicAuthMiddleware(mux))
}

// ─── Status ───

func (s *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{
		System:    s.sysStats.GetStats(),
		Switcher:  s.switcher.GetStats(),
		Outputs:   s.outputManager.GetStats(),
		Audio:     s.audioMeter.GetLevels(),
		Recording: s.recorder.GetStatus(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── Config ───

func (s *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.switcher.GetConfig())
	case http.MethodPut:
		var cfg SwitcherConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		s.switcher.UpdateConfig(cfg)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Outputs ───

func (s *APIServer) handleOutputs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.outputManager.GetOutputs())
	case http.MethodPost:
		var config OutputConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if config.ID == "" {
			config.ID = fmt.Sprintf("out_%d", time.Now().UnixMilli())
		}
		if config.Name == "" {
			config.Name = config.ID
		}
		if config.Codec == "" {
			config.Codec = CodecH265Passthrough
		}
		if err := s.outputManager.AddOutput(config); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(config)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleOutputAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path[len("/api/outputs/"):]
	id := path
	action := ""
	for _, a := range []string{"/start", "/stop", "/logs"} {
		if len(path) > len(a) && path[len(path)-len(a):] == a {
			id = path[:len(path)-len(a)]
			action = a[1:]
			break
		}
	}
	switch {
	case action == "start" && r.Method == http.MethodPost:
		if err := s.outputManager.StartOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "started"})
	case action == "stop" && r.Method == http.MethodPost:
		if err := s.outputManager.StopOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "stopped"})
	case action == "logs" && r.Method == http.MethodGet:
		logs, err := s.outputManager.GetLogs(id)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"logs": logs})
	case action == "" && r.Method == http.MethodPut:
		var config OutputConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := s.outputManager.UpdateOutput(id, config); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	case action == "" && r.Method == http.MethodDelete:
		if err := s.outputManager.RemoveOutput(id); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Quick Actions ───

func (s *APIServer) handleQuickAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	action := r.URL.Path[len("/api/actions/"):]
	switch action {
	case "restart-srt":
		s.switcher.RestartSRT()
		json.NewEncoder(w).Encode(map[string]string{"status": "SRT restart requested"})
	case "restart-fallback":
		s.switcher.RestartFallback()
		json.NewEncoder(w).Encode(map[string]string{"status": "Fallback restart requested"})
	case "restart-outputs":
		s.outputManager.RestartAll()
		json.NewEncoder(w).Encode(map[string]string{"status": "All outputs restarted"})
	case "restart-all":
		s.switcher.RestartSRT()
		s.switcher.RestartFallback()
		s.outputManager.RestartAll()
		json.NewEncoder(w).Encode(map[string]string{"status": "Full restart requested"})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown action"})
	}
}

// ─── History ───

func (s *APIServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"events": s.switcher.GetSwitchHistory()})
}

// ─── Recording ───

func (s *APIServer) handleRecording(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.recorder.GetStatus())
	case http.MethodPost:
		action := r.URL.Query().Get("action")
		switch action {
		case "start":
			if err := s.recorder.Start(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "recording started"})
		case "stop":
			if err := s.recorder.Stop(); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "recording stopped"})
		default:
			http.Error(w, "use ?action=start or ?action=stop", http.StatusBadRequest)
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleRecordings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"recordings": s.recorder.ListRecordings()})
}

func (s *APIServer) handleRecordingAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	name := r.URL.Path[len("/api/recordings/"):]

	// Path traversal protection
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "" {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Download file
		filePath := filepath.Join(s.recorder.recordDir, name)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeFile(w, r, filePath)

	case http.MethodDelete:
		// Delete file
		filePath := filepath.Join(s.recorder.recordDir, name)
		if err := os.Remove(filePath); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

	case http.MethodPut:
		// Rename file
		var body struct {
			NewName string `json:"new_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.NewName == "" {
			http.Error(w, "provide new_name", http.StatusBadRequest)
			return
		}
		// Sanitize new name too
		safeName := filepath.Base(body.NewName)
		if safeName == "." || safeName == ".." || safeName == "" {
			http.Error(w, "invalid new_name", http.StatusBadRequest)
			return
		}
		oldPath := filepath.Join(s.recorder.recordDir, name)
		newPath := filepath.Join(s.recorder.recordDir, safeName)
		if err := os.Rename(oldPath, newPath); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "renamed"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── WebSocket ───

func (s *APIServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.clientsMu.Lock()
	s.clients[conn] = true
	s.clientsMu.Unlock()
	go func() {
		defer func() {
			s.clientsMu.Lock()
			delete(s.clients, conn)
			s.clientsMu.Unlock()
			conn.Close()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (s *APIServer) broadcastLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.clientsMu.Lock()
		if len(s.clients) == 0 {
			s.clientsMu.Unlock()
			continue
		}
		resp := StatusResponse{
			System:    s.sysStats.GetStats(),
			Switcher:  s.switcher.GetStats(),
			Outputs:   s.outputManager.GetStats(),
			Audio:     s.audioMeter.GetLevels(),
			Recording: s.recorder.GetStatus(),
		}
		data, err := json.Marshal(resp)
		if err != nil {
			s.clientsMu.Unlock()
			continue
		}
		// Collect clients under lock, send without lock
		conns := make([]*websocket.Conn, 0, len(s.clients))
		for conn := range s.clients {
			conns = append(conns, conn)
		}
		s.clientsMu.Unlock()

		var failed []*websocket.Conn
		for _, conn := range conns {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				conn.Close()
				failed = append(failed, conn)
			}
		}
		if len(failed) > 0 {
			s.clientsMu.Lock()
			for _, conn := range failed {
				delete(s.clients, conn)
			}
			s.clientsMu.Unlock()
		}
	}
}

// ─── Uploads ───

func (s *APIServer) handleUploadFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.ParseMultipartForm(50 << 20)
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(handler.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".ts" && ext != ".mp4" {
		http.Error(w, "Invalid file type", http.StatusBadRequest)
		return
	}
	destPath := filepath.Join(s.dataDir, "fallback"+ext)
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()
	io.Copy(dest, file)
	s.switcher.UpdateFallbackPath(destPath)
	s.switcher.RestartFallback()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success":true}`))
}

func (s *APIServer) handleUploadWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.ParseMultipartForm(10 << 20)
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if strings.ToLower(filepath.Ext(handler.Filename)) != ".png" {
		http.Error(w, "Watermark must be PNG", http.StatusBadRequest)
		return
	}
	destPath := filepath.Join(s.dataDir, "watermark.png")
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()
	io.Copy(dest, file)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success":true}`))
}

// ─── Preview ───

func (s *APIServer) handlePreviewFrame(w http.ResponseWriter, r *http.Request) {
	frame := s.preview.GetFrame()
	if frame == nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write(frame)
}

func (s *APIServer) handlePreviewSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		fps, width, height := s.preview.GetSettings()
		json.NewEncoder(w).Encode(map[string]int{"fps": fps, "width": width, "height": height})
	case http.MethodPut:
		fps, _ := strconv.Atoi(r.URL.Query().Get("fps"))
		width, _ := strconv.Atoi(r.URL.Query().Get("w"))
		height, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if fps < 0 {
			fps = 0
		}
		if width <= 0 {
			width = 640
		}
		if height <= 0 {
			height = 360
		}
		s.preview.UpdateSettings(fps, width, height)
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Bbox ───

func (s *APIServer) handleBboxStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.bboxManager.GetStatus()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

func (s *APIServer) handleBboxAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	var err error
	switch action {
	case "start":
		err = s.bboxManager.Start()
	case "stop":
		err = s.bboxManager.Stop()
	case "restart":
		err = s.bboxManager.Restart()
	default:
		http.Error(w, "Invalid action", http.StatusBadRequest)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

func (s *APIServer) handleBboxLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := s.bboxManager.GetLogs(100)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"logs": logs})
}

func (s *APIServer) handleBboxConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		config, err := s.bboxManager.ReadConfig()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"content": "{}"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"content": config})
	} else if r.Method == http.MethodPut {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if err := s.bboxManager.WriteConfig(req.Content); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleBboxCompose(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		config, err := s.bboxManager.ReadCompose()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"content": ""})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"content": config})
	} else if r.Method == http.MethodPut {
		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if err := s.bboxManager.WriteCompose(req.Content); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
