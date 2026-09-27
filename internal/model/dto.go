package model

// 请求 DTO（HTTP 层直接绑定，service 不依赖 api 包）

type CreateMemberInput struct {
	Name         string `json:"name"`
	IDType       string `json:"id_type"`
	IDNumber     string `json:"id_number"`
	IDExpireAt   string `json:"id_expire_at"`
	Phone        string `json:"phone"`
	PhotoRef     string `json:"photo_ref"`
	PhotoQuality string `json:"photo_quality"`
	Role         string `json:"role"`
	FaceExempt   bool   `json:"face_exempt"`
	BirthDate    string `json:"birth_date"`
}

type CreateCardInput struct {
	Type           string              `json:"type"`
	HolderName     string              `json:"holder_name"`
	Phone          string              `json:"phone"`
	ValidFrom      string              `json:"valid_from"`
	ValidTo        string              `json:"valid_to"`
	Parks          []string            `json:"parks"`
	TimeSlotStart  string              `json:"time_slot_start"`
	TimeSlotEnd    string              `json:"time_slot_end"`
	AnnualQuota    int                 `json:"annual_quota"` // 0 => 取售卖规则默认
	DailyLimit     int                 `json:"daily_limit"`
	HolidayAllowed *bool               `json:"holiday_allowed"`
	Members        []CreateMemberInput `json:"members"` // 至少含持卡人
}

type AddMemberInput struct {
	CreateMemberInput
}

type UpdatePhotoInput struct {
	PhotoRef     string `json:"photo_ref"`
	PhotoQuality string `json:"photo_quality"`
}

type ChangePhoneInput struct {
	Phone string `json:"phone"`
}

type CreateTempGrantInput struct {
	CardID    string `json:"card_id"`
	Name      string `json:"name"`
	IDNumber  string `json:"id_number"`
	Phone     string `json:"phone"`
	Reason    string `json:"reason"`
	ValidFrom string `json:"valid_from"`
	ValidTo   string `json:"valid_to"`
}

type CreateReservationInput struct {
	CardID    string   `json:"card_id"`
	ParkID    string   `json:"park_id"`
	Date      string   `json:"date"`
	Slot      string   `json:"slot"`
	MemberIDs []string `json:"member_ids"`
}

type UpgradeCardInput struct {
	NewType string `json:"new_type"`
}

type RefundCardInput struct {
	Reason string `json:"reason"`
}

type FreezeCardInput struct {
	Reason string `json:"reason"`
}

type RegisterDeviceInput struct {
	ParkID   string `json:"park_id"`
	Entrance string `json:"entrance"`
}

type OfflineBatchInput struct {
	DeviceID string        `json:"device_id"`
	At       string        `json:"at"`
	Attempts []GateAttempt `json:"attempts"`
}

type ResolveManualInput struct {
	EventID     string `json:"event_id"`
	Action      string `json:"action"` // confirm_pass / reject
	Operator    string `json:"operator"`
	Note        string `json:"note"`
	RefundQuota bool   `json:"refund_quota"` // 设备问题等非游客过错：放行且不扣/回补权益
}

type CreateAppealInput struct {
	CardID   string   `json:"card_id"`
	EventIDs []string `json:"event_ids"`
	Reason   string   `json:"reason"`
	Detail   string   `json:"detail"`
}

type ReviewAppealInput struct {
	Status  string `json:"status"` // approved / rejected
	Handler string `json:"handler"`
	Note    string `json:"note"`
}

type CardView struct {
	Card       *Card                 `json:"card"`
	Members    []*Member             `json:"members"`
	TempGrants []*TempCompanionGrant `json:"temp_grants"`
}
