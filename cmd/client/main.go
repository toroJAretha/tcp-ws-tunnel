package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/toroJ/port-tunnel/pkg/client"
)

func main() {
	// コマンドライン引数の定義
	serverURL := flag.String("server", "", "トンネルサーバーのWebSocket URL（例: wss://tunnel.example.com）")
	localAddr := flag.String("local", "localhost:25565", "転送先のローカルアドレス")
	userID := flag.String("user", "", "認証ユーザーID")
	password := flag.String("password", "", "認証パスワード")
	flag.Parse()

	// 必須パラメータのバリデーション
	if *serverURL == "" {
		log.Fatal("-server flag is required (e.g., -server wss://tunnel.example.com)")
	}
	if *userID == "" || *password == "" {
		log.Fatal("-user and -password are required")
	}

	// SIGINT/SIGTERMでグレースフルシャットダウン
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &client.Client{
		ServerURL: *serverURL,
		LocalAddr: *localAddr,
		UserID:    *userID,
		Password:  *password,
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := c.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
