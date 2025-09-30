package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// ----------------------------------------------------
// Config defaults (can be overridden via .env)
// ----------------------------------------------------
var (
	serverAddr      string
	allowedOrigin   string
	readLimit       int64
	writeTimeout    time.Duration
	pingInterval    time.Duration
	roomCleanupWait time.Duration
	oneUserWait     time.Duration
	maxRoomClients  int
)

func init() {
	_ = godotenv.Load()

	host := getEnv("HOST", "0.0.0.0")
	port := getEnv("PORT", "8080")
	serverAddr = host + ":" + port

	allowedOrigin = getEnv("ALLOWED_ORIGIN", "*")
	readLimit = getEnvInt64("READ_LIMIT", 2*1024*1024)
	writeTimeout = getEnvDuration("WRITE_TIMEOUT", 5*time.Second)
	pingInterval = getEnvDuration("PING_INTERVAL", 30*time.Second)
	roomCleanupWait = getEnvDuration("ROOM_CLEANUP_WAIT", 20*time.Second)
	oneUserWait = getEnvDuration("ONE_USER_WAIT", 2*time.Minute)
	maxRoomClients = getEnvInt("MAX_ROOM_CLIENTS", 10)

	upgrader.CheckOrigin = func(r *http.Request) bool {
		if allowedOrigin == "*" {
			return true
		}
		return r.Header.Get("Origin") == allowedOrigin
	}
}

// helpers for env
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

// ----------------------------------------------------
// Types
// ----------------------------------------------------
type Room struct {
	id         string
	clients    map[*websocket.Conn]bool
	mu         sync.Mutex
	cleanup    *time.Timer
	oneUserTmr *time.Timer
}

var (
	rooms = make(map[string]*Room)
	rmMu  sync.RWMutex
)

var upgrader = websocket.Upgrader{} // CheckOrigin set in init()

// ----------------------------------------------------
// Main
// ----------------------------------------------------
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleWebSocket)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	srv := &http.Server{Addr: serverAddr, Handler: mux}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("Server shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		closeAllRooms()
	}()

	log.Printf("WebSocket signalling server running on %s/ws", serverAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// ----------------------------------------------------
// WebSocket Handling
// ----------------------------------------------------
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "default"
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[%s] upgrade failed: %v", roomID, err)
		return
	}
	conn.SetReadLimit(readLimit)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	room := getOrCreateRoom(roomID)
	if !addClient(room, conn) {
		conn.WriteJSON(map[string]string{"type": "error", "message": "Room full"})
		conn.Close()
		return
	}
	defer removeClient(room, conn)

	go pingLoop(conn)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[%s] read error: %v", roomID, err)
			break
		}
		room.broadcast(conn, msg)
	}
}

// ----------------------------------------------------
// Room logic
// ----------------------------------------------------
func getOrCreateRoom(id string) *Room {
	rmMu.Lock()
	defer rmMu.Unlock()
	if r, ok := rooms[id]; ok {
		return r
	}
	r := &Room{id: id, clients: make(map[*websocket.Conn]bool)}
	rooms[id] = r
	return r
}

func addClient(r *Room, c *websocket.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.clients) >= maxRoomClients {
		return false
	}
	stopTimer(&r.cleanup)
	r.clients[c] = true
	log.Printf("joined room %s (total %d)", r.id, len(r.clients))
	r.broadcastRoomSize()
	r.startOneUserTimerIfNeeded()
	return true
}

func removeClient(r *Room, c *websocket.Conn) {
	r.mu.Lock()
	delete(r.clients, c)
	count := len(r.clients)
	r.mu.Unlock()

	c.Close()
	log.Printf("left room %s (remaining %d)", r.id, count)

	if count == 0 {
		r.scheduleCleanup()
	} else if count == 1 {
		r.startOneUserTimerIfNeeded()
	}
	r.broadcastRoomSize()
}

func (r *Room) broadcast(sender *websocket.Conn, msg []byte) {
	r.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(r.clients))
	for c := range r.clients {
		if c != sender {
			conns = append(conns, c)
		}
	}
	r.mu.Unlock()
	for _, c := range conns {
		_ = writeWithDeadline(c, websocket.TextMessage, msg)
	}
}

func (r *Room) broadcastRoomSize() {
	r.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(r.clients))
	for c := range r.clients {
		conns = append(conns, c)
	}
	size := len(conns)
	r.mu.Unlock()

	payload, _ := json.Marshal(map[string]interface{}{"type": "roomSize", "count": size})
	for _, c := range conns {
		_ = writeWithDeadline(c, websocket.TextMessage, payload)
	}
}

func (r *Room) startOneUserTimerIfNeeded() {
	if len(r.clients) == 1 {
		stopTimer(&r.oneUserTmr)
		r.oneUserTmr = time.AfterFunc(oneUserWait, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.clients) == 1 {
				log.Printf("room %s timed out (only one user)", r.id)
				for c := range r.clients {
					_ = writeJSONWithDeadline(c, map[string]string{
						"type":    "timeout",
						"message": "No one joined within 2 minutes. Please end the call.",
					})
				}
			}
		})
	} else if len(r.clients) >= 2 {
		stopTimer(&r.oneUserTmr)
	}
}

func (r *Room) scheduleCleanup() {
	if r.cleanup != nil {
		return
	}
	r.cleanup = time.AfterFunc(roomCleanupWait, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.clients) == 0 {
			rmMu.Lock()
			delete(rooms, r.id)
			rmMu.Unlock()
			log.Printf("room %s cleaned up after inactivity", r.id)
		}
	})
}

// ----------------------------------------------------
// Helpers
// ----------------------------------------------------
func writeWithDeadline(c *websocket.Conn, msgType int, data []byte) error {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.WriteMessage(msgType, data)
}
func writeJSONWithDeadline(c *websocket.Conn, v interface{}) error {
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.WriteJSON(v)
}
func stopTimer(t **time.Timer) {
	if *t != nil {
		if !(*t).Stop() {
			select { case <-(*t).C: default: }
		}
		*t = nil
	}
}
func pingLoop(c *websocket.Conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for range ticker.C {
		c.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := c.WriteMessage(websocket.PingMessage, nil); err != nil {
			return
		}
	}
}
func closeAllRooms() {
	rmMu.RLock()
	defer rmMu.RUnlock()
	for _, r := range rooms {
		r.mu.Lock()
		for c := range r.clients {
			_ = c.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "server shutting down"),
				time.Now().Add(time.Second))
			c.Close()
		}
		r.mu.Unlock()
	}
}
