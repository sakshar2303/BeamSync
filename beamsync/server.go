package beamsync

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type EventCallback func(eventName string, data string)

//go:embed ui/*.html
var uiFS embed.FS

// serverState holds per-instance connection tracking (no more package-level globals).
type serverState struct {
	mu            sync.Mutex
	lastHeartbeat time.Time
	isConnected   bool
	uploading     bool // true while a file copy is actively streaming
}

func (s *serverState) markHeartbeat() (wasConnected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastHeartbeat = time.Now()
	wasConnected = s.isConnected
	s.isConnected = true
	return
}

// setUploading marks whether a file upload is currently streaming.
// The watchdog will not fire device_disconnected while uploading is true.
func (s *serverState) setUploading(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploading = v
	if v {
		// Reset heartbeat timer so the 15s clock starts fresh when upload ends
		s.lastHeartbeat = time.Now()
	}
}

func (s *serverState) checkTimeout() (wasConnected bool, timedOut bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never consider it a timeout while data is actively being received
	if s.isConnected && !s.uploading && time.Since(s.lastHeartbeat) > 15*time.Second {
		s.isConnected = false
		return true, true
	}
	return s.isConnected, false
}

// HTTPServer wraps http.Server so we can shut it down cleanly.
type HTTPServer struct {
	server *http.Server
	cancel context.CancelFunc
}

func (s *HTTPServer) Shutdown() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// progressWriter wraps an io.Writer and emits upload_progress events
// as bytes are written. Uses an adaptive interval to avoid event flooding.
type progressWriter struct {
	w           io.Writer
	total       int64
	written     int64
	filename    string
	emit        func(string, string)
	lastEmit    time.Time
	minInterval time.Duration
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)
	now := time.Now()
	if now.Sub(pw.lastEmit) >= pw.minInterval {
		data := fmt.Sprintf("%s|%d|%d", pw.filename, pw.written, pw.total)
		pw.emit("upload_progress", data)
		pw.lastEmit = now
	}
	return n, err
}

// downloadProgressWriter wraps an io.Writer and emits download_progress events
// as bytes are written. Uses an adaptive interval to avoid event flooding.
type downloadProgressWriter struct {
	w           io.Writer
	total       int64
	written     int64
	filename    string
	emit        func(string, string)
	lastEmit    time.Time
	minInterval time.Duration
}

func (dw *downloadProgressWriter) Write(p []byte) (int, error) {
	n, err := dw.w.Write(p)
	dw.written += int64(n)
	now := time.Now()
	if now.Sub(dw.lastEmit) >= dw.minInterval {
		data := fmt.Sprintf("%s|%d|%d", dw.filename, dw.written, dw.total)
		dw.emit("download_progress", data)
		dw.lastEmit = now
	}
	return n, err
}

// copyChunked reads src in large chunks before writing to dst.
// Go's multipart.Part has an internal 4 KB bufio, so Part.Read returns ≤4 KB
// per call regardless of the dst buffer size. Without this helper, we end up
// making thousands of tiny Write() syscalls per second which kills throughput.
// copyChunked accumulates those 4 KB reads into a single chunkSize Write(),
// giving the OS large sequential disk I/O instead of random small writes.
func copyChunked(dst io.Writer, src io.Reader, chunkSize int) (int64, error) {
	buf := make([]byte, chunkSize)
	var total int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			nw, werr := dst.Write(buf[:n])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// generateToken creates a 16-byte (32 hex char) crypto-random session token.
func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: timestamp-based (unlikely but safe)
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// validateToken middleware — returns 403 if the token query-param doesn't match.
// Exempt routes: "/" (serves UI page).
func tokenMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if got := r.URL.Query().Get("token"); got != token {
			http.Error(w, "403 Forbidden: invalid token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// autoRenamePath returns a non-colliding file path by appending (1), (2), …
func autoRenamePath(dir, filename string) string {
	dst := filepath.Join(dir, filename)
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 1; i < 1000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s(%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	// Absolute fallback: timestamp suffix
	return filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
}

// startWatchdog launches the heartbeat watchdog goroutine.
func startWatchdog(ctx context.Context, state *serverState, emit func(string, string)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️ Watchdog panic: %v\n", r)
			}
		}()

		fmt.Println("👁️ Watchdog started")
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				fmt.Println("🛑 Watchdog stopped")
				return
			case <-ticker.C:
				_, timedOut := state.checkTimeout()
				if timedOut {
					emit("device_disconnected", "")
					fmt.Println("💔 Device Disconnected (Timeout)")
				}
			}
		}
	}()
}

// safeEmit dispatches an event in its own goroutine with panic recovery.
func safeEmit(emit EventCallback, event, data string) {
	go func(evt, dt string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("⚠️ Event callback panic: %v\n", r)
			}
		}()
		fmt.Printf("📡 Emitting event: %s | data: %s\n", evt, dt)
		if emit != nil {
			emit(evt, dt)
			fmt.Printf("✅ Event emitted: %s\n", evt)
		}
	}(event, data)
}

// StartServer starts the file-receiver HTTP server.
// Returns (server handle, port string, session token).
func StartServer(uploadDir string, startPort int, callback EventCallback) (*HTTPServer, string, string) {
	fmt.Println("🚀 StartServer() called")

	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("🚨 PANIC IN StartServer: %v\n%s\n", r, debug.Stack())
		}
	}()

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		fmt.Println("❌ Failed to create upload directory:", err)
		return nil, "", ""
	}
	fmt.Printf("📁 Upload directory: %s\n", uploadDir)

	token := generateToken()
	emit := func(evt, data string) { safeEmit(callback, evt, data) }

	state := &serverState{}
	ctx, cancel := context.WithCancel(context.Background())

	startWatchdog(ctx, state, emit)

	mux := http.NewServeMux()

	// ── Heartbeat ────────────────────────────────────────────────────────────
	mux.HandleFunc("/heartbeat", tokenMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Println("💓 Heartbeat received")
		wasConnected := state.markHeartbeat()
		if !wasConnected {
			emit("device_connected", "Android Device")
			fmt.Println("💚 Device Connected!")
		}
		w.WriteHeader(http.StatusOK)
	}))

	// ── Serve UI (no token required — this IS the page that shows the token) ─
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w)
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Println("🌐 GET / - Serving upload UI")
		content, err := uiFS.ReadFile("ui/upload.html")
		if err != nil {
			http.Error(w, "UI Load Error", http.StatusInternalServerError)
			return
		}
		// Inject token so the upload page knows it
		html := strings.Replace(string(content), "{{TOKEN}}", token, 1)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(html))

		// Emit device_connected immediately — the phone loading this page
		// is already proof of connection; no need to wait for first heartbeat.
		wasConnected := state.markHeartbeat()
		if !wasConnected {
			fmt.Println("💚 Device Connected (page load)!")
			emit("device_connected", "Android Device")
		}
	})

	// ── Upload ────────────────────────────────────────────────────────────────
	mux.HandleFunc("/upload", tokenMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("📤 POST /upload - Upload started")

		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("❌ PANIC in upload handler: %v\n%s\n", r, debug.Stack())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Mark as uploading — prevents watchdog from firing device_disconnected
		// during a long transfer where no heartbeats arrive.
		state.setUploading(true)
		defer state.setUploading(false)

		// Update heartbeat on upload activity
		state.markHeartbeat()

		// 100 GB max — guard runaway clients
		r.Body = http.MaxBytesReader(w, r.Body, 100*1024*1024*1024)

		// ── High-throughput streaming multipart ───────────────────────────────
		// Strategy: parse the boundary ourselves so we can wrap r.Body in a
		// large bufio.Reader before it reaches the multipart parser.
		// Without this, multipart.Reader uses a 4 KB internal buffer and every
		// Part.Read() call returns ≤4 KB — giving us thousands of tiny disk
		// writes per second and killing throughput on large files.
		contentType := r.Header.Get("Content-Type")
		mediaType, params, ctErr := mime.ParseMediaType(contentType)
		if ctErr != nil || !strings.HasPrefix(mediaType, "multipart/") {
			fmt.Println("❌ Invalid Content-Type:", contentType)
			http.Error(w, "Expected multipart/form-data", http.StatusBadRequest)
			return
		}
		boundary := params["boundary"]

		// 8 MB network read buffer — reduces TCP recv() syscalls dramatically.
		netReader := bufio.NewReaderSize(r.Body, 8*1024*1024)
		mr := multipart.NewReader(netReader, boundary)

		fileCount := 0
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Println("❌ Multipart read error:", err)
				http.Error(w, "Multipart read error", http.StatusBadRequest)
				return
			}

			// Only process file parts (skip non-file form fields)
			filename := part.FileName()
			if filename == "" {
				part.Close()
				continue
			}

			fileCount++
			fmt.Printf("📄 Processing file #%d: %s\n", fileCount, filename)

			rawName := filepath.Base(filename)
			if rawName == "" || rawName == "." {
				rawName = fmt.Sprintf("upload_%d.bin", time.Now().Unix())
			}

			// Auto-rename on conflict
			dstPath := autoRenamePath(uploadDir, rawName)
			savedName := filepath.Base(dstPath)
			fmt.Printf("💾 Saving to: %s\n", dstPath)

			dst, err := os.Create(dstPath)
			if err != nil {
				fmt.Println("❌ File creation error:", err)
				part.Close()
				continue
			}

			// 8 MB disk write buffer — turns thousands of 4 KB part.Read() returns
			// into large sequential disk flushes.
			diskBuf := bufio.NewWriterSize(dst, 8*1024*1024)

			// progress tracking wraps the buffered disk writer
			pw := &progressWriter{
				w:           diskBuf,
				total:       r.ContentLength, // reads real size from HTTP Content-Length header
				filename:    savedName,
				emit:        emit,
				minInterval: 500 * time.Millisecond,
			}
			// copyChunked accumulates the 4 KB part reads into 8 MB writes,
			// which, combined with diskBuf, means real disk I/O happens in
			// 8 MB sequential bursts instead of random 4 KB writes.
			written, err := copyChunked(pw, part, 8*1024*1024)
			diskBuf.Flush() // flush any remaining bytes in the write buffer
			dst.Close()
			part.Close()

			if err != nil {
				fmt.Println("❌ Copy error:", err)
				continue
			}

			// Final 100% progress event (written == total now that we're done)
			emit("upload_progress", fmt.Sprintf("%s|%d|%d", savedName, written, written))
			fmt.Printf("✅ File saved: %s (%d bytes)\n", savedName, written)

			go func(fname string) {
				time.Sleep(100 * time.Millisecond)
				emit("file_received", fname)
			}(savedName)
		}

		if fileCount == 0 {
			http.Error(w, "No files uploaded", http.StatusBadRequest)
			return
		}

		fmt.Println("✅ Upload handler completed")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("✅ Upload Complete"))
	}))

	portInt, listener, err := FindAvailablePort(startPort, 2, 50)
	if err != nil {
		fmt.Println("❌ Failed to find available port for Receiver:", err)
		if strings.Contains(err.Error(), "permission") || strings.Contains(err.Error(), "access") {
			fmt.Println("🔒 Permission error — attempting firewall setup...")
			if fwErr := RunFirewallSetup(); fwErr != nil {
				fmt.Printf("❌ Firewall setup failed: %v\n", fwErr)
			} else {
				portInt, listener, err = FindAvailablePort(startPort, 2, 50)
				if err != nil {
					fmt.Println("❌ Still failed after firewall setup:", err)
					cancel()
					return nil, "", ""
				}
			}
		} else {
			cancel()
			return nil, "", ""
		}
	}
	portStr := fmt.Sprintf("%d", portInt)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  4 * time.Hour, // support 100 GB over slow Wi-Fi (~7 MB/s)
		WriteTimeout: 4 * time.Hour,
		IdleTimeout:  60 * time.Second,
	}
	httpServer := &HTTPServer{server: srv, cancel: cancel}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("❌ Server panic: %v\n", r)
			}
		}()
		fmt.Printf("🚀 Starting HTTP receiver on :%s...\n", portStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Printf("❌ Server error: %v\n", err)
		}
	}()

	fmt.Println("✅ StartServer() completed")
	return httpServer, portStr, token
}

// StartSender starts the file-sender HTTP server.
// Returns (server handle, port string, session token).
func StartSender(filePaths []string, callback EventCallback) (*HTTPServer, string, string) {
	token := generateToken()
	emit := func(evt, data string) { safeEmit(callback, evt, data) }

	state := &serverState{}
	ctx, cancel := context.WithCancel(context.Background())

	// Sender also gets a watchdog
	startWatchdog(ctx, state, emit)

	mux := http.NewServeMux()

	// ── Heartbeat ─────────────────────────────────────────────────────────────
	mux.HandleFunc("/heartbeat", tokenMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fmt.Println("💓 Sender Heartbeat received")
		wasConnected := state.markHeartbeat()
		if !wasConnected {
			emit("device_connected", "Mobile (Downloader)")
			fmt.Println("💚 Device Connected to Sender!")
		}
		w.WriteHeader(http.StatusOK)
	}))

	// ── Serve files (no token on / — mobile opens the download page directly) ─
	// The generated download URL embedded in the QR already carries the token.

	buildFileBlock := func(filePaths []string) string {
		var b strings.Builder
		if len(filePaths) == 1 {
			name := filepath.Base(filePaths[0])
			b.WriteString(fmt.Sprintf(`<div class="file-card">
				<div class="file-info">%s</div>
				<a href="#" class="download-btn" onclick="event.preventDefault(); startDownload('/download?token=%s'); return false;">⬇️ SAVE</a>
			</div>`, name, token))
		} else {
			for i, path := range filePaths {
				name := filepath.Base(path)
				b.WriteString(fmt.Sprintf(`<div class="file-card">
				<div class="file-info">%s</div>
				<a href="#" class="download-btn" onclick="event.preventDefault(); startDownload('/download/%d?token=%s'); return false;">⬇️ SAVE</a>
			</div>`, name, i, token))
			}
		}
		return b.String()
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Content-Type", "text/html")
		content, err := uiFS.ReadFile("ui/download.html")
		if err != nil {
			http.Error(w, "UI Load Error", http.StatusInternalServerError)
			return
		}
		html := strings.Replace(string(content), "{{FILES}}", buildFileBlock(filePaths), 1)
		html = strings.Replace(html, "{{TOKEN}}", token, 1)
		w.Write([]byte(html))
	})

	if len(filePaths) == 1 {
		filePath := filePaths[0]
		filename := filepath.Base(filePath)
		mux.HandleFunc("/download", tokenMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
			setCORSHeaders(w)
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
			
			// Track download progress
			file, err := os.Open(filePath)
			if err != nil {
				http.Error(w, "File not found", http.StatusNotFound)
				return
			}
			defer file.Close()
			
			fileInfo, err := file.Stat()
			if err != nil {
				http.Error(w, "Failed to stat file", http.StatusInternalServerError)
				return
			}
			
			w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
			w.Header().Set("Content-Type", "application/octet-stream")
			
			progressWriter := &downloadProgressWriter{
				w:           w,
				total:       fileInfo.Size(),
				written:     0,
				filename:    filename,
				emit:        emit,
				lastEmit:    time.Now(),
				minInterval: 500 * time.Millisecond,
			}
			
			io.Copy(progressWriter, file)
		}))
	} else {
		for i, path := range filePaths {
			idx := i
			filePath := path
			mux.HandleFunc(fmt.Sprintf("/download/%d", idx), tokenMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
				setCORSHeaders(w)
				w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
				w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(filePath)))
				
				// Track download progress
				file, err := os.Open(filePath)
				if err != nil {
					http.Error(w, "File not found", http.StatusNotFound)
					return
				}
				defer file.Close()
				
				fileInfo, err := file.Stat()
				if err != nil {
					http.Error(w, "Failed to stat file", http.StatusInternalServerError)
					return
				}
				
				w.Header().Set("Content-Length", fmt.Sprintf("%d", fileInfo.Size()))
				w.Header().Set("Content-Type", "application/octet-stream")
				
				progressWriter := &downloadProgressWriter{
					w:           w,
					total:       fileInfo.Size(),
					written:     0,
					filename:    filepath.Base(filePath),
					emit:        emit,
					lastEmit:    time.Now(),
					minInterval: 500 * time.Millisecond,
				}
				
				io.Copy(progressWriter, file)
			}))
		}
	}

	portInt, listener, err := FindAvailablePort(3005, 2, 50)
	if err != nil {
		fmt.Println("❌ Failed to find available port for Sender:", err)
		if strings.Contains(err.Error(), "permission") || strings.Contains(err.Error(), "access") {
			if fwErr := RunFirewallSetup(); fwErr == nil {
				portInt, listener, err = FindAvailablePort(3005, 2, 50)
				if err != nil {
					cancel()
					return nil, "", ""
				}
			} else {
				cancel()
				return nil, "", ""
			}
		} else {
			cancel()
			return nil, "", ""
		}
	}
	portStr := fmt.Sprintf("%d", portInt)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  30 * time.Second,
	}
	httpServer := &HTTPServer{server: srv, cancel: cancel}

	go func() {
		fmt.Printf("🚀 Starting sender on :%s...\n", portStr)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Println("❌ Sender error:", err)
		}
	}()

	return httpServer, portStr, token
}
