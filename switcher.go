package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// SwitcherState represents the current state of the switcher
type SwitcherState int

const (
	StateLive        SwitcherState = iota // SRT active, forwarding SRT data
	StateFallback                         // SRT down, forwarding fallback data
	StateSRTStarting                      // SRT process restarting, waiting for keyframe
)

func (s SwitcherState) String() string {
	switch s {
	case StateLive:
		return "live"
	case StateFallback:
		return "fallback"
	case StateSRTStarting:
		return "srt_starting"
	default:
		return "unknown"
	}
}

// SwitcherConfig holds dynamic configuration that can be changed via the web UI
type SwitcherConfig struct {
	SRTAddr                  string `json:"srt_addr"`
	SRTMode                  string `json:"srt_mode"`
	SRTTimeout               int    `json:"srt_timeout"`
	StatsURL                 string `json:"stats_url"`
	FallbackPath             string `json:"fallback_path"`
	MinBitrateKbps           int    `json:"min_bitrate_kbps"`
	BitrateHysteresisSeconds int    `json:"bitrate_hysteresis_seconds"`
}

// SwitcherStats holds real-time statistics
type SwitcherStats struct {
	State             string  `json:"state"`
	SRTConnected      bool    `json:"srt_connected"`
	InputBitrateKbps  float64 `json:"input_bitrate_kbps"`
	PacketsReceived   uint64  `json:"packets_received"`
	PacketsForwarded  uint64  `json:"packets_forwarded"`
	BytesReceived     uint64  `json:"bytes_received"`
	KeyframesDetected uint64  `json:"keyframes_detected"`
	SwitchCount       uint64  `json:"switch_count"`
	LastSwitchTime    string  `json:"last_switch_time"`
	Uptime            string  `json:"uptime"`
	VideoPID          uint16  `json:"video_pid"`
	AudioPID          uint16  `json:"audio_pid"`
}

// Broadcaster distributes MPEGTS packets to multiple subscribers
type Broadcaster struct {
	subscribers map[string]chan []byte
	mu          sync.RWMutex
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subscribers: make(map[string]chan []byte),
	}
}

func (b *Broadcaster) Subscribe(id string) chan []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	// If already subscribed, unsubscribe first to prevent double-close
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
	ch := make(chan []byte, 50000) // Buffer 50000 packets (~9.4MB)
	b.subscribers[id] = ch
	return ch
}

func (b *Broadcaster) Unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
}

func (b *Broadcaster) Broadcast(data []byte) {
	buf := make([]byte, len(data))
	copy(buf, data)

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subscribers {
		select {
		case ch <- buf:
		default:
			// Subscriber too slow, drop packet
		}
	}
}

func (b *Broadcaster) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// InputProcess manages an FFmpeg input process
type InputProcess struct {
	name    string
	args    []string
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	running atomic.Bool
	mu      sync.Mutex
}

func NewInputProcess(name string, args []string) *InputProcess {
	return &InputProcess{
		name: name,
		args: args,
	}
}

func (ip *InputProcess) Start() error {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	ip.cmd = exec.Command("ffmpeg", ip.args...)
	ip.cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	var err error

	ip.stdout, err = ip.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	ip.stderr, err = ip.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := ip.cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	ip.running.Store(true)

	// Drain stderr in background
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := ip.stderr.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	log.Printf("[%s] FFmpeg process started (PID %d)", ip.name, ip.cmd.Process.Pid)
	return nil
}

func (ip *InputProcess) Stop() {
	ip.mu.Lock()
	defer ip.mu.Unlock()

	if ip.cmd != nil && ip.cmd.Process != nil {
		ip.cmd.Process.Kill()
		ip.cmd.Wait()
	}
	ip.running.Store(false)
	log.Printf("[%s] FFmpeg process stopped", ip.name)
}

func (ip *InputProcess) IsRunning() bool {
	return ip.running.Load()
}

// Switcher is the core engine that switches between SRT input and fallback
type Switcher struct {
	// Configuration (dynamic, editable via web UI)
	config     SwitcherConfig
	configMu   sync.RWMutex
	configPath string

	srtEnabled atomic.Bool

	// State
	state   SwitcherState
	stateMu sync.RWMutex

	startTime time.Time

	// MPEGTS state
	videoPID uint16
	audioPID uint16
	pmtPID   uint16

	// Statistics (atomic for lock-free reads)
	packetsReceived   atomic.Uint64
	packetsForwarded  atomic.Uint64
	bytesReceived     atomic.Uint64
	keyframesDetected atomic.Uint64
	switchCount       atomic.Uint64
	lastSwitchTime    time.Time
	lastSwitchTimeMu  sync.RWMutex

	// Bitrate tracking
	bitrateBytes       atomic.Uint64
	currentBitrateKbps atomic.Uint64

	// Bitrate threshold tracking
	lowBitrateStart time.Time
	inLowBitrate    bool

	// Output distribution
	broadcaster *Broadcaster

	// General
	dataDir string
}

func NewSwitcher(srtAddr, srtMode, fallbackPath string, timeoutMs int, statsURL string, dataDir string, configPath string) *Switcher {
	sw := &Switcher{
		config: SwitcherConfig{
			SRTAddr:                  srtAddr,
			SRTMode:                  srtMode,
			FallbackPath:             fallbackPath,
			SRTTimeout:               timeoutMs,
			StatsURL:                 statsURL,
			MinBitrateKbps:           0, // disabled by default
			BitrateHysteresisSeconds: 5,
		},
		configPath:  configPath,
		dataDir:     dataDir,
		state:       StateFallback,
		broadcaster: NewBroadcaster(),
	}
	sw.srtEnabled.Store(true)

	// Try to load saved config (overrides CLI flags)
	sw.loadConfig()

	return sw
}

func (s *Switcher) GetConfig() SwitcherConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Switcher) UpdateConfig(cfg SwitcherConfig) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config = cfg
	s.saveConfig()
	log.Printf("[switcher] Config updated: SRT=%s mode=%s timeout=%d minBitrate=%d hysteresis=%d",
		cfg.SRTAddr, cfg.SRTMode, cfg.SRTTimeout, cfg.MinBitrateKbps, cfg.BitrateHysteresisSeconds)
}

func (s *Switcher) UpdateFallbackPath(path string) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.config.FallbackPath = path
	s.saveConfig()
	log.Printf("[switcher] Fallback path updated dynamically to: %s", path)
}

func (s *Switcher) saveConfig() {
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		log.Printf("[switcher] Failed to save config: %v", err)
		return
	}
	if err := os.WriteFile(s.configPath, data, 0644); err != nil {
		log.Printf("[switcher] Failed to write config file: %v", err)
	}
}

func (s *Switcher) loadConfig() {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return // File doesn't exist yet, use CLI defaults
	}
	var cfg SwitcherConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("[switcher] Failed to parse config: %v", err)
		return
	}
	s.config = cfg
	log.Printf("[switcher] Loaded config from %s", s.configPath)
}

func (s *Switcher) GetState() SwitcherState {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.state
}

func (s *Switcher) setState(state SwitcherState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state != state {
		log.Printf("[switcher] State change: %s -> %s", s.state, state)
		s.state = state
		if state == StateLive || state == StateFallback {
			s.switchCount.Add(1)
			s.lastSwitchTimeMu.Lock()
			s.lastSwitchTime = time.Now()
			s.lastSwitchTimeMu.Unlock()
		}
	}
}

func (s *Switcher) GetStats() SwitcherStats {
	s.lastSwitchTimeMu.RLock()
	lastSwitch := s.lastSwitchTime
	s.lastSwitchTimeMu.RUnlock()

	lastSwitchStr := ""
	if !lastSwitch.IsZero() {
		lastSwitchStr = lastSwitch.Format(time.RFC3339)
	}

	uptimeStr := ""
	if !s.startTime.IsZero() {
		uptimeStr = time.Since(s.startTime).Truncate(time.Second).String()
	}

	return SwitcherStats{
		State:             s.GetState().String(),
		SRTConnected:      s.GetState() == StateLive,
		InputBitrateKbps:  float64(s.currentBitrateKbps.Load()),
		PacketsReceived:   s.packetsReceived.Load(),
		PacketsForwarded:  s.packetsForwarded.Load(),
		BytesReceived:     s.bytesReceived.Load(),
		KeyframesDetected: s.keyframesDetected.Load(),
		SwitchCount:       s.switchCount.Load(),
		LastSwitchTime:    lastSwitchStr,
		Uptime:            uptimeStr,
		VideoPID:          s.videoPID,
		AudioPID:          s.audioPID,
	}
}

func (s *Switcher) buildSRTArgs() []string {
	s.configMu.RLock()
	srtAddr := s.config.SRTAddr
	srtMode := s.config.SRTMode
	s.configMu.RUnlock()

	separator := "?"
	if strings.Contains(srtAddr, "?") {
		separator = "&"
	}
	srtURL := fmt.Sprintf("srt://%s%smode=%s&latency=500000&timeout=5000000", srtAddr, separator, srtMode)
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-i", srtURL,
		"-c", "copy",
		"-f", "mpegts",
		"-flush_packets", "1",
		"pipe:1",
	}
}

func (s *Switcher) buildFallbackArgs() []string {
	s.configMu.RLock()
	fallbackPath := s.config.FallbackPath
	s.configMu.RUnlock()

	ext := strings.ToLower(filepath.Ext(fallbackPath))
	if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
		return []string{
			"-hide_banner", "-loglevel", "error",
			"-loop", "1",
			"-re",
			"-framerate", "30",
			"-i", fallbackPath,
			"-f", "lavfi",
			"-i", "anullsrc=r=48000:cl=stereo", // Generate silent audio
			"-c:v", "libx265",
			"-preset", "ultrafast",
			"-x265-params", "keyint=60:min-keyint=60",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-b:a", "128k",
			"-f", "mpegts",
			"-flush_packets", "1",
			"pipe:1",
		}
	}

	return []string{
		"-hide_banner", "-loglevel", "error",
		"-re",
		"-stream_loop", "-1",
		"-i", fallbackPath,
		"-c", "copy",
		"-f", "mpegts",
		"-flush_packets", "1",
		"pipe:1",
	}
}

func (s *Switcher) statsPoller(ctx context.Context, forceFallbackCh chan struct{}) {
	s.configMu.RLock()
	statsURL := s.config.StatsURL
	s.configMu.RUnlock()

	if statsURL == "" {
		return
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.configMu.RLock()
			url := s.config.StatsURL
			s.configMu.RUnlock()

			if url == "" {
				continue
			}

			resp, err := client.Get(url)
			if err != nil {
				continue
			}

			var stats struct {
				Publishers map[string]interface{} `json:"publishers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&stats); err == nil {
				if len(stats.Publishers) == 0 {
					// No publishers, force fallback and disable SRT connection
					s.srtEnabled.Store(false)
					select {
					case forceFallbackCh <- struct{}{}:
					default:
					}
				} else {
					s.srtEnabled.Store(true)
				}
			}
			resp.Body.Close()
		}
	}
}

// checkBitrateThreshold checks if bitrate is below minimum and returns true if fallback should be triggered
func (s *Switcher) checkBitrateThreshold() bool {
	s.configMu.RLock()
	minBitrate := s.config.MinBitrateKbps
	hysteresis := s.config.BitrateHysteresisSeconds
	s.configMu.RUnlock()

	if minBitrate <= 0 {
		return false // Disabled
	}

	currentBitrate := s.currentBitrateKbps.Load()
	if currentBitrate > 0 && int(currentBitrate) < minBitrate {
		if !s.inLowBitrate {
			s.inLowBitrate = true
			s.lowBitrateStart = time.Now()
			log.Printf("[switcher] Bitrate dropped below minimum (%d < %d kbps), starting hysteresis timer (%ds)", currentBitrate, minBitrate, hysteresis)
		} else if time.Since(s.lowBitrateStart) >= time.Duration(hysteresis)*time.Second {
			log.Printf("[switcher] Bitrate below minimum for %ds, triggering fallback", hysteresis)
			s.inLowBitrate = false
			return true
		}
	} else {
		if s.inLowBitrate {
			log.Printf("[switcher] Bitrate recovered above minimum (%d kbps)", currentBitrate)
		}
		s.inLowBitrate = false
	}
	return false
}

// Run starts the switcher main loop
func (s *Switcher) Run(ctx context.Context) error {
	s.startTime = time.Now()
	log.Println("[switcher] Starting...")

	// Start bitrate calculator
	go s.bitrateLoop(ctx)

	// Start fallback FFmpeg (always running, with auto-restart)
	fallbackDataCh := make(chan []byte, 10000)
	go s.fallbackManagerLoop(ctx, fallbackDataCh)

	// Channels for reading data from both sources
	srtDataCh := make(chan []byte, 10000)
	srtDiedCh := make(chan struct{}, 1)
	forceFallbackCh := make(chan struct{}, 1)

	// Start HTTP poller
	go s.statsPoller(ctx, forceFallbackCh)

	// Start SRT process and reader
	go s.srtManagerLoop(ctx, srtDataCh, srtDiedCh)

	// Start in fallback state
	s.setState(StateFallback)

	s.configMu.RLock()
	timeoutDuration := time.Duration(s.config.SRTTimeout) * time.Millisecond
	s.configMu.RUnlock()

	timeout := time.NewTimer(timeoutDuration)
	defer timeout.Stop()

	// Bitrate check ticker (every second)
	bitrateCheckTicker := time.NewTicker(1 * time.Second)
	defer bitrateCheckTicker.Stop()

	for {
		currentState := s.GetState()

		switch currentState {
		case StateLive:
			select {
			case data, ok := <-srtDataCh:
				if !ok {
					log.Println("[switcher] SRT channel closed, exiting")
					return nil
				}
				s.processPackets(data)
				if info, ok := ParseTSPacket(data); ok {
					// Only reset timeout if it's a video packet
					if s.videoPID == 0 || info.PID == s.videoPID {
						s.configMu.RLock()
						td := time.Duration(s.config.SRTTimeout) * time.Millisecond
						s.configMu.RUnlock()
						timeout.Reset(td)
					}
				}
				s.broadcaster.Broadcast(data)
				s.packetsForwarded.Add(1)

			case <-timeout.C:
				log.Println("[switcher] SRT timeout, switching to fallback")
				s.setState(StateFallback)

			case <-bitrateCheckTicker.C:
				if s.checkBitrateThreshold() {
					s.setState(StateFallback)
				}

			case <-forceFallbackCh:
				log.Println("[switcher] API reported no publishers, forcing fallback")
				s.setState(StateFallback)

			case <-srtDiedCh:
				log.Println("[switcher] SRT process died, switching to fallback")
				s.setState(StateFallback)

			case <-ctx.Done():
				return nil
			}

		case StateFallback, StateSRTStarting:
			select {
			case data := <-fallbackDataCh:
				s.broadcaster.Broadcast(data)
				s.packetsForwarded.Add(1)

			case data, ok := <-srtDataCh:
				if ok {
					// Check if this contains a keyframe
					if s.containsKeyframe(data) {
						log.Println("[switcher] SRT keyframe detected, switching to LIVE")
						s.setState(StateLive)

						s.configMu.RLock()
						td := time.Duration(s.config.SRTTimeout) * time.Millisecond
						s.configMu.RUnlock()

						timeout.Reset(td)
						s.processPackets(data)
						s.broadcaster.Broadcast(data)
						s.packetsForwarded.Add(1)
					} else {
						s.setState(StateSRTStarting)
					}
				}

			case <-forceFallbackCh:
				s.setState(StateFallback)

			case <-srtDiedCh:
				// SRT still down, waiting for reconnection

			case <-ctx.Done():
				return nil
			}
		}
	}
}

// fallbackManagerLoop manages the fallback FFmpeg process with auto-restart
func (s *Switcher) fallbackManagerLoop(ctx context.Context, dataCh chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		s.configMu.RLock()
		args := s.buildFallbackArgs()
		s.configMu.RUnlock()

		fallbackProc := NewInputProcess("fallback", args)
		if err := fallbackProc.Start(); err != nil {
			log.Printf("[switcher] Fallback start failed: %v, retrying in 3s", err)
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		// Read loop — exits when FFmpeg dies
		procDiedCh := make(chan struct{}, 1)
		s.readInputLoop(ctx, fallbackProc, dataCh, procDiedCh, true)

		fallbackProc.Stop()
		log.Println("[switcher] Fallback process died, restarting in 2s...")

		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// srtManagerLoop manages the SRT FFmpeg process lifecycle with reconnection
func (s *Switcher) srtManagerLoop(ctx context.Context, dataCh chan []byte, diedCh chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Wait until SRT is enabled
		for !s.srtEnabled.Load() {
			select {
			case <-time.After(1 * time.Second):
			case <-ctx.Done():
				return
			}
		}

		newProc := NewInputProcess("srt", s.buildSRTArgs())
		if err := newProc.Start(); err != nil {
			log.Printf("[switcher] SRT start failed: %v, retrying in 3s", err)
			select {
			case <-time.After(3 * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}

		// Watchdog to kill proc if disabled
		watchdogDone := make(chan struct{})
		go func() {
			for {
				select {
				case <-watchdogDone:
					return
				case <-time.After(1 * time.Second):
					if !s.srtEnabled.Load() {
						log.Println("[switcher] SRT disabled by API, disconnecting...")
						newProc.Stop()
						return
					}
				}
			}
		}()

		// Read loop
		procDiedCh := make(chan struct{}, 1)
		go s.readInputLoop(ctx, newProc, dataCh, procDiedCh, false)

		select {
		case <-procDiedCh:
		case <-ctx.Done():
			return
		}

		close(watchdogDone)
		newProc.Stop()

		select {
		case diedCh <- struct{}{}:
		default:
		}

		// Wait before reconnecting
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// readInputLoop reads MPEGTS data from an input process and sends to channel
func (s *Switcher) readInputLoop(ctx context.Context, proc *InputProcess, dataCh chan []byte, diedCh chan struct{}, throttle bool) {
	aligner := NewPacketAligner()
	readBuf := make([]byte, 1316*7)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		startTime := time.Now()
		n, err := proc.stdout.Read(readBuf)
		if err != nil {
			log.Printf("[%s] read error: %v", proc.name, err)
			if diedCh != nil {
				select {
				case diedCh <- struct{}{}:
				default:
				}
			}
			return
		}

		if throttle {
			elapsed := time.Since(startTime)
			if elapsed < 10*time.Millisecond {
				time.Sleep(10*time.Millisecond - elapsed)
			}
		}

		if n > 0 {
			if !throttle {
				s.packetsReceived.Add(1)
				s.bytesReceived.Add(uint64(n))
				s.bitrateBytes.Add(uint64(n))
			}
			aligner.Feed(readBuf[:n])
			for {
				pkt := aligner.Next()
				if pkt == nil {
					break
				}
				select {
				case dataCh <- pkt:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// processPackets inspects MPEGTS packets for PAT/PMT to discover PIDs
func (s *Switcher) processPackets(data []byte) {
	if len(data) < tsPacketSize {
		return
	}

	info, ok := ParseTSPacket(data)
	if !ok {
		return
	}

	// Parse PAT to find PMT PID
	if info.PID == patPID && info.PUSI && info.HasPayload {
		if pmtPID := ParsePAT(data[info.PayloadOffset:]); pmtPID != 0 {
			s.pmtPID = pmtPID
		}
	}

	// Parse PMT to find video/audio PIDs
	if info.PID == s.pmtPID && s.pmtPID != 0 && info.PUSI && info.HasPayload {
		vPID, aPID := ParsePMT(data[info.PayloadOffset:])
		if vPID != 0 {
			if s.videoPID != vPID {
				log.Printf("[switcher] Video PID detected: %d", vPID)
			}
			s.videoPID = vPID
		}
		if aPID != 0 {
			if s.audioPID != aPID {
				log.Printf("[switcher] Audio PID detected: %d", aPID)
			}
			s.audioPID = aPID
		}
	}
}

// containsKeyframe checks if a packet contains an H.265 keyframe
func (s *Switcher) containsKeyframe(data []byte) bool {
	if len(data) < tsPacketSize {
		return false
	}

	info, ok := ParseTSPacket(data)
	if !ok {
		return false
	}

	// First, learn PIDs from PAT/PMT
	s.processPackets(data)

	// Check for keyframe in video packets
	if info.PID == s.videoPID && s.videoPID != 0 && info.PUSI && info.HasPayload {
		pesPayload := ExtractPESPayload(data, info.PayloadOffset)
		if pesPayload != nil && IsKeyframe(pesPayload) {
			s.keyframesDetected.Add(1)
			return true
		}
	}

	return false
}

// bitrateLoop calculates input bitrate every second
func (s *Switcher) bitrateLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			bytes := s.bitrateBytes.Swap(0)
			kbps := (bytes * 8) / 1000
			s.currentBitrateKbps.Store(kbps)

		case <-ctx.Done():
			return
		}
	}
}
