package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// -------------------------------------------------------------------
// 1️⃣  Upgrader – tighten the Origin check before moving to prod.
// -------------------------------------------------------------------
var upgrader = websocket.Upgrader{
	// TODO (production): restrict to intranet domains, e.g.:
	// CheckOrigin: func(r *http.Request) bool {
	//     return strings.HasSuffix(r.Host, ".arb.com")
	// },
	CheckOrigin: func(r *http.Request) bool { return true },
}

// -------------------------------------------------------------------
// 2️⃣  Message structures exchanged over the socket.
// -------------------------------------------------------------------
type welcomeMsg struct {
	Type    string `json:"type"`    // always "welcome"
	Message string `json:"message"` // human‑readable greeting
}

type streamMsg struct {
	Type      string  `json:"type"`      // "update"
	Timestamp string  `json:"timestamp"` // ISO‑8601
	Value     float64 `json:"value"`     // dummy periodic data
}

type logMsg struct {
	Type    string `json:"type"`    // always "log"
	Payload string `json:"payload"` // a line printed by the child process
}

type infoMsg struct {
	Type    string `json:"type"`    // "info"
	Message string `json:"message"` // free‑form informational text
}

type errorMsg struct {
	Type    string `json:"type"`    // "error"
	Message string `json:"message"` // error description
}

// -------------------------------------------------------------------
// 3️⃣  Whitelisted executables (key → absolute, signed path)
// -------------------------------------------------------------------
var allowedExecutables = map[string]string{
	"stresstester": `stresstester`,
	// Add more entries as needed – each must be approved by ARB security.
}

// -------------------------------------------------------------------
// 4️⃣  Argument validation – extend the switch whitelist as required.
// -------------------------------------------------------------------
func validateArgs(args []string) error {
	if len(args) == 0 {
		return nil // nothing to validate
	}
	// ----- 4.1  No empty arguments -----
	for _, a := range args {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("empty argument detected")
		}
	}
	// ----- 4.2  Disallow shell meta‑characters -----
	badTokens := []string{";", "&", "|", "`", "$(", ")", ">", "<", "*", "?"}
	for _, a := range args {
		for _, t := range badTokens {
			if strings.Contains(a, t) {
				return fmt.Errorf("argument %q contains forbidden token %q", a, t)
			}
		}
	}
	// ----- 4.3  Whitelist of allowed flags -----
	allowedSwitches := map[string]bool{
		"--workers":  true,
		"--tpath":    true,
		"--messages": true,
		"--strategy": true,
		"--per":      true,
		"--duration": true,
		"--frqnc":    true,
		// Keep any legacy flags you might still use:
		"-p":       true,
		"--mode":   true,
		"--output": true,
	}
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !allowedSwitches[a] {
			return fmt.Errorf("argument %d: flag %q not permitted", i, a)
		}
		_ = i // silence unused‑variable warning if loop body is empty later
	}
	return nil
}

// -------------------------------------------------------------------
// 5️⃣  Run an external process and stream its output back to the client.
// -------------------------------------------------------------------
func runExternalProcess(ctx context.Context, exePath string, args []string, send chan<- interface{}) error {
	// Build the command (exec.CommandContext already ties the child's lifetime to ctx).
	cmd := exec.CommandContext(ctx, exePath, args...)

	// Merge stdout + stderr → a single pipe.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	// Start the process.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	// Scan line‑by‑line.
	scanner := bufio.NewScanner(stdoutPipe)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for scanner.Scan() {
			line := scanner.Text()
			select {
			case send <- logMsg{Type: "log", Payload: line}:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for completion or cancellation.
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done(): // client disconnected or server shutting down
		_ = cmd.Process.Kill()
		<-done // ensure Wait() returns
		wg.Wait()
		return ctx.Err()
	case err := <-done: // process terminated on its own
		wg.Wait()
		if err != nil {
			return fmt.Errorf("process finished with error: %w", err)
		}
		return nil
	}
}

// -------------------------------------------------------------------
// 6️⃣  Request envelope – the JSON you now send from the front‑end.
// -------------------------------------------------------------------
type envelope struct {
	Request innerRequest `json:"request"`
}

// innerRequest matches the old “raw” request object (cmd, exe, args).
type innerRequest struct {
	Cmd  string        `json:"cmd"`  // must be "run"
	Exe  string        `json:"exe"`  // key in allowedExecutables
	Args []interface{} `json:"args"` // can be numbers, strings, bools …
}

// -------------------------------------------------------------------
// 7️⃣  Helper – convert []interface{} → []string (required by exec.Command).
// -------------------------------------------------------------------
func toStringSlice(in []interface{}) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, v := range in {
		// fmt.Sprintf("%v") works for numbers, booleans, strings, nil, etc.
		s := fmt.Sprintf("%v", v)
		out = append(out, s)
	}
	return out, nil
}

// -------------------------------------------------------------------
// 8️⃣  WebSocket handler – wiring everything together.
// -------------------------------------------------------------------
func wsHandler(w http.ResponseWriter, r *http.Request) {
	// -------------------- Upgrade to WebSocket --------------------
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ERROR] Upgrade failed from %s: %v", r.RemoteAddr, err)
		return
	}
	defer func() {
		_ = conn.Close()
		log.Printf("[INFO] Closed WS for %s", r.RemoteAddr)
	}()

	// -------------------- Per‑client context --------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// -------------------- Single‑writer goroutine --------------------
	// All JSON messages go through this buffered channel → we never call
	// conn.WriteJSON from multiple goroutines (gorilla/websocket is not thread‑safe).
	send := make(chan interface{}, 16)
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		for msg := range send {
			if err := conn.WriteJSON(msg); err != nil {
				log.Printf("[ERROR] WriteJSON failed: %v", err)
				cancel() // abort the whole session
				return
			}
		}
	}()

	// -------------------- Welcome message --------------------
	send <- welcomeMsg{Type: "welcome", Message: "Connected to Stress Tester service"}

	// -------------------- Read pump (incoming client messages) --------------------
	go func() {
		defer cancel() // any read error aborts the whole session
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err,
					websocket.CloseGoingAway,
					websocket.CloseNormalClosure) {
					log.Printf("[ERROR] Unexpected read error from %s: %v", r.RemoteAddr, err)
				} else {
					log.Printf("[INFO] Client %s closed WS", r.RemoteAddr)
				}
				return
			}

			// -------- 8.1  Decode the envelope --------
			var env envelope
			if err := json.Unmarshal(payload, &env); err != nil {
				send <- errorMsg{Type: "error", Message: "invalid JSON request"}
				continue
			}
			req := env.Request // inner request struct

			// -------- 8.2  Only “run” command is supported --------
			if strings.ToLower(req.Cmd) != "run" {
				send <- errorMsg{Type: "error", Message: "unsupported command"}
				continue
			}

			// -------- 8.3  Resolve executable path from whitelist --------
			exePath, ok := allowedExecutables[req.Exe]
			if !ok {
				send <- errorMsg{Type: "error", Message: fmt.Sprintf("executable %q not whitelisted", req.Exe)}
				continue
			}

			//--ansh
			absPath, err := getRealPath(exePath)
			if err != nil {
				log.Printf("Error getting Message template abs path: %v, relative pathe used.\n", err)
			} else {
				exePath = absPath
			}
			log.Printf("path:", exePath)
			// -------- 8.4  Convert heterogeneous args → []string --------
			argsStr, err := toStringSlice(req.Args)
			if err != nil {
				send <- errorMsg{Type: "error", Message: "cannot convert args to strings"}
				continue
			}

			// -------- 8.5  Validate arguments (flags, no meta‑chars, etc.) --------
			if err := validateArgs(argsStr); err != nil {
				send <- errorMsg{Type: "error", Message: "argument validation failed: " + err.Error()}
				continue
			}

			// -------- 8.6  Acknowledge and launch the process ----------
			send <- infoMsg{Type: "info", Message: fmt.Sprintf("starting %s …", req.Exe)}

			go func(exeKey, exePath string, args []string) {
				if err := runExternalProcess(ctx, exePath, args, send); err != nil && err != context.Canceled {
					send <- errorMsg{Type: "error", Message: fmt.Sprintf("%s terminated with error: %v", exeKey, err)}
				} else {
					send <- infoMsg{Type: "info", Message: fmt.Sprintf("%s finished successfully", exeKey)}
				}
			}(req.Exe, exePath, argsStr)
		}
	}()

	// -------------------- Optional periodic dummy stream (keep or delete) --------------------
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(send)     // tell writer to exit
			writerWg.Wait() // wait for pending writes
			return
		case t := <-ticker.C:
			update := streamMsg{
				Type:      "update",
				Timestamp: t.UTC().Format(time.RFC3339),
				Value:     rand.Float64() * 100,
			}
			// Non‑blocking send – if the writer channel is full we drop this tick.
			select {
			case send <- update:
			case <-ctx.Done():
			}
		}
	}
}

// -------------------------------------------------------------------
// 🆕 File Upload Handlers
// -------------------------------------------------------------------

// Serve the index.html page at the root URL
func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "stressserver.html")
}

// Handle incoming file uploads via POST requests
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		http.ServeFile(w, r, "upload.html")

	case http.MethodPost:
		// Limit file size to 10 MB
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "File is too large (Max 10MB)", http.StatusBadRequest)
			return
		}
		if r.PostFormValue("filetype") == "" {
			http.Error(w, "Error no file type selected", http.StatusBadRequest)
		}

		file, handler, err := r.FormFile("myFile")
		if err != nil {
			http.Error(w, "Error retrieving the file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// 1️⃣ Extract and sanitize the folder type
		targetFolder := r.PostFormValue("filetype")
		// Security: Prevent path traversal (e.g., "filetype" being "../../../etc")
		targetFolder = filepath.Base(targetFolder)

		// 2️⃣ Build the directory path and create it if it doesn't exist
		uploadDir := filepath.Join(".", targetFolder)

		safeFilename := filepath.Base(handler.Filename)
		filePath := filepath.Join(uploadDir, safeFilename)

		dst, err := os.Create(filePath)
		if err != nil {
			log.Printf("error uploading %v", err)
			http.Error(w, "Error creating destination file"+filePath, http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		if _, err := io.Copy(dst, file); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		fmt.Fprintf(w, "Successfully uploaded file: %s", handler.Filename)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

}

// -------------------------------------------------------------------
// 9️⃣  Server entry‑point – graceful shutdown.
// -------------------------------------------------------------------
func main() {
	addr := flag.String("addr", ":8080", "TCP address to listen on")
	shutdownTO := flag.Duration("shutdown-timeout", 10*time.Second,
		"Graceful shutdown timeout")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)

	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/upload", uploadHandler)

	// Simple health‑check endpoint (required for load‑balancer / monitoring)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run the HTTP server in a background goroutine.
	go func() {
		log.Printf("[INFO] Stress Tester WebSocket server listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] ListenAndServe: %v", err)
		}
	}()

	// Wait for a termination signal (Ctrl‑C, SIGTERM, …).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt) // add syscall.SIGTERM if you run in containers
	<-stop
	log.Println("[INFO] Shutdown signal received")

	// Give active connections a grace period.
	ctx, cancel := context.WithTimeout(context.Background(), *shutdownTO)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("[FATAL] Forced shutdown: %v", err)
	}
	log.Println("[INFO] Server stopped gracefully")
}

func getRealPath(relativepath string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return relativepath, err
	}
	absPath := filepath.Join(wd, relativepath)
	return absPath, nil
}
