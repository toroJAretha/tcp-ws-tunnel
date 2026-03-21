package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"github.com/toroJ/port-tunnel/pkg/server"
)

func main() {
	// コマンドライン引数の定義
	controlAddr := flag.String("control", ":8080", "クライアントが接続するHTTP/WSアドレス")
	flag.Parse()

	// SIGINT/SIGTERMでグレースフルシャットダウン
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := &server.Server{
		ControlAddr: *controlAddr,
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
