package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/toroJ/port-tunnel/pkg/protocol"
)

// Client はトンネルサーバーに接続し、トラフィックをローカルサービスに転送するクライアント。
type Client struct {
	ServerURL string // トンネルサーバーのWebSocket URL（例: "wss://tunnel.example.com"）
	LocalAddr string // 転送先のローカルアドレス（例: "localhost:49152"）
	AuthToken string // サーバー認証用の事前共有トークン（空の場合は認証なし）
}

// Run はクライアントを起動する。コンテキストがキャンセルされるまでブロックし、
// 切断時には指数バックオフで自動再接続する。
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second

	for {
		// コンテキストのキャンセルを確認
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// サーバーに接続（切断されるまでブロック）
		err := c.connect(ctx)
		if err != nil {
			log.Printf("[client] connection lost: %v", err)
		}

		// 再接続前にバックオフ時間だけ待機
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		// 指数バックオフ（最大30秒）
		backoff = backoff * 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		log.Printf("[client] reconnecting in %v...", backoff)
	}
}

// authHeader は認証用のHTTPヘッダーを返す。トークン未設定の場合はnilを返す。
func (c *Client) authHeader() http.Header {
	if c.AuthToken == "" {
		return nil
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.AuthToken)
	return h
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
					log.Printf("[client] connection %s error: %v", connID[:8], err)
				}
			}(msg.ConnectionID)

		case protocol.TypePing:
			// アプリケーションレベルのping（WebSocketのping/pongとは別）

		case protocol.TypeConnectionClosed:
			log.Printf("[client] server closed connection %s", msg.ConnectionID[:8])
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

	log.Printf("[client] data channel established for connection %s", connID[:8])
	// ローカル接続とWebSocket間でデータを双方向に転送
	bridge(localConn, ws)
	return nil
}

// bridge はTCP接続とWebSocket間でデータを双方向にコピーする。
// いずれか一方が終了すると、両方の接続を閉じる。
func bridge(tcp net.Conn, ws *websocket.Conn) {
	done := make(chan struct{}, 2)

	// TCP → WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := tcp.Read(buf)
			if err != nil {
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WebSocket → TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if _, err := tcp.Write(data); err != nil {
				return
			}
		}
	}()

	// 片方が終了したら両方閉じる
	<-done
	tcp.Close()
	ws.Close()
	<-done
}
