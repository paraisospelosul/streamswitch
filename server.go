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

// APIServer handles the web UI and REST API
type APIServer struct {
	switcher      *Switcher
	outputManager *OutputManager
	sysStats      *SysStatsMonitor
	preview       *PreviewManager
	dataDir       string
	port          int

	upgrader  websocket.Upgrader
	clients   map[*websocket.Conn]bool
	clientsMu sync.Mutex
}

func NewAPIServer(switcher *Switcher, outputManager *OutputManager, sysStats *SysStatsMonitor, preview *PreviewManager, dataDir string, port int) *APIServer {
	return &APIServer{
		switcher:      switcher,
		outputManager: outputManager,
		sysStats:      sysStats,
		preview:       preview,
		dataDir:       dataDir,
		port:          port,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[*websocket.Conn]bool),
	}
}

// StatusResponse is the full status payload sent via WebSocket and GET /api/status
type StatusResponse struct {
	System   SystemStats   `json:"system"`
	Switcher SwitcherStats `json:"switcher"`
	Outputs  []OutputStats `json:"outputs"`
}

func (s *APIServer) Run() error {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/outputs", s.handleOutputs)
	mux.HandleFunc("/api/outputs/", s.handleOutputAction)
	mux.HandleFunc("/api/upload/fallback", s.handleUploadFallback)
	mux.HandleFunc("/api/upload/watermark", s.handleUploadWatermark)
	mux.HandleFunc("/api/preview/frame", s.handlePreviewFrame)
	mux.HandleFunc("/api/preview/settings", s.handlePreviewSettings)
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Static files (embedded web UI)
	webContent, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("embed fs: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(webContent)))

	// Start WebSocket broadcaster
	go s.broadcastLoop()

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[server] Web UI available at http://0.0.0.0%s", addr)
	return http.ListenAndServe(addr, mux)
}

// GET /api/status
func (s *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := StatusResponse{
		System:   s.sysStats.GetStats(),
		Switcher: s.switcher.GetStats(),
		Outputs:  s.outputManager.GetStats(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// GET/PUT /api/config
func (s *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		cfg := s.switcher.GetConfig()
		json.NewEncoder(w).Encode(cfg)

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

// GET/POST /api/outputs
func (s *APIServer) handleOutputs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		outputs := s.outputManager.GetOutputs()
		json.NewEncoder(w).Encode(outputs)

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

// Routes: /api/outputs/{id}, /api/outputs/{id}/start, /api/outputs/{id}/stop
func (s *APIServer) handleOutputAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse path: /api/outputs/{id}[/action]
	path := r.URL.Path[len("/api/outputs/"):]
	id := path
	action := ""

	// Check for action suffix
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

// WebSocket handler for real-time stats
func (s *APIServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] WebSocket upgrade error: %v", err)
		return
	}

	s.clientsMu.Lock()
	s.clients[conn] = true
	s.clientsMu.Unlock()

	log.Printf("[server] WebSocket client connected (%d total)", len(s.clients))

	// Read loop (handles pings and close)
	go func() {
		defer func() {
			s.clientsMu.Lock()
			delete(s.clients, conn)
			s.clientsMu.Unlock()
			conn.Close()
			log.Printf("[server] WebSocket client disconnected")
		}()

		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}()
}

// broadcastLoop sends status updates to all WebSocket clients every second
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
			System:   s.sysStats.GetStats(),
			Switcher: s.switcher.GetStats(),
			Outputs:  s.outputManager.GetStats(),
		}

		data, err := json.Marshal(resp)
		if err != nil {
			s.clientsMu.Unlock()
			continue
		}

		for conn := range s.clients {
			err := conn.WriteMessage(websocket.TextMessage, data)
			if err != nil {
				conn.Close()
				delete(s.clients, conn)
			}
		}
		s.clientsMu.Unlock()
	}
}

func (s *APIServer) handleUploadFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(50 << 20) // 50 MB max
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(handler.Filename))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".ts" && ext != ".mp4" {
		http.Error(w, "Invalid file type. Only JPG, PNG, TS, MP4 allowed.", http.StatusBadRequest)
		return
	}

	destPath := filepath.Join(s.dataDir, "fallback"+ext)
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()

	if _, err := io.Copy(dest, file); err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}

	// Update switcher's fallback path dynamically
	s.switcher.UpdateFallbackPath(destPath)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true}`))
}

func (s *APIServer) handleUploadWatermark(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	err := r.ParseMultipartForm(10 << 20) // 10 MB max
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Error retrieving file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(handler.Filename))
	if ext != ".png" {
		http.Error(w, "Watermark must be a PNG file", http.StatusBadRequest)
		return
	}

	destPath := filepath.Join(s.dataDir, "watermark.png")
	dest, err := os.Create(destPath)
	if err != nil {
		http.Error(w, "Error creating file", http.StatusInternalServerError)
		return
	}
	defer dest.Close()

	if _, err := io.Copy(dest, file); err != nil {
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"success":true}`))
}

// GET /api/preview/frame — returns latest JPEG frame
func (s *APIServer) handlePreviewFrame(w http.ResponseWriter, r *http.Request) {
	frame := s.preview.GetFrame()
	if frame == nil {
		// Return a 1x1 transparent pixel as placeholder
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Write(frame)
}

// GET/PUT /api/preview/settings — manage preview FPS and resolution
func (s *APIServer) handlePreviewSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		fps, width, height := s.preview.GetSettings()
		json.NewEncoder(w).Encode(map[string]int{
			"fps": fps, "width": width, "height": height,
		})

	case http.MethodPut:
		fps, _ := strconv.Atoi(r.URL.Query().Get("fps"))
		width, _ := strconv.Atoi(r.URL.Query().Get("w"))
		height, _ := strconv.Atoi(r.URL.Query().Get("h"))

		if fps <= 0 {
			fps = 2
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
