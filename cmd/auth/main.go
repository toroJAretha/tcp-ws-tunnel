package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/toroJ/port-tunnel/pkg/auth"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "add-user" {
		addUserCmd()
		return
	}
	serveCmd()
}

// serveCmd は認証APIサーバーを起動する。
func serveCmd() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8081", "認証APIのリッスンアドレス")
	secret := fs.String("secret", "", "JWT署名用のHMAC秘密鍵（未設定の場合は環境変数AUTH_SECRETを参照）")
	usersFile := fs.String("users", "users.json", "ユーザー情報JSONファイルのパス")
	expiry := fs.String("expiry", "24h", "JWTの有効期限（例: 1h, 24h, 7d）")
	fs.Parse(os.Args[1:])

	// フラグ未設定の場合は環境変数から取得
	if *secret == "" {
		*secret = os.Getenv("AUTH_SECRET")
	}
	if *secret == "" {
		log.Fatal("-secret or AUTH_SECRET environment variable is required")
	}

	s := &auth.Server{
		Secret:    *secret,
		UsersFile: *usersFile,
		Expiry:    *expiry,
	}

	if err := s.LoadUsers(); err != nil {
		log.Fatalf("failed to load users: %v", err)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("[auth] listening on %s (users: %s)", *addr, *usersFile)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// addUserCmd はユーザーを追加する。
func addUserCmd() {
	fs := flag.NewFlagSet("add-user", flag.ExitOnError)
	usersFile := fs.String("users", "users.json", "ユーザー情報JSONファイルのパス")
	userID := fs.String("user", "", "ユーザーID")
	password := fs.String("password", "", "パスワード")
	fs.Parse(os.Args[2:])

	if *userID == "" || *password == "" {
		log.Fatal("-user and -password are required")
	}

	s := &auth.Server{UsersFile: *usersFile}
	if err := s.LoadUsers(); err != nil {
		log.Fatalf("failed to load users: %v", err)
	}

	if err := s.AddUser(*userID, *password); err != nil {
		log.Fatalf("failed to add user: %v", err)
	}

	fmt.Printf("user '%s' added to %s\n", *userID, *usersFile)
}
