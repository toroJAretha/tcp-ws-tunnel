package auth

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// rateLimiter はIPアドレスごとの認証試行を制限する。
type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	max      int           // 期間内の最大試行回数
	window   time.Duration // 制限ウィンドウ
	maxIPs   int           // 追跡するIPアドレスの上限
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		attempts: make(map[string][]time.Time),
		max:      max,
		window:   window,
		maxIPs:   10000,
	}
	// 定期的に期限切れエントリを掃除
	go func() {
		ticker := time.NewTicker(window)
		defer ticker.Stop()
		for range ticker.C {
			rl.cleanup()
		}
	}()
	return rl
}

// cleanup は期限切れのエントリをマップから削除する。
func (rl *rateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.window)
	for ip, times := range rl.attempts {
		valid := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		if len(valid) == 0 {
			delete(rl.attempts, ip)
		} else {
			rl.attempts[ip] = valid
		}
	}
}

// allow は指定IPの試行を許可するかを返す。
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// 期限切れのエントリを除去
	valid := rl.attempts[ip][:0]
	for _, t := range rl.attempts[ip] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) == 0 {
		delete(rl.attempts, ip)
	}

	if len(valid) >= rl.max {
		rl.attempts[ip] = valid
		return false
	}

	// IPアドレス数の上限を超えた場合は新規IPを拒否
	if len(valid) == 0 && len(rl.attempts) >= rl.maxIPs {
		return false
	}

	rl.attempts[ip] = append(valid, now)
	return true
}

// User はユーザー情報を保持する。
type User struct {
	UserID       string `json:"user_id"`
	PasswordHash string `json:"password_hash"`
}

// Server はJWTを発行する認証APIサーバー。
type Server struct {
	Secret    string // JWT署名用のHMAC秘密鍵
	UsersFile string // ユーザー情報JSONファイルのパス
	Expiry    string // JWTの有効期限（例: "24h", "7d"）

	mu          sync.RWMutex
	users       []User
	limiter     *rateLimiter
	limiterOnce sync.Once
}

// tokenRequest はトークン発行リクエストのJSON構造。
type tokenRequest struct {
	UserID   string `json:"user_id"`
	Password string `json:"password"`
}

// tokenResponse はトークン発行レスポンスのJSON構造。
type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// errorResponse はエラーレスポンスのJSON構造。
type errorResponse struct {
	Error string `json:"error"`
}

// LoadUsers はJSONファイルからユーザー情報を読み込む。
// ファイルが存在しない場合は空のリストで初期化する。
func (s *Server) LoadUsers() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.UsersFile)
	if err != nil {
		if os.IsNotExist(err) {
			s.users = []User{}
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.users)
}

// saveUsersLocked はユーザー情報をJSONファイルに保存する。
// 呼び出し元がs.muを保持していること。
func (s *Server) saveUsersLocked() error {
	data, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.UsersFile, data, 0600)
}

// AddUser はユーザーを追加する。パスワードはbcryptでハッシュ化して保存する。
// 同じIDのユーザーが存在する場合はパスワードを更新する。
func (s *Server) AddUser(userID, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 既存ユーザーの更新チェック
	for i, u := range s.users {
		if u.UserID == userID {
			s.users[i].PasswordHash = string(hash)
			return s.saveUsersLocked()
		}
	}
	s.users = append(s.users, User{UserID: userID, PasswordHash: string(hash)})
	return s.saveUsersLocked()
}

// dummyHash はユーザーが存在しない場合でも一定時間のbcrypt比較を行うためのダミーハッシュ。
// タイミング攻撃によるユーザー列挙を防止する。
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

// authenticate はユーザーIDとパスワードを検証する。
func (s *Server) authenticate(userID, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, u := range s.users {
		if u.UserID == userID {
			return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
		}
	}
	// ユーザーが存在しなくてもbcrypt比較を実行し、応答時間を均一にする
	bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	return false
}

// Handler はHTTPハンドラを返す。
func (s *Server) Handler() http.Handler {
	s.limiterOnce.Do(func() {
		// 1分間に最大5回の認証試行を許可
		s.limiter = newRateLimiter(5, time.Minute)
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	return mux
}

// parseExpiry は有効期限文字列をtime.Durationに変換する。
// "7d" のような日単位の指定にも対応する。
func parseExpiry(expiry string) (time.Duration, error) {
	if strings.HasSuffix(expiry, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(expiry, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(expiry)
}

// handleToken はPOST /token でJWTを発行する。
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// レートリミットチェック
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	if !s.limiter.allow(ip) {
		log.Printf("[auth] rate limit exceeded from %s", r.RemoteAddr)
		writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "too many requests"})
		return
	}

	var req tokenRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}

	if !s.authenticate(req.UserID, req.Password) {
		log.Printf("[auth] failed login attempt for user: %s from %s", req.UserID, r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "invalid credentials"})
		return
	}

	// 有効期限を算出
	duration, err := parseExpiry(s.Expiry)
	if err != nil {
		log.Printf("[auth] invalid expiry format: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "server configuration error"})
		return
	}
	expiresAt := time.Now().Add(duration)

	// JWTを生成
	claims := jwt.RegisteredClaims{
		Subject:   req.UserID,
		ExpiresAt: jwt.NewNumericDate(expiresAt),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(s.Secret))
	if err != nil {
		log.Printf("[auth] token signing error: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "token generation failed"})
		return
	}

	log.Printf("[auth] token issued for user: %s (expires: %s)", req.UserID, expiresAt.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, tokenResponse{
		Token:     tokenStr,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	})
}

// writeJSON はJSONレスポンスを書き込む。
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
