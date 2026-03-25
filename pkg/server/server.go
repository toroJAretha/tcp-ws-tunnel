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

// エラーコード定義
// E1xxx: 認証エラー
// E2xxx: リソース制限エラー
// E3xxx: リクエストエラー
// E5xxx: サーバー内部エラー
const (
	ErrCodeTokenMissing  = "E1001" // トークンが未指定
	ErrCodeTokenInvalid  = "E1002" // トークンが無効または期限切れ
	ErrCodeForbidden     = "E1003" // アクセス権限なし
	ErrCodeTunnelLimit   = "E2001" // トンネル数の上限超過
	ErrCodeBadRequest    = "E3001" // リクエストパラメータ不正
	ErrCodeNotFound      = "E3002" // リソースが見つからない
	ErrCodePortExhausted = "E5001" // ポート割り当て失敗
)

// WebSocketアップグレーダー
// Originヘッダーなし（CLIクライアント）は許可、あり（ブラウザ経由）は拒否
// 注意: CSRF防御は主にJWT認証に依存する。Originチェックは補助的な防御層。
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == ""
	},
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
	clientIP   string           // クライアントの接続元IPアドレス
	publicPort int              // 外部に公開するポート番号
	controlWS  *websocket.Conn  // クライアントとの制御チャネル
	controlMu  sync.Mutex       // 制御チャネルへの書き込みを直列化するミューテックス
	listener   net.Listener     // 公開ポートのTCPリスナー
}

// Server は外部公開用のトンネルサーバー。
// 複数のクライアントが同時に異なるポートでトンネルを作成できる。
type Server struct {
	ControlAddr    string // HTTP/WSリッスンアドレス（例: ":8080"）
	PortMin        int    // ランダムポート割り当て範囲の下限
	PortMax        int    // ランダムポート割り当て範囲の上限
	JWTSecret      string // JWT署名検証用のHMAC秘密鍵（空の場合は認証なし）
	MaxPerUser     int    // 1ユーザーあたりの最大トンネル数（0の場合は制限なし）

	mu        sync.Mutex
	tunnels   map[int]*tunnel         // 公開ポート番号をキーとしたトンネルマップ
	tunnelsByID map[string]*tunnel    // トンネルIDをキーとした逆引きマップ
	pending   map[string]*pendingConn // 接続IDをキーとした待機中のTCP接続マップ
}

// Run はサーバーを起動する。コンテキストがキャンセルされるまでブロックする。
func (s *Server) Run(ctx context.Context) error {
	s.tunnels = make(map[int]*tunnel)
	s.tunnelsByID = make(map[string]*tunnel)
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
		Addr:              s.ControlAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// タイムアウトした待機中接続の定期クリーンアップ
	go s.cleanupPending(ctx)

	// コンテキストキャンセル時にHTTPサーバーをシャットダウン（10秒タイムアウト）
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("[server] http shutdown error: %v", err)
		}
	}()

	// ファイアウォールのカスタムチェーンを初期化
	if err := initFirewall(s.PortMin, s.PortMax); err != nil {
		log.Printf("[server] firewall initialization failed (continuing without firewall): %v", err)
	} else {
		// サーバー終了時にファイアウォールをクリーンアップ
		defer cleanupFirewall(s.PortMin, s.PortMax)
	}

	if s.JWTSecret == "" {
		log.Printf("[server] WARNING: -secret is not set. Authentication, per-user tunnel limits, and session binding are disabled.")
	}
	log.Printf("[server] control channel: %s, port range: %d-%d", s.ControlAddr, s.PortMin, s.PortMax)
	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// allocatePort はランダムな未使用ポートを割り当て、リスナーを返す。
// ロック内でマップ確認とnet.Listenを一括で行い、競合を防ぐ。
func (s *Server) allocatePort() (int, net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	portRange := s.PortMax - s.PortMin + 1
	start := rand.Intn(portRange)
	for i := 0; i < portRange; i++ {
		port := s.PortMin + (start+i)%portRange
		if _, exists := s.tunnels[port]; exists {
			continue
		}
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		return port, l, nil
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
		log.Printf("[server] %s: missing token from %s", ErrCodeTokenMissing, r.RemoteAddr)
		http.Error(w, ErrCodeTokenMissing, http.StatusUnauthorized)
		return "", false
	}

	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(s.JWTSecret), nil
	})
	if err != nil || !token.Valid {
		log.Printf("[server] %s: invalid token from %s: %v", ErrCodeTokenInvalid, r.RemoteAddr, err)
		http.Error(w, ErrCodeTokenInvalid, http.StatusUnauthorized)
		return "", false
	}

	// JWTのsubjectからユーザーIDを取得
	subject, _ := token.Claims.GetSubject()
	if subject == "" {
		log.Printf("[server] %s: empty subject in token from %s", ErrCodeTokenInvalid, r.RemoteAddr)
		http.Error(w, ErrCodeTokenInvalid, http.StatusUnauthorized)
		return "", false
	}
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

	// 1ユーザーあたりのトンネル数制限チェック（ロック内）
	if s.MaxPerUser > 0 && userID != "" {
		s.mu.Lock()
		count := 0
		for _, t := range s.tunnels {
			if t.ownerID == userID {
				count++
			}
		}
		exceeded := count >= s.MaxPerUser
		s.mu.Unlock()
		if exceeded {
			log.Printf("[server] user %s exceeded max tunnels (%d)", userID, s.MaxPerUser)
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ErrCodeTunnelLimit))
			ws.Close()
			return
		}
	}

	// ポート割り当て（ロック内でマップ確認とListenを一括実行）
	port, listener, err := s.allocatePort()
	if err != nil {
		log.Printf("[server] port allocation failed: %v", err)
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, ErrCodePortExhausted))
		ws.Close()
		return
	}

	tunnelID, err := protocol.GenerateConnectionID()
	if err != nil {
		listener.Close()
		log.Printf("[server] %s: failed to generate tunnel id: %v", ErrCodePortExhausted, err)
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, ErrCodePortExhausted))
		ws.Close()
		return
	}
	tunnelCtx, tunnelCancel := context.WithCancel(ctx)

	// クライアントIPを取得
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

	t := &tunnel{
		id:         tunnelID,
		ownerID:    userID,
		clientIP:   clientIP,
		publicPort: port,
		controlWS:  ws,
		listener:   listener,
	}

	// ポートをマップに登録（ロック内）
	s.mu.Lock()
	s.tunnels[port] = t
	s.tunnelsByID[tunnelID] = t
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
		if closeErr := closePort(port); closeErr != nil {
			log.Printf("[server] firewall close failed (port %d): %v", port, closeErr)
		}
		s.mu.Lock()
		delete(s.tunnels, port)
		delete(s.tunnelsByID, tunnelID)
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
		delete(s.tunnelsByID, tunnelID)
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
		http.Error(w, ErrCodeBadRequest, http.StatusBadRequest)
		return
	}

	// 待機中の接続を取得
	s.mu.Lock()
	p, ok := s.pending[connID]
	if !ok {
		s.mu.Unlock()
		http.Error(w, ErrCodeNotFound, http.StatusNotFound)
		return
	}

	// 接続IDの所有者を検証
	ownerMatch := true
	if t, ok := s.tunnelsByID[p.tunnelID]; ok {
		if userID != "" {
			// JWT認証モード: ユーザーIDで照合
			if t.ownerID != userID {
				ownerMatch = false
			}
		} else {
			// 認証なしモード: クライアントIPで照合
			reqIP, _, _ := net.SplitHostPort(r.RemoteAddr)
			if t.clientIP != reqIP {
				ownerMatch = false
			}
		}
	}
	if !ownerMatch {
		s.mu.Unlock()
		log.Printf("[server] user %s attempted to access connection %s owned by another user", userID, protocol.ShortID(connID))
		http.Error(w, ErrCodeForbidden, http.StatusForbidden)
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

	log.Printf("[server] data channel established for connection %s", protocol.ShortID(connID))
	// TCP接続とWebSocket間で双方向データ転送を開始
	protocol.Bridge(p.conn, ws)
}

// maxConnsPerTunnel はトンネルあたりの同時TCP接続数の上限。
const maxConnsPerTunnel = 100

// acceptTCP は指定リスナーでTCP接続を受け付け、それぞれgoroutineで処理する。
// 同時接続数をmaxConnsPerTunnelで制限する。
func (s *Server) acceptTCP(ctx context.Context, listener net.Listener, t *tunnel) {
	sem := make(chan struct{}, maxConnsPerTunnel)
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

		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				s.handleTCPConn(conn, t)
			}()
		default:
			log.Printf("[server] connection limit reached (port %d), rejecting", t.publicPort)
			conn.Close()
		}
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

	log.Printf("[server] new connection %s from %s (port %d)", protocol.ShortID(connID), conn.RemoteAddr(), t.publicPort)

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
					log.Printf("[server] pending connection %s timed out", protocol.ShortID(id))
					p.conn.Close()
					delete(s.pending, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

