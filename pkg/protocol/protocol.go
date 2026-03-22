package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// 制御チャネルのメッセージタイプ
const (
	TypeNewConnection    = "new_connection"    // 新しい外部接続の通知
	TypeConnectionClosed = "connection_closed" // 接続の切断通知
	TypePortAssigned     = "port_assigned"     // サーバーが割り当てたポート番号の通知
	TypePing             = "ping"              // キープアライブ要求
	TypePong             = "pong"              // キープアライブ応答
)

// ControlMessage は制御WebSocket上でやり取りされるJSONメッセージ。
type ControlMessage struct {
	Type         string `json:"type"`                    // メッセージタイプ
	ConnectionID string `json:"connection_id,omitempty"` // 接続を一意に識別するID
	Port         int    `json:"port,omitempty"`          // 割り当てられたポート番号
	Error        string `json:"error,omitempty"`         // エラー内容
}

// ShortID はIDの先頭8文字を返す。8文字未満の場合はそのまま返す。
func ShortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// GenerateConnectionID はランダムな16バイトの16進数文字列を生成する。
func GenerateConnectionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate connection id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
