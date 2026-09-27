package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"scenicpass/internal/model"
)

// 运营统计计数器（每次核验事件累加）
func (s *Store) IncEventCounters(ctx context.Context, e *model.GateEvent) error {
	ts, err := time.Parse(time.RFC3339, e.At)
	if err != nil {
		ts = time.Now()
	}
	date := ts.Format("20060102")
	hour := ts.Format("15")
	pipe := s.RDB.Pipeline()
	pipe.Incr(ctx, "stats:events:total")
	pipe.Incr(ctx, "stats:events:verdict:"+e.Verdict)
	pipe.Incr(ctx, "stats:peak:"+date+":"+hour)
	pipe.Expire(ctx, "stats:peak:"+date+":"+hour, 400*24*time.Hour)
	for _, r := range e.Reasons {
		if r == model.ReasonOK {
			continue
		}
		pipe.Incr(ctx, "stats:events:reason:"+r)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) CounterInt(ctx context.Context, key string) int64 {
	n, _ := s.RDB.Get(ctx, key).Int64()
	return n
}

func (s *Store) ScanCounterPrefix(ctx context.Context, prefix string) map[string]int64 {
	out := map[string]int64{}
	var cursor uint64
	for {
		keys, c, err := s.RDB.Scan(ctx, cursor, prefix+"*", 200).Result()
		if err != nil {
			break
		}
		for _, k := range keys {
			out[k] = s.CounterInt(ctx, k)
		}
		cursor = c
		if cursor == 0 {
			break
		}
	}
	return out
}

// ListCardIDs 返回全部年卡 ID
func (s *Store) ListCardIDs(ctx context.Context) ([]string, error) {
	return s.RDB.SMembers(ctx, "cards").Result()
}

// ---- 离线核验指纹：24h 去重，48h TTL ----

func offlineFingerKey(deviceID, fp string) string {
	return "offline:finger:" + deviceID + ":" + fp
}

// MarkOfflineFinger 首次返回 true；指纹已存在返回 false（离线重复扣次）。
func (s *Store) MarkOfflineFinger(ctx context.Context, deviceID, fp string) (bool, error) {
	key := offlineFingerKey(deviceID, fp)
	// SET NX：仅当指纹不存在时写入成功，从而识别设备本地重试导致的重复上报。
	return s.RDB.SetNX(ctx, key, "1", 48*time.Hour).Result()
}

// ---- 预约状态原子更新（冻结期间预约仍可查询/保留） ----

func (s *Store) ReserveReservationStatus(ctx context.Context, id, from, to string) (bool, error) {
	// 轻量 CAS：由 service 层读改写，这里通过 WATCH 保证不并发踩踏
	txf := func(tx *redis.Tx) error {
		var r model.Reservation
		if err := getJSON(ctx, tx, resvKey(id), &r); err != nil {
			return err
		}
		if r.Status != from {
			return errCasMismatch
		}
		r.Status = to
		b, _ := json.Marshal(r)
		_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			return pipe.Set(ctx, resvKey(id), b, 0).Err()
		})
		return err
	}
	err := s.RDB.Watch(ctx, txf, resvKey(id))
	if err == errCasMismatch {
		return false, nil
	}
	return err == nil, err
}

var errCasMismatch = casErr{}

type casErr struct{}

func (casErr) Error() string { return "cas mismatch" }
