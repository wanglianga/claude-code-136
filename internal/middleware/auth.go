package middleware

import (
	"github.com/gofiber/fiber/v2"

	"scenicpass/internal/config"
)

// Role 基于 X-API-Key 的简单角色划分：
//
//	admin    运营管理（规则/冻结/退款/统计）
//	staff    现场工作人员/闸机设备（核验/人工处理）
//	customer 游客侧（购卡/预约/申诉）
func RequireRoles(cfg config.Config, roles ...string) fiber.Handler {
	allowed := map[string]bool{}
	keyToRole := map[string]string{
		cfg.AdminKey:    "admin",
		cfg.StaffKey:    "staff",
		cfg.CustomerKey: "customer",
	}
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *fiber.Ctx) error {
		key := c.Get("X-API-Key")
		role, ok := keyToRole[key]
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "无效或缺失 API Key"})
		}
		if !allowed[role] {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "该角色无权访问"})
		}
		c.Locals("role", role)
		return c.Next()
	}
}
