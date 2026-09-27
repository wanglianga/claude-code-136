package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"

	"scenicpass/internal/model"
	"scenicpass/internal/store"
)

// 跨设备短时间连续入园视为疑似转借的时间窗（毫秒）。
const transferWindowMs = 60 * 60 * 1000 // 1 小时内出现在不同设备

type GateService struct {
	st *store.Store
}

func NewGateService(st *store.Store) *GateService { return &GateService{st: st} }

// Verify 在线闸机核验。
func (g *GateService) Verify(ctx context.Context, a model.GateAttempt) (*model.GateDecision, *model.GateEvent, error) {
	now := time.Now()
	if a.At != "" {
		if t, err := time.Parse(time.RFC3339, a.At); err == nil {
			now = t
		}
	}
	return g.verify(ctx, a, now, false)
}

func (g *GateService) verify(ctx context.Context, a model.GateAttempt, now time.Time, offline bool) (*model.GateDecision, *model.GateEvent, error) {
	eventID := "E" + uuid.NewString()[:12]

	// 1) 幂等：同一 request_id 的网络抖动重试直接返回首次结论，绝不重复扣权益。
	if a.RequestID != "" {
		if prior, err := g.st.LookupIdempotent(ctx, a.RequestID); err == nil && prior != nil {
			return g.replayDecision(a, prior), prior, nil
		}
	}

	// 2) 设备与在线状态
	deviceStatus := model.DeviceOnline
	deviceOffline := false
	if _, err := g.st.GetDevice(ctx, a.DeviceID); err != nil {
		deviceStatus = model.DeviceOffline
		deviceOffline = true
	} else {
		online, _ := g.st.IsDeviceOnline(ctx, a.DeviceID)
		if !online {
			deviceStatus = model.DeviceOffline
			deviceOffline = true
		}
	}

	ev := &model.GateEvent{
		ID:            eventID,
		RequestID:     a.RequestID,
		DeviceID:      a.DeviceID,
		ParkID:        a.ParkID,
		CardID:        a.CardID,
		MemberID:      a.MemberID,
		CompanionIDs:  append(append([]string{}, a.CompanionIDs...), a.TempGrantIDs...),
		At:            now.Format(time.RFC3339),
		Offline:       offline,
		ReservationID: a.ReservationID,
	}

	decision := model.DecisionPass
	reasons := []string{}
	addReason := func(r string) { reasons = append(reasons, r) }

	// 在线核验时设备离线：不做自动扣减，转人工并提示走离线补报。
	// 离线补报时设备当时离线属正常，不再作为异常原因。
	if deviceOffline && !offline {
		addReason(model.ReasonDeviceOffline)
		decision = model.DecisionManual
	}

	// 3) 卡与主核验人
	card, err := g.st.GetCard(ctx, a.CardID)
	if err != nil {
		addReason(model.ReasonCardNotFound)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, 0, card, now, deviceStatus)
	}
	member, err := g.st.GetMember(ctx, a.MemberID)
	if err != nil || !member.Active {
		addReason(model.ReasonCompanionUnbound)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, 0, card, now, deviceStatus)
	}
	if ok, _ := g.st.MemberBelongsToCard(ctx, a.CardID, a.MemberID); !ok {
		addReason(model.ReasonCompanionUnbound)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, 0, card, now, deviceStatus)
	}

	// 4) 静态核验项（证件/有效期/景点/节假日/时段/预约/二维码）
	today := now.Format("2006-01-02")
	hardDeny := false
	upgrade := func(d, r string) {
		addReason(r)
		if d == model.DecisionDeny {
			hardDeny = true
			decision = d
		} else if decision != model.DecisionDeny && decision != model.DecisionSuspect {
			decision = d
		}
	}

	if card.Status == model.CardStatusFrozen {
		upgrade(model.DecisionDeny, model.ReasonCardFrozen)
	}
	if card.Status == model.CardStatusRefunded {
		upgrade(model.DecisionDeny, model.ReasonCardRefunded)
	}
	if today < card.ValidFrom || today > card.ValidTo {
		upgrade(model.DecisionDeny, model.ReasonCardExpired)
	}
	if !contains(card.Parks, a.ParkID) {
		upgrade(model.DecisionDeny, model.ReasonParkNotAllowed)
	}
	if isHoliday, _, _ := g.st.IsHoliday(ctx, today); isHoliday && !card.HolidayAllowed {
		upgrade(model.DecisionDeny, model.ReasonHolidayRestricted)
	}
	if !inTimeSlot(now, card.TimeSlotStart, card.TimeSlotEnd) {
		upgrade(model.DecisionDeny, model.ReasonOutsideTimeSlot)
	}
	if park, err := g.st.GetPark(ctx, a.ParkID); err == nil {
		if !inTimeSlot(now, park.SlotStart, park.SlotEnd) {
			upgrade(model.DecisionDeny, model.ReasonOutsideTimeSlot)
		}
	}
	if a.QRCode != "" && a.QRCode != a.CardID {
		upgrade(model.DecisionDeny, model.ReasonQrInvalid)
	}
	if a.ReservationID != "" {
		r, err := g.st.GetReservation(ctx, a.ReservationID)
		if err != nil || r.CardID != a.CardID || r.ParkID != a.ParkID || r.Date != today ||
			r.Status != model.ReservationBooked || !contains(r.MemberIDs, a.MemberID) {
			upgrade(model.DecisionDeny, model.ReasonQrInvalid) // 预约无效按二维码/凭证无效处理
		}
	}

	// 证件有效期（主核验人 + 同行成员全部校验）
	if member.IDExpireAt != "" && member.IDExpireAt < today {
		upgrade(model.DecisionDeny, model.ReasonIDExpired)
	}
	for _, cid := range a.CompanionIDs {
		c, err := g.st.GetMember(ctx, cid)
		if err != nil {
			upgrade(model.DecisionDeny, model.ReasonCompanionUnbound)
			continue
		}
		if ok, _ := g.st.MemberBelongsToCard(ctx, a.CardID, cid); !ok || !c.Active {
			upgrade(model.DecisionDeny, model.ReasonCompanionUnbound)
			continue
		}
		if c.IDExpireAt != "" && c.IDExpireAt < today {
			upgrade(model.DecisionDeny, model.ReasonIDExpired)
		}
	}

	// 临时同行授权校验
	for _, tid := range a.TempGrantIDs {
		tg, err := g.st.GetTempGrant(ctx, tid)
		if err != nil || tg.CardID != a.CardID || tg.Status != model.TempStatusActive ||
			today < tg.ValidFrom || today > tg.ValidTo {
			upgrade(model.DecisionDeny, model.ReasonTempCompanionInvalid)
		}
	}

	// 同行人数（含持卡人）
	seats := 1 + len(a.CompanionIDs) + len(a.TempGrantIDs)
	if seats > card.CompanionLimit {
		upgrade(model.DecisionDeny, model.ReasonCompanionOverLimit)
	}

	// 5) 人脸核验
	if member.FaceExempt {
		// 老人等无法刷脸：跳过自动比对，转人工核验证件后放行
		upgrade(model.DecisionManual, model.ReasonFaceExempt)
	} else if a.Face == nil {
		upgrade(model.DecisionManual, model.ReasonPhotoUnclear)
	} else {
		switch {
		case a.Face.Quality == "unclear":
			upgrade(model.DecisionManual, model.ReasonPhotoUnclear)
		case a.Face.Match == "mismatch":
			addReason(model.ReasonPhotoMismatch)
			decision = model.DecisionSuspect
		case a.Face.Match == "unknown":
			upgrade(model.DecisionManual, model.ReasonPhotoUnclear)
		}
	}
	if member.PhotoQuality == "unclear" {
		upgrade(model.DecisionManual, model.ReasonPhotoUnclear)
	}

	if hardDeny || decision == model.DecisionDeny || decision == model.DecisionSuspect ||
		decision == model.DecisionManual {
		return g.finalize(ctx, a, ev, decision, reasons, seats, card, now, deviceStatus)
	}

	// 6) 静态项全部通过：原子扣减（额度/高频/跨设备转借/容量/幂等一次完成）
	park, _ := g.st.GetPark(ctx, a.ParkID)
	dailyCap := 0
	if park != nil {
		dailyCap = park.DailyCap
	}
	res, err := g.st.CommitAdmission(ctx, store.CommitArgs{
		RequestID:  a.RequestID,
		EventID:    eventID,
		CardID:     a.CardID,
		MemberID:   a.MemberID,
		ParkID:     a.ParkID,
		DeviceID:   a.DeviceID,
		At:         now,
		WindowMs:   transferWindowMs,
		DailyLimit: card.DailyLimit,
		Seats:      seats,
		DailyCap:   dailyCap,
	})
	if err != nil {
		return nil, nil, err
	}
	switch res.Code {
	case store.CommitOK:
		// 放行
	case store.CommitReplay:
		// 并发场景下首个请求可能刚完成 Lua 扣减、流水尚在落库，短暂等待其可见。
		var prior *model.GateEvent
		for i := 0; i < 30; i++ {
			prior, err = g.st.GetEvent(ctx, res.Detail)
			if err == nil && prior != nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if prior != nil {
			return g.replayDecision(a, prior), prior, nil
		}
		addReason(model.ReasonDuplicateDeduction)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, seats, card, now, deviceStatus)
	case store.CommitFrozen:
		addReason(model.ReasonCardFrozen)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, seats, card, now, deviceStatus)
	case store.CommitQuota:
		addReason(model.ReasonQuotaInsufficient)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, seats, card, now, deviceStatus)
	case store.CommitHighFreq:
		addReason(model.ReasonHighFrequency)
		return g.finalize(ctx, a, ev, model.DecisionManual, reasons, seats, card, now, deviceStatus)
	case store.CommitTransfer:
		addReason(model.ReasonSuspectedTransfer)
		return g.finalize(ctx, a, ev, model.DecisionSuspect, reasons, seats, card, now, deviceStatus)
	case store.CommitCapFull:
		addReason(model.ReasonParkFull)
		return g.finalize(ctx, a, ev, model.DecisionDeny, reasons, seats, card, now, deviceStatus)
	}

	// 扣减成功：刷新卡、核销预约（CAS 保证并发核验下预约只核销一次）
	card, _ = g.st.GetCard(ctx, a.CardID)
	if a.ReservationID != "" {
		_, _ = g.st.ReserveReservationStatus(ctx, a.ReservationID, model.ReservationBooked, model.ReservationUsed)
	}
	addReason(model.ReasonOK)
	return g.finalize(ctx, a, ev, model.DecisionPass, reasons, seats, card, now, deviceStatus)
}

// finalize 落库核验流水并组装闸机返回。
func (g *GateService) finalize(ctx context.Context, a model.GateAttempt, ev *model.GateEvent,
	decision string, reasons []string, seats int, card *model.Card, now time.Time, deviceStatus string) (
	*model.GateDecision, *model.GateEvent, error) {

	if len(reasons) == 0 {
		reasons = []string{model.ReasonOK}
	}
	ev.Decision = decision
	ev.Reasons = reasons
	verdict := map[string]string{
		model.DecisionPass:    model.VerdictPass,
		model.DecisionManual:  model.VerdictPending,
		model.DecisionDeny:    model.VerdictDeny,
		model.DecisionSuspect: model.VerdictSuspect,
	}[decision]
	ev.Verdict = verdict

	remaining := 0
	if card != nil {
		remaining = card.QuotaRemaining()
	}
	if decision == model.DecisionPass {
		ev.DeductedCount = seats
		ev.QuotaAfter = remaining
	}

	if err := g.st.SaveEvent(ctx, ev); err != nil {
		return nil, nil, err
	}
	_ = g.st.IncEventCounters(ctx, ev)

	d := &model.GateDecision{
		RequestID:      a.RequestID,
		EventID:        ev.ID,
		Decision:       decision,
		PrimaryReason:  reasons[0],
		Reasons:        reasons,
		QuotaRemaining: remaining,
		DeductedCount:  ev.DeductedCount,
		NeedManual:     decision == model.DecisionManual || decision == model.DecisionSuspect,
		DeviceStatus:   deviceStatus,
		Message:        messageFor(decision, reasons),
	}
	if d.NeedManual {
		d.ManualTicket = ev.ID
	}
	return d, ev, nil
}

func (g *GateService) replayDecision(a model.GateAttempt, prior *model.GateEvent) *model.GateDecision {
	reasons := append([]string{}, prior.Reasons...)
	reasons = append(reasons, model.ReasonDuplicateDeduction)
	return &model.GateDecision{
		RequestID:      a.RequestID,
		EventID:        prior.ID,
		Decision:       prior.Decision,
		PrimaryReason:  prior.Reasons[0],
		Reasons:        reasons,
		QuotaRemaining: prior.QuotaAfter,
		DeductedCount:  0, // 重试不再扣减
		NeedManual:     prior.Verdict == model.VerdictPending || prior.Verdict == model.VerdictSuspect,
		ManualTicket:   ternary(prior.Verdict == model.VerdictPending, prior.ID, ""),
		DeviceStatus:   model.DeviceOnline,
		Message:        "重复请求，已返回首次核验结果（未重复扣减权益）",
		Replayed:       true,
	}
}

// OfflineBatch 设备联网后补报离线期间的核验流水：去重、不重复扣次。
func (g *GateService) OfflineBatch(ctx context.Context, in model.OfflineBatchInput) ([]*model.GateDecision, error) {
	batchAt := time.Now()
	if in.At != "" {
		if t, err := time.Parse(time.RFC3339, in.At); err == nil {
			batchAt = t
		}
	}
	out := make([]*model.GateDecision, 0, len(in.Attempts))
	for _, a := range in.Attempts {
		if a.DeviceID == "" {
			a.DeviceID = in.DeviceID
		}
		at := batchAt
		if a.At != "" {
			if t, err := time.Parse(time.RFC3339, a.At); err == nil {
				at = t
			}
		}
		fp := a.RequestID
		if fp == "" {
			fp = offlineFingerprint(a.DeviceID, a.CardID, a.MemberID, at)
			a.RequestID = "offline-" + fp
		}
		first, err := g.st.MarkOfflineFinger(ctx, a.DeviceID, fp)
		if err != nil {
			return nil, err
		}
		if !first {
			// 设备离线期间重试导致的重复上报：记录但不扣减，并关联首次流水
			ev := g.duplicateEvent(a, at)
			decision := model.DecisionDeny
			verdict := model.VerdictDeny
			if prior, perr := g.st.LookupIdempotent(ctx, a.RequestID); perr == nil && prior != nil {
				ev.DuplicateOf = prior.ID
				ev.QuotaAfter = prior.QuotaAfter
				ev.Reasons = append(append([]string{}, prior.Reasons...), model.ReasonDuplicateDeduction)
				decision = prior.Decision
				verdict = prior.Verdict
			}
			ev.Decision = decision
			ev.Verdict = verdict
			_ = g.st.SaveEvent(ctx, ev)
			_ = g.st.IncEventCounters(ctx, ev)
			out = append(out, &model.GateDecision{
				RequestID:     a.RequestID,
				EventID:       ev.ID,
				Decision:      decision,
				PrimaryReason: model.ReasonDuplicateDeduction,
				Reasons:       ev.Reasons,
				NeedManual:    false,
				Message:       "离线重复上报已拦截，未重复扣减权益",
				Replayed:      true,
			})
			continue
		}
		d, _, err := g.verify(ctx, a, at, true)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (g *GateService) duplicateEvent(a model.GateAttempt, at time.Time) *model.GateEvent {
	return &model.GateEvent{
		ID:            "E" + uuid.NewString()[:12],
		RequestID:     a.RequestID,
		DeviceID:      a.DeviceID,
		ParkID:        a.ParkID,
		CardID:        a.CardID,
		MemberID:      a.MemberID,
		CompanionIDs:  append(append([]string{}, a.CompanionIDs...), a.TempGrantIDs...),
		At:            at.Format(time.RFC3339),
		Verdict:       model.VerdictDeny,
		Decision:      model.DecisionDeny,
		Reasons:       []string{model.ReasonDuplicateDeduction},
		Offline:       true,
		ReservationID: a.ReservationID,
	}
}

func offlineFingerprint(deviceID, cardID, memberID string, at time.Time) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%s|%s", deviceID, cardID, memberID, at.Format("200601021504"))))
	return hex.EncodeToString(sum[:])
}

// ---- 查询 ----

func (g *GateService) GetEvent(ctx context.Context, id string) (*model.GateEvent, error) {
	return g.st.GetEvent(ctx, id)
}

func (g *GateService) ListCardEvents(ctx context.Context, cardID string, limit int64) ([]*model.GateEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return g.st.ListCardEvents(ctx, cardID, limit)
}

func (g *GateService) ListParkEvents(ctx context.Context, parkID string, limit int64) ([]*model.GateEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return g.st.ListParkEvents(ctx, parkID, limit)
}

func (g *GateService) ManualQueue(ctx context.Context, limit int64) ([]*model.GateEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return g.st.ManualQueue(ctx, limit)
}

// ---- 辅助 ----

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func inTimeSlot(t time.Time, start, end string) bool {
	if start == "" || end == "" {
		return true
	}
	cur := t.Hour()*60 + t.Minute()
	sh, sm := parseHHMM(start)
	eh, em := parseHHMM(end)
	return cur >= sh*60+sm && cur <= eh*60+em
}

func parseHHMM(s string) (int, int) {
	var h, m int
	fmt.Sscanf(s, "%d:%d", &h, &m)
	return h, m
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func messageFor(decision string, reasons []string) string {
	switch decision {
	case model.DecisionPass:
		return "放行"
	case model.DecisionManual:
		return "转人工核验：" + reasons[0]
	case model.DecisionSuspect:
		return "疑似转借，请工作人员介入：" + reasons[0]
	default:
		return "拒绝入园：" + reasons[0]
	}
}
