package config

import "os"

// Config 服务运行配置，全部可通过环境变量覆盖。
type Config struct {
	HTTPAddr    string
	RedisAddr   string
	RedisDB     string
	AdminKey    string
	StaffKey    string
	CustomerKey string
	// 设备心跳超时秒数：超过该时长未心跳即判定离线
	DeviceOfflineSec string
}

func Get() Config {
	return Config{
		HTTPAddr:         env("HTTP_ADDR", ":8080"),
		RedisAddr:        env("REDIS_ADDR", "127.0.0.1:6379"),
		RedisDB:          env("REDIS_DB", "0"),
		AdminKey:         env("ADMIN_API_KEY", "admin123"),
		StaffKey:         env("STAFF_API_KEY", "staff123"),
		CustomerKey:      env("CUSTOMER_API_KEY", "customer123"),
		DeviceOfflineSec: env("DEVICE_OFFLINE_SEC", "90"),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
