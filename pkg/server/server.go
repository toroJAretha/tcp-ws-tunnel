package server

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	ownerID    string           // トンネル所有者のユーザーID
	publicPort int              // 外部に公開するポート番号
	controlWS  *websocket.Conn  // クライアントとの制御チャネル
	controlMu  sync.Mutex       // 制御チャネルへの書き込みを直列化するミューテックス
	listener   net.Listener     // 公開ポートのTCPリスナー
}

// Server は外部公開用のトンネルサーバー。
// 複数のクライアントが同時に異なるポートでトンネルを作成できる。
type Server struct {
	ControlAddr string // HTTP/WSリッスンアドレス（例: ":8080"）
	PortMin     int    // ランダムポート割り当て範囲の下限
	PortMax     int    // ランダムポート割り当て範囲の上限
	JWTSecret   string // JWT署名検証用のHMAC秘密鍵（空の場合は認証なし）

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
		userID, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		s.handleControl(ctx, w, r, userID)
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		s.handleData(w, r, userID)
	})

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

	// ファイアウォールのカスタムチェーンを初期化
	if err := initFirewall(s.PortMin, s.PortMax); err != nil {
		log.Printf("[server] firewall initialization failed (continuing without firewall): %v", err)
	} else {
		// サーバー終了時にファイアウォールをクリーンアップ
		defer cleanupFirewall(s.PortMin, s.PortMax)
	}

	log.Printf("[server] control channel: %s, port range: %d-%d", s.ControlAddr, s.PortMin, s.PortMax)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// allocatePort はポート範囲内から未使用のランダムポートを割り当てる。
// 空きがない場合はエラーを返す。
func (s *Server) allocatePort() (int, net.Listener, error) {
	portRange := s.PortMax - s.PortMin + 1

	// ランダムな開始位置から順に空きポートを探す
	start := rand.Intn(portRange)
	for i := 0; i < portRange; i++ {
		port := s.PortMin + (start+i)%portRange

		if _, exists := s.tunnels[port]; exists {
			continue
		}

		listenAddr := fmt.Sprintf(":%d", port)
		listener, err := net.Listen("tcp", listenAddr)
		if err != nil {
			continue // OSレベルで使用中の場合はスキップ
		}

		return port, listener, nil
	}

	return 0, nil, fmt.Errorf("no available port in range %d-%d", s.PortMin, s.PortMax)
}

// authenticate はリクエストのAuthorizationヘッダーのJWTを検証し、ユーザーIDを返す。
// JWTSecret未設定の場合は常に許可する。接続時のみ検証し、接続中は再検証しない。
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.JWTSecret == "" {
		return "", true
	}
	auth := r.Header.Get("Authorization")
	tokenStr := strings.TrimPrefix(auth, "Bearer ")
	if tokenStr == "" || tokenStr == auth {
		log.Printf("[server] missing token from %s", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}

	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(s.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		log.Printf("[server] invalid token from %s: %v", r.RemoteAddr, err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}

	// JWTのsubjectからユーザーIDを取得
	subject, _ := token.Claims.GetSubject()
	return subject, true
}

// handleControl はクライアントからの制御チャネル接続を処理する。
// ランダムなポートを割り当て、クライアントに通知する。
func (s *Server) handleControl(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) {
	// HTTPからWebSocketへアップグレード
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] control upgrade error: %v", err)
		return
	}

	// ランダムポートを割り当て
	s.mu.Lock()
	port, listener, err := s.allocatePort()
	if err != nil {
		s.mu.Unlock()
		log.Printf("[server] port allocation failed: %v", err)
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error()))
		ws.Close()
		return
	}

	tunnelID, _ := protocol.GenerateConnectionID()
	tunnelCtx, tunnelCancel := context.WithCancel(ctx)

	t := &tunnel{
		id:         tunnelID,
		ownerID:    userID,
		publicPort: port,
		controlWS:  ws,
		listener:   listener,
	}

	s.tunnels[port] = t
	s.mu.Unlock()

	// ファイアウォールでポートを開放
	if err := openPort(port); err != nil {
		log.Printf("[server] firewall open failed (port %d): %v", port, err)
	}

	// 割り当てたポートをクライアントに通知
	t.controlMu.Lock()
	err = ws.WriteJSON(protocol.ControlMessage{
		Type: protocol.TypePortAssigned,
		Port: port,
	})
	t.controlMu.Unlock()
	if err != nil {
		tunnelCancel()
		listener.Close()
		s.mu.Lock()
		delete(s.tunnels, port)
		s.mu.Unlock()
		ws.Close()
		return
	}

	log.Printf("[server] tunnel client connected from %s, assigned port :%d", r.RemoteAddr, port)

	// このトンネルのTCP接続受付を開始
	go s.acceptTCP(tunnelCtx, listener, t)

	// クライアント切断時のクリーンアップ
	defer func() {
		tunnelCancel()
		listener.Close()

		// ファイアウォールでポートを閉鎖
		if err := closePort(port); err != nil {
			log.Printf("[server] firewall close failed (port %d): %v", port, err)
		}

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
func (s *Server) handleData(w http.ResponseWriter, r *http.Request, userID string) {
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

	// 接続IDの所有者を検証（トンネルの所有者とJWTのユーザーIDが一致するか）
	ownerMatch := true
	if userID != "" {
		for _, t := range s.tunnels {
			if t.id == p.tunnelID {
				if t.ownerID != userID {
					ownerMatch = false
				}
				break
			}
		}
	}
	if !ownerMatch {
		s.mu.Unlock()
		log.Printf("[server] user %s attempted to access connection %s owned by another user", userID, connID[:8])
		http.Error(w, "forbidden", http.StatusForbidden)
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
