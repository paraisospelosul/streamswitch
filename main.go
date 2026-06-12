package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	srtAddr := flag.String("srt-addr", "localhost:8282", "SRT source address")
	srtMode := flag.String("srt-mode", "caller", "SRT mode: caller or listener")
	fallbackPath := flag.String("fallback", "", "Path to fallback media file")
	webPort := flag.Int("port", 80, "Web UI port")
	srtTimeout := flag.Int("srt-timeout", 2000, "SRT timeout in ms")
	statsURL := flag.String("stats-url", "", "HTTP URL for bbox stats")
	dataDir := flag.String("data-dir", "", "Directory for config/data files")
	bboxDir := flag.String("bbox-dir", "", "Directory where bbox docker-compose.yml is located (default: data-dir)")
	webUser := flag.String("web-user", "belabox", "Web UI username for basic auth")
	webPass := flag.String("web-pass", "belabox", "Web UI password for basic auth")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "StreamSwitch v3.0 — H.265 Failover Relay + Multi-Output RTMP\n\n")
		fmt.Fprintf(os.Stderr, "Usage: streamswitch [flags]\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *dataDir == "" {
		exe, err := os.Executable()
		if err != nil {
			*dataDir = "."
		} else {
			*dataDir = filepath.Dir(exe)
		}
	}

	// Load .env
	loadEnvFile(filepath.Join(*dataDir, ".env"))
	loadEnvFile(".env")

	if *srtAddr == "localhost:8282" {
		if v := os.Getenv("SRT_ADDR"); v != "" {
			*srtAddr = v
		}
	}
	if *srtMode == "caller" {
		if v := os.Getenv("SRT_MODE"); v != "" {
			*srtMode = v
		}
	}
	if *srtTimeout == 2000 {
		if v := os.Getenv("SRT_TIMEOUT"); v != "" {
			fmt.Sscanf(v, "%d", srtTimeout)
		}
	}
	if *statsURL == "" {
		if v := os.Getenv("STATS_URL"); v != "" {
			*statsURL = v
		}
	}
	if *fallbackPath == "" {
		if v := os.Getenv("FALLBACK_PATH"); v != "" {
			*fallbackPath = v
		}
	}
	if *fallbackPath == "" {
		*fallbackPath = filepath.Join(*dataDir, "fallback.ts")
	}
	if *webUser == "" {
		if v := os.Getenv("WEB_USER"); v != "" {
			*webUser = v
		}
	}
	if *webPass == "" {
		if v := os.Getenv("WEB_PASS"); v != "" {
			*webPass = v
		}
	}

	configPath := filepath.Join(*dataDir, "outputs.json")
	switcherConfigPath := filepath.Join(*dataDir, "switcher.json")

	os.MkdirAll(*dataDir, 0755)

	fmt.Println("╔═══════════════════════════════════════════════╗")
	fmt.Println("║         StreamSwitch v3.0                     ║")
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
	log.Printf("Data Dir:      %s", *dataDir)
	fmt.Println()

	if err := checkFFmpeg(); err != nil {
		log.Fatalf("ERROR: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sysStats := NewSysStatsMonitor()
	go sysStats.Start(ctx)

	switcher := NewSwitcher(*srtAddr, *srtMode, *fallbackPath, *srtTimeout, *statsURL, *dataDir, switcherConfigPath)
	outputManager := NewOutputManager(switcher.broadcaster, configPath, *dataDir)
	preview := NewPreviewManager(switcher.broadcaster)
	go preview.Start()
	audioMeter := NewAudioMeter(switcher.broadcaster)
	go audioMeter.Start()
	
	if *bboxDir == "" {
		*bboxDir = *dataDir
	}
	bboxManager := NewBboxManager(*bboxDir)
	recorder := NewRecorder(switcher.broadcaster, *dataDir)

	apiServer := NewAPIServer(switcher, outputManager, sysStats, preview, audioMeter, recorder, bboxManager, *dataDir, *webPort, *webUser, *webPass)

	go func() {
		if err := switcher.Run(ctx); err != nil {
			log.Printf("Switcher error: %v", err)
		}
	}()

	go func() {
		if err := apiServer.Run(); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	sig := <-sigCh
	log.Printf("Received signal %v, shutting down...", sig)
	cancel()
	outputManager.StopAll()
	preview.Stop()
	audioMeter.Stop()
	if recorder.recording.Load() {
		recorder.Stop()
	}
	log.Println("StreamSwitch stopped.")
}

func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
	log.Printf("Loaded env from %s", path)
}

func checkFFmpeg() error {
	path, err := findExecutable("ffmpeg")
	if err != nil {
		return fmt.Errorf("ffmpeg not found in PATH")
	}
	log.Printf("FFmpeg found: %s", path)
	return nil
}

func findExecutable(name string) (string, error) {
	paths := []string{"/usr/bin/" + name, "/usr/local/bin/" + name, "/snap/bin/" + name}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}
