package api

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"scenicpass/internal/config"
	"scenicpass/internal/middleware"
	"scenicpass/internal/model"
	"scenicpass/internal/service"
	"scenicpass/internal/store"
)

type Server struct {
	cfg   config.Config
	st    *store.Store
	cards *service.CardService
	gate  *service.GateService
	ops   *service.OpsService
	stats *service.StatsService
}

func NewServer(cfg config.Config, st *store.Store) *Server {
	s := &Server{
		cfg:   cfg,
		st:    st,
		cards: service.NewCardService(st),
		gate:  service.NewGateService(st),
		ops:   service.NewOpsService(st),
		stats: service.NewStatsService(st),
	}
	_ = s.cards.SeedDefaults(context.Background(), tNow())
	return s
}

// now 读取 X-Now 覆盖时钟（运营演练/自动化测试用），否则取本机时间。
func now(c *fiber.Ctx) time.Time {
	if v := c.Get("X-Now"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Now()
}

// tNow 供初始化使用
func tNow() time.Time { return time.Now() }

func (s *Server) App() *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
		return fail(c, err)
	}})

	app.Get("/healthz", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	api := app.Group("/api/v1")

	any := middleware.RequireRoles(s.cfg, "admin", "staff", "customer")
	admin := middleware.RequireRoles(s.cfg, "admin")
	staff := middleware.RequireRoles(s.cfg, "staff")
	gate := middleware.RequireRoles(s.cfg, "staff")
	adminStaff := middleware.RequireRoles(s.cfg, "admin", "staff")
	customer := middleware.RequireRoles(s.cfg, "customer", "admin")

	// 年卡
	api.Post("/cards", customer, s.createCard)
	api.Get("/cards/:id", any, s.getCard)
	api.Post("/cards/:id/members", customer, s.addMember)
	api.Put("/cards/:id/phone", customer, s.changePhone)
	api.Put("/cards/:id/members/:mid/photo", customer, s.updatePhoto)
	api.Post("/cards/:id/temp-grants", customer, s.createTempGrant)
	api.Get("/cards/:id/events", any, s.listCardEvents)
	api.Post("/cards/:id/freeze", admin, s.freezeCard)
	api.Post("/cards/:id/unfreeze", admin, s.unfreezeCard)
	api.Post("/cards/:id/refund", admin, s.refundCard)
	api.Post("/cards/:id/upgrade", customer, s.upgradeCard)

	// 预约
	api.Post("/reservations", customer, s.createReservation)
	api.Get("/cards/:id/reservations", any, s.listReservations)

	// 闸机核验
	api.Post("/gate/verify", gate, s.gateVerify)
	api.Post("/gate/offline-batch", gate, s.offlineBatch)
	api.Get("/events/:id", adminStaff, s.getEvent)
	api.Get("/parks/:pid/events", adminStaff, s.listParkEvents)

	// 现场人工
	api.Get("/manual/queue", staff, s.manualQueue)
	api.Post("/manual/resolve", staff, s.resolveManual)

	// 设备
	api.Post("/devices/:id", admin, s.registerDevice)
	api.Post("/devices/:id/heartbeat", gate, s.deviceHeartbeat)
	api.Get("/devices/:id", adminStaff, s.deviceStatus)

	// 申诉
	api.Post("/appeals", customer, s.createAppeal)
	api.Get("/appeals", adminStaff, s.listAppeals)
	api.Get("/appeals/:id", any, s.getAppeal)
	api.Post("/appeals/:id/review", adminStaff, s.reviewAppeal)

	// 运营
	api.Get("/ops/stats", admin, s.opsStats)
	api.Get("/ops/sale-rules", adminStaff, s.listSaleRules)
	api.Put("/ops/sale-rules/:type", admin, s.upsertSaleRule)
	api.Get("/ops/parks", adminStaff, s.listParks)
	api.Post("/ops/parks", admin, s.createPark)
	api.Get("/ops/holidays", adminStaff, s.listHolidays)
	api.Post("/ops/holidays", admin, s.addHoliday)

	return app
}

// ---------- 年卡 ----------

func (s *Server) createCard(c *fiber.Ctx) error {
	var in model.CreateCardInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	card, err := s.cards.CreateCard(c.UserContext(), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(card)
}

func (s *Server) getCard(c *fiber.Ctx) error {
	v, err := s.cards.GetCardView(c.UserContext(), c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(v)
}

func (s *Server) addMember(c *fiber.Ctx) error {
	var in model.AddMemberInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	mb, err := s.cards.AddMember(c.UserContext(), c.Params("id"), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(mb)
}

func (s *Server) changePhone(c *fiber.Ctx) error {
	var in model.ChangePhoneInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	if err := s.cards.ChangePhone(c.UserContext(), c.Params("id"), in.Phone); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "ok"})
}

func (s *Server) updatePhoto(c *fiber.Ctx) error {
	var in model.UpdatePhotoInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	if err := s.cards.UpdatePhoto(c.UserContext(), c.Params("id"), c.Params("mid"), in); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "ok"})
}

func (s *Server) createTempGrant(c *fiber.Ctx) error {
	var in model.CreateTempGrantInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	in.CardID = c.Params("id")
	g, err := s.cards.CreateTempGrant(c.UserContext(), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(g)
}

func (s *Server) listCardEvents(c *fiber.Ctx) error {
	limit, _ := strconv.ParseInt(c.Query("limit", "50"), 10, 64)
	evs, err := s.gate.ListCardEvents(c.UserContext(), c.Params("id"), limit)
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(evs)
}

func (s *Server) freezeCard(c *fiber.Ctx) error {
	if err := s.cards.Freeze(c.UserContext(), c.Params("id")); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "frozen"})
}

func (s *Server) unfreezeCard(c *fiber.Ctx) error {
	if err := s.cards.Unfreeze(c.UserContext(), c.Params("id")); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "active"})
}

func (s *Server) refundCard(c *fiber.Ctx) error {
	var in model.RefundCardInput
	_ = c.BodyParser(&in)
	if err := s.cards.Refund(c.UserContext(), c.Params("id"), in.Reason, now(c)); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "refunded"})
}

func (s *Server) upgradeCard(c *fiber.Ctx) error {
	var in model.UpgradeCardInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	card, err := s.cards.Upgrade(c.UserContext(), c.Params("id"), in.NewType, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(card)
}

// ---------- 预约 ----------

func (s *Server) createReservation(c *fiber.Ctx) error {
	var in model.CreateReservationInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	r, err := s.ops.CreateReservation(c.UserContext(), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(r)
}

func (s *Server) listReservations(c *fiber.Ctx) error {
	rs, err := s.ops.ListReservations(c.UserContext(), c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(rs)
}

// ---------- 闸机 ----------

func (s *Server) gateVerify(c *fiber.Ctx) error {
	var in model.GateAttempt
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	d, _, err := s.gate.Verify(c.UserContext(), in)
	if err != nil {
		return fail(c, err)
	}
	code := fiber.StatusOK
	if d.Decision == model.DecisionPass {
		code = fiber.StatusOK
	}
	// 业务结论仍以 200 + decision 字段返回，便于闸机程序统一处理
	return c.Status(code).JSON(d)
}

func (s *Server) offlineBatch(c *fiber.Ctx) error {
	var in model.OfflineBatchInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	ds, err := s.gate.OfflineBatch(c.UserContext(), in)
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"results": ds, "count": len(ds)})
}

func (s *Server) getEvent(c *fiber.Ctx) error {
	ev, err := s.gate.GetEvent(c.UserContext(), c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(ev)
}

func (s *Server) listParkEvents(c *fiber.Ctx) error {
	limit, _ := strconv.ParseInt(c.Query("limit", "50"), 10, 64)
	evs, err := s.gate.ListParkEvents(c.UserContext(), c.Params("pid"), limit)
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(evs)
}

// ---------- 人工 ----------

func (s *Server) manualQueue(c *fiber.Ctx) error {
	limit, _ := strconv.ParseInt(c.Query("limit", "50"), 10, 64)
	evs, err := s.gate.ManualQueue(c.UserContext(), limit)
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(evs)
}

func (s *Server) resolveManual(c *fiber.Ctx) error {
	var in model.ResolveManualInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	res, err := s.ops.ResolveManual(c.UserContext(), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(res)
}

// ---------- 设备 ----------

func (s *Server) registerDevice(c *fiber.Ctx) error {
	var in model.RegisterDeviceInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	offlineSec, _ := strconv.Atoi(s.cfg.DeviceOfflineSec)
	d, err := s.ops.RegisterDevice(c.UserContext(), c.Params("id"), in, offlineSec)
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(d)
}

func (s *Server) deviceHeartbeat(c *fiber.Ctx) error {
	offlineSec, _ := strconv.Atoi(s.cfg.DeviceOfflineSec)
	if err := s.ops.Heartbeat(c.UserContext(), c.Params("id"), offlineSec); err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"status": "ok"})
}

func (s *Server) deviceStatus(c *fiber.Ctx) error {
	st, err := s.ops.DeviceStatus(c.UserContext(), c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"device_id": c.Params("id"), "status": st})
}

// ---------- 申诉 ----------

func (s *Server) createAppeal(c *fiber.Ctx) error {
	var in model.CreateAppealInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	a, err := s.ops.CreateAppeal(c.UserContext(), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(a)
}

func (s *Server) listAppeals(c *fiber.Ctx) error {
	as, err := s.ops.ListOpenAppeals(c.UserContext())
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(as)
}

func (s *Server) getAppeal(c *fiber.Ctx) error {
	a, err := s.ops.GetAppeal(c.UserContext(), c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(a)
}

func (s *Server) reviewAppeal(c *fiber.Ctx) error {
	var in model.ReviewAppealInput
	if _, err := parseBody(c, &in); err != nil {
		return err
	}
	a, err := s.ops.ReviewAppeal(c.UserContext(), c.Params("id"), in, now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(a)
}

// ---------- 运营 ----------

func (s *Server) opsStats(c *fiber.Ctx) error {
	r, err := s.stats.Report(c.UserContext(), now(c))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(r)
}

func (s *Server) listSaleRules(c *fiber.Ctx) error {
	rs, err := s.ops.ListSaleRules(c.UserContext())
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(rs)
}

func (s *Server) upsertSaleRule(c *fiber.Ctx) error {
	var r model.SaleRule
	if _, err := parseBody(c, &r); err != nil {
		return err
	}
	if t := c.Params("type"); t != "" {
		r.CardType = t
	}
	if err := s.ops.SaveSaleRule(c.UserContext(), r); err != nil {
		return fail(c, err)
	}
	return c.JSON(r)
}

func (s *Server) listParks(c *fiber.Ctx) error {
	ps, err := s.ops.ListParks(c.UserContext())
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(ps)
}

func (s *Server) createPark(c *fiber.Ctx) error {
	var p model.Park
	if _, err := parseBody(c, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "景区 id 必填")
	}
	if err := s.ops.SavePark(c.UserContext(), &p); err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(p)
}

func (s *Server) listHolidays(c *fiber.Ctx) error {
	hs, err := s.ops.ListHolidays(c.UserContext())
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(hs)
}

func (s *Server) addHoliday(c *fiber.Ctx) error {
	var h model.Holiday
	if _, err := parseBody(c, &h); err != nil {
		return err
	}
	if err := s.ops.AddHoliday(c.UserContext(), h); err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(h)
}

// ---------- 辅助 ----------

func parseBody(c *fiber.Ctx, v interface{}) (bool, error) {
	if err := c.BodyParser(v); err != nil {
		return false, fiber.NewError(fiber.StatusBadRequest, "请求体解析失败: "+err.Error())
	}
	return true, nil
}

func fail(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, service.ErrValidation):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	case errors.Is(err, service.ErrConflict):
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
	default:
		if fe, ok := err.(*fiber.Error); ok {
			return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
}
