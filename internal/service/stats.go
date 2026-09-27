package service

import (
	"context"
	"strings"
	"time"

	"scenicpass/internal/store"
)

type StatsService struct{ st *store.Store }

func NewStatsService(st *store.Store) *StatsService { return &StatsService{st: st} }

// StatsReport 运营看板：卡类型分布、入园峰值、异常核验分布、权益使用率。
type StatsReport struct {
	Date                 string                    `json:"date"`
	CardCountByType      map[string]int            `json:"card_count_by_type"`
	TotalEvents          int64                     `json:"total_events"`
	EventsByVerdict      map[string]int64          `json:"events_by_verdict"`
	EventsByReason       map[string]int64          `json:"events_by_reason"`
	QuotaUsageByCardType map[string]QuotaUsageView `json:"quota_usage_by_card_type"`
	ParkAdmittedToday    map[string]int            `json:"park_admitted_today"`
	PeakByHour           map[string]int64          `json:"peak_by_hour"`
	OpenManualTickets    int64                     `json:"open_manual_tickets"`
	OpenAppeals          int64                     `json:"open_appeals"`
}

type QuotaUsageView struct {
	Cards     int     `json:"cards"`
	Total     int     `json:"total_quota"`
	Used      int     `json:"used_quota"`
	Refunded  int     `json:"refunded_quota"`
	UsageRate float64 `json:"usage_rate"`
}

func (s *StatsService) Report(ctx context.Context, now time.Time) (*StatsReport, error) {
	r := &StatsReport{
		Date:                 now.Format("2006-01-02"),
		CardCountByType:      map[string]int{},
		EventsByVerdict:      map[string]int64{},
		EventsByReason:       map[string]int64{},
		QuotaUsageByCardType: map[string]QuotaUsageView{},
		ParkAdmittedToday:    map[string]int{},
		PeakByHour:           map[string]int64{},
	}

	r.TotalEvents = s.st.CounterInt(ctx, "stats:events:total")

	verdicts := s.st.ScanCounterPrefix(ctx, "stats:events:verdict:")
	for k, v := range verdicts {
		r.EventsByVerdict[strings.TrimPrefix(k, "stats:events:verdict:")] = v
	}
	reasons := s.st.ScanCounterPrefix(ctx, "stats:events:reason:")
	for k, v := range reasons {
		r.EventsByReason[strings.TrimPrefix(k, "stats:events:reason:")] = v
	}

	dateKey := now.Format("20060102")
	peaks := s.st.ScanCounterPrefix(ctx, "stats:peak:"+dateKey+":")
	for k, v := range peaks {
		r.PeakByHour[strings.TrimPrefix(k, "stats:peak:"+dateKey+":")] = v
	}

	// 卡类型分布与权益使用率
	usage := map[string]*QuotaUsageView{}
	ids, _ := s.st.ListCardIDs(ctx)
	for _, id := range ids {
		c, err := s.st.GetCard(ctx, id)
		if err != nil {
			continue
		}
		r.CardCountByType[c.Type]++
		u := usage[c.Type]
		if u == nil {
			u = &QuotaUsageView{}
			usage[c.Type] = u
		}
		u.Cards++
		u.Total += c.AnnualQuota
		u.Used += c.QuotaUsed
		u.Refunded += c.QuotaRefunded
	}
	for t, u := range usage {
		if u.Total > 0 {
			u.UsageRate = float64(u.Used) / float64(u.Total)
		}
		r.QuotaUsageByCardType[t] = *u
	}

	parks, _ := s.st.ListParks(ctx)
	for _, p := range parks {
		n, _ := s.st.ParkAdmitted(ctx, p.ID, dateKey)
		r.ParkAdmittedToday[p.ID] = n
	}

	if q, err := s.st.ManualQueue(ctx, 10000); err == nil {
		r.OpenManualTickets = int64(len(q))
	}
	if ap, err := s.st.ListOpenAppeals(ctx); err == nil {
		r.OpenAppeals = int64(len(ap))
	}
	return r, nil
}
