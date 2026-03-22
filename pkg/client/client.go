package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/toroJ/port-tunnel/pkg/protocol"
)

// Client はトンネルサーバーに接続し、トラフィックをローカルサービスに転送するクライアント。
type Client struct {
	ServerURL string // トンネルサーバーのWebSocket URL（例: "wss://tunnel.example.com"）
	LocalAddr string // 転送先のローカルアドレス（例: "localhost:49152"）
	UserID    string // 認証ユーザーID
	Password  string // 認証パスワード

	token string // 認証APIから取得したJWT（内部管理）
}

// Run はクライアントを起動する。コンテキストがキャンセルされるまでブロックし、
// 切断時には指数バックオフで自動再接続する。
func (c *Client) Run(ctx context.Context) error {
	// 接続前に認証APIからJWTを取得
	if err := c.fetchToken(); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	backoff := time.Second

	for {
		// コンテキストのキャンセルを確認
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// サーバーに接続（切断されるまでブロック）
		connStart := time.Now()
		err := c.connect(ctx)
		if err != nil {
			log.Printf("[client] connection lost: %v", err)
			// 認証エラーの場合はトークンを再取得
			if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "E1002") {
				log.Printf("[client] re-authenticating...")
				if authErr := c.fetchToken(); authErr != nil {
					log.Printf("[client] re-authentication failed: %v", authErr)
				}
			}
		}

		// 一定時間以上接続が維持されていた場合はバックオフをリセット
		if time.Since(connStart) > 30*time.Second {
			backoff = time.Second
		}

		// 指数バックオフ（最大30秒）
		log.Printf("[client] reconnecting in %v...", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = backoff * 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// authHeader は認証用のHTTPヘッダーを返す。
func (c *Client) authHeader() http.Header {
	if c.token == "" {
		return nil
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	return h
}

// fetchToken は認証APIからJWTを取得する。
// ServerURLのwss://をhttps://に変換し、/token エンドポイントにPOSTする。
func (c *Client) fetchToken() error {
	// wss://example.com → https://example.com/token
	parsed, err := url.Parse(c.ServerURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/token"
	authURL := parsed.String()

	body, _ := json.Marshal(map[string]string{
		"user_id":  c.UserID,
		"password": c.Password,
	})

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Post(authURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("auth failed (status %d): %s", resp.StatusCode, errResp.Error)
	}

	var tokenResp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("auth response parse error: %w", err)
	}

	c.token = tokenResp.Token
	log.Printf("[client] authenticated (expires: %s)", tokenResp.ExpiresAt)
	return nil
}

// connect はサーバーとの制御チャネルを確立し、メッセージの受信ループを実行する。
func (c *Client) connect(ctx context.Context) error {
	// 制御チャネルに接続
	controlURL := c.ServerURL + "/control"
	log.Printf("[client] connecting to %s", controlURL)

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, controlURL, c.authHeader())
	if err != nil {
		return fmt.Errorf("dial control: %w", err)
	}
	defer ws.Close()

	log.Printf("[client] connected to server")

	// キープアライブの設定
	// 60秒以内にサーバーからの応答がなければ切断とみなす
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	ws.SetPingHandler(func(appData string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	// コンテキストがキャンセルされたらWebSocket接続を閉じて読み取りループを抜ける
	go func() {
		<-ctx.Done()
		ws.Close()
	}()

	// 全データチャネルの終了を待つためのWaitGroup
	var wg sync.WaitGroup
	defer wg.Wait()

	// 制御メッセージの受信ループ
	for {
		var msg protocol.ControlMessage
		if err := ws.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				return fmt.Errorf("control read: %w", err)
			}
			return nil
		}

		switch msg.Type {
		case protocol.TypePortAssigned:
			// サーバーから割り当てられたポート番号を表示
			log.Printf("[client] assigned public port: %d", msg.Port)

		case protocol.TypeNewConnection:
			// 新しい外部接続を受信 → goroutineでデータチャネルを処理
			wg.Add(1)
			go func(connID string) {
				defer wg.Done()
				if err := c.handleNewConnection(ctx, connID); err != nil {
					log.Printf("[client] connection %s error: %v", protocol.ShortID(connID), err)
				}
			}(msg.ConnectionID)

		case protocol.TypePing:
			// アプリケーションレベルのping（WebSocketのping/pongとは別）

		case protocol.TypeConnectionClosed:
			log.Printf("[client] server closed connection %s", protocol.ShortID(msg.ConnectionID))
		}
	}
}

// handleNewConnection は新しい外部接続に対して、ローカルサービスへの接続と
// サーバーとのデータチャネルを確立し、双方向のデータ転送を行う。
func (c *Client) handleNewConnection(ctx context.Context, connID string) error {
	// ローカルサービスに接続（5秒タイムアウト）
	localConn, err := net.DialTimeout("tcp", c.LocalAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial local %s: %w", c.LocalAddr, err)
	}

	// サーバーとのデータチャネルを開設
	dataURL := c.ServerURL + "/data?id=" + url.QueryEscape(connID)
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, dataURL, c.authHeader())
	if err != nil {
		localConn.Close()
		return fmt.Errorf("dial data channel: %w", err)
	}

	log.Printf("[client] data channel established for connection %s", protocol.ShortID(connID))
	// ローカル接続とWebSocket間でデータを双方向に転送
	protocol.Bridge(localConn, ws)
	return nil
}

