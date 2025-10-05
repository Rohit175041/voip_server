package main

import (
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
	clients map[*websocket.Conn]bool
	timer   *time.Timer
}

var (
	rooms = make(map[string]*Room)
	mu    sync.Mutex
)

func main() {
	http.HandleFunc("/ws", handleWebSocket)

	log.Println("WebSocket signalling server running on :8080/ws")
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

	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		room = &Room{clients: make(map[*websocket.Conn]bool)}
		rooms[roomID] = room
	}
	// If there was a cleanup timer pending, stop it because a user joined
	if room.timer != nil {
		room.timer.Stop()
		room.timer = nil
	}
	room.clients[c] = true
	mu.Unlock()

	log.Printf("Client joined room: %s (total %d)\n", roomID, len(room.clients))

	defer func() {
		mu.Lock()
		delete(room.clients, c)
		empty := len(room.clients) == 0
		mu.Unlock()

		c.Close()
		log.Printf("Client left room: %s\n", roomID)

		// If the room is empty, schedule cleanup after 20s
		if empty {
			scheduleRoomCleanup(roomID)
		}
	}()

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

func scheduleRoomCleanup(roomID string) {
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		mu.Unlock()
		return
	}

	if room.timer != nil {
		mu.Unlock()
		return // already scheduled
	}

	room.timer = time.AfterFunc(20*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		if r, exists := rooms[roomID]; exists && len(r.clients) == 0 {
			delete(rooms, roomID)
			log.Printf("🧹 Room %s cleaned up after 20s of inactivity\n", roomID)
		}
	})
	mu.Unlock()
}
