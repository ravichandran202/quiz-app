package entity

import (
	"time"
)

type User struct {
	ID        string `gorm:"type:varchar(250);primaryKey"`
	Username  string `gorm:"type:varchar(250);uniqueIndex"`
	Email     string
	CreatedAt time.Time
}
