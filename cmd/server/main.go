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
	portMin := flag.Int("port-min", 49152, "ランダムポート割り当て範囲の下限")
	portMax := flag.Int("port-max", 65535, "ランダムポート割り当て範囲の上限")
	flag.Parse()

	if *portMin < 1 || *portMax > 65535 || *portMin > *portMax {
		log.Fatal("invalid port range: port-min must be <= port-max and within 1-65535")
	}

	// SIGINT/SIGTERMでグレースフルシャットダウン
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s := &server.Server{
		ControlAddr: *controlAddr,
		PortMin:     *portMin,
		PortMax:     *portMax,
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
