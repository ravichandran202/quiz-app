package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/data/entity"
	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/websocket"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	ws "github.com/gorilla/websocket"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var (
	channelManager *websocket.ChannelManager
	db             *gorm.DB
)

const (
	DBUsername = "root"
	DBHost     = "common-rds.allen-internal-sandbox.in"
	DBPassword = "aScrEHD9myHeBPZe6co5"
	DBName     = "bravestones_hackathon"
)

func main() {
	// Initialize database
	populatedDBSource := fmt.Sprintf("%s:%s@tcp(%s)/%s", DBUsername, DBPassword, DBHost, DBName)
	dsn := fmt.Sprintf("%s?charset=utf8mb4&parseTime=True&loc=Local", populatedDBSource)
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})

	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate
	err = db.AutoMigrate(
		&entity.Quiz{},
		&entity.Score{},
		&entity.User{},
		&entity.Topic{}, // ensure Topic is migrated too
	)

	if err != nil {
		log.Fatal("Auto-migration failed:", err)
	}

	// Initialize ChannelManager
	channelManager = websocket.NewChannelManager(db, 10) // maxUsers = 10 (example)

	r := mux.NewRouter()

	// Example: minimal /join endpoint
	r.HandleFunc("/join", handleJoin).Methods("POST")

	// WebSocket endpoint
	r.HandleFunc("/ws", handleWebSocket)

	log.Println("Starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func handleJoin(w http.ResponseWriter, r *http.Request) {
	// Parse form or JSON to get topic + username
	topicName := r.FormValue("topic")
	username := r.FormValue("username")
	if topicName == "" || username == "" {
		http.Error(w, "Missing topic or username", http.StatusBadRequest)
		return
	}

	// Check if user exists or create
	var user entity.User
	db.Where("username = ?", username).First(&user)
	if user.ID == "" {
		user.ID = uuid.New().String()
		user.Username = username
		user.CreatedAt = time.Now()
		db.Create(&user)
	}

	// NOTE: We now capture the returned hub.
	hub, err := channelManager.JoinQuiz(topicName, &user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// Return channelID (quiz ID) in JSON response
	response := map[string]interface{}{
		"message":   "Joined quiz successfully",
		"channelId": hub.ChannelID(),
		"userId":    user.ID,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleWebSocket upgrades the connection and registers the client to the relevant Hub
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// quizID is your "channel"
	quizID := r.URL.Query().Get("channel")
	userID := r.URL.Query().Get("user")
	if quizID == "" {
		http.Error(w, "Channel (quizID) required", http.StatusBadRequest)
		return
	}

	// If there's no existing hub, create one (or handle error)
	hub := channelManager.GetOrCreateHub(quizID) // You can implement a helper
	if hub == nil {
		http.Error(w, "No hub available", http.StatusNotFound)
		return
	}

	upgrader := ws.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &websocket.Client{
		Conn:   conn,
		Send:   make(chan []byte, 256),
		UserID: userID,
	}
	hub.Register <- client

	go client.WritePump()
	go client.ReadPump(hub)
}
