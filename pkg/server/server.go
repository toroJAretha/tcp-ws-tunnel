package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/toroJ/port-tunnel/pkg/protocol"
)

// WebSocketアップグレーダー（全オリジン許可）
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// pendingConn はデータチャネルの確立を待っているTCP接続を保持する。
type pendingConn struct {
	conn     net.Conn  // 外部から受け付けたTCP接続
	tunnelID string    // 所属するトンネルのID
	created  time.Time // 作成時刻（タイムアウト管理用）
}

// tunnel は1つのクライアントのトンネルセッションを表す。
type tunnel struct {
	id         string           // トンネルの一意なID
	publicPort int              // 外部に公開するポート番号
	controlWS  *websocket.Conn  // クライアントとの制御チャネル
	controlMu  sync.Mutex       // 制御チャネルへの書き込みを直列化するミューテックス
	listener   net.Listener     // 公開ポートのTCPリスナー
}

// Server は外部公開用のトンネルサーバー。
// 複数のクライアントが同時に異なるポートでトンネルを作成できる。
type Server struct {
	ControlAddr string // HTTP/WSリッスンアドレス（例: ":8080"）

	mu      sync.Mutex
	tunnels map[int]*tunnel         // 公開ポート番号をキーとしたトンネルマップ
	pending map[string]*pendingConn // 接続IDをキーとした待機中のTCP接続マップ
}

// Run はサーバーを起動する。コンテキストがキャンセルされるまでブロックする。
func (s *Server) Run(ctx context.Context) error {
	s.tunnels = make(map[int]*tunnel)
	s.pending = make(map[string]*pendingConn)

	// HTTPルーティングの設定
	mux := http.NewServeMux()
	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		s.handleControl(ctx, w, r)
	})
	mux.HandleFunc("/data", s.handleData)

	httpServer := &http.Server{
		Addr:    s.ControlAddr,
		Handler: mux,
	}

	// タイムアウトした待機中接続の定期クリーンアップ
	go s.cleanupPending(ctx)

	// コンテキストキャンセル時にHTTPサーバーをシャットダウン
	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	log.Printf("[server] control channel: %s (multi-tenant mode)", s.ControlAddr)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// handleControl はクライアントからの制御チャネル接続を処理する。
// クエリパラメータ ?port= で指定されたポートのTCPリスナーを動的に作成する。
func (s *Server) handleControl(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// ポート番号のバリデーション
	portStr := r.URL.Query().Get("port")
	if portStr == "" {
		http.Error(w, "missing port parameter (e.g., /control?port=49152)", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "invalid port number", http.StatusBadRequest)
		return
	}

	// HTTPからWebSocketへアップグレード
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] control upgrade error: %v", err)
		return
	}

	// 指定ポートが既に他のトンネルで使用されていないか確認
	s.mu.Lock()
	if _, exists := s.tunnels[port]; exists {
		s.mu.Unlock()
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation,
				fmt.Sprintf("port %d is already in use by another client", port)))
		ws.Close()
		return
	}
	s.mu.Unlock()

	// 指定ポートでTCPリスナーを起動
	listenAddr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Printf("[server] failed to listen on %s: %v", listenAddr, err)
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr,
				fmt.Sprintf("cannot listen on port %d: %v", port, err)))
		ws.Close()
		return
	}

	tunnelID, _ := protocol.GenerateConnectionID()
	tunnelCtx, tunnelCancel := context.WithCancel(ctx)

	t := &tunnel{
		id:         tunnelID,
		publicPort: port,
		controlWS:  ws,
		listener:   listener,
	}

	// トンネルをマップに登録（リスナー作成後に再度重複チェック）
	s.mu.Lock()
	if _, exists := s.tunnels[port]; exists {
		s.mu.Unlock()
		tunnelCancel()
		listener.Close()
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation,
				fmt.Sprintf("port %d is already in use by another client", port)))
		ws.Close()
		return
	}
	s.tunnels[port] = t
	s.mu.Unlock()

	log.Printf("[server] tunnel client connected from %s, public port :%d", r.RemoteAddr, port)

	// このトンネルのTCP接続受付を開始
	go s.acceptTCP(tunnelCtx, listener, t)

	// クライアント切断時のクリーンアップ
	defer func() {
		tunnelCancel()
		listener.Close()

		s.mu.Lock()
		// トンネルをマップから削除
		delete(s.tunnels, port)
		// このトンネルに属する待機中の接続をすべて閉じる
		for id, p := range s.pending {
			if p.tunnelID == tunnelID {
				p.conn.Close()
				delete(s.pending, id)
			}
		}
		s.mu.Unlock()

		ws.Close()
		log.Printf("[server] tunnel client disconnected, released port :%d", port)
	}()

	// キープアライブの設定（60秒以内に応答がなければ切断とみなす）
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// 30秒間隔でpingを送信
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tunnelCtx.Done():
				return
			case <-ticker.C:
				t.controlMu.Lock()
				err := ws.WriteMessage(websocket.PingMessage, nil)
				t.controlMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	// 制御メッセージの受信ループ
	for {
		var msg protocol.ControlMessage
		if err := ws.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[server] control read error (port %d): %v", port, err)
			}
			return
		}

		switch msg.Type {
		case protocol.TypeConnectionClosed:
			// クライアントから接続切断の通知を受信
			s.mu.Lock()
			if p, ok := s.pending[msg.ConnectionID]; ok {
				p.conn.Close()
				delete(s.pending, msg.ConnectionID)
			}
			s.mu.Unlock()
		case protocol.TypePong:
			// キープアライブ応答
		}
	}
}

// handleData はクライアントからのデータチャネル接続を処理する。
// 待機中のTCP接続とWebSocketを紐付けて双方向転送を開始する。
func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	connID := r.URL.Query().Get("id")
	if connID == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}

	// 待機中の接続を取得
	s.mu.Lock()
	p, ok := s.pending[connID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, "unknown connection id", http.StatusNotFound)
		return
	}
	delete(s.pending, connID)
	s.mu.Unlock()

	// WebSocketへアップグレード
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] data upgrade error: %v", err)
		p.conn.Close()
		return
	}

	log.Printf("[server] data channel established for connection %s", connID[:8])
	// TCP接続とWebSocket間で双方向データ転送を開始
	bridge(p.conn, ws)
}

// acceptTCP は指定リスナーでTCP接続を受け付け、それぞれgoroutineで処理する。
func (s *Server) acceptTCP(ctx context.Context, listener net.Listener, t *tunnel) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[server] tcp accept error (port %d): %v", t.publicPort, err)
				continue
			}
		}

		go s.handleTCPConn(conn, t)
	}
}

// handleTCPConn は外部からのTCP接続を処理する。
// 接続を待機マップに登録し、制御チャネル経由でクライアントに新規接続を通知する。
func (s *Server) handleTCPConn(conn net.Conn, t *tunnel) {
	// 接続IDを生成
	connID, err := protocol.GenerateConnectionID()
	if err != nil {
		log.Printf("[server] failed to generate connection id: %v", err)
		conn.Close()
		return
	}

	// 待機マップに登録
	s.mu.Lock()
	s.pending[connID] = &pendingConn{conn: conn, tunnelID: t.id, created: time.Now()}
	s.mu.Unlock()

	log.Printf("[server] new connection %s from %s (port %d)", connID[:8], conn.RemoteAddr(), t.publicPort)

	// 制御チャネル経由でクライアントに新規接続を通知
	msg := protocol.ControlMessage{
		Type:         protocol.TypeNewConnection,
		ConnectionID: connID,
	}
	t.controlMu.Lock()
	err = t.controlWS.WriteJSON(msg)
	t.controlMu.Unlock()

	if err != nil {
		log.Printf("[server] failed to notify client (port %d): %v", t.publicPort, err)
		s.mu.Lock()
		delete(s.pending, connID)
		s.mu.Unlock()
		conn.Close()
	}
}

// cleanupPending は5秒ごとにタイムアウトした待機中の接続を削除する。
// データチャネルが10秒以内に確立されなかった接続を閉じる。
func (s *Server) cleanupPending(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for id, p := range s.pending {
				if now.Sub(p.created) > 10*time.Second {
					log.Printf("[server] pending connection %s timed out", id[:8])
					p.conn.Close()
					delete(s.pending, id)
				}
			}
			s.mu.Unlock()
		}
	}
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
