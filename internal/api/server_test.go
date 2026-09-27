package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"scenicpass/internal/config"
	"scenicpass/internal/model"
	"scenicpass/internal/store"
)

const (
	keyAdmin    = "admin123"
	keyStaff    = "staff123"
	keyCustomer = "customer123"
)

type testEnv struct {
	t    *testing.T
	app  *fiber.App
	st   *store.Store
	port string
	cmd  *exec.Cmd
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return p
}

func startRedis(t *testing.T) (string, *exec.Cmd) {
	port := freePort(t)
	cmd := exec.Command("redis-server", "--port", strconv.Itoa(port),
		"--save", "", "--appendonly", "no", "--daemonize", "no")
	require.NoError(t, cmd.Start())
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	c := redis.NewClient(&redis.Options{Addr: addr})
	for time.Now().Before(deadline) {
		if c.Ping(context.Background()).Err() == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NoError(t, c.Ping(context.Background()).Err())
	return addr, cmd
}

func newEnv(t *testing.T) *testEnv {
	addr, cmd := startRedis(t)
	st, err := store.New(addr, "0")
	require.NoError(t, err)
	require.NoError(t, st.FlushDB(context.Background()))
	cfg := config.Get()
	cfg.AdminKey = keyAdmin
	cfg.StaffKey = keyStaff
	cfg.CustomerKey = keyCustomer
	cfg.DeviceOfflineSec = "90"
	srv := NewServer(cfg, st)
	env := &testEnv{t: t, app: srv.App(), st: st, cmd: cmd}
	// 默认 P001 设为全天时段，避免用例受运行时刻影响；容量保持默认。
	code, _ := env.do("POST", "/api/v1/ops/parks", keyAdmin, map[string]interface{}{
		"id": "P001", "name": "主峰景区", "daily_cap": 5000,
		"slot_start": "00:00", "slot_end": "23:59",
	}, nil)
	require.True(t, code == 200 || code == 201)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return env
}

// ---------- HTTP 辅助 ----------

func (e *testEnv) do(method, url, key string, body interface{}, headers map[string]string) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.app.Test(req, 5000)
	require.NoError(e.t, err)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (e *testEnv) mustDecode(code int, data []byte, v interface{}) {
	require.Equalf(e.t, 200, code, "body=%s", string(data))
	if code/100 == 2 && v != nil {
		require.NoError(e.t, json.Unmarshal(data, v), string(data))
	}
}

func jget(data []byte, p string) interface{} {
	var m interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	cur := m
	for _, seg := range splitPath(p) {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[seg]
	}
	return cur
}

func splitPath(p string) []string {
	var out []string
	cur := ""
	for _, r := range p {
		if r == '.' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	return append(out, cur)
}

// ---------- 业务构造辅助 ----------

func holder(name string, expire string) model.CreateMemberInput {
	return model.CreateMemberInput{
		Name: name, IDType: "idcard", IDNumber: "ID-" + name,
		IDExpireAt: expire, PhotoRef: "photo/" + name, PhotoQuality: "ok",
		Role: model.RoleHolder,
	}
}

func member(name, role string, faceExempt bool, quality string) model.CreateMemberInput {
	m := holder(name, "2099-01-01")
	m.Role = role
	m.FaceExempt = faceExempt
	if quality != "" {
		m.PhotoQuality = quality
	}
	return m
}

func (e *testEnv) createCard(cardType string, members []model.CreateMemberInput, extra map[string]interface{}) *model.Card {
	in := map[string]interface{}{
		"type":        cardType,
		"holder_name": members[0].Name,
		"phone":       "13800000000",
		"parks":       []string{"P001"},
		"members":     members,
	}
	for k, v := range extra {
		in[k] = v
	}
	code, data := e.do("POST", "/api/v1/cards", keyCustomer, in, nil)
	require.Equalf(e.t, 201, code, "createCard: %s", data)
	var c model.Card
	require.NoError(e.t, json.Unmarshal(data, &c))
	return &c
}

func (e *testEnv) registerDevice(id, park string) {
	code, data := e.do("POST", path.Join("/api/v1/devices", id), keyAdmin,
		map[string]string{"park_id": park, "entrance": "东门"}, nil)
	require.Equalf(e.t, 201, code, "registerDevice: %s", data)
}

func (e *testEnv) face(match string) map[string]interface{} {
	return map[string]interface{}{"photo_ref": "live", "quality": "ok", "match": match}
}

func (e *testEnv) verifyBody(card *model.Card, memberID, device, requestID string) map[string]interface{} {
	return map[string]interface{}{
		"request_id": requestID,
		"device_id":  device,
		"park_id":    "P001",
		"card_id":    card.ID,
		"qr_code":    card.ID,
		"member_id":  memberID,
		"face":       e.face("match"),
	}
}

func (e *testEnv) verify(card *model.Card, memberID, device, requestID string, at string) model.GateDecision {
	body := e.verifyBody(card, memberID, device, requestID)
	var d model.GateDecision
	headers := map[string]string{}
	if at != "" {
		headers["X-Now"] = at
		body["at"] = at
	}
	code, data := e.do("POST", "/api/v1/gate/verify", keyStaff, body, headers)
	e.mustDecode(code, data, &d)
	return d
}

func memberID(card *model.CardView, role string) string {
	for _, m := range card.Members {
		if m.Role == role {
			return m.ID
		}
	}
	return card.Members[0].ID
}

func (e *testEnv) view(cardID string) *model.CardView {
	code, data := e.do("GET", "/api/v1/cards/"+cardID, keyStaff, nil, nil)
	require.Equalf(e.t, 200, code, "view: %s", data)
	var v model.CardView
	require.NoError(e.t, json.Unmarshal(data, &v))
	return &v
}

// ---------- 测试用例 ----------

func TestHealthAndAuth(t *testing.T) {
	e := newEnv(t)
	code, _ := e.do("GET", "/healthz", "", nil, nil)
	require.Equal(t, 200, code)

	code, _ = e.do("GET", "/api/v1/ops/stats", "", nil, nil)
	require.Equal(t, 401, code)
	code, _ = e.do("GET", "/api/v1/ops/stats", keyCustomer, nil, nil)
	require.Equal(t, 403, code)
	code, _ = e.do("GET", "/api/v1/ops/stats", keyAdmin, nil, nil)
	require.Equal(t, 200, code)
}

func TestHappyPathDeductOnce(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	d := e.verify(card, memberID(v, model.RoleHolder), "D1", "req-1", "")
	require.Equal(t, model.DecisionPass, d.Decision)
	require.Equal(t, 1, d.DeductedCount)
	require.Equal(t, 59, d.QuotaRemaining)
}

// 网络抖动：同一 request_id 高并发重试，只能扣一次。
func TestIdempotentConcurrent(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	body := e.verifyBody(card, memberID(v, model.RoleHolder), "D1", "same-req")

	const n = 20
	var wg sync.WaitGroup
	results := make([]model.GateDecision, n)
	codes := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", "/api/v1/gate/verify", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", keyStaff)
			resp, err := e.app.Test(req, -1)
			require.NoError(t, err)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
			_ = json.Unmarshal(raw, &results[i])
		}(i)
	}
	wg.Wait()
	replayed := 0
	eventIDs := map[string]int{}
	for i, r := range results {
		require.Equal(t, 200, codes[i])
		require.Equal(t, model.DecisionPass, r.Decision)
		if r.Replayed {
			replayed++
		}
		eventIDs[r.EventID]++
	}
	require.Equal(t, n-1, replayed, "除首次外都应命中幂等")
	require.Len(t, eventIDs, 1, "所有响应必须指向同一条流水")

	cv := e.view(card.ID)
	require.Equal(t, 1, cv.Card.QuotaUsed, "并发重试只允许扣一次权益")
}

// 多设备/多请求并发：不同 request_id 也受每日次数与原子扣减保护。
func TestConcurrentDistinctRequestsQuotaAndDaily(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")},
		map[string]interface{}{"daily_limit": 1, "annual_quota": 100})
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	const n = 10
	var wg sync.WaitGroup
	ds := make([]model.GateDecision, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ds[i] = e.verify(card, mid, "D1", fmt.Sprintf("r-%d", i), "")
		}(i)
	}
	wg.Wait()
	pass, manual := 0, 0
	for _, d := range ds {
		switch d.Decision {
		case model.DecisionPass:
			pass++
		case model.DecisionManual:
			manual++
			require.Contains(t, d.Reasons, model.ReasonHighFrequency)
		}
	}
	require.Equal(t, 1, pass)
	require.Equal(t, n-1, manual)
	cv := e.view(card.ID)
	require.Equal(t, 1, cv.Card.QuotaUsed)
}

// 短时间内不同设备入园 => 疑似转借。
func TestCrossDeviceSuspectedTransfer(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("DA", "P001")
	e.registerDevice("DB", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	d1 := e.verify(card, mid, "DA", "a1", "")
	require.Equal(t, model.DecisionPass, d1.Decision)
	d2 := e.verify(card, mid, "DB", "a2", "")
	require.Equal(t, model.DecisionSuspect, d2.Decision)
	require.Contains(t, d2.Reasons, model.ReasonSuspectedTransfer)

	// 疑似流水进入人工队列并可解释、可处理
	code, data := e.do("GET", "/api/v1/manual/queue", keyStaff, nil, nil)
	require.Equal(t, 200, code)
	require.Contains(t, string(data), d2.EventID)
}

func TestFreezeDoesNotAffectReservation(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)
	today := time.Now().Format("2006-01-02")

	// 预约
	code, data := e.do("POST", "/api/v1/reservations", keyCustomer, map[string]interface{}{
		"card_id": card.ID, "park_id": "P001", "date": today,
		"slot": "上午", "member_ids": []string{mid},
	}, nil)
	require.Equal(t, 201, code, string(data))
	var r model.Reservation
	require.NoError(t, json.Unmarshal(data, &r))

	// 冻结：预约保留
	code, _ = e.do("POST", "/api/v1/cards/"+card.ID+"/freeze", keyAdmin, nil, nil)
	require.Equal(t, 200, code)
	rs := []model.Reservation{}
	code, data = e.do("GET", "/api/v1/cards/"+card.ID+"/reservations", keyStaff, nil, nil)
	require.NoError(t, json.Unmarshal(data, &rs))
	require.Equal(t, model.ReservationBooked, rs[0].Status)

	// 冻结期间核验拒绝
	d := e.verify(card, mid, "D1", "fz1", "")
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonCardFrozen)

	// 解冻：预约仍可用
	code, _ = e.do("POST", "/api/v1/cards/"+card.ID+"/unfreeze", keyAdmin, nil, nil)
	require.Equal(t, 200, code)
	body := e.verifyBody(card, mid, "D1", "fz2")
	body["reservation_id"] = r.ID
	code, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.Equal(t, 200, code, string(data))
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionPass, d.Decision)

	rs = []model.Reservation{}
	code, data = e.do("GET", "/api/v1/cards/"+card.ID+"/reservations", keyStaff, nil, nil)
	require.NoError(t, json.Unmarshal(data, &rs))
	require.Equal(t, model.ReservationUsed, rs[0].Status)
}

func TestIDExpired(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2020-01-01")}, nil)
	v := e.view(card.ID)
	d := e.verify(card, memberID(v, model.RoleHolder), "D1", "id1", "")
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonIDExpired)
	require.Equal(t, 0, d.DeductedCount)
}

func TestPhotoUnclearAndMismatchAndManualFlow(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	// 照片不清 -> 人工
	body := e.verifyBody(card, mid, "D1", "p1")
	body["face"] = map[string]string{"quality": "unclear", "match": "unknown"}
	code, data := e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.Equal(t, 200, code)
	var d model.GateDecision
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionManual, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonPhotoUnclear)
	require.NotEmpty(t, d.ManualTicket)
	require.Equal(t, 0, e.view(card.ID).Card.QuotaUsed)

	// 人工放行：结论可追溯
	code, data = e.do("POST", "/api/v1/manual/resolve", keyStaff, map[string]interface{}{
		"event_id": d.EventID, "action": "confirm_pass", "operator": "王工",
		"note": "现场核对证件无误", "refund_quota": true,
	}, nil)
	require.Equal(t, 200, code, string(data))

	code, data = e.do("GET", "/api/v1/events/"+d.EventID, keyStaff, nil, nil)
	require.Equal(t, 200, code)
	var ev model.GateEvent
	require.NoError(t, json.Unmarshal(data, &ev))
	require.True(t, ev.Resolved)
	require.Equal(t, model.VerdictPass, ev.Verdict)
	require.Equal(t, "王工", ev.Resolution.Operator)

	// 重复处理被拒绝
	code, _ = e.do("POST", "/api/v1/manual/resolve", keyStaff, map[string]interface{}{
		"event_id": d.EventID, "action": "reject", "operator": "王工",
	}, nil)
	require.Equal(t, 409, code)

	// 人脸不匹配 -> 疑似转借
	body = e.verifyBody(card, mid, "D1", "p2")
	body["face"] = map[string]string{"quality": "ok", "match": "mismatch"}
	code, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionSuspect, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonPhotoMismatch)
}

// 老人无法刷脸：转人工，不扣权益。
func TestElderFaceExempt(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeFamily, []model.CreateMemberInput{
		holder("张三", "2099-01-01"),
		member("张奶奶", model.RoleElder, true, "ok"),
	}, nil)
	v := e.view(card.ID)
	elder := memberID(v, model.RoleElder)
	d := e.verify(card, elder, "D1", "eld1", "")
	require.Equal(t, model.DecisionManual, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonFaceExempt)
	require.Equal(t, 0, e.view(card.ID).Card.QuotaUsed)
}

// 孩子长大换照片：此前底库不可用，更新后正常入园。
func TestChildPhotoUpdate(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeParent, []model.CreateMemberInput{
		holder("张三", "2099-01-01"),
		member("张小娃", model.RoleChild, false, "unclear"),
	}, nil)
	v := e.view(card.ID)
	child := memberID(v, model.RoleChild)

	d := e.verify(card, child, "D1", "kid1", "")
	require.Equal(t, model.DecisionManual, d.Decision)

	code, _ := e.do("PUT", fmt.Sprintf("/api/v1/cards/%s/members/%s/photo", card.ID, child), keyCustomer,
		map[string]string{"photo_ref": "photo/kid-new", "photo_quality": "ok"}, nil)
	require.Equal(t, 200, code)
	d = e.verify(card, child, "D1", "kid2", "")
	require.Equal(t, model.DecisionPass, d.Decision)
}

func TestCompanionUnboundAndTempGrant(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeFamily, []model.CreateMemberInput{
		holder("张三", "2099-01-01"),
	}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	// 未绑定同行人
	body := e.verifyBody(card, mid, "D1", "c1")
	body["companion_ids"] = []string{"M-not-exist"}
	code, data := e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.Equal(t, 200, code)
	var d model.GateDecision
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonCompanionUnbound)

	// 临时更换家庭成员：限期授权
	code, data = e.do("POST", "/api/v1/cards/"+card.ID+"/temp-grants", keyCustomer, map[string]interface{}{
		"name": "李临时", "id_number": "ID-LS", "phone": "13900000000",
		"reason": "原家庭成员生病，亲友陪同", "valid_from": "2000-01-01", "valid_to": "2099-01-01",
	}, nil)
	require.Equal(t, 201, code, string(data))
	var g model.TempCompanionGrant
	require.NoError(t, json.Unmarshal(data, &g))

	body = e.verifyBody(card, mid, "D1", "c2")
	body["temp_grant_ids"] = []string{g.ID}
	code, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionPass, d.Decision, string(data))
	require.Equal(t, 2, d.DeductedCount)

	// 过期授权
	code, data = e.do("POST", "/api/v1/cards/"+card.ID+"/temp-grants", keyCustomer, map[string]interface{}{
		"name": "王过期", "id_number": "ID-WG",
		"valid_from": "2000-01-01", "valid_to": "2001-01-01",
	}, nil)
	require.NoError(t, json.Unmarshal(data, &g))
	body = e.verifyBody(card, mid, "D1", "c3")
	body["temp_grant_ids"] = []string{g.ID}
	code, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonTempCompanionInvalid)
}

func TestHolidayRestriction(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	// 家庭年卡默认节假日不可用
	card := e.createCard(model.CardTypeFamily, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)
	at := "2026-10-01T10:00:00+08:00"
	d := e.verify(card, mid, "D1", "h1", at)
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonHolidayRestricted)

	// 节假日当天也无法预约
	code, _ := e.do("POST", "/api/v1/reservations", keyCustomer, map[string]interface{}{
		"card_id": card.ID, "park_id": "P001", "date": "2026-10-01",
		"slot": "上午", "member_ids": []string{mid},
	}, nil)
	require.Equal(t, 400, code)
}

func TestCapacityFull(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	code, _ := e.do("POST", "/api/v1/ops/parks", keyAdmin, map[string]interface{}{
		"id": "P999", "name": "限流景点", "daily_cap": 1,
		"slot_start": "00:00", "slot_end": "23:59",
	}, nil)
	require.Equal(t, 201, code)

	c1 := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("甲", "2099-01-01")},
		map[string]interface{}{"parks": []string{"P999"}})
	c2 := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("乙", "2099-01-01")},
		map[string]interface{}{"parks": []string{"P999"}})
	m1 := memberID(e.view(c1.ID), model.RoleHolder)
	m2 := memberID(e.view(c2.ID), model.RoleHolder)

	body := e.verifyBody(c1, m1, "D1", "cap1")
	body["park_id"] = "P999"
	var d model.GateDecision
	_, data := e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionPass, d.Decision)

	body = e.verifyBody(c2, m2, "D1", "cap2")
	body["park_id"] = "P999"
	_, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonParkFull)
}

func TestQuotaInsufficient(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")},
		map[string]interface{}{"annual_quota": 1, "daily_limit": 5})
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	d := e.verify(card, mid, "D1", "q1", "")
	require.Equal(t, model.DecisionPass, d.Decision)
	require.Equal(t, 0, d.QuotaRemaining)

	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02") + "T10:00:00+08:00"
	d = e.verify(card, mid, "D1", "q2", tomorrow)
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonQuotaInsufficient)
}

// 闸机离线：在线请求转人工；离线补报去重，申诉成立回补。
func TestOfflineBatchDedupAndAppealRefund(t *testing.T) {
	e := newEnv(t)
	// 不注册设备 => 设备离线
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	d := e.verify(card, mid, "DOFF", "on1", "")
	require.Equal(t, model.DecisionManual, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonDeviceOffline)

	// 设备本地放行人，联网后补报
	batch := map[string]interface{}{
		"device_id": "DOFF",
		"attempts": []map[string]interface{}{
			{
				"request_id": "off-req-1", "device_id": "DOFF", "park_id": "P001",
				"card_id": card.ID, "qr_code": card.ID, "member_id": mid,
				"face": e.face("match"),
			},
		},
	}
	code, data := e.do("POST", "/api/v1/gate/offline-batch", keyStaff, batch, nil)
	require.Equal(t, 200, code, string(data))
	require.Equal(t, float64(1), jget(data, "count"))
	firstResults := jget(data, "results").([]interface{})
	evID := jgetMarshal(t, firstResults[0], "event_id")
	d1 := decodeResult(t, firstResults[0])
	require.Equal(t, model.DecisionPass, d1.Decision)
	require.Equal(t, 1, e.view(card.ID).Card.QuotaUsed)

	// 网络原因整批重复上报：不得重复扣次
	code, data = e.do("POST", "/api/v1/gate/offline-batch", keyStaff, batch, nil)
	require.Equal(t, 200, code)
	require.Equal(t, float64(1), jget(data, "count"))
	require.Equal(t, 1, e.view(card.ID).Card.QuotaUsed, "离线重复上报不能重复扣权益")
	d2 := decodeResult(t, jget(data, "results").([]interface{})[0])
	require.True(t, d2.Replayed)

	// 游客就该次扣减申诉，客服核实为离线异常 -> 回补
	code, data = e.do("POST", "/api/v1/appeals", keyCustomer, map[string]interface{}{
		"card_id": card.ID, "event_ids": []string{evID},
		"reason": "设备离线重复扣次", "detail": "闸机故障",
	}, nil)
	require.Equal(t, 201, code, string(data))
	var ap model.Appeal
	require.NoError(t, json.Unmarshal(data, &ap))

	code, data = e.do("POST", "/api/v1/appeals/"+ap.ID+"/review", keyStaff, map[string]interface{}{
		"status": "approved", "handler": "客服小李", "note": "核实离线日志，回补1人次",
	}, nil)
	require.Equal(t, 200, code, string(data))
	require.NoError(t, json.Unmarshal(data, &ap))
	require.Equal(t, 1, ap.RefundQuota)
	require.Equal(t, 0, e.view(card.ID).Card.QuotaUsed)
	require.Equal(t, 1, e.view(card.ID).Card.QuotaRefunded)

	// 流水留痕：人工/客服可解释
	code, data = e.do("GET", "/api/v1/events/"+evID, keyStaff, nil, nil)
	var ev model.GateEvent
	require.NoError(t, json.Unmarshal(data, &ev))
	require.True(t, ev.RefundedQuota)
	require.Equal(t, ap.ID, ev.AppealID)

	// 二次审批同一申诉被拒绝（状态机）
	code, _ = e.do("POST", "/api/v1/appeals/"+ap.ID+"/review", keyStaff, map[string]interface{}{
		"status": "approved", "handler": "x",
	}, nil)
	require.Equal(t, 409, code)
}

func decodeResult(t *testing.T, v interface{}) model.GateDecision {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var d model.GateDecision
	require.NoError(t, json.Unmarshal(b, &d))
	return d
}

func jgetMarshal(t *testing.T, v interface{}, key string) string {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	return m[key].(string)
}

func TestOfflineFingerprintDedupWithoutRequestID(t *testing.T) {
	e := newEnv(t)
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("钱七", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)
	at := time.Now().Format(time.RFC3339)
	attempt := func() map[string]interface{} {
		return map[string]interface{}{
			"device_id": "DOFF",
			"attempts": []map[string]interface{}{{
				"device_id": "DOFF", "park_id": "P001",
				"card_id": card.ID, "qr_code": card.ID, "member_id": mid,
				"at": at, "face": e.face("match"),
			}},
		}
	}
	code, data := e.do("POST", "/api/v1/gate/offline-batch", keyStaff, attempt(), nil)
	require.Equal(t, 200, code)
	require.Equal(t, float64(1), jget(data, "count"))
	require.Equal(t, model.DecisionPass, decodeResult(t, jget(data, "results").([]interface{})[0]).Decision)

	// 设备本地重试、且未带 request_id：同一分钟指纹必须去重，不重复扣
	code, data = e.do("POST", "/api/v1/gate/offline-batch", keyStaff, attempt(), nil)
	require.Equal(t, 200, code)
	d := decodeResult(t, jget(data, "results").([]interface{})[0])
	require.True(t, d.Replayed)
	require.Equal(t, 1, e.view(card.ID).Card.QuotaUsed)
}

func TestRefundAndUpgrade(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)
	today := time.Now().Format("2006-01-02")

	// 预约一笔
	code, data := e.do("POST", "/api/v1/reservations", keyCustomer, map[string]interface{}{
		"card_id": card.ID, "park_id": "P001", "date": today,
		"slot": "上午", "member_ids": []string{mid},
	}, nil)
	require.Equal(t, 201, code)

	// 用一次
	d := e.verify(card, mid, "D1", "u1", "")
	require.Equal(t, model.DecisionPass, d.Decision)

	// 退款：卡作废、有效期截止当日、预约取消
	code, _ = e.do("POST", "/api/v1/cards/"+card.ID+"/refund", keyAdmin,
		map[string]string{"reason": "游客反悔"}, nil)
	require.Equal(t, 200, code)
	cv2 := e.view(card.ID)
	require.Equal(t, model.CardStatusRefunded, cv2.Card.Status)
	require.Equal(t, today, cv2.Card.ValidTo)
	d = e.verify(card, mid, "D1", "u2", "")
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonCardRefunded)

	// 新购单人卡 -> 升级家庭卡：额度/有效期/同行人数重算，已用结转
	card2 := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("李四", "2099-01-01")}, nil)
	v2 := e.view(card2.ID)
	d = e.verify(card2, memberID(v2, model.RoleHolder), "D1", "up0", "")
	require.Equal(t, model.DecisionPass, d.Decision)

	code, data = e.do("POST", "/api/v1/cards/"+card2.ID+"/upgrade", keyCustomer,
		map[string]string{"new_type": model.CardTypeFamily}, nil)
	require.Equal(t, 200, code, string(data))
	var uc model.Card
	require.NoError(t, json.Unmarshal(data, &uc))
	require.Equal(t, model.CardTypeFamily, uc.Type)
	require.Equal(t, 240, uc.AnnualQuota)
	require.Equal(t, 1, uc.QuotaUsed, "已用额度结转")
	require.Equal(t, 4, uc.CompanionLimit)
	require.False(t, uc.HolidayAllowed)
	expected := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
	require.Equal(t, expected, uc.ValidTo, "有效期自升级日重新计算一年")

	// 已退款卡不能升级
	code, _ = e.do("POST", "/api/v1/cards/"+card.ID+"/upgrade", keyCustomer,
		map[string]string{"new_type": model.CardTypeFamily}, nil)
	require.Equal(t, 409, code)
}

func TestChangePhoneAndMiscRejections(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")},
		map[string]interface{}{
			"parks":           []string{"P002"},
			"time_slot_start": "12:00",
			"time_slot_end":   "13:00",
		})
	v := e.view(card.ID)
	mid := memberID(v, model.RoleHolder)

	// 换手机号
	code, _ := e.do("PUT", "/api/v1/cards/"+card.ID+"/phone", keyCustomer,
		map[string]string{"phone": "13911112222"}, nil)
	require.Equal(t, 200, code)
	require.Equal(t, "13911112222", e.view(card.ID).Card.Phone)

	// 非可入园景点
	body := e.verifyBody(card, mid, "D1", "m1") // body 默认 P001
	var d model.GateDecision
	_, data := e.do("POST", "/api/v1/gate/verify", keyStaff, body, nil)
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonParkNotAllowed)

	// 时段外（P002 08:00-16:30，卡 12:00-13:00；上午10点）
	body = e.verifyBody(card, mid, "D1", "m2")
	body["park_id"] = "P002"
	_, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, map[string]string{"X-Now": "2026-09-26T10:00:00+08:00"})
	body["at"] = "2026-09-26T10:00:00+08:00"
	// 重新发一次（带 at 体）
	_, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, map[string]string{"X-Now": "2026-09-26T10:00:00+08:00"})
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonOutsideTimeSlot)

	// 二维码无效
	body = e.verifyBody(card, mid, "D1", "m3")
	body["park_id"] = "P002"
	body["qr_code"] = "forged"
	body["at"] = "2026-09-26T12:30:00+08:00"
	_, data = e.do("POST", "/api/v1/gate/verify", keyStaff, body, map[string]string{"X-Now": "2026-09-26T12:30:00+08:00"})
	require.NoError(t, json.Unmarshal(data, &d))
	require.Equal(t, model.DecisionDeny, d.Decision)
	require.Contains(t, d.Reasons, model.ReasonQrInvalid)

	// 卡不存在
	code, _ = e.do("GET", "/api/v1/cards/C-nope", keyStaff, nil, nil)
	require.Equal(t, 404, code)
}

func TestOpsStatsAndSaleRules(t *testing.T) {
	e := newEnv(t)
	e.registerDevice("D1", "P001")
	card := e.createCard(model.CardTypeSingle, []model.CreateMemberInput{holder("张三", "2099-01-01")}, nil)
	v := e.view(card.ID)
	_ = e.verify(card, memberID(v, model.RoleHolder), "D1", "s1", "")
	// 制造一条拒绝
	e.verify(card, memberID(v, model.RoleHolder), "D1", "s2", "")

	code, data := e.do("GET", "/api/v1/ops/stats", keyAdmin, nil, nil)
	require.Equal(t, 200, code, string(data))
	rep := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(data, &rep))
	require.GreaterOrEqual(t, rep["total_events"].(float64), 2.0)
	verdicts := rep["events_by_verdict"].(map[string]interface{})
	require.Greater(t, verdicts["pass"].(float64), 0.0)
	usage := rep["quota_usage_by_card_type"].(map[string]interface{})
	single := usage[model.CardTypeSingle].(map[string]interface{})
	require.Greater(t, single["usage_rate"].(float64), 0.0)
	require.Greater(t, rep["peak_by_hour"].(map[string]interface{})[time.Now().Format("15")].(float64), 0.0)

	// 运营调整售卖规则：暂停家庭年卡售卖
	code, _ = e.do("PUT", "/api/v1/ops/sale-rules/"+model.CardTypeFamily, keyAdmin, model.SaleRule{
		CardType: model.CardTypeFamily, SaleEnabled: false, AnnualQuota: 240,
		DailyLimit: 2, HolidayAllowed: false, CompanionLimit: 4,
	}, nil)
	require.Equal(t, 200, code)
	code, _ = e.do("POST", "/api/v1/cards", keyCustomer, map[string]interface{}{
		"type": model.CardTypeFamily, "holder_name": "x", "parks": []string{"P001"},
		"members": []model.CreateMemberInput{holder("x", "2099-01-01")},
	}, nil)
	require.Equal(t, 400, code)
}
