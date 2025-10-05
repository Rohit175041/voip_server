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
	oneUserTmr *time.Timer // 2 min timer if only 1 user
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

	// Join room
	mu.Lock()
	room, ok := rooms[roomID]
	if !ok {
		room = &Room{clients: make(map[*websocket.Conn]bool)}
		rooms[roomID] = room
	}
	// cancel pending cleanup
	if room.cleanup != nil {
		room.cleanup.Stop()
		room.cleanup = nil
	}
	room.clients[c] = true
	clientCount := len(room.clients)

	// Start 2-min timer if only one user
	if clientCount == 1 {
		if room.oneUserTmr != nil {
			room.oneUserTmr.Stop()
		}
		room.oneUserTmr = time.AfterFunc(2*time.Minute, func() {
			mu.Lock()
			defer mu.Unlock()
			if r, exists := rooms[roomID]; exists && len(r.clients) == 1 {
				log.Printf("⏳ Room %s timed out after 2 min (only one user)", roomID)
				for client := range r.clients {
					client.WriteJSON(map[string]string{
						"type":    "timeout",
						"message": "No one joined within 2 minutes. Please end the call.",
					})
				}
			}
		})
	} else if clientCount >= 2 && room.oneUserTmr != nil {
		// cancel one-user timer once someone else joins
		room.oneUserTmr.Stop()
		room.oneUserTmr = nil
	}

	mu.Unlock()

	log.Printf("Client joined room: %s (total %d)\n", roomID, clientCount)
	broadcastRoomSize(roomID)

	// Cleanup on leave
	defer func() {
		mu.Lock()
		delete(room.clients, c)
		clientCount := len(room.clients)
		mu.Unlock()

		c.Close()
		log.Printf("Client left room: %s (remaining %d)\n", roomID, clientCount)
		broadcastRoomSize(roomID)

		if clientCount == 0 {
			scheduleRoomCleanup(roomID)
		} else if clientCount == 1 {
			// restart 2-min timer for the remaining single user
			mu.Lock()
			r := rooms[roomID]
			if r != nil {
				if r.oneUserTmr != nil {
					r.oneUserTmr.Stop()
				}
				r.oneUserTmr = time.AfterFunc(2*time.Minute, func() {
					mu.Lock()
					defer mu.Unlock()
					if rr, exists := rooms[roomID]; exists && len(rr.clients) == 1 {
						log.Printf("⏳ Room %s timed out after 2 min (only one user left)", roomID)
						for client := range rr.clients {
							client.WriteJSON(map[string]string{
								"type":    "timeout",
								"message": "No one joined within 2 minutes. Please end the call.",
							})
						}
					}
				})
			}
			mu.Unlock()
		}
	}()

	// Listen messages and forward to peers
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

	if room.cleanup != nil {
		mu.Unlock()
		return // already scheduled
	}

	room.cleanup = time.AfterFunc(20*time.Second, func() {
		mu.Lock()
		defer mu.Unlock()
		if r, exists := rooms[roomID]; exists && len(r.clients) == 0 {
			delete(rooms, roomID)
			log.Printf("🧹 Room %s cleaned up after 20s of inactivity\n", roomID)
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
	size := len(room.clients)
	msg, _ := json.Marshal(map[string]interface{}{
		"type":  "roomSize",
		"count": size,
	})
	for c := range room.clients {
		c.WriteMessage(websocket.TextMessage, msg)
	}
}
