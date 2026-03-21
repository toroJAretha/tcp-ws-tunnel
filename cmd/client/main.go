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
	serverURL := flag.String("server", "", "トンネルサーバーのWebSocket URL（例: ws://vps:8080）")
	publicPort := flag.Int("public", 0, "サーバー側で公開するポート番号")
	localAddr := flag.String("local", "localhost:49152", "転送先のローカルアドレス")
	flag.Parse()

	// 必須パラメータのバリデーション
	if *serverURL == "" {
		log.Fatal("-server flag is required (e.g., -server ws://your-vps:8080)")
	}
	if *publicPort < 1 || *publicPort > 65535 {
		log.Fatal("-public flag is required and must be a valid port number (1-65535)")
	}

	// SIGINT/SIGTERMでグレースフルシャットダウン
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &client.Client{
		ServerURL:  *serverURL,
		PublicPort: *publicPort,
		LocalAddr:  *localAddr,
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := c.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
