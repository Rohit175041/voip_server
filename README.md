# 🛰️ WebRTC Signaling Server (Go)

A lightweight, high-performance **WebSocket signaling server** built in **Golang** for managing peer connections in a WebRTC-based video calling system.

This backend is designed to work with a React-based WebRTC frontend for **real-time peer-to-peer video, audio, and data communication**.

---

## ⚡ Features

✅ Handles multiple rooms with up to 2 users each  
✅ Supports secure WebSocket signaling (WSS / WS)  
✅ Cleans up inactive rooms automatically  
✅ Includes built-in health and metrics endpoints  
✅ Graceful shutdown with cleanup  
✅ Works perfectly with STUN/TURN servers for NAT traversal  

---

## 🏗️ Tech Stack

- **Language:** Go (Golang)
- **WebSocket:** Gorilla WebSocket
- **Logging:** Logrus
- **Environment Management:** godotenv
- **Frontend Compatible:** React, Vanilla JS, or any WebRTC client

---

## 🚀 Quick Start

### 1️⃣ Clone the repository

```bash
git clone https://github.com/Rohit175041/voip_server.git
cd voip_server
