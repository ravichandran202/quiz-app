package websocket

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/data/entity"
	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/message"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
)

type Hub struct {
	ID         string
	channelID  string // same as quizID
	db         *gorm.DB
	clients    map[*Client]bool
	broadcast  chan message.WsMessage
	Register   chan *Client
	unregister chan *Client

	activeQuiz  *QuizSession
	joinedUsers []string
	mutex       sync.Mutex
	incoming    chan inboundMessage
}

type inboundMessage struct {
	client *Client
	msg    message.WsMessage
}

type Client struct {
	Conn   *websocket.Conn
	Send   chan []byte
	UserID string
}

type QuizSession struct {
	CurrentQuestion *message.QuestionPayload
	Timer           *time.Timer
	Scores          map[string]int
	HostIndex       int
}

func NewHub(channelID string, db *gorm.DB) *Hub {
	return &Hub{
		ID:         uuid.New().String(),
		channelID:  channelID,
		db:         db,
		clients:    make(map[*Client]bool),
		broadcast:  make(chan message.WsMessage),
		Register:   make(chan *Client),
		unregister: make(chan *Client),
		activeQuiz: nil,
		incoming:   make(chan inboundMessage),
	}
}

func (h *Hub) ChannelID() string {
	return h.channelID
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.clients[client] = true
			log.Printf("[Hub %s] Client registered. Total clients: %d", h.channelID, len(h.clients))
		case client := <-h.unregister:
			delete(h.clients, client)
			close(client.Send)
			log.Printf("[Hub %s] Client unregistered. Total clients: %d", h.channelID, len(h.clients))
		case msg := <-h.broadcast:
			log.Printf("[Hub %s] Received message of type %s", h.channelID, msg.Type)
			h.handleMessage(msg)
		case inbound := <-h.incoming:
			// inbound is (client + WsMessage)
			h.handleInboundMessage(inbound.client, inbound.msg)
		}
	}
}

func (h *Hub) handleInboundMessage(c *Client, msg message.WsMessage) {
	// If it’s a heartbeat, respond only to that client
	if msg.Type == "heartbeat" {
		// Example: send back a minimal response, just to keep the connection alive
		c.Send <- []byte(`{"type":"heartbeat","payload":{}}`)
		return
	}
	// Otherwise, handle it via the existing message logic
	h.handleMessage(msg)
}

func (h *Hub) addJoinedUser(userID string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	for _, uid := range h.joinedUsers {
		if uid == userID {
			return
		}
	}
	h.joinedUsers = append(h.joinedUsers, userID)
	log.Printf("[Hub %s] Added joined user %s. Total joined: %d", h.channelID, userID, len(h.joinedUsers))
}

func (h *Hub) handleMessage(msg message.WsMessage) {
	switch msg.Type {
	case "start_quiz":
		h.handleStartQuiz(msg.Payload)

	case "question_posted":
		h.handleQuestionPosted(msg.Payload)

	case : "self_start":
		h.autoStartQuiz(quizID)

	case "submit_answer":
		h.handleAnswer(msg.Payload)

	case "end_question":
		h.questionCompleted()

	case "user_joined":
		log.Printf("[Hub %s] New user joined: %v", h.channelID, msg.Payload)
		// Re-broadcast to all connected clients
		h.broadcastMessage(msg)

	default:
		log.Printf("[Hub %s] Unknown message type: %s", h.channelID, msg.Type)
	}
}

// --- Start Quiz ---

func (h *Hub) handleStartQuiz(payload interface{}) {
	pl, ok := payload.(message.StartQuizPayload)
	if !ok {
		log.Printf("[Hub %s] Invalid payload for start_quiz", h.channelID)
		return
	}
	quizID := pl.ChannelID
	hostID := pl.HostID
	participants := pl.Participants
	h.startQuiz(quizID, hostID, participants)
}

func (h *Hub) startQuiz(quizID, hostID string, participants []message.ParticipantInfo) {
	log.Printf("[Hub %s] Starting quiz with host=%s", quizID, hostID)

	var quiz entity.Quiz
	h.db.First(&quiz, "id = ?", quizID)
	if quiz.ID == "" {
		log.Printf("[Hub %s] No quiz found with ID=%s", h.channelID, quizID)
		return
	}

	if quiz.Status == "waiting" {
		quiz.Status = "active"
		quiz.HostID = hostID
		quiz.UpdatedAt = time.Now()
		if h.db != nil {
			h.db.Save(&quiz)
			log.Printf("[Hub %s] Quiz status changed to 'active'. Host set to %s", quizID, hostID)
		}
	}

	if h.activeQuiz == nil {
		h.activeQuiz = &QuizSession{
			Scores:    make(map[string]int),
			HostIndex: 0,
		}
	}

	h.broadcastMessage(message.WsMessage{
		Type: "quiz_started",
		Payload: message.StartQuizPayload{
			ChannelID:    quiz.ID,
			HostID:       hostID,
			Participants: participants,
		},
	})
}

// --- Question Posted ---

func (h *Hub) handleQuestionPosted(payload interface{}) {
	var qp message.QuestionPayload
	bytes, _ := json.Marshal(payload)
	if err := json.Unmarshal(bytes, &qp); err != nil {
		log.Printf("[Hub %s] Invalid QuestionPayload: %v", h.channelID, err)
		return
	}

	log.Printf("[Hub %s] Host is posting question: %s (index=%d)", h.channelID, qp.Question, qp.QuestionIndex)

	var quiz entity.Quiz
	h.db.First(&quiz, "id = ?", h.channelID)
	if quiz.ID == "" {
		log.Printf("[Hub %s] No quiz found for channelID=%s in handleQuestionPosted", h.channelID, h.channelID)
		return
	}
	if quiz.Status != "active" {
		log.Printf("[Hub %s] Quiz (ID=%s) not active; ignoring question_posted", h.channelID, quiz.ID)
		return
	}

	// Initialize session if needed
	if h.activeQuiz == nil {
		h.activeQuiz = &QuizSession{
			Scores: make(map[string]int),
		}
	}

	h.activeQuiz.CurrentQuestion = &qp
	quiz.QuestionIndex++
	h.db.Save(&quiz)

	// Increment "QuestionsAsked" for the host
	var hostScore entity.Score
	h.db.Where("quiz_id = ? AND user_id = ?", quiz.ID, quiz.HostID).First(&hostScore)
	if hostScore.ID != "" {
		hostScore.QuestionsAsked++
		hostScore.UpdatedAt = time.Now()
		h.db.Save(&hostScore)
		log.Printf("[Hub %s] Incremented QuestionsAsked for host %s", h.channelID, quiz.HostID)
	}

	// Broadcast question to all
	h.broadcastMessage(message.WsMessage{
		Type: "question_posted",
		Payload: struct {
			Question      string   `json:"question"`
			Options       []string `json:"options"`
			TimerDuration int      `json:"timerDuration"`
			QuestionIndex int      `json:"questionIndex"`
		}{
			Question:      qp.Question,
			Options:       qp.Options,
			TimerDuration: qp.TimerDuration,
			QuestionIndex: quiz.QuestionIndex,
		},
	})

	// Start timer (default 30s if none specified)
	duration := 30
	if qp.TimerDuration > 0 {
		duration = qp.TimerDuration
	}
	if h.activeQuiz.Timer != nil {
		h.activeQuiz.Timer.Stop()
	}
	h.activeQuiz.Timer = time.AfterFunc(time.Duration(duration)*time.Second, func() {
		h.questionCompleted()
	})

	log.Printf("[Hub %s] Question timer started for %d seconds", h.channelID, duration)
}

// --- Answer Submission ---

func (h *Hub) handleAnswer(payload interface{}) {
	var ans message.AnswerPayload
	bytes, _ := json.Marshal(payload)
	if err := json.Unmarshal(bytes, &ans); err != nil {
		log.Printf("[Hub %s] Invalid AnswerPayload: %v", h.channelID, err)
		return
	}

	if h.activeQuiz == nil || h.activeQuiz.CurrentQuestion == nil {
		log.Printf("[Hub %s] No active question; ignoring answer from user=%s", h.channelID, ans.UserID)
		return
	}
	if ans.QuestionIndex != h.activeQuiz.CurrentQuestion.QuestionIndex {
		log.Printf("[Hub %s] Stale answer for question index=%d (current=%d). Ignoring.",
			h.channelID, ans.QuestionIndex, h.activeQuiz.CurrentQuestion.QuestionIndex)
		return
	}

	log.Printf("[Hub %s] Received answer from user=%s for questionIndex=%d",
		h.channelID, ans.UserID, ans.QuestionIndex)

	// Retrieve quiz
	var quiz entity.Quiz
	h.db.First(&quiz, "id = ?", ans.ChannelID)
	if quiz.ID == "" {
		log.Printf("[Hub %s] No quiz found for ID=%s in handleAnswer", h.channelID, ans.ChannelID)
		return
	}

	// Check correctness
	isCorrect := ans.AnswerIndex == h.activeQuiz.CurrentQuestion.CorrectAnswer
	log.Printf("[Hub %s] User=%s answeredIndex=%d, correctAnswer=%d => isCorrect=%v",
		h.channelID, ans.UserID, ans.AnswerIndex, h.activeQuiz.CurrentQuestion.CorrectAnswer, isCorrect)

	// Update Score row
	var score entity.Score
	h.db.Where("quiz_id = ? AND user_id = ?", ans.ChannelID, ans.UserID).First(&score)
	if score.ID == "" {
		// Should never happen if user joined properly
		score.ID = uuid.New().String()
		score.QuizID = quiz.ID
		score.UserID = ans.UserID
		score.CreatedAt = time.Now()
	}

	if isCorrect {
		score.TotalScore += 10
		score.LastScore = 10
	} else {
		score.LastScore = 0
	}
	score.LastQuestion = ans.QuestionIndex
	score.UpdatedAt = time.Now()
	h.db.Save(&score)
	log.Printf("Score record after save: ID=%s, totalScore=%d", score.ID, score.TotalScore)
	// Also track ephemeral score
	if h.activeQuiz != nil {
		h.activeQuiz.Scores[ans.UserID] = score.TotalScore
	}
}

// --- Question Completion ---

func (h *Hub) questionCompleted() {
	if h.activeQuiz == nil || h.activeQuiz.CurrentQuestion == nil {
		log.Printf("[Hub %s] questionCompleted called but no active question", h.channelID)
		return
	}
	if h.activeQuiz.Timer != nil {
		h.activeQuiz.Timer.Stop()
		h.activeQuiz.Timer = nil
	}

	log.Printf("[Hub %s] Question completed. Broadcasting scores...", h.channelID)

	var scores []struct {
		UserID    string `json:"userId"`
		Total     int    `json:"total"`
		LastScore int    `json:"lastScore"`
	}
	h.db.Model(&entity.Score{}).
		Where("quiz_id = ?", h.channelID).
		Select("user_id, total_score as total, last_score as last_score").
		Scan(&scores)

	qIndex := h.activeQuiz.CurrentQuestion.QuestionIndex

	h.broadcastMessage(message.WsMessage{
		Type: "question_completed",
		Payload: struct {
			Scores []struct {
				UserID    string `json:"userId"`
				Total     int    `json:"total"`
				LastScore int    `json:"lastScore"`
			} `json:"scores"`
			QuestionIndex int `json:"questionIndex"`
		}{
			Scores:        scores,
			QuestionIndex: qIndex,
		},
	})

	h.rotateHostOrEnd()
	if h.activeQuiz != nil {
		h.activeQuiz.CurrentQuestion = nil
	}
}

// --- Host Rotation / End Quiz ---

func (h *Hub) rotateHostOrEnd() {
	var quiz entity.Quiz
	h.db.First(&quiz, "id = ?", h.channelID)
	if quiz.ID == "" {
		log.Printf("[Hub %s] rotateHostOrEnd: no quiz found", h.channelID)
		return
	}

	// Find current host index
	currHostIndex := -1
	for i, uid := range h.joinedUsers {
		if uid == quiz.HostID {
			currHostIndex = i
			break
		}
	}
	if currHostIndex == -1 {
		currHostIndex = 0
	}

	nextIndex := currHostIndex + 1
	if nextIndex >= len(h.joinedUsers) {
		log.Printf("[Hub %s] All joined users have hosted. Ending quiz...", h.channelID)
		h.endQuiz()
		return
	}

	newHost := h.joinedUsers[nextIndex]
	quiz.HostID = newHost
	quiz.UpdatedAt = time.Now()
	h.db.Save(&quiz)

	log.Printf("[Hub %s] Next host = %s", h.channelID, newHost)

	h.broadcastMessage(message.WsMessage{
		Type: "new_host",
		Payload: struct {
			NewHostID string `json:"newHostId"`
		}{
			NewHostID: newHost,
		},
	})
}

func (h *Hub) endQuiz() {
	var quiz entity.Quiz
	h.db.First(&quiz, "id = ?", h.channelID)
	if quiz.ID == "" {
		log.Printf("[Hub %s] endQuiz: no quiz found", h.channelID)
		return
	}
	quiz.Status = "completed"
	quiz.UpdatedAt = time.Now()
	h.db.Save(&quiz)

	finalScores := h.getFinalScores()
	log.Printf("[Hub %s] Quiz ended. Broadcasting final scores...", h.channelID)

	h.broadcastMessage(message.WsMessage{
		Type: "quiz_ended",
		Payload: struct {
			FinalScores []entity.Score `json:"finalScores"`
		}{
			FinalScores: finalScores,
		},
	})

	h.activeQuiz = nil
}

// getFinalScores fetches Score rows for this quiz
func (h *Hub) getFinalScores() []entity.Score {
	var scores []entity.Score
	h.db.Where("quiz_id = ?", h.channelID).Find(&scores)
	return scores
}

func (h *Hub) broadcastMessage(msg message.WsMessage) {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[Hub %s] broadcastMessage: marshal error: %v", h.channelID, err)
		return
	}
	for client := range h.clients {
		select {
		case client.Send <- msgBytes:
		default:
			close(client.Send)
			delete(h.clients, client)
		}
	}
}

// --- Client Pumps ---

func (c *Client) ReadPump(hub *Hub) {
	defer func() {
		hub.unregister <- c
		c.Conn.Close()
	}()

	for {
		_, msg, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[Hub %s] WebSocket read error: %v", hub.channelID, err)
			}
			break
		}

		var wsMsg message.WsMessage
		if err := json.Unmarshal(msg, &wsMsg); err != nil {
			log.Printf("[Hub %s] Message parsing error: %v", hub.channelID, err)
			continue
		}

		hub.incoming <- inboundMessage{
			client: c,
			msg:    wsMsg,
		}
	}
}

func (c *Client) WritePump() {
	defer c.Conn.Close()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				log.Printf("[Client] Send channel closed; ending WritePump")
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("[Client] Write error: %v", err)
				return
			}
		}
	}
}
