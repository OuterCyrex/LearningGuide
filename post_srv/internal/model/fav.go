package model

type Fav struct {
	BaseModel

	UserId int32 `json:"user_id" gorm:"type:int;not null"`
	PostId int32 `json:"post_id" gorm:"type:int;not null"`
}

func (Fav) TableName() string {
	return "fav"
}
