package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// PreviewManager generates a live JPEG preview from the broadcaster stream
type PreviewManager struct {
	broadcaster *Broadcaster

	// Settings (protected by mu)
	fps     int
	width   int
	height  int
	quality int
	mu      sync.RWMutex

	// Latest frame (protected by frameMu)
	latestFrame []byte
	frameMu     sync.RWMutex

	// Process management
	cmd     *exec.Cmd
	running bool
	stopCh  chan struct{}
}

func NewPreviewManager(broadcaster *Broadcaster) *PreviewManager {
	return &PreviewManager{
		broadcaster: broadcaster,
		fps:         2,
		width:       640,
		height:      360,
		quality:     5, // JPEG quality (2=best, 31=worst)
	}
}

func (p *PreviewManager) GetSettings() (fps, width, height int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fps, p.width, p.height
}

func (p *PreviewManager) UpdateSettings(fps, width, height int) {
	p.mu.Lock()
	if fps < 0 {
		fps = 0
	}
	if fps > 15 {
		fps = 15
	}
	p.fps = fps
	p.width = width
	p.height = height
	p.mu.Unlock()

	if fps == 0 {
		// Turn off preview
		if p.running {
			p.Stop()
		}
		p.frameMu.Lock()
		p.latestFrame = nil
		p.frameMu.Unlock()
		log.Printf("[preview] Preview disabled")
		return
	}

	// Restart with new settings
	if p.running {
		p.Stop()
	}
	go p.Start()
	log.Printf("[preview] Settings updated: %dfps %dx%d", fps, width, height)
}

func (p *PreviewManager) GetFrame() []byte {
	p.frameMu.RLock()
	defer p.frameMu.RUnlock()
	if p.latestFrame == nil {
		return nil
	}
	frame := make([]byte, len(p.latestFrame))
	copy(frame, p.latestFrame)
	return frame
}

func (p *PreviewManager) Start() {
	p.mu.RLock()
	fps := p.fps
	width := p.width
	height := p.height
	quality := p.quality
	p.mu.RUnlock()

	p.stopCh = make(chan struct{})
	p.running = true

	log.Printf("[preview] Starting preview: %dfps %dx%d q%d", fps, width, height, quality)

	for p.running {
		p.runPreviewProcess(fps, width, height, quality)

		if !p.running {
			break
		}

		select {
		case <-time.After(2 * time.Second):
		case <-p.stopCh:
			return
		}
	}
}

func (p *PreviewManager) Stop() {
	p.running = false
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}

	p.mu.Lock()
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	p.mu.Unlock()

	// Wait a moment for cleanup
	time.Sleep(100 * time.Millisecond)
}

func (p *PreviewManager) runPreviewProcess(fps, width, height, quality int) {
	// Subscribe to broadcaster
	subID := "preview_internal"
	dataCh := p.broadcaster.Subscribe(subID)
	defer p.broadcaster.Unsubscribe(subID)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "mpegts",
		"-i", "pipe:0",
		"-vf", fmt.Sprintf("scale=%d:%d", width, height),
		"-r", fmt.Sprintf("%d", fps),
		"-q:v", fmt.Sprintf("%d", quality),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[preview] stdin pipe error: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[preview] stdout pipe error: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[preview] start error: %v", err)
		return
	}

	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()

	log.Printf("[preview] FFmpeg started (PID %d)", cmd.Process.Pid)

	// Write broadcaster data to FFmpeg stdin
	go func() {
		defer stdin.Close()
		for {
			select {
			case data, ok := <-dataCh:
				if !ok {
					return
				}
				if _, err := stdin.Write(data); err != nil {
					return
				}
			case <-p.stopCh:
				return
			}
		}
	}()

	// Read JPEG frames from FFmpeg stdout
	p.readFrames(stdout)

	cmd.Wait()
	log.Printf("[preview] FFmpeg exited")
}

// readFrames reads JPEG frames from the FFmpeg image2pipe output
// JPEG frames start with 0xFFD8 (SOI) and end with 0xFFD9 (EOI)
func (p *PreviewManager) readFrames(reader io.Reader) {
	buf := make([]byte, 64*1024)
	var frameBuf bytes.Buffer
	inFrame := false

	for {
		n, err := reader.Read(buf)
		if err != nil {
			return
		}

		for i := 0; i < n; i++ {
			if !inFrame {
				// Look for JPEG SOI marker: 0xFF 0xD8
				if i+1 < n && buf[i] == 0xFF && buf[i+1] == 0xD8 {
					frameBuf.Reset()
					frameBuf.WriteByte(buf[i])
					inFrame = true
				}
			} else {
				frameBuf.WriteByte(buf[i])
				// Look for JPEG EOI marker: 0xFF 0xD9
				if frameBuf.Len() >= 2 {
					b := frameBuf.Bytes()
					last2 := b[len(b)-2:]
					if last2[0] == 0xFF && last2[1] == 0xD9 {
						// Complete frame
						frame := make([]byte, frameBuf.Len())
						copy(frame, frameBuf.Bytes())
						p.frameMu.Lock()
						p.latestFrame = frame
						p.frameMu.Unlock()
						inFrame = false
					}
				}
			}
		}
	}
}
