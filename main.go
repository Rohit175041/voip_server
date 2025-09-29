package main

import (
    "log"
    "net/http"
    "os"

    "github.com/joho/godotenv"
    "github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        allowed := os.Getenv("ALLOWED_ORIGIN")
        if allowed == "*" || allowed == "" {
            return true
        }
        return r.Header.Get("Origin") == allowed
    },
}

var peers = make(map[*websocket.Conn]bool)

func main() {
    log.SetFlags(log.LstdFlags | log.Lshortfile)
    log.Println("Server starting...")

    _ = godotenv.Load()
    host := os.Getenv("HOST")
    if host == "" {
        host = "0.0.0.0"
    }
    port := os.Getenv("PORT")
    if port == "" {
        port = "8080"
    }

    http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
        c, err := upgrader.Upgrade(w, r, nil)
        if err != nil {
            log.Println("Upgrade error:", err)
            return
        }
        log.Println("Client connected")
        peers[c] = true

        defer func() {
            log.Println("Client disconnected")
            delete(peers, c)
            c.Close()
        }()

        for {
            _, msg, err := c.ReadMessage()
            if err != nil {
                log.Println("Read error:", err)
                break
            }
            for p := range peers {
                if p != c {
                    if err := p.WriteMessage(websocket.TextMessage, msg); err != nil {
                        log.Println("Write error:", err)
                    }
                }
            }
        }
    })

    addr := host + ":" + port
    log.Println("WebSocket signalling server running on", addr, "/ws")
    log.Fatal(http.ListenAndServe(addr, nil))
}
