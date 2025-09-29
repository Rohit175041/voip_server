package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Room struct {
	clients    map[*websocket.Conn]bool
	cleanup    *time.Timer // cleanup after empty
	oneUserTmr *time.Timer // 2 min timer if only one user
}

var (
	rooms = make(map[string]*Room)
	mu    sync.Mutex
)

func main() {
	http.HandleFunc("/ws", handleWebSocket)
	log.Println("✅ WebSocket signalling server running on :8080/ws")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	if roomID == "" {
		roomID = "default"
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// ---------------- JOIN ROOM ----------------
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		room = &Room{clients: make(map[*websocket.Conn]bool)}
		rooms[roomID] = room
	}
	// cancel any pending cleanup
	if room.cleanup != nil {
		room.cleanup.Stop()
		room.cleanup = nil
	}

	room.clients[c] = true
	clientCount := len(room.clients)
	mu.Unlock()

	log.Printf("👤 Client joined room: %s (total %d)", roomID, clientCount)

	// broadcast size & start one-user timer if needed
	broadcastRoomSize(roomID)
	startOneUserTimerIfNeeded(roomID)

	// ---------------- CLEANUP ON LEAVE ----------------
	defer func() {
		mu.Lock()
		if r, ok := rooms[roomID]; ok {
			delete(r.clients, c)
			clientCount := len(r.clients)
			mu.Unlock()

			c.Close()
			log.Printf("👋 Client left room: %s (remaining %d)", roomID, clientCount)
			broadcastRoomSize(roomID)

			if clientCount == 0 {
				scheduleRoomCleanup(roomID)
			} else if clientCount == 1 {
				startOneUserTimerIfNeeded(roomID)
			}
		} else {
			mu.Unlock()
		}
	}()

	// ---------------- LISTEN & RELAY MESSAGES ----------------
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			break
		}
		mu.Lock()
		for peer := range room.clients {
			if peer != c {
				if err := peer.WriteMessage(websocket.TextMessage, msg); err != nil {
					log.Println("Write error:", err)
				}
			}
		}
		mu.Unlock()
	}
}

// --- UTILITIES ---

// Start a 2-min timer if the room has exactly one user
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
		room.oneUserTmr = time.AfterFunc(2*time.Minute, func() {
			mu.Lock()
			defer mu.Unlock()
			if r, exists := rooms[roomID]; exists && len(r.clients) == 1 {
				log.Printf("⏳ Room %s timed out after 2 minutes with only one user", roomID)
				for client := range r.clients {
					_ = client.WriteJSON(map[string]string{
						"type":    "timeout",
						"message": "No one joined within 2 minutes. Please end the call.",
					})
				}
			}
		})
	} else if len(room.clients) >= 2 && room.oneUserTmr != nil {
		// cancel timer if now 2+ users
		room.oneUserTmr.Stop()
		room.oneUserTmr = nil
	}
}

// Clean up a room after 20 seconds if empty
func scheduleRoomCleanup(roomID string) {
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		mu.Unlock()
		return
	}

	if room.cleanup != nil {
		mu.Unlock()
		return // already scheduled
	}

	room.cleanup = time.AfterFunc(20*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		if r, exists := rooms[roomID]; exists && len(r.clients) == 0 {
			delete(rooms, roomID)
			log.Printf("🧹 Room %s cleaned up after 20s inactivity", roomID)
		}
	})
	mu.Unlock()
}

// Send current room size to all clients
func broadcastRoomSize(roomID string) {
	mu.Lock()
	defer mu.Unlock()
	room, ok := rooms[roomID]
	if !ok {
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{
		"type":  "roomSize",
		"count": len(room.clients),
	})
	for c := range room.clients {
		if err := c.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Println("Write error:", err)
		}
	}
}
