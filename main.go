package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	// CLI flags
	srtAddr := flag.String("srt-addr", "localhost:8282", "SRT source address (host:port)")
	srtMode := flag.String("srt-mode", "caller", "SRT mode: 'caller' (connect to source) or 'listener' (wait for source)")
	fallbackPath := flag.String("fallback", "", "Path to fallback media file (.ts, .jpg, .png)")
	webPort := flag.Int("port", 80, "Web UI port")
	srtTimeout := flag.Int("srt-timeout", 2000, "SRT timeout in milliseconds before switching to fallback")
	statsURL := flag.String("stats-url", "", "Optional HTTP URL to poll for bbox stats (forces fallback if publishers is empty)")
	dataDir := flag.String("data-dir", "", "Directory for config and data files (default: same as binary)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `StreamSwitch - H.265 Stream Failover Relay with Web UI

Receives an SRT stream (H.265/HEVC), monitors for data loss, and automatically
switches to a fallback video when the stream drops. Outputs to multiple RTMP
destinations with H.265 passthrough or H.264 transcoding.

Usage:
  streamswitch [flags]

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Examples:
  # Basic usage (connect to SRTLA on localhost:8282)
  streamswitch --fallback /opt/stream/fallback.ts

  # Listen for SRT on port 5000
  streamswitch --srt-mode listener --srt-addr 0.0.0.0:5000 --fallback /opt/stream/fallback.ts

  # Custom web port and timeout
  streamswitch --fallback /opt/stream/fallback.ts --port 8080 --srt-timeout 3000
`)
	}

	flag.Parse()

	// Validate fallback file
	if *fallbackPath == "" {
		log.Fatal("ERROR: --fallback is required. Provide a path to a media file (.ts, .jpg, .png).")
	}

	// Determine data directory
	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			*dataDir = "."
		} else {
			*dataDir = filepath.Dir(exe)
		}
	}

	if _, err := os.Stat(*fallbackPath); os.IsNotExist(err) {
		log.Fatalf("ERROR: Fallback file not found: %s", *fallbackPath)
	}

	configPath := filepath.Join(*dataDir, "outputs.json")
	switcherConfigPath := filepath.Join(*dataDir, "switcher.json")

	// Print startup banner
	fmt.Println("╔═══════════════════════════════════════════════╗")
	fmt.Println("║         StreamSwitch v2.0                     ║")
	fmt.Println("║   H.265 Failover Relay + Multi-Output RTMP    ║")
	fmt.Println("╚═══════════════════════════════════════════════╝")
	fmt.Println()
	log.Printf("SRT Source:    %s (mode: %s)", *srtAddr, *srtMode)
	log.Printf("Fallback:      %s", *fallbackPath)
	log.Printf("Web UI:        http://0.0.0.0:%d", *webPort)
	log.Printf("SRT Timeout:   %d ms", *srtTimeout)
	if *statsURL != "" {
		log.Printf("Stats URL:     %s", *statsURL)
	}
	log.Printf("Config:        %s", configPath)
	fmt.Println()

	// Check FFmpeg
	if err := checkFFmpeg(); err != nil {
		log.Fatalf("ERROR: %v", err)
	}

	// Context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Create components
	sysStats := NewSysStatsMonitor()
	go sysStats.Start(ctx)

	switcher := NewSwitcher(*srtAddr, *srtMode, *fallbackPath, *srtTimeout, *statsURL, *dataDir, switcherConfigPath)
	outputManager := NewOutputManager(switcher.broadcaster, configPath, *dataDir)
	preview := NewPreviewManager(switcher.broadcaster)
	go preview.Start()
	apiServer := NewAPIServer(switcher, outputManager, sysStats, preview, *dataDir, *webPort)

	// Start switcher in background
	go func() {
		if err := switcher.Run(ctx); err != nil {
			log.Printf("Switcher error: %v", err)
		}
	}()

	// Start API server in background
	go func() {
		if err := apiServer.Run(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for signal
	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)
	cancel()

	// Stop all outputs
	outputManager.StopAll()

	log.Println("StreamSwitch stopped.")
}

func checkFFmpeg() error {
	path, err := findExecutable("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH. Install FFmpeg 7.0+ for Enhanced RTMP H.265 support")
	}
	log.Printf("FFmpeg found: %s", path)
	return nil
}

func findExecutable(name string) (string, error) {
	// Check common locations
	paths := []string{
		"/usr/bin/" + name,
		"/usr/local/bin/" + name,
		"/snap/bin/" + name,
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	// Try PATH
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("%s not found", name)
}
