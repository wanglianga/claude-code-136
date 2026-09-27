package model

// ===== 年卡类型 =====

const (
	CardTypeSingle = "single" // 单人年卡
	CardTypeFamily = "family" // 家庭年卡
	CardTypeParent = "parent" // 亲子年卡：1 名成人 + 1 名儿童
)

// ===== 年卡状态 =====

const (
	CardStatusActive   = "active"
	CardStatusFrozen   = "frozen"
	CardStatusRefunded = "refunded"
)

// ===== 核验决策 =====

const (
	DecisionPass    = "PASS"               // 放行
	DecisionManual  = "MANUAL_CHECK"       // 人工核验
	DecisionDeny    = "DENY"               // 权益不足 / 硬性拒绝
	DecisionSuspect = "SUSPECTED_TRANSFER" // 疑似转借，转人工
)

// ===== 核验失败 / 异常原因码 =====
//
// 客服与现场工作人员据此判断本次核验为什么失败。
const (
	ReasonOK                   = "OK"
	ReasonCardNotFound         = "CARD_NOT_FOUND"
	ReasonCardFrozen           = "CARD_FROZEN" // 年卡冻结
	ReasonCardRefunded         = "CARD_REFUNDED"
	ReasonCardExpired          = "CARD_EXPIRED"           // 年卡超出有效期
	ReasonIDExpired            = "ID_EXPIRED"             // 证件过期
	ReasonPhotoUnclear         = "PHOTO_UNCLEAR"          // 照片不清 / 人脸质量差
	ReasonPhotoMismatch        = "PHOTO_MISMATCH"         // 人脸与底库不匹配
	ReasonFaceExempt           = "FACE_EXEMPT_REQUIRED"   // 老人无法刷脸，需人工核验
	ReasonCompanionUnbound     = "COMPANION_UNBOUND"      // 同行人未绑定
	ReasonTempCompanionInvalid = "TEMP_COMPANION_INVALID" // 临时同行授权无效/过期
	ReasonCompanionOverLimit   = "COMPANION_OVER_LIMIT"   // 同行人数超出年卡限制
	ReasonQuotaInsufficient    = "QUOTA_INSUFFICIENT"     // 权益剩余额度不足
	ReasonHolidayRestricted    = "HOLIDAY_RESTRICTED"     // 节假日权益不适用
	ReasonParkNotAllowed       = "PARK_NOT_ALLOWED"       // 该景点不在可入园范围
	ReasonOutsideTimeSlot      = "OUTSIDE_TIME_SLOT"      // 非入园时段
	ReasonParkFull             = "PARK_CAPACITY_FULL"     // 景区容量已满
	ReasonDeviceOffline        = "DEVICE_OFFLINE"         // 设备离线
	ReasonHighFrequency        = "HIGH_FREQUENCY"         // 高频异常（短时间多次入园尝试）
	ReasonSuspectedTransfer    = "SUSPECTED_TRANSFER"     // 疑似转借：多设备/异地并发
	ReasonQrInvalid            = "QR_INVALID"             // 二维码无效/伪造
	ReasonDuplicateDeduction   = "DUPLICATE_DEDUCTION"    // 网络抖动重复请求，幂等拦截
	ReasonPendingManual        = "PENDING_MANUAL"         // 已转人工，闸机等待
)

// ===== 人员角色 =====

const (
	RoleHolder = "holder" // 持卡人
	RoleSpouse = "spouse"
	RoleChild  = "child" // 孩子（人脸随成长可更新）
	RoleElder  = "elder" // 老人（可登记免人脸）
	RoleOther  = "other"
)

// ===== 临时同行授权状态 =====

const (
	TempStatusActive  = "active"
	TempStatusUsed    = "used"
	TempStatusRevoked = "revoked"
	TempStatusExpired = "expired"
)

// ===== 设备状态 =====

const (
	DeviceOnline  = "online"
	DeviceOffline = "offline"
)

// ===== 预约状态 =====

const (
	ReservationBooked   = "booked"
	ReservationUsed     = "used"
	ReservationCanceled = "canceled"
)

// ===== 核验流水状态 =====

const (
	VerdictPass    = "pass"
	VerdictManual  = "manual"
	VerdictDeny    = "deny"
	VerdictSuspect = "suspect"
	VerdictPending = "pending"
)

// ===== 人工处理动作 =====

const (
	ManualConfirmPass = "confirm_pass" // 人工放行
	ManualReject      = "reject"
)

// ===== 申诉状态 =====

const (
	AppealOpen     = "open"
	AppealApproved = "approved" // 申诉成立，回补权益
	AppealRejected = "rejected"
)

// Member 实名成员（持卡人 / 家庭成员）
type Member struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IDType       string `json:"id_type"`      // idcard / passport
	IDNumber     string `json:"id_number"`    // 证件号
	IDExpireAt   string `json:"id_expire_at"` // 证件有效期 YYYY-MM-DD
	Phone        string `json:"phone"`
	PhotoRef     string `json:"photo_ref"`     // 人脸底库照片引用（对象存储 key / URL）
	PhotoQuality string `json:"photo_quality"` // ok / unclear
	Role         string `json:"role"`
	FaceExempt   bool   `json:"face_exempt"` // 老人等无法刷脸：true 时人脸环节转人工
	BirthDate    string `json:"birth_date,omitempty"`
	BoundAt      string `json:"bound_at"`
	Active       bool   `json:"active"`
}

// Card 年卡聚合
type Card struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Status         string   `json:"status"`
	HolderName     string   `json:"holder_name"`
	Phone          string   `json:"phone"`           // 绑定手机号（可换绑）
	ValidFrom      string   `json:"valid_from"`      // 有效期起
	ValidTo        string   `json:"valid_to"`        // 有效期止（退款/升级时重算）
	CompanionLimit int      `json:"companion_limit"` // 同行人数上限（含持卡人）
	Parks          []string `json:"parks"`           // 可入园景点
	HolidayAllowed bool     `json:"holiday_allowed"` // 节假日是否可用
	TimeSlotStart  string   `json:"time_slot_start"` // 每日可入园时段 HH:MM
	TimeSlotEnd    string   `json:"time_slot_end"`
	AnnualQuota    int      `json:"annual_quota"`   // 年度权益总额度（人次）
	QuotaUsed      int      `json:"quota_used"`     // 已用额度
	QuotaRefunded  int      `json:"quota_refunded"` // 异常/申诉回补累计
	DailyLimit     int      `json:"daily_limit"`    // 每成员每日入园次数上限（防高频/转借）
	CreatedAt      string   `json:"created_at"`
	Version        int64    `json:"version"`
}

// QuotaRemaining 权益剩余
func (c *Card) QuotaRemaining() int { return c.AnnualQuota - c.QuotaUsed }

// TempCompanionGrant 家庭成员临时更换授权
type TempCompanionGrant struct {
	ID        string `json:"id"`
	CardID    string `json:"card_id"`
	Name      string `json:"name"`
	IDNumber  string `json:"id_number"`
	Phone     string `json:"phone"`
	Reason    string `json:"reason"`
	ValidFrom string `json:"valid_from"`
	ValidTo   string `json:"valid_to"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// Device 闸机设备
type Device struct {
	ID       string `json:"id"`
	ParkID   string `json:"park_id"`
	Entrance string `json:"entrance"` // 多入口
	Status   string `json:"status"`
	LastBeat string `json:"last_beat"`
}

// Reservation 已预约未入园订单（冻结/解冻不影响）
type Reservation struct {
	ID        string   `json:"id"`
	CardID    string   `json:"card_id"`
	ParkID    string   `json:"park_id"`
	Date      string   `json:"date"`
	Slot      string   `json:"slot"`
	MemberIDs []string `json:"member_ids"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

// GateAttempt 闸机核验请求
type GateAttempt struct {
	RequestID     string      `json:"request_id"` // 客户端幂等键：同一刷脸请求重试保持一致
	DeviceID      string      `json:"device_id"`
	ParkID        string      `json:"park_id"`
	CardID        string      `json:"card_id"`
	QRCode        string      `json:"qr_code"`
	MemberID      string      `json:"member_id"` // 主核验成员（通常为持卡人）
	Face          *FaceSample `json:"face"`
	CompanionIDs  []string    `json:"companion_ids"`  // 一起入园的已绑定成员
	TempGrantIDs  []string    `json:"temp_grant_ids"` // 临时同行授权
	ReservationID string      `json:"reservation_id,omitempty"`
	At            string      `json:"at,omitempty"` // 核验时刻（RFC3339），离线补报时由设备给出
}

// FaceSample 闸机采集的人脸
type FaceSample struct {
	PhotoRef string `json:"photo_ref"`
	Quality  string `json:"quality"` // ok / unclear
	Match    string `json:"match"`   // match / mismatch / unknown（由前端比对设备给出）
}

// GateDecision 闸机返回
type GateDecision struct {
	RequestID      string   `json:"request_id"`
	EventID        string   `json:"event_id"`
	Decision       string   `json:"decision"`
	PrimaryReason  string   `json:"primary_reason"`
	Reasons        []string `json:"reasons"`
	QuotaRemaining int      `json:"quota_remaining"`
	DeductedCount  int      `json:"deducted_count"` // 本次扣减人次（含同行）
	NeedManual     bool     `json:"need_manual"`
	ManualTicket   string   `json:"manual_ticket,omitempty"`
	Message        string   `json:"message"`
	DeviceStatus   string   `json:"device_status"`
	Replayed       bool     `json:"replayed,omitempty"` // 幂等命中（网络抖动重试）
}

// GateEvent 闸机核验流水（客服/现场可查的完整记录）
type GateEvent struct {
	ID            string            `json:"id"`
	RequestID     string            `json:"request_id"`
	DeviceID      string            `json:"device_id"`
	ParkID        string            `json:"park_id"`
	CardID        string            `json:"card_id"`
	MemberID      string            `json:"member_id"`
	CompanionIDs  []string          `json:"companion_ids"`
	At            string            `json:"at"`
	Verdict       string            `json:"verdict"`
	Decision      string            `json:"decision"`
	Reasons       []string          `json:"reasons"`
	DeductedCount int               `json:"deducted_count"`
	QuotaAfter    int               `json:"quota_after"`
	Offline       bool              `json:"offline"` // 离线补报
	Resolved      bool              `json:"resolved"`
	Resolution    *ManualResolution `json:"resolution,omitempty"`
	AppealID      string            `json:"appeal_id,omitempty"`
	RefundedQuota bool              `json:"refunded_quota"`         // 该流水扣减是否已通过申诉/人工回补
	DuplicateOf   string            `json:"duplicate_of,omitempty"` // 重复请求对应的首次流水
	ReservationID string            `json:"reservation_id,omitempty"`
}

// ManualResolution 现场人工处理结果
type ManualResolution struct {
	Action      string `json:"action"`
	Operator    string `json:"operator"`
	Note        string `json:"note"`
	RefundQuota bool   `json:"refund_quota"` // 放行但非游客过错时回补（如设备问题）
	At          string `json:"at"`
}

// Appeal 客服申诉
type Appeal struct {
	ID          string   `json:"id"`
	CardID      string   `json:"card_id"`
	EventIDs    []string `json:"event_ids"`
	Reason      string   `json:"reason"`
	Detail      string   `json:"detail"`
	Status      string   `json:"status"`
	RefundQuota int      `json:"refund_quota"`
	Handler     string   `json:"handler"`
	Note        string   `json:"note"`
	CreatedAt   string   `json:"created_at"`
	HandledAt   string   `json:"handled_at"`
}

// Park 景区与容量
type Park struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DailyCap  int    `json:"daily_cap"` // 日承载量
	SlotStart string `json:"slot_start"`
	SlotEnd   string `json:"slot_end"`
}

// Holiday 节假日限制日历
type Holiday struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

// SaleRule 运营售卖规则（按卡类型）
type SaleRule struct {
	CardType       string `json:"card_type"`
	SaleEnabled    bool   `json:"sale_enabled"`
	AnnualQuota    int    `json:"annual_quota"`
	DailyLimit     int    `json:"daily_limit"`
	HolidayAllowed bool   `json:"holiday_allowed"`
	CompanionLimit int    `json:"companion_limit"`
	Note           string `json:"note"`
}
