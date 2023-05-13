package rest

import (
	"time"
)

const (
	//read only member
	SPACE_MEMBER_ROLE_READ_ONLY = "READ_ONLY"
	//read write member
	SPACE_MEMBER_ROLE_READ_WRITE = "READ_WRITE"
	//read write member
	SPACE_MEMBER_ROLE_ADMIN = "ADMIN"
)

/**
 * space member
 */
type SpaceMember struct {
	Uuid       string    `json:"uuid" gorm:"type:char(36);primary_key;unique"`
	Sort       int64     `json:"sort" gorm:"type:bigint(20) not null"`
	UpdateTime time.Time `json:"updateTime" gorm:"type:timestamp not null;default:CURRENT_TIMESTAMP"`
	CreateTime time.Time `json:"createTime" gorm:"type:timestamp not null;default:'2018-01-01 00:00:00'"`
	SpaceUuid  string    `json:"spaceUuid" gorm:"type:char(36);index:idx_space_uuid"`
	UserUuid   string    `json:"userUuid" gorm:"type:char(36);index:idx_user_uuid"`
	Role       string    `json:"role" gorm:"type:varchar(45)"`
}