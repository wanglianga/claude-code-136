package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"scenicpass/internal/model"
	"scenicpass/internal/store"
)

var (
	ErrValidation = errors.New("validation error")
	ErrConflict   = errors.New("conflict")
)

type CardService struct {
	st *store.Store
}

func NewCardService(st *store.Store) *CardService { return &CardService{st: st} }

// SeedDefaults 写入默认景区、节假日与售卖规则（仅在缺失时）。
func (s *CardService) SeedDefaults(ctx context.Context, now time.Time) error {
	parks := []model.Park{
		{ID: "P001", Name: "主峰景区", DailyCap: 5000, SlotStart: "07:00", SlotEnd: "17:00"},
		{ID: "P002", Name: "峡谷景区", DailyCap: 3000, SlotStart: "08:00", SlotEnd: "16:30"},
		{ID: "P003", Name: "亲子乐园", DailyCap: 2000, SlotStart: "09:00", SlotEnd: "18:00"},
	}
	for _, p := range parks {
		if _, err := s.st.GetPark(ctx, p.ID); errors.Is(err, store.ErrNotFound) {
			if err := s.st.SavePark(ctx, &p); err != nil {
				return err
			}
		}
	}
	holidays := []model.Holiday{
		{Date: "2026-10-01", Name: "国庆节"}, {Date: "2026-10-02", Name: "国庆节"},
		{Date: "2026-10-03", Name: "国庆节"},
	}
	for _, h := range holidays {
		if ok, _, _ := s.st.IsHoliday(ctx, h.Date); !ok {
			if err := s.st.AddHoliday(ctx, h); err != nil {
				return err
			}
		}
	}
	rules := []model.SaleRule{
		{CardType: model.CardTypeSingle, SaleEnabled: true, AnnualQuota: 60, DailyLimit: 1, HolidayAllowed: true, CompanionLimit: 1, Note: "单人年卡：仅本人"},
		{CardType: model.CardTypeFamily, SaleEnabled: true, AnnualQuota: 240, DailyLimit: 2, HolidayAllowed: false, CompanionLimit: 4, Note: "家庭年卡：最多4人同行，法定节假日不可用"},
		{CardType: model.CardTypeParent, SaleEnabled: true, AnnualQuota: 120, DailyLimit: 1, HolidayAllowed: true, CompanionLimit: 2, Note: "亲子年卡：1成人+1儿童"},
	}
	for _, r := range rules {
		if _, err := s.st.GetSaleRule(ctx, r.CardType); errors.Is(err, store.ErrNotFound) {
			if err := s.st.SaveSaleRule(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func defaultSlot(cardType string) (string, string) { return "00:00", "23:59" }

// CreateCard 游客购买年卡：保存实名信息、人脸、证件、额度、景点、节假日限制等。
func (s *CardService) CreateCard(ctx context.Context, in model.CreateCardInput, now time.Time) (*model.Card, error) {
	rule, err := s.st.GetSaleRule(ctx, in.Type)
	if err != nil {
		return nil, fmt.Errorf("%w: 未知年卡类型 %s", ErrValidation, in.Type)
	}
	if !rule.SaleEnabled {
		return nil, fmt.Errorf("%w: %s 年卡已暂停售卖", ErrValidation, in.Type)
	}
	if len(in.Members) == 0 || in.Members[0].Role == "" {
		return nil, fmt.Errorf("%w: 至少需要一名持卡人实名信息", ErrValidation)
	}
	if len(in.Parks) == 0 {
		return nil, fmt.Errorf("%w: 需选择可入园景点", ErrValidation)
	}
	validFrom := orDefault(in.ValidFrom, now.Format("2006-01-02"))
	validTo := in.ValidTo
	if validTo == "" {
		t, _ := time.Parse("2006-01-02", validFrom)
		validTo = t.AddDate(1, 0, 0).Format("2006-01-02")
	}
	slotStart, slotEnd := defaultSlot(in.Type)
	if in.TimeSlotStart != "" {
		slotStart = in.TimeSlotStart
	}
	if in.TimeSlotEnd != "" {
		slotEnd = in.TimeSlotEnd
	}
	holiday := rule.HolidayAllowed
	if in.HolidayAllowed != nil {
		holiday = *in.HolidayAllowed
	}
	card := &model.Card{
		ID:             "C" + uuid.NewString()[:12],
		Type:           in.Type,
		Status:         model.CardStatusActive,
		HolderName:     orDefault(in.HolderName, in.Members[0].Name),
		Phone:          in.Phone,
		ValidFrom:      validFrom,
		ValidTo:        validTo,
		CompanionLimit: rule.CompanionLimit,
		Parks:          in.Parks,
		HolidayAllowed: holiday,
		TimeSlotStart:  slotStart,
		TimeSlotEnd:    slotEnd,
		AnnualQuota:    firstPositive(in.AnnualQuota, rule.AnnualQuota),
		DailyLimit:     firstPositive(in.DailyLimit, rule.DailyLimit),
		CreatedAt:      now.Format(time.RFC3339),
	}
	if err := s.st.SaveCard(ctx, card); err != nil {
		return nil, err
	}
	for i, mi := range in.Members {
		mi.Role = normalizeRole(mi.Role, i == 0)
		if err := s.bindMember(ctx, card.ID, mi, now); err != nil {
			return nil, err
		}
	}
	return card, nil
}

func normalizeRole(role string, isHolder bool) string {
	if isHolder {
		return model.RoleHolder
	}
	if role == "" {
		return model.RoleOther
	}
	return role
}

func (s *CardService) bindMember(ctx context.Context, cardID string, mi model.CreateMemberInput, now time.Time) error {
	if mi.Name == "" || mi.IDNumber == "" {
		return fmt.Errorf("%w: 成员姓名与证件号必填", ErrValidation)
	}
	quality := mi.PhotoQuality
	if quality == "" {
		quality = "ok"
	}
	idType := mi.IDType
	if idType == "" {
		idType = "idcard"
	}
	mb := &model.Member{
		ID:           "M" + uuid.NewString()[:12],
		Name:         mi.Name,
		IDType:       idType,
		IDNumber:     mi.IDNumber,
		IDExpireAt:   mi.IDExpireAt,
		Phone:        mi.Phone,
		PhotoRef:     mi.PhotoRef,
		PhotoQuality: quality,
		Role:         mi.Role,
		FaceExempt:   mi.FaceExempt,
		BirthDate:    mi.BirthDate,
		BoundAt:      now.Format(time.RFC3339),
		Active:       true,
	}
	return s.st.SaveMember(ctx, cardID, mb)
}

// AddMember 家庭成员追加绑定（受同行人数上限约束）。
func (s *CardService) AddMember(ctx context.Context, cardID string, in model.AddMemberInput, now time.Time) (*model.Member, error) {
	card, err := s.st.GetCard(ctx, cardID)
	if err != nil {
		return nil, err
	}
	existing, _ := s.st.ListMembers(ctx, cardID)
	if len(existing) >= card.CompanionLimit {
		return nil, fmt.Errorf("%w: 已达年卡同行人数上限 %d", ErrValidation, card.CompanionLimit)
	}
	in.Role = normalizeRole(in.Role, false)
	if err := s.bindMember(ctx, cardID, in.CreateMemberInput, now); err != nil {
		return nil, err
	}
	members, _ := s.st.ListMembers(ctx, cardID)
	return members[len(members)-1], nil
}

// GetCardView 聚合卡 + 成员 + 临时授权（客服/现场查看）。
func (s *CardService) GetCardView(ctx context.Context, cardID string) (*model.CardView, error) {
	card, err := s.st.GetCard(ctx, cardID)
	if err != nil {
		return nil, err
	}
	members, _ := s.st.ListMembers(ctx, cardID)
	grants, _ := s.st.ListTempGrants(ctx, cardID)
	return &model.CardView{Card: card, Members: members, TempGrants: grants}, nil
}

// ChangePhone 换绑手机号（实名信息不变）。
func (s *CardService) ChangePhone(ctx context.Context, cardID, phone string) error {
	if _, err := s.st.GetCard(ctx, cardID); err != nil {
		return err
	}
	if phone == "" {
		return fmt.Errorf("%w: 手机号不能为空", ErrValidation)
	}
	return s.st.UpdatePhone(ctx, cardID, phone)
}

// UpdatePhoto 孩子长大照片变化等场景：更新人脸底库，质量恢复为可用。
func (s *CardService) UpdatePhoto(ctx context.Context, cardID, memberID string, in model.UpdatePhotoInput) error {
	if ok, _ := s.st.MemberBelongsToCard(ctx, cardID, memberID); !ok {
		return fmt.Errorf("%w: 成员不属于该年卡", ErrValidation)
	}
	mb, err := s.st.GetMember(ctx, memberID)
	if err != nil {
		return err
	}
	mb.PhotoRef = in.PhotoRef
	if in.PhotoQuality != "" {
		mb.PhotoQuality = in.PhotoQuality
	} else {
		mb.PhotoQuality = "ok"
	}
	return s.st.SaveMember(ctx, cardID, mb)
}

// CreateTempGrant 家庭成员临时更换：限期实名授权。
func (s *CardService) CreateTempGrant(ctx context.Context, in model.CreateTempGrantInput, now time.Time) (*model.TempCompanionGrant, error) {
	card, err := s.st.GetCard(ctx, in.CardID)
	if err != nil {
		return nil, err
	}
	if in.Name == "" || in.IDNumber == "" {
		return nil, fmt.Errorf("%w: 临时同行人需实名", ErrValidation)
	}
	validFrom := orDefault(in.ValidFrom, now.Format("2006-01-02"))
	validTo := in.ValidTo
	if validTo == "" {
		t, _ := time.Parse("2006-01-02", validFrom)
		validTo = t.AddDate(0, 0, 7).Format("2006-01-02")
	}
	if validTo < validFrom {
		return nil, fmt.Errorf("%w: 授权结束时间早于开始时间", ErrValidation)
	}
	_ = card
	g := &model.TempCompanionGrant{
		ID:        "T" + uuid.NewString()[:12],
		CardID:    in.CardID,
		Name:      in.Name,
		IDNumber:  in.IDNumber,
		Phone:     in.Phone,
		Reason:    in.Reason,
		ValidFrom: validFrom,
		ValidTo:   validTo,
		Status:    model.TempStatusActive,
		CreatedAt: now.Format(time.RFC3339),
	}
	if err := s.st.SaveTempGrant(ctx, g); err != nil {
		return nil, err
	}
	return g, nil
}

// Freeze / Unfreeze：仅改卡状态；已预约未入园订单不受影响。
func (s *CardService) Freeze(ctx context.Context, cardID string) error {
	if _, err := s.st.GetCard(ctx, cardID); err != nil {
		return err
	}
	return s.st.SetCardStatus(ctx, cardID, model.CardStatusFrozen)
}

func (s *CardService) Unfreeze(ctx context.Context, cardID string) error {
	card, err := s.st.GetCard(ctx, cardID)
	if err != nil {
		return err
	}
	if card.Status != model.CardStatusFrozen {
		return fmt.Errorf("%w: 年卡当前状态为 %s，无需解冻", ErrConflict, card.Status)
	}
	return s.st.SetCardStatus(ctx, cardID, model.CardStatusActive)
}

// Refund 退款：年卡作废，有效期截止当日；预约同步取消。
func (s *CardService) Refund(ctx context.Context, cardID, reason string, now time.Time) error {
	card, err := s.st.GetCard(ctx, cardID)
	if err != nil {
		return err
	}
	today := now.Format("2006-01-02")
	card.Status = model.CardStatusRefunded
	card.ValidTo = today
	if err := s.st.SaveCard(ctx, card); err != nil {
		return err
	}
	resvs, _ := s.st.ListReservations(ctx, cardID)
	for _, r := range resvs {
		if r.Status == model.ReservationBooked {
			r.Status = model.ReservationCanceled
			_ = s.st.UpdateReservation(ctx, r)
		}
	}
	return nil
}

// Upgrade 升级年卡：按新类型重新计算同行人数、景点权益、额度与有效期。
// 已使用额度结转，新有效期自升级日起顺延一年。
func (s *CardService) Upgrade(ctx context.Context, cardID, newType string, now time.Time) (*model.Card, error) {
	card, err := s.st.GetCard(ctx, cardID)
	if err != nil {
		return nil, err
	}
	if card.Status == model.CardStatusRefunded {
		return nil, fmt.Errorf("%w: 已退款年卡不能升级", ErrConflict)
	}
	rule, err := s.st.GetSaleRule(ctx, newType)
	if err != nil {
		return nil, fmt.Errorf("%w: 未知年卡类型 %s", ErrValidation, newType)
	}
	card.Type = newType
	card.CompanionLimit = rule.CompanionLimit
	card.HolidayAllowed = rule.HolidayAllowed
	card.AnnualQuota = rule.AnnualQuota
	card.ValidFrom = now.Format("2006-01-02")
	card.ValidTo = now.AddDate(1, 0, 0).Format("2006-01-02")
	card.Version++
	if err := s.st.UpdateCardAfterUpgrade(ctx, card); err != nil {
		return nil, err
	}
	return card, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
