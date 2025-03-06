package entity

import (
	"time"
)

type Topic struct {
	ID          string `gorm:"type:varchar(250);primaryKey"`
	Name        string `gorm:"type:varchar(250);uniqueIndex"`
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
