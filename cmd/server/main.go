package main

import (
	"log"
	"strconv"

	"scenicpass/internal/api"
	"scenicpass/internal/config"
	"scenicpass/internal/store"
)

func main() {
	cfg := config.Get()
	db, _ := strconv.Atoi(cfg.RedisDB)
	_ = db
	st, err := store.New(cfg.RedisAddr, cfg.RedisDB)
	if err != nil {
		log.Fatalf("连接 Redis 失败 (%s): %v", cfg.RedisAddr, err)
	}
	srv := api.NewServer(cfg, st)
	log.Printf("景区年卡核验服务监听 %s (Redis %s, DB=%s)", cfg.HTTPAddr, cfg.RedisAddr, cfg.RedisDB)
	if err := srv.App().Listen(cfg.HTTPAddr); err != nil {
		log.Fatal(err)
	}
}
