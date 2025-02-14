package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ConnectionMetadata struct {
	ID            string
	SourceAddr    string
	DestAddr      string
	BytesSent     int64
	BytesReceived int64
	StartTime     time.Time
	LastActivity  time.Time
}

type LogEntry struct {
	Timestamp  time.Time
	ConnID     string
	Direction  string
	Data       []byte
	MetaData   *ConnectionMetadata
	IsMetadata bool
}

type AsyncLogger struct {
	logChan   chan LogEntry
	wg        sync.WaitGroup
	logFile   *os.File
	consoleMu sync.Mutex
}

type ServerPool struct {
	servers []string
	mutex   sync.RWMutex
	status  map[string]bool
}

func (p *ServerPool) GetServers() []string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return append([]string{}, p.servers...)
}

func NewAsyncLogger(logDir string) (*AsyncLogger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %v", err)
	}

	timestamp := time.Now().Format("2006-01-02_15-04-05")
	logPath := filepath.Join(logDir, fmt.Sprintf("proxy_log_%s.txt", timestamp))

	file, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %v", err)
	}

	logger := &AsyncLogger{
		logChan: make(chan LogEntry, 1000),
		logFile: file,
	}

	go logger.processLogs()

	log.Printf("Logging to file: %s", logPath)
	return logger, nil
}

func (l *AsyncLogger) processLogs() {
	for entry := range l.logChan {
		if entry.IsMetadata {
			l.writeMetadata(entry)
		} else {
			l.writeDataLog(entry)
		}
	}
}

func formatHexDump(data []byte) string {
	var result strings.Builder

	for i := 0; i < len(data); i += 16 {
		end := i + 16
		if end > len(data) {
			end = len(data)
		}

		chunk := data[i:end]

		hexPart := ""
		for _, b := range chunk {
			hexPart += fmt.Sprintf("%02x ", b)
		}
		for len(hexPart) < 48 {
			hexPart += "   "
		}

		asciiPart := ""
		for _, b := range chunk {
			if b >= 32 && b <= 126 {
				asciiPart += string(b)
			} else {
				asciiPart += "."
			}
		}

		result.WriteString(fmt.Sprintf("\033[36m║ %04x  %s |%s|\033[0m\n", i, hexPart, asciiPart))
	}

	return result.String()
}

func decodeProtocolData(data []byte) string {
	if len(data) < 2 {
		return fmt.Sprintf("Data too short: %X", data)
	}

	var decoded strings.Builder
	decoded.WriteString("\n║ Protocol Decode:\n")

	// Try to decode based on known protocol patterns
	pos := 0
	for pos < len(data) {
		// Check for enough remaining bytes
		if pos+4 > len(data) {
			break
		}

		// Read potential header or field identifier
		fieldType := binary.BigEndian.Uint16(data[pos : pos+2])

		// Add field type interpretation
		decoded.WriteString(fmt.Sprintf("║   Field Type: 0x%04X\n", fieldType))

		// Read length if available
		if pos+3 < len(data) {
			length := uint8(data[pos+2])
			decoded.WriteString(fmt.Sprintf("║   Length: %d bytes\n", length))

			// Read value based on length
			if pos+3+int(length) <= len(data) {
				value := data[pos+3 : pos+3+int(length)]

				// Try to interpret value based on length
				switch length {
				case 1:
					decoded.WriteString(fmt.Sprintf("║   Value: %d (0x%02X)\n", value[0], value[0]))
				case 2:
					val := binary.BigEndian.Uint16(value)
					decoded.WriteString(fmt.Sprintf("║   Value: %d (0x%04X)\n", val, val))
				case 4:
					val := binary.BigEndian.Uint32(value)
					decoded.WriteString(fmt.Sprintf("║   Value: %d (0x%08X)\n", val, val))
				default:
					// Try to decode as string if printable
					if isPrintable(value) {
						decoded.WriteString(fmt.Sprintf("║   Value (ASCII): %s\n", string(value)))
					} else {
						decoded.WriteString(fmt.Sprintf("║   Value (HEX): %X\n", value))
					}
				}

				decoded.WriteString("║   ----------------\n")
				pos += 3 + int(length)
			} else {
				pos++
			}
		} else {
			pos++
		}
	}

	return decoded.String()
}

func isPrintable(data []byte) bool {
	for _, b := range data {
		if b < 32 || b > 126 {
			return false
		}
	}
	return true
}

func formatLogEntry(entry LogEntry) string {
	rawMsg := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '.'
		}
		return r
	}, string(entry.Data))

	decodedData := decodeProtocolData(entry.Data)

	return fmt.Sprintf(
		"\033[32m╔═══════════════════════════════════════════════════════════════════════════════\033[0m\n"+
			"║ \033[1mConnection ID:\033[0m %s\n"+
			"║ \033[1mDirection:    \033[0m %s\n"+
			"║ \033[1mTimestamp:    \033[0m %s\n"+
			"║ \033[1mSize:         \033[0m %d bytes\n"+
			"\033[32m╟───────────────────────────────────────────────────────────────────────────────\033[0m\n"+
			"║ \033[1mRaw Message:\033[0m\n║ %s\n"+
			"\033[32m╟───────────────────────────────────────────────────────────────────────────────\033[0m\n"+
			"║ \033[1mDecoded Data:\033[0m%s\n"+
			"\033[32m╟───────────────────────────────────────────────────────────────────────────────\033[0m\n"+
			"║ \033[1mHex Dump:\033[0m\n%s"+
			"\033[32m╚═══════════════════════════════════════════════════════════════════════════════\033[0m\n",
		entry.ConnID,
		entry.Direction,
		entry.Timestamp.Format("2006-01-02 15:04:05.000"),
		len(entry.Data),
		rawMsg,
		decodedData,
		formatHexDump(entry.Data))
}

func (l *AsyncLogger) writeDataLog(entry LogEntry) {

	formattedLog := formatLogEntry(entry)

	l.consoleMu.Lock()
	fmt.Print(formattedLog)
	l.consoleMu.Unlock()

	l.logFile.WriteString(formattedLog)
}

func (l *AsyncLogger) writeMetadata(entry LogEntry) {
	metadata := entry.MetaData
	formattedMeta := fmt.Sprintf(`
╔════════════════════════════════════ 
║ CONNECTION SUMMARY - %s 
╠════════════════════════════════════ 
║ Source: %s 
║ Destination: %s 
║ Duration: %v 
║ Bytes Sent: %d 
║ Bytes Received: %d 
║ Last Activity: %s 
╚════════════════════════════════════ 
`,
		metadata.ID,
		metadata.SourceAddr,
		metadata.DestAddr,
		time.Since(metadata.StartTime),
		metadata.BytesSent,
		metadata.BytesReceived,
		metadata.LastActivity.Format("2006-01-02 15:04:05.000"))

	l.consoleMu.Lock()
	fmt.Print(formattedMeta)
	l.consoleMu.Unlock()

	l.logFile.WriteString(formattedMeta)
}

func (l *AsyncLogger) Close() {
	close(l.logChan)
	l.logFile.Close()
}

func handleConnection(clientConn net.Conn, serverAddrs []string, logger *AsyncLogger) {
	defer clientConn.Close()

	// Generate unique connection ID
	connID := fmt.Sprintf("CONN_%s", time.Now().Format("150405.000"))

	// Connect to all servers
	var serverConns []net.Conn
	for _, addr := range serverAddrs {
		serverConn, err := net.Dial("tcp", addr)
		if err != nil {
			log.Printf("Failed to connect to server %s: %v", addr, err)
			continue
		}
		serverConns = append(serverConns, serverConn)
		defer serverConn.Close()
	}

	if len(serverConns) == 0 {
		log.Printf("No server connections established")
		return
	}

	// Initialize metadata
	metadata := ConnectionMetadata{
		ID:           connID,
		SourceAddr:   clientConn.RemoteAddr().String(),
		DestAddr:     strings.Join(serverAddrs, ", "),
		StartTime:    time.Now(),
		LastActivity: time.Now(),
	}

	// Create channels to synchronize copying goroutines
	done := make(chan bool, len(serverConns)+1)

	// Copy from client to all servers
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := clientConn.Read(buf)
			if n > 0 {
				data := append([]byte{}, buf[:n]...)
				metadata.LastActivity = time.Now()
				metadata.BytesSent += int64(n)

				// Forward to all servers
				for _, conn := range serverConns {
					if _, err := conn.Write(data); err != nil {
						log.Printf("Error writing to server: %v", err)
					}
				}

				// Log the data
				logger.logChan <- LogEntry{
					Timestamp: time.Now(),
					ConnID:    connID,
					Direction: "Client → Servers",
					Data:      data,
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from client: %v", err)
				}
				break
			}
		}
		done <- true
	}()

	// Copy from first server to client (to avoid duplicate responses)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := serverConns[0].Read(buf)
			if n > 0 {
				data := append([]byte{}, buf[:n]...)
				metadata.LastActivity = time.Now()
				metadata.BytesReceived += int64(n)

				if _, err := clientConn.Write(data); err != nil {
					log.Printf("Error writing to client: %v", err)
					break
				}

				// Log the data
				logger.logChan <- LogEntry{
					Timestamp: time.Now(),
					ConnID:    connID,
					Direction: "Server → Client",
					Data:      data,
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("Error reading from server: %v", err)
				}
				break
			}
		}
		done <- true
	}()

	// Wait for both copies to complete
	<-done
	<-done

	// Log final metadata
	logger.logChan <- LogEntry{
		Timestamp:  time.Now(),
		ConnID:     connID,
		MetaData:   &metadata,
		IsMetadata: true,
	}
}

func main() {
	listenAddr := flag.String("listen", ":8080", "Address to listen on")
	serversFlag := flag.String("servers", "localhost:9090", "Comma-separated list of target server addresses")
	logDir := flag.String("logdir", "logs", "Directory for log files")
	flag.Parse()

	// Parse server addresses
	servers := strings.Split(*serversFlag, ",")
	for i, server := range servers {
		servers[i] = strings.TrimSpace(server)
	}

	// Initialize logger
	logger, err := NewAsyncLogger(*logDir)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Close()

	// Start listening
	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Close()

	log.Printf("Proxy listening on %s, forwarding to %s", *listenAddr, strings.Join(servers, ", "))

	// Accept connections
	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}

		go handleConnection(clientConn, servers, logger)
	}
}
