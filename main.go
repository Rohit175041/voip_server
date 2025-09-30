package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	log "github.com/sirupsen/logrus"
)

// -------------------- Config --------------------

var (
	serverAddr      string
	allowedOrigin   string
	maxRoomClients  int
	oneUserWait     time.Duration
	roomCleanupWait time.Duration
	readLimit       int64
	writeTimeout    time.Duration
	pingInterval    time.Duration
)

func init() {
	_ = godotenv.Load()

	// --- Load environment ---
	host := getEnv("HOST", "0.0.0.0")
	port := getEnv("PORT", "8080")
	serverAddr = host + ":" + port

	allowedOrigin = getEnv("ALLOWED_ORIGIN", "*")
	maxRoomClients = getEnvInt("MAX_ROOM_CLIENTS", 2)
	oneUserWait = getEnvDuration("ONE_USER_WAIT", 2*time.Minute)
	roomCleanupWait = getEnvDuration("ROOM_CLEANUP_WAIT", 20*time.Second)
	readLimit = getEnvInt64("READ_LIMIT", 2*1024*1024)
	writeTimeout = getEnvDuration("WRITE_TIMEOUT", 5*time.Second)
	pingInterval = getEnvDuration("PING_INTERVAL", 30*time.Second)

	// --- Logging config ---
	switch getEnv("LOG_LEVEL", "info") {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	// --- WebSocket origin check ---
	upgrader.CheckOrigin = func(r *http.Request) bool {
		if allowedOrigin == "*" {
			return true
		}
		return r.Header.Get("Origin") == allowedOrigin
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			return val
		}
	}
	return def
}
func getEnvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if val, err := strconv.ParseInt(v, 10, 64); err == nil {
			return val
		}
	}
	return def
}
func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if val, err := time.ParseDuration(v); err == nil {
			return val
		}
	}
	return def
}

// -------------------- Types --------------------

var upgrader = websocket.Upgrader{}

type Room struct {
	clients    map[*websocket.Conn]bool
	cleanup    *time.Timer
	oneUserTmr *time.Timer
}

var (
	rooms = make(map[string]*Room)
	mu    sync.Mutex
)

// -------------------- Main --------------------

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	log.Infof("✅ WebSocket signalling server running on %s/ws", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}

// -------------------- Handler --------------------

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "default"
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warnf("Upgrade error: %v", err)
		return
	}
	c.SetReadLimit(readLimit)
	c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error {
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	joined := false

	// --- JOIN ROOM ---
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		room = &Room{clients: make(map[*websocket.Conn]bool)}
		rooms[roomID] = room
	}
	if room.cleanup != nil {
		room.cleanup.Stop()
		room.cleanup = nil
	}
	if len(room.clients) >= maxRoomClients {
		mu.Unlock()
		log.Warnf("❌ Room %s is full (max %d)", roomID, maxRoomClients)
		_ = c.WriteJSON(map[string]string{"type": "error", "message": "Room full"})
		c.Close()
		return
	}
	room.clients[c] = true
	joined = true
	count := len(room.clients)
	mu.Unlock()

	log.Infof("👤 Client joined room %s (total %d)", roomID, count)
	broadcastRoomSize(roomID)
	startOneUserTimerIfNeeded(roomID)

	if joined {
		defer func() {
			mu.Lock()
			if r, ok := rooms[roomID]; ok {
				delete(r.clients, c)
				remaining := len(r.clients)
				mu.Unlock()

				_ = c.Close()
				log.Infof("👋 Client left room %s (remaining %d)", roomID, remaining)
				broadcastRoomSize(roomID)

				if remaining == 0 {
					scheduleRoomCleanup(roomID)
				} else if remaining == 1 {
					startOneUserTimerIfNeeded(roomID)
				}
			} else {
				mu.Unlock()
			}
		}()
	}

	// --- LISTEN & RELAY ---
	for joined {
		_, msg, err := c.ReadMessage()
		if err != nil {
			// classify error
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Infof("Normal disconnect: %v", err)
			} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				log.Infof("Read timeout: %v", err)
			} else {
				log.Warnf("Read error: %v", err)
			}
			break
		}
		log.Debugf("[%s] relay %d bytes", roomID, len(msg))

		mu.Lock()
		for peer := range room.clients {
			if peer != c {
				if err := writeWithDeadline(peer, websocket.TextMessage, msg); err != nil {
					log.Warnf("Write error: %v", err)
				}
			}
		}
		mu.Unlock()
	}
}

// -------------------- Utilities --------------------

func startOneUserTimerIfNeeded(roomID string) {
	mu.Lock()
	defer mu.Unlock()
	room, ok := rooms[roomID]
	if !ok {
		return
	}
	if len(room.clients) == 1 {
		if room.oneUserTmr != nil {
			room.oneUserTmr.Stop()
		}
		room.oneUserTmr = time.AfterFunc(oneUserWait, func() {
			mu.Lock()
			defer mu.Unlock()
			if r, exists := rooms[roomID]; exists && len(r.clients) == 1 {
				log.Infof("⏳ Room %s timed out after %v with only one user", roomID, oneUserWait)
				for client := range r.clients {
					_ = writeJSONWithDeadline(client, map[string]string{
						"type":    "timeout",
						"message": "No one joined within the wait time. Please end the call.",
					})
				}
			}
		})
	} else if len(room.clients) >= 2 && room.oneUserTmr != nil {
		room.oneUserTmr.Stop()
		room.oneUserTmr = nil
	}
}

func scheduleRoomCleanup(roomID string) {
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		mu.Unlock()
		return
	}
	if room.cleanup != nil {
		mu.Unlock()
		return
	}
	room.cleanup = time.AfterFunc(roomCleanupWait, func() {
		mu.Lock()
		defer mu.Unlock()
		if r, exists := rooms[roomID]; exists && len(r.clients) == 0 {
			delete(rooms, roomID)
			log.Infof("🧹 Room %s cleaned up after %v inactivity", roomID, roomCleanupWait)
		}
	})
	mu.Unlock()
}

func broadcastRoomSize(roomID string) {
	mu.Lock()
	defer mu.Unlock()
	room, ok := rooms[roomID]
	if !ok {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "roomSize", "count": len(room.clients)})
	for c := range room.clients {
		_ = writeWithDeadline(c, websocket.TextMessage, msg)
	}
}

// -------------------- WS helpers --------------------

func writeWithDeadline(c *websocket.Conn, msgType int, data []byte) error {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.WriteMessage(msgType, data)
}
func writeJSONWithDeadline(c *websocket.Conn, v interface{}) error {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.WriteJSON(v)
}
