package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"scenicpass/internal/model"
)

var ErrNotFound = errors.New("not found")

// 提交结果码（Lua 返回）
const (
	CommitOK       = 1
	CommitReplay   = 2
	CommitFrozen   = 3
	CommitQuota    = 4
	CommitHighFreq = 5
	CommitTransfer = 6
	CommitCapFull  = 7
)

// commitScript 原子完成：幂等判重 -> 状态 -> 剩余额度 -> 当日高频 ->
// 跨设备短时间移动（疑似转借）-> 景区容量 -> 扣减。
//
// KEYS[1]=idem:{requestID}
// KEYS[2]=card:{cardID}            (hash, 含 status / quota_used / annual_quota)
// KEYS[3]=entrycnt:{memberID}:{date}
// KEYS[4]=moves:{memberID}:{date}  (zset: member=`eventID|deviceID`, score=tsMs)
// KEYS[5]=cap:{parkID}:{date}
//
// ARGV: requestID, eventID, nowTsMs, deviceID, windowMs, dailyLimit,
//
//	seats, dailyCap, ttlSec, override(0|1)
var commitScript = redis.NewScript(`
local prior = redis.call('GET', KEYS[1])
if prior ~= false then
  return {2, prior}
end
local status = redis.call('HGET', KEYS[2], 'status')
local override = tonumber(ARGV[10])
if (status == 'frozen' or status == 'refunded') and override == 0 then
  return {3, status}
end
local used = tonumber(redis.call('HGET', KEYS[2], 'quota_used') or '0')
local total = tonumber(redis.call('HGET', KEYS[2], 'annual_quota') or '0')
local seats = tonumber(ARGV[7])
if used + seats > total and override == 0 then
  return {4, tostring(total - used)}
end
-- 跨设备短时间移动（疑似转借）优先于一般高频判定
local nowTs = tonumber(ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[4], '-inf', nowTs - 86400000)
local last = redis.call('ZREVRANGEBYSCORE', KEYS[4], nowTs, '-inf', 'LIMIT', 0, 1)
if #last > 0 and override == 0 then
  local lastScore = tonumber(redis.call('ZSCORE', KEYS[4], last[1]))
  local div = string.find(last[1], '|', 1, true)
  local lastDev = ''
  if div then lastDev = string.sub(last[1], div + 1) end
  if lastScore and (nowTs - lastScore) < tonumber(ARGV[5]) and lastDev ~= ARGV[4] then
    return {6, last[1]}
  end
end
-- 同一设备当日多次入园：高频异常，转人工
local dailyLimit = tonumber(ARGV[6])
local cnt = tonumber(redis.call('GET', KEYS[3]) or '0')
if cnt + 1 > dailyLimit and override == 0 then
  return {5, tostring(cnt)}
end
local cap = tonumber(ARGV[8])
if cap > 0 then
  local inpark = tonumber(redis.call('GET', KEYS[5]) or '0')
  if inpark + seats > cap and override == 0 then
    return {7, tostring(inpark)}
  end
  redis.call('INCRBY', KEYS[5], seats)
  redis.call('EXPIRE', KEYS[5], tonumber(ARGV[9]))
end
redis.call('SET', KEYS[1], ARGV[2], 'EX', tonumber(ARGV[9]))
redis.call('HINCRBY', KEYS[2], 'quota_used', seats)
redis.call('INCR', KEYS[3])
redis.call('EXPIRE', KEYS[3], tonumber(ARGV[9]))
redis.call('ZADD', KEYS[4], nowTs, ARGV[2] .. '|' .. ARGV[4])
redis.call('EXPIRE', KEYS[4], tonumber(ARGV[9]))
return {1, ARGV[2]}
`)

type Store struct {
	RDB *redis.Client
}

func New(addr, db string) (*Store, error) {
	n, _ := strconv.Atoi(db)
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: n})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &Store{RDB: rdb}, nil
}

func NewWithClient(c *redis.Client) *Store { return &Store{RDB: c} }

// ---------- 工具 ----------

func (s *Store) key(parts ...string) string { return strings.Join(parts, ":") }

func setJSON(ctx context.Context, rdb redis.Cmdable, key string, v interface{}, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if ttl > 0 {
		return rdb.Set(ctx, key, b, ttl).Err()
	}
	return rdb.Set(ctx, key, b, 0).Err()
}

func getJSON(ctx context.Context, rdb redis.Cmdable, key string, v interface{}) error {
	b, err := rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ---------- 年卡（hash 存储，便于 Lua 原子读写额度与状态） ----------

func cardKey(id string) string { return "card:" + id }

// SaveCard 全量写入 card hash（保留 quota_used 由调用方给定）。
func (s *Store) SaveCard(ctx context.Context, c *model.Card) error {
	fields := map[string]interface{}{
		"id":              c.ID,
		"type":            c.Type,
		"status":          c.Status,
		"holder_name":     c.HolderName,
		"phone":           c.Phone,
		"valid_from":      c.ValidFrom,
		"valid_to":        c.ValidTo,
		"companion_limit": c.CompanionLimit,
		"parks":           strings.Join(c.Parks, ","),
		"holiday_allowed": boolStr(c.HolidayAllowed),
		"time_slot_start": c.TimeSlotStart,
		"time_slot_end":   c.TimeSlotEnd,
		"annual_quota":    c.AnnualQuota,
		"quota_used":      c.QuotaUsed,
		"quota_refunded":  c.QuotaRefunded,
		"daily_limit":     c.DailyLimit,
		"version":         c.Version,
		"created_at":      c.CreatedAt,
	}
	if err := s.RDB.HSet(ctx, cardKey(c.ID), fields).Err(); err != nil {
		return err
	}
	return s.RDB.SAdd(ctx, "cards", c.ID).Err()
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func (s *Store) GetCard(ctx context.Context, id string) (*model.Card, error) {
	m, err := s.RDB.HGetAll(ctx, cardKey(id)).Result()
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		return nil, ErrNotFound
	}
	return cardFromHash(m), nil
}

func cardFromHash(m map[string]string) *model.Card {
	atoi := func(k string) int { n, _ := strconv.Atoi(m[k]); return n }
	parks := []string{}
	if m["parks"] != "" {
		parks = strings.Split(m["parks"], ",")
	}
	return &model.Card{
		ID:             m["id"],
		Type:           m["type"],
		Status:         m["status"],
		HolderName:     m["holder_name"],
		Phone:          m["phone"],
		ValidFrom:      m["valid_from"],
		ValidTo:        m["valid_to"],
		CompanionLimit: atoi("companion_limit"),
		Parks:          parks,
		HolidayAllowed: m["holiday_allowed"] == "1",
		TimeSlotStart:  m["time_slot_start"],
		TimeSlotEnd:    m["time_slot_end"],
		AnnualQuota:    atoi("annual_quota"),
		QuotaUsed:      atoi("quota_used"),
		QuotaRefunded:  atoi("quota_refunded"),
		DailyLimit:     atoi("daily_limit"),
		Version:        int64(atoi("version")),
		CreatedAt:      m["created_at"],
	}
}

// SetCardStatus / IncQuotaRefunded 供冻结、退款、升级、申诉复用。
func (s *Store) SetCardStatus(ctx context.Context, id, status string) error {
	return s.RDB.HSet(ctx, cardKey(id), "status", status).Err()
}

func (s *Store) IncQuotaUsed(ctx context.Context, id string, delta int) (int64, error) {
	return s.RDB.HIncrBy(ctx, cardKey(id), "quota_used", int64(delta)).Result()
}

func (s *Store) IncQuotaRefunded(ctx context.Context, id string, delta int) error {
	return s.RDB.HIncrBy(ctx, cardKey(id), "quota_refunded", int64(delta)).Err()
}

func (s *Store) UpdateCardAfterUpgrade(ctx context.Context, c *model.Card) error {
	return s.RDB.HSet(ctx, cardKey(c.ID), map[string]interface{}{
		"type":            c.Type,
		"companion_limit": c.CompanionLimit,
		"parks":           strings.Join(c.Parks, ","),
		"holiday_allowed": boolStr(c.HolidayAllowed),
		"annual_quota":    c.AnnualQuota,
		"quota_used":      c.QuotaUsed,
		"valid_from":      c.ValidFrom,
		"valid_to":        c.ValidTo,
		"version":         c.Version,
	}).Err()
}

func (s *Store) UpdatePhone(ctx context.Context, cardID, phone string) error {
	return s.RDB.HSet(ctx, cardKey(cardID), "phone", phone).Err()
}

// ---------- 成员 ----------

func memberKey(id string) string { return "member:" + id }

func (s *Store) SaveMember(ctx context.Context, cardID string, mb *model.Member) error {
	if err := setJSON(ctx, s.RDB, memberKey(mb.ID), mb, 0); err != nil {
		return err
	}
	return s.RDB.SAdd(ctx, s.key("card", cardID, "members"), mb.ID).Err()
}

func (s *Store) GetMember(ctx context.Context, id string) (*model.Member, error) {
	var mb model.Member
	if err := getJSON(ctx, s.RDB, memberKey(id), &mb); err != nil {
		return nil, err
	}
	return &mb, nil
}

func (s *Store) ListMembers(ctx context.Context, cardID string) ([]*model.Member, error) {
	ids, err := s.RDB.SMembers(ctx, s.key("card", cardID, "members")).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*model.Member, 0, len(ids))
	for _, id := range ids {
		mb, err := s.GetMember(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, mb)
	}
	return out, nil
}

func (s *Store) MemberBelongsToCard(ctx context.Context, cardID, memberID string) (bool, error) {
	return s.RDB.SIsMember(ctx, s.key("card", cardID, "members"), memberID).Result()
}

// ---------- 临时同行授权 ----------

func tempKey(id string) string { return "temp:" + id }

func (s *Store) SaveTempGrant(ctx context.Context, g *model.TempCompanionGrant) error {
	if err := setJSON(ctx, s.RDB, tempKey(g.ID), g, 0); err != nil {
		return err
	}
	return s.RDB.SAdd(ctx, s.key("card", g.CardID, "temps"), g.ID).Err()
}

func (s *Store) GetTempGrant(ctx context.Context, id string) (*model.TempCompanionGrant, error) {
	var g model.TempCompanionGrant
	if err := getJSON(ctx, s.RDB, tempKey(id), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) ListTempGrants(ctx context.Context, cardID string) ([]*model.TempCompanionGrant, error) {
	ids, err := s.RDB.SMembers(ctx, s.key("card", cardID, "temps")).Result()
	if err != nil {
		return nil, err
	}
	out := []*model.TempCompanionGrant{}
	for _, id := range ids {
		if g, err := s.GetTempGrant(ctx, id); err == nil {
			out = append(out, g)
		}
	}
	return out, nil
}

// ---------- 设备与心跳 ----------

func deviceKey(id string) string { return "device:" + id }

func (s *Store) SaveDevice(ctx context.Context, d *model.Device) error {
	return setJSON(ctx, s.RDB, deviceKey(d.ID), d, 0)
}

func (s *Store) GetDevice(ctx context.Context, id string) (*model.Device, error) {
	var d model.Device
	if err := getJSON(ctx, s.RDB, deviceKey(id), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Heartbeat 刷新心跳 TTL 键；在线判定 = 心跳键存在。
func (s *Store) Heartbeat(ctx context.Context, id string, offlineSec int) error {
	return s.RDB.Set(ctx, s.key("device", id, "hb"), "1", time.Duration(offlineSec)*time.Second).Err()
}

func (s *Store) IsDeviceOnline(ctx context.Context, id string) (bool, error) {
	n, err := s.RDB.Exists(ctx, s.key("device", id, "hb")).Result()
	return n > 0, err
}

// ---------- 预约 ----------

func resvKey(id string) string { return "resv:" + id }

func (s *Store) SaveReservation(ctx context.Context, r *model.Reservation) error {
	if err := setJSON(ctx, s.RDB, resvKey(r.ID), r, 0); err != nil {
		return err
	}
	return s.RDB.SAdd(ctx, s.key("card", r.CardID, "reservations"), r.ID).Err()
}

func (s *Store) GetReservation(ctx context.Context, id string) (*model.Reservation, error) {
	var r model.Reservation
	if err := getJSON(ctx, s.RDB, resvKey(id), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) ListReservations(ctx context.Context, cardID string) ([]*model.Reservation, error) {
	ids, err := s.RDB.SMembers(ctx, s.key("card", cardID, "reservations")).Result()
	if err != nil {
		return nil, err
	}
	out := []*model.Reservation{}
	for _, id := range ids {
		r, err := s.GetReservation(ctx, id)
		if err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) UpdateReservation(ctx context.Context, r *model.Reservation) error {
	return setJSON(ctx, s.RDB, resvKey(r.ID), r, 0)
}

// ---------- 闸机核验流水 ----------

func eventKey(id string) string { return "event:" + id }

func (s *Store) SaveEvent(ctx context.Context, e *model.GateEvent) error {
	if err := setJSON(ctx, s.RDB, eventKey(e.ID), e, 0); err != nil {
		return err
	}
	ts, _ := time.Parse(time.RFC3339, e.At)
	pipe := s.RDB.Pipeline()
	pipe.ZAdd(ctx, s.key("card", e.CardID, "events"), redis.Z{Score: float64(ts.UnixMilli()), Member: e.ID})
	pipe.ZAdd(ctx, s.key("park", e.ParkID, "events"), redis.Z{Score: float64(ts.UnixMilli()), Member: e.ID})
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: "stream:gate_events",
		Values: []string{"event_id", e.ID, "verdict", e.Verdict},
	})
	// 待人工处理：转人工与疑似转借都需现场介入
	if e.Verdict == model.VerdictPending || e.Verdict == model.VerdictSuspect {
		pipe.LPush(ctx, "manual:queue", e.ID)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) GetEvent(ctx context.Context, id string) (*model.GateEvent, error) {
	var e model.GateEvent
	if err := getJSON(ctx, s.RDB, eventKey(id), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// LookupIdempotent 按 request_id 找到首次核验事件（网络抖动重试）。
func (s *Store) LookupIdempotent(ctx context.Context, requestID string) (*model.GateEvent, error) {
	eventID, err := s.RDB.Get(ctx, s.key("idem", requestID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetEvent(ctx, eventID)
}

func (s *Store) RememberIdempotent(ctx context.Context, requestID, eventID string) error {
	return s.RDB.Set(ctx, s.key("idem", requestID), eventID, 48*time.Hour).Err()
}

func (s *Store) ListCardEvents(ctx context.Context, cardID string, limit int64) ([]*model.GateEvent, error) {
	ids, err := s.RDB.ZRevRange(ctx, s.key("card", cardID, "events"), 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return s.loadEvents(ctx, ids)
}

func (s *Store) ListParkEvents(ctx context.Context, parkID string, limit int64) ([]*model.GateEvent, error) {
	ids, err := s.RDB.ZRevRange(ctx, s.key("park", parkID, "events"), 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return s.loadEvents(ctx, ids)
}

func (s *Store) loadEvents(ctx context.Context, ids []string) ([]*model.GateEvent, error) {
	out := make([]*model.GateEvent, 0, len(ids))
	for _, id := range ids {
		e, err := s.GetEvent(ctx, id)
		if err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// ---------- 人工核验队列 ----------

func (s *Store) ManualQueue(ctx context.Context, limit int64) ([]*model.GateEvent, error) {
	ids, err := s.RDB.LRange(ctx, "manual:queue", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return s.loadEvents(ctx, ids)
}

func (s *Store) RemoveFromManualQueue(ctx context.Context, eventID string) error {
	return s.RDB.LRem(ctx, "manual:queue", 0, eventID).Err()
}

// ---------- 申诉 ----------

func appealKey(id string) string { return "appeal:" + id }

func (s *Store) SaveAppeal(ctx context.Context, a *model.Appeal) error {
	if err := setJSON(ctx, s.RDB, appealKey(a.ID), a, 0); err != nil {
		return err
	}
	pipe := s.RDB.Pipeline()
	pipe.SAdd(ctx, "appeals", a.ID)
	if a.Status == model.AppealOpen {
		pipe.SAdd(ctx, "appeals:open", a.ID)
	} else {
		pipe.SRem(ctx, "appeals:open", a.ID)
	}
	pipe.SAdd(ctx, s.key("card", a.CardID, "appeals"), a.ID)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) GetAppeal(ctx context.Context, id string) (*model.Appeal, error) {
	var a model.Appeal
	if err := getJSON(ctx, s.RDB, appealKey(id), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) ListOpenAppeals(ctx context.Context) ([]*model.Appeal, error) {
	ids, err := s.RDB.SMembers(ctx, "appeals:open").Result()
	if err != nil {
		return nil, err
	}
	out := []*model.Appeal{}
	for _, id := range ids {
		if a, err := s.GetAppeal(ctx, id); err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}

// ---------- 景区 / 节假日 / 售卖规则 ----------

func (s *Store) SavePark(ctx context.Context, p *model.Park) error {
	if err := setJSON(ctx, s.RDB, s.key("park", p.ID, "meta"), p, 0); err != nil {
		return err
	}
	return s.RDB.SAdd(ctx, "parks", p.ID).Err()
}

func (s *Store) GetPark(ctx context.Context, id string) (*model.Park, error) {
	var p model.Park
	if err := getJSON(ctx, s.RDB, s.key("park", id, "meta"), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) ListParks(ctx context.Context) ([]*model.Park, error) {
	ids, err := s.RDB.SMembers(ctx, "parks").Result()
	if err != nil {
		return nil, err
	}
	out := []*model.Park{}
	for _, id := range ids {
		if p, err := s.GetPark(ctx, id); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Store) AddHoliday(ctx context.Context, h model.Holiday) error {
	return s.RDB.HSet(ctx, "holidays", h.Date, h.Name).Err()
}

func (s *Store) IsHoliday(ctx context.Context, date string) (bool, string, error) {
	name, err := s.RDB.HGet(ctx, "holidays", date).Result()
	if errors.Is(err, redis.Nil) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, name, nil
}

func (s *Store) ListHolidays(ctx context.Context) (map[string]string, error) {
	return s.RDB.HGetAll(ctx, "holidays").Result()
}

func (s *Store) SaveSaleRule(ctx context.Context, r model.SaleRule) error {
	return setJSON(ctx, s.RDB, s.key("salerule", r.CardType), r, 0)
}

func (s *Store) GetSaleRule(ctx context.Context, t string) (*model.SaleRule, error) {
	var r model.SaleRule
	if err := getJSON(ctx, s.RDB, s.key("salerule", t), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) ListSaleRules(ctx context.Context) (map[string]*model.SaleRule, error) {
	out := map[string]*model.SaleRule{}
	for _, t := range []string{model.CardTypeSingle, model.CardTypeFamily, model.CardTypeParent} {
		if r, err := s.GetSaleRule(ctx, t); err == nil {
			out[t] = r
		}
	}
	return out, nil
}

// ---------- 容量 / 当日入园计数（运营统计） ----------

func (s *Store) ParkAdmitted(ctx context.Context, parkID, date string) (int, error) {
	n, err := s.RDB.Get(ctx, s.key("cap", parkID, date)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

// CommitResult 原子提交返回
type CommitResult struct {
	Code    int
	EventID string
	Detail  string
}

// CommitAdmission 执行 Lua 原子扣减。
func (s *Store) CommitAdmission(ctx context.Context, req CommitArgs) (*CommitResult, error) {
	date := req.At.Format("20060102")
	keys := []string{
		s.key("idem", req.RequestID),
		cardKey(req.CardID),
		s.key("entrycnt", req.MemberID, date),
		s.key("moves", req.MemberID, date),
		s.key("cap", req.ParkID, date),
	}
	argv := []interface{}{
		req.RequestID,
		req.EventID,
		req.At.UnixMilli(),
		req.DeviceID,
		req.WindowMs,
		req.DailyLimit,
		req.Seats,
		req.DailyCap,
		172800, // 2 天 TTL
		boolNum(req.Override),
	}
	res, err := commitScript.Run(ctx, s.RDB, keys, argv...).Result()
	if err != nil {
		return nil, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) < 2 {
		return nil, fmt.Errorf("unexpected lua result: %v", res)
	}
	code := int(arr[0].(int64))
	detail := ""
	if arr[1] != nil {
		detail = fmt.Sprint(arr[1])
	}
	return &CommitResult{Code: code, EventID: detail, Detail: detail}, nil
}

func boolNum(b bool) int {
	if b {
		return 1
	}
	return 0
}

type CommitArgs struct {
	RequestID  string
	EventID    string
	CardID     string
	MemberID   string
	ParkID     string
	DeviceID   string
	At         time.Time
	WindowMs   int64
	DailyLimit int
	Seats      int
	DailyCap   int
	Override   bool
}

// RefundAdmission 撤销一次入园扣减（申诉成立/重复扣次回补）。
func (s *Store) RefundAdmission(ctx context.Context, cardID string, seats int) error {
	if _, err := s.IncQuotaUsed(ctx, cardID, -seats); err != nil {
		return err
	}
	return s.IncQuotaRefunded(ctx, cardID, seats)
}

// ---------- 测试辅助 ----------

func (s *Store) FlushDB(ctx context.Context) error { return s.RDB.FlushDB(ctx).Err() }
