package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LogBuffer stores recent log entries
type LogBuffer struct {
	entries []string
	mutex   sync.RWMutex
	maxSize int
}

func NewLogBuffer(maxSize int) *LogBuffer {
	return &LogBuffer{
		entries: make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

func (b *LogBuffer) Add(entry string) {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	// Add new entry at the beginning (newest first)
	b.entries = append([]string{entry}, b.entries...)

	// Trim if exceeding max size
	if len(b.entries) > b.maxSize {
		b.entries = b.entries[:b.maxSize]
	}
}

func (b *LogBuffer) GetAll() []string {
	b.mutex.RLock()
	defer b.mutex.RUnlock()

	// Return a copy to avoid race conditions
	result := make([]string, len(b.entries))
	copy(result, b.entries)
	return result
}

// WebServer handles the log viewing interface
type WebServer struct {
	addr       string
	logDir     string
	buffer     *LogBuffer
	clients    map[*websocket.Conn]bool
	clientsMux sync.Mutex
	upgrader   websocket.Upgrader
}

func NewWebServer(addr, logDir string) *WebServer {
	// Create web directory if it doesn't exist
	os.MkdirAll("web", 0755)

	return &WebServer{
		addr:    addr,
		logDir:  logDir,
		buffer:  NewLogBuffer(1000),
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

func (s *WebServer) Start() error {
	// Create index.html if it doesn't exist
	indexPath := "web/index.html"
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		indexHTML := `<!DOCTYPE html>
<html>
<head>
    <title>Proxy Log Viewer</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #1e1e1e;
            color: #d4d4d4;
            margin: 0;
            padding: 20px;
        }
        #log-container {
            margin-top: 20px;
            white-space: pre-wrap;
        }
        .log-entry {
            border-bottom: 1px solid #333;
            padding: 10px 0;
        }
        .client-to-server {
            color: #6A8759;
        }
        .server-to-client {
            color: #CC7832;
        }
        .metadata {
            color: #4EC9B0;
        }
    </style>
</head>
<body>
    <h1>Proxy Log Viewer</h1>
    <div id="log-container"></div>
    
    <script>
        const logContainer = document.getElementById('log-container');
        
        function connectWebSocket() {
            const ws = new WebSocket('ws://' + window.location.host + '/logs');
            
            ws.onopen = function() {
                console.log('WebSocket connected');
            };
            
            ws.onmessage = function(event) {
                const logEntry = document.createElement('div');
                logEntry.className = 'log-entry';
                
                const logText = event.data;
                if (logText.includes('CONNECTION SUMMARY')) {
                    logEntry.classList.add('metadata');
                } else if (logText.includes('Client → Servers')) {
                    logEntry.classList.add('client-to-server');
                } else if (logText.includes('Server → Client')) {
                    logEntry.classList.add('server-to-client');
                }
                
                logEntry.textContent = logText;
                logContainer.insertBefore(logEntry, logContainer.firstChild);
            };
            
            ws.onclose = function() {
                // Attempt to reconnect after 1 second
                setTimeout(connectWebSocket, 1000);
            };
        }
        
        connectWebSocket();
    </script>
</body>
</html>`
		if err := os.WriteFile(indexPath, []byte(indexHTML), 0644); err != nil {
			return fmt.Errorf("failed to create index.html: %v", err)
		}
	}

	// Setup file watcher for the log directory
	go s.watchLogDirectory()

	// Setup HTTP routes
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "web/index.html")
		} else {
			http.NotFound(w, r)
		}
	})

	http.HandleFunc("/logs", s.handleWebSocket)

	// Start the server
	log.Printf("Web interface listening on %s", s.addr)
	return http.ListenAndServe(s.addr, nil)
}

func (s *WebServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	// Add client to pool
	s.clientsMux.Lock()
	s.clients[conn] = true
	s.clientsMux.Unlock()

	// Send existing logs
	for _, entry := range s.buffer.GetAll() {
		conn.WriteMessage(websocket.TextMessage, []byte(entry))
	}

	// Handle disconnect
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				s.clientsMux.Lock()
				delete(s.clients, conn)
				s.clientsMux.Unlock()
				conn.Close()
				break
			}
		}
	}()
}

func (s *WebServer) broadcastToClients(message string) {
	s.buffer.Add(message)

	s.clientsMux.Lock()
	defer s.clientsMux.Unlock()

	for client := range s.clients {
		err := client.WriteMessage(websocket.TextMessage, []byte(message))
		if err != nil {
			client.Close()
			delete(s.clients, client)
		}
	}
}

func (s *WebServer) watchLogDirectory() {
	for {
		// Scan for latest log file
		files, err := filepath.Glob(filepath.Join(s.logDir, "proxy_log_*.txt"))
		if err != nil {
			log.Printf("Error scanning log directory: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if len(files) == 0 {
			time.Sleep(1 * time.Second)
			continue
		}

		// Sort files by modification time (newest first)
		latestFile := files[0]
		latestTime := time.Time{}
		for _, file := range files {
			info, err := os.Stat(file)
			if err != nil {
				continue
			}
			if info.ModTime().After(latestTime) {
				latestTime = info.ModTime()
				latestFile = file
			}
		}

		// Watch the latest file
		s.watchLogFile(latestFile)
	}
}

func (s *WebServer) watchLogFile(filePath string) {
	log.Printf("Watching log file: %s", filePath)

	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening log file: %v", err)
		return
	}
	defer file.Close()

	// Seek to end of file
	info, err := file.Stat()
	if err != nil {
		log.Printf("Error getting file stats: %v", err)
		return
	}

	_, err = file.Seek(info.Size(), 0)
	if err != nil {
		log.Printf("Error seeking in file: %v", err)
		return
	}

	var buffer strings.Builder
	for {
		// Check if file still exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			log.Printf("Log file no longer exists: %s", filePath)
			return
		}

		// Read new content
		buf := make([]byte, 4096)
		n, err := file.Read(buf)
		if err != nil && err.Error() != "EOF" {
			log.Printf("Error reading log file: %v", err)
			return
		}

		if n > 0 {
			buffer.Write(buf[:n])

			// Process complete log entries
			data := buffer.String()
			entries := s.extractLogEntries(data)

			if len(entries) > 0 {
				// Keep remaining partial entry in buffer
				lastNewline := strings.LastIndex(data, "\n")
				if lastNewline >= 0 {
					buffer.Reset()
					buffer.WriteString(data[lastNewline+1:])
				}

				// Broadcast complete entries
				for _, entry := range entries {
					s.broadcastToClients(entry)
				}
			}
		}

		// Wait a little before next read
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *WebServer) extractLogEntries(data string) []string {
	var entries []string

	// Extract connection summaries
	summaryStart := "╔════════════════════════════════════"
	summaryEnd := "╚════════════════════════════════════"
	for {
		startIdx := strings.Index(data, summaryStart)
		if startIdx == -1 {
			break
		}

		endIdx := strings.Index(data[startIdx:], summaryEnd)
		if endIdx == -1 {
			break
		}

		endIdx += startIdx + len(summaryEnd)

		// Extract the complete entry
		entry := data[startIdx:endIdx]
		entries = append(entries, entry)

		// Remove processed part
		if endIdx < len(data) {
			data = data[endIdx:]
		} else {
			data = ""
			break
		}
	}

	// Extract data logs
	logStart := "┌──────────────────────────────────────────────────────────────────────────────"
	logEnd := "└──────────────────────────────────────────────────────────────────────────────"
	for {
		startIdx := strings.Index(data, logStart)
		if startIdx == -1 {
			break
		}

		endIdx := strings.Index(data[startIdx:], logEnd)
		if endIdx == -1 {
			break
		}

		endIdx += startIdx + len(logEnd)

		// Extract the complete entry
		entry := data[startIdx:endIdx]
		entries = append(entries, entry)

		// Remove processed part
		if endIdx < len(data) {
			data = data[endIdx:]
		} else {
			data = ""
			break
		}
	}

	return entries
}

func main() {
	webAddr := flag.String("addr", ":8082", "Web UI address")
	logDir := flag.String("logdir", "logs", "Directory for log files")
	flag.Parse()

	// Start web server
	server := NewWebServer(*webAddr, *logDir)
	log.Fatal(server.Start())
}
