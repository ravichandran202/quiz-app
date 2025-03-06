package entity

import (
	"time"
)

type Score struct {
	ID             string `gorm:"primaryKey"`
	QuizID         string
	UserID         string
	TotalScore     int
	LastQuestion   int
	LastScore      int
	QuestionsAsked int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
