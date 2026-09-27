package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"scenicpass/internal/model"
	"scenicpass/internal/store"
)

type OpsService struct {
	st *store.Store
}

func NewOpsService(st *store.Store) *OpsService { return &OpsService{st: st} }

// ---------- 设备 ----------

func (o *OpsService) RegisterDevice(ctx context.Context, id string, in model.RegisterDeviceInput, offlineSec int) (*model.Device, error) {
	d := &model.Device{ID: id, ParkID: in.ParkID, Entrance: in.Entrance, Status: model.DeviceOnline}
	if err := o.st.SaveDevice(ctx, d); err != nil {
		return nil, err
	}
	_ = o.st.Heartbeat(ctx, id, offlineSec)
	return d, nil
}

func (o *OpsService) Heartbeat(ctx context.Context, id string, offlineSec int) error {
	if _, err := o.st.GetDevice(ctx, id); err != nil {
		return fmt.Errorf("%w: 设备未注册", ErrValidation)
	}
	return o.st.Heartbeat(ctx, id, offlineSec)
}

func (o *OpsService) DeviceStatus(ctx context.Context, id string) (string, error) {
	if _, err := o.st.GetDevice(ctx, id); err != nil {
		return "", err
	}
	online, _ := o.st.IsDeviceOnline(ctx, id)
	if online {
		return model.DeviceOnline, nil
	}
	return model.DeviceOffline, nil
}

// ---------- 预约（冻结/解冻不影响已预约未入园订单） ----------

func (o *OpsService) CreateReservation(ctx context.Context, in model.CreateReservationInput, now time.Time) (*model.Reservation, error) {
	card, err := o.st.GetCard(ctx, in.CardID)
	if err != nil {
		return nil, err
	}
	if !contains(card.Parks, in.ParkID) {
		return nil, fmt.Errorf("%w: 该景点不在年卡可入园范围", ErrValidation)
	}
	if in.Date == "" {
		in.Date = now.Format("2006-01-02")
	}
	if isHoliday, _, _ := o.st.IsHoliday(ctx, in.Date); isHoliday && !card.HolidayAllowed {
		return nil, fmt.Errorf("%w: 节假日权益不适用，无法预约", ErrValidation)
	}
	for _, mid := range in.MemberIDs {
		if ok, _ := o.st.MemberBelongsToCard(ctx, in.CardID, mid); !ok {
			return nil, fmt.Errorf("%w: 成员 %s 未绑定到该年卡", ErrValidation, mid)
		}
	}
	r := &model.Reservation{
		ID:        "R" + uuid.NewString()[:12],
		CardID:    in.CardID,
		ParkID:    in.ParkID,
		Date:      in.Date,
		Slot:      in.Slot,
		MemberIDs: in.MemberIDs,
		Status:    model.ReservationBooked,
		CreatedAt: now.Format(time.RFC3339),
	}
	if err := o.st.SaveReservation(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (o *OpsService) ListReservations(ctx context.Context, cardID string) ([]*model.Reservation, error) {
	return o.st.ListReservations(ctx, cardID)
}

// ---------- 现场人工核验处理 ----------

type ManualResult struct {
	Event      *model.GateEvent `json:"event"`
	QuotaAfter int              `json:"quota_after"`
	Action     string           `json:"action"`
}

// ResolveManual 现场工作人员对转人工/疑似转借流水给出结论。
// confirm_pass + refundQuota=true：非游客过错（如设备离线误判）→ 放行且回补/不扣权益。
func (o *OpsService) ResolveManual(ctx context.Context, in model.ResolveManualInput, now time.Time) (*ManualResult, error) {
	ev, err := o.st.GetEvent(ctx, in.EventID)
	if err != nil {
		return nil, err
	}
	if ev.Resolved {
		return nil, fmt.Errorf("%w: 该核验已处理：%s", ErrConflict, ev.Resolution.Action)
	}
	if in.Action != model.ManualConfirmPass && in.Action != model.ManualReject {
		return nil, fmt.Errorf("%w: 非法处理动作", ErrValidation)
	}

	oldVerdict := ev.Verdict
	res := &model.ManualResolution{
		Action:      in.Action,
		Operator:    in.Operator,
		Note:        in.Note,
		RefundQuota: in.RefundQuota,
		At:          now.Format(time.RFC3339),
	}
	ev.Resolved = true
	ev.Resolution = res

	card, _ := o.st.GetCard(ctx, ev.CardID)
	quotaAfter := 0
	if card != nil {
		quotaAfter = card.QuotaRemaining()
	}

	if in.Action == model.ManualConfirmPass {
		// 自动核验未扣减（pending/suspect/deny）→ 人工放行时补登容量；
		// 仅在非游客过错且明确要求时回补（对已扣场景），保证“人工放行后客服可解释”。
		ev.Verdict = model.VerdictPass
		ev.Decision = model.DecisionPass
		seats := partySize(ev)
		if oldVerdict != model.VerdictPass {
			o.st.CapacityIn(ctx, ev.ParkID, seats, now)
			if in.RefundQuota && card != nil {
				// 该流水原本未自动扣额度；这里记录处置口径，不实际扣减，
				// refundQuota=true 表示“免权益放行”。
				ev.RefundedQuota = true
			}
		}
		o.st.AdjustVerdictCounter(ctx, oldVerdict, model.VerdictPass, 1)
	} else {
		ev.Verdict = model.VerdictDeny
		ev.Decision = model.DecisionDeny
		o.st.AdjustVerdictCounter(ctx, oldVerdict, model.VerdictDeny, 1)
	}

	if err := o.st.SaveEvent(ctx, ev); err != nil {
		return nil, err
	}
	_ = o.st.RemoveFromManualQueue(ctx, ev.ID)
	if card, err := o.st.GetCard(ctx, ev.CardID); err == nil {
		quotaAfter = card.QuotaRemaining()
	}
	return &ManualResult{Event: ev, QuotaAfter: quotaAfter, Action: in.Action}, nil
}

func partySize(ev *model.GateEvent) int {
	return 1 + len(ev.CompanionIDs)
}

// ---------- 客服申诉 ----------

func (o *OpsService) CreateAppeal(ctx context.Context, in model.CreateAppealInput, now time.Time) (*model.Appeal, error) {
	if _, err := o.st.GetCard(ctx, in.CardID); err != nil {
		return nil, err
	}
	if len(in.EventIDs) == 0 {
		return nil, fmt.Errorf("%w: 申诉需关联核验流水", ErrValidation)
	}
	a := &model.Appeal{
		ID:        "A" + uuid.NewString()[:12],
		CardID:    in.CardID,
		EventIDs:  in.EventIDs,
		Reason:    in.Reason,
		Detail:    in.Detail,
		Status:    model.AppealOpen,
		CreatedAt: now.Format(time.RFC3339),
	}
	if err := o.st.SaveAppeal(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// ReviewAppeal 申诉成立：逐条回补已扣权益（如闸机离线重复扣次），
// 并在流水上留痕，保证闸机日志/现场处理/客服申诉一致可对账。
func (o *OpsService) ReviewAppeal(ctx context.Context, appealID string, in model.ReviewAppealInput, now time.Time) (*model.Appeal, error) {
	a, err := o.st.GetAppeal(ctx, appealID)
	if err != nil {
		return nil, err
	}
	if a.Status != model.AppealOpen {
		return nil, fmt.Errorf("%w: 申诉已处理", ErrConflict)
	}
	a.Handler = in.Handler
	a.Note = in.Note
	a.HandledAt = now.Format(time.RFC3339)

	if in.Status == model.AppealApproved {
		a.Status = model.AppealApproved
		for _, eid := range a.EventIDs {
			ev, err := o.st.GetEvent(ctx, eid)
			if err != nil {
				continue
			}
			// 只回补“确实扣过且尚未回补”的流水，杜绝重复回补。
			if ev.DeductedCount > 0 && !ev.RefundedQuota {
				if err := o.st.RefundAdmission(ctx, a.CardID, ev.DeductedCount); err == nil {
					ev.RefundedQuota = true
					ev.AppealID = a.ID
					_ = o.st.SaveEvent(ctx, ev)
					a.RefundQuota += ev.DeductedCount
				}
			}
		}
	} else if in.Status == model.AppealRejected {
		a.Status = model.AppealRejected
	} else {
		return nil, fmt.Errorf("%w: 非法申诉结论", ErrValidation)
	}

	if err := o.st.SaveAppeal(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (o *OpsService) GetAppeal(ctx context.Context, id string) (*model.Appeal, error) {
	return o.st.GetAppeal(ctx, id)
}

func (o *OpsService) ListOpenAppeals(ctx context.Context) ([]*model.Appeal, error) {
	return o.st.ListOpenAppeals(ctx)
}

// ---------- 景区 / 节假日 / 售卖规则 ----------

func (o *OpsService) SavePark(ctx context.Context, p *model.Park) error {
	return o.st.SavePark(ctx, p)
}

func (o *OpsService) ListParks(ctx context.Context) ([]*model.Park, error) {
	return o.st.ListParks(ctx)
}

func (o *OpsService) AddHoliday(ctx context.Context, h model.Holiday) error {
	return o.st.AddHoliday(ctx, h)
}

func (o *OpsService) ListHolidays(ctx context.Context) (map[string]string, error) {
	return o.st.ListHolidays(ctx)
}

func (o *OpsService) SaveSaleRule(ctx context.Context, r model.SaleRule) error {
	return o.st.SaveSaleRule(ctx, r)
}

func (o *OpsService) ListSaleRules(ctx context.Context) (map[string]*model.SaleRule, error) {
	return o.st.ListSaleRules(ctx)
}
