package entity

import (
	"time"
)

type Quiz struct {
	ID            string `gorm:"primaryKey"`
	TopicID       string
	HostID        string
	MaxUsers      int
	CurrentUsers  int
	Status        string // "waiting", "active", "completed"
	CreatedAt     time.Time
	UpdatedAt     time.Time
	QuestionIndex int
}
