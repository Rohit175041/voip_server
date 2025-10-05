package main

import (
    "fmt"
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
    // Load .env file if present
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
            fmt.Println("Upgrade error:", err)
            return
        }
        fmt.Println("Client connected")
        peers[c] = true

        defer func() {
            fmt.Println("Client disconnected")
            delete(peers, c)
            c.Close()
        }()

        for {
            _, msg, err := c.ReadMessage()
            if err != nil {
                fmt.Println("Read error:", err)
                break
            }
            for p := range peers {
                if p != c {
                    if err := p.WriteMessage(websocket.TextMessage, msg); err != nil {
                        fmt.Println("Write error:", err)
                    }
                }
            }
        }
    })

    addr := host + ":" + port
    log.Println("WebSocket signalling server running on", addr, "/ws")
    log.Fatal(http.ListenAndServe(addr, nil))
}
