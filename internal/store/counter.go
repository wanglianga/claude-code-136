package store

import (
	"context"
	"time"
)

// AdjustVerdictCounter 人工处理改变流水结论时修正统计计数。
func (s *Store) AdjustVerdictCounter(ctx context.Context, oldVerdict, newVerdict string, delta int64) {
	if oldVerdict == newVerdict {
		return
	}
	pipe := s.RDB.Pipeline()
	pipe.IncrBy(ctx, "stats:events:verdict:"+oldVerdict, -delta)
	pipe.IncrBy(ctx, "stats:events:verdict:"+newVerdict, delta)
	_, _ = pipe.Exec(ctx)
}

// CapacityIn 人工放行但不扣权益时，仍登记在园人数（容量一致性）。
func (s *Store) CapacityIn(ctx context.Context, parkID string, seats int, at time.Time) {
	date := at.Format("20060102")
	key := s.key("cap", parkID, date)
	pipe := s.RDB.Pipeline()
	pipe.IncrBy(ctx, key, int64(seats))
	pipe.Expire(ctx, key, 172800*time.Second)
	_, _ = pipe.Exec(ctx)
}
