package websocket

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/data/entity"
	"github.com/Ravichandran-T-S/Bravestones-hackathon/internal/message"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ChannelManager struct {
	db         *gorm.DB
	hubs       map[string]*Hub
	mutex      sync.RWMutex
	maxUsers   int
	quizTimers map[string]*time.Timer // track auto-start timers
}

func NewChannelManager(db *gorm.DB, maxUsers int) *ChannelManager {
	return &ChannelManager{
		db:         db,
		hubs:       make(map[string]*Hub),
		maxUsers:   maxUsers,
		quizTimers: make(map[string]*time.Timer),
	}
}

func (cm *ChannelManager) JoinQuiz(topicName string, user *entity.User) (*Hub, error) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// 1) Fetch or create topic
	var topic entity.Topic
	cm.db.Where("name = ?", topicName).First(&topic)
	if topic.ID == "" {
		topic = entity.Topic{
			ID:        uuid.New().String(),
			Name:      topicName,
			CreatedAt: time.Now(),
		}
		cm.db.Create(&topic)
		log.Printf("[ChannelManager] Created new topic: %s (ID=%s)", topicName, topic.ID)
	}

	// 2) Find a 'waiting' quiz or create new
	var quiz entity.Quiz
	cm.db.Where("topic_id = ? AND status = 'waiting' AND current_users < ?", topic.ID, cm.maxUsers).
		First(&quiz)

	isNew := false
	if quiz.ID == "" {
		isNew = true
		quiz = entity.Quiz{
			ID:           uuid.New().String(),
			TopicID:      topic.ID,
			Status:       "waiting",
			MaxUsers:     cm.maxUsers,
			CurrentUsers: 1,
			CreatedAt:    time.Now(),
		}
		cm.db.Create(&quiz)
		log.Printf("[ChannelManager] Created new quiz (ID=%s) for topic %s", quiz.ID, topicName)

		// Create & run hub
		hub := NewHub(quiz.ID, cm.db)
		cm.hubs[quiz.ID] = hub
		go hub.Run()
	} else {
		// If quiz found, but not waiting => no join
		if quiz.Status != "waiting" {
			log.Printf("[ChannelManager] Quiz (ID=%s) is not waiting (status=%s). Join denied.", quiz.ID, quiz.Status)
			return nil, errors.New("quiz already started or completed")
		}
		quiz.CurrentUsers++
		cm.db.Save(&quiz)
		log.Printf("[ChannelManager] Found existing waiting quiz (ID=%s). Current users=%d", quiz.ID, quiz.CurrentUsers)
	}

	// 3) Get the hub
	hub := cm.hubs[quiz.ID]
	if hub == nil {
		log.Printf("[ChannelManager] Failed to retrieve hub for quiz (ID=%s)", quiz.ID)
		return nil, errors.New("failed to find or create hub")
	}

	hub.addJoinedUser(user.ID)
	log.Printf("[ChannelManager] User (ID=%s) joined quiz (ID=%s).", user.ID, quiz.ID)

	// Broadcast "user_joined" signal
	hub.broadcast <- message.WsMessage{
		Type: "user_joined",
		Payload: struct {
			UserID string `json:"userId"`
		}{
			UserID: user.ID,
		},
	}
	log.Printf("[ChannelManager] Broadcast 'user_joined' for user=%s on quiz=%s", user.ID, quiz.ID)
	// 4) Create Score row if not existing
	var existingScore entity.Score
	cm.db.Where("quiz_id = ? AND user_id = ?", quiz.ID, user.ID).First(&existingScore)
	if existingScore.ID == "" {
		score := entity.Score{
			ID:        uuid.New().String(),
			QuizID:    quiz.ID,
			UserID:    user.ID,
			CreatedAt: time.Now(),
		}
		cm.db.Create(&score)
		log.Printf("[ChannelManager] Created new Score entry for user (ID=%s) in quiz (ID=%s).", user.ID, quiz.ID)
	}

	// 5) If brand new quiz, set auto-start timer
	if isNew {
		timer := time.AfterFunc(60*time.Second, func() {
			cm.autoStartQuiz(quiz.ID)
		})
		cm.quizTimers[quiz.ID] = timer
		log.Printf("[ChannelManager] Set 1-min auto-start timer for quiz (ID=%s).", quiz.ID)
	}

	return hub, nil
}

// autoStartQuiz triggers if quiz is still "waiting" after 1 minute
func (cm *ChannelManager) autoStartQuiz(quizID string) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	var quiz entity.Quiz
	cm.db.First(&quiz, "id = ?", quizID)
	if quiz.ID == "" {
		return
	}
	if quiz.Status != "waiting" {
		log.Printf("[ChannelManager] Auto-start for quiz (ID=%s) skipped; status=%s", quizID, quiz.Status)
		return
	}

	hub, ok := cm.hubs[quizID]
	if !ok {
		log.Printf("[ChannelManager] No hub found for quiz (ID=%s) in autoStartQuiz", quizID)
		return
	}

	// pick first joined user as host if available
	var hostID string
	if len(hub.joinedUsers) > 0 {
		hostID = hub.joinedUsers[0]
	}

	quiz.Status = "active"
	quiz.HostID = hostID
	quiz.UpdatedAt = time.Now()
	cm.db.Save(&quiz)
	log.Printf("[ChannelManager] Auto-starting quiz (ID=%s). Host = %s", quiz.ID, hostID)

	// ---------------------------------------------------------------------
	// 1) Gather all participants for this quiz
	// ---------------------------------------------------------------------

	var participants []message.ParticipantInfo

	// Find all scores (one row per user who joined)
	var scoreRows []entity.Score
	if err := cm.db.Where("quiz_id = ?", quizID).Find(&scoreRows).Error; err == nil {
		for _, s := range scoreRows {
			log.Printf("[ChannelManager] Found score row for user (ID=%s) in quiz (ID=%s)", s.UserID, s.QuizID)
			// Retrieve the associated user
			var u entity.User
			if err := cm.db.First(&u, "id = ?", s.UserID).Error; err == nil && u.ID != "" {
				log.Printf("[ChannelManager] Found user (ID=%s) for score row", u.ID)
				participants = append(participants, message.ParticipantInfo{
					UserID:   u.ID,
					Username: u.Username,
				})
			}
		}
	}
	// ---------------------------------------------------------------------
	x := message.StartQuizPayload{
		ChannelID:    quiz.ID,
		HostID:       hostID,
		Participants: participants, // <--- new field
	}
	// Broadcast "start_quiz" with participants
	hub.broadcast <- message.WsMessage{
		Type:    "start_quiz",
		Payload: x,
	}
}

// Optional helper
func (cm *ChannelManager) GetOrCreateHub(quizID string) *Hub {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	hub, ok := cm.hubs[quizID]
	if !ok {
		var quiz entity.Quiz
		cm.db.First(&quiz, "id = ?", quizID)
		if quiz.ID == "" {
			return nil
		}
		hub = NewHub(quizID, cm.db)
		cm.hubs[quizID] = hub
		go hub.Run()
		log.Printf("[ChannelManager] Created new hub for existing quiz (ID=%s)", quizID)
	}
	return hub
}
