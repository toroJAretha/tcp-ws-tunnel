package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

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

	mu    sync.RWMutex
	users []User
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

// SaveUsers はユーザー情報をJSONファイルに保存する。
func (s *Server) SaveUsers() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
	// 既存ユーザーの更新チェック
	for i, u := range s.users {
		if u.UserID == userID {
			s.users[i].PasswordHash = string(hash)
			s.mu.Unlock()
			return s.SaveUsers()
		}
	}
	s.users = append(s.users, User{UserID: userID, PasswordHash: string(hash)})
	s.mu.Unlock()
	return s.SaveUsers()
}

// authenticate はユーザーIDとパスワードを検証する。
func (s *Server) authenticate(userID, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, u := range s.users {
		if u.UserID == userID {
			return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
		}
	}
	return false
}

// Handler はHTTPハンドラを返す。
func (s *Server) Handler() http.Handler {
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

	var req tokenRequest
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
