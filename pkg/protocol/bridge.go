package protocol

import (
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// Bridge はTCP接続とWebSocket間でデータを双方向にコピーする。
// いずれか一方が終了すると、両方の接続を速やかに閉じる。
func Bridge(tcp net.Conn, ws *websocket.Conn) {
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

	// 片方が終了したらデッドラインを設定して他方を速やかに終了させる
	<-done
	tcp.SetDeadline(time.Now().Add(5 * time.Second))
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	<-done
	tcp.Close()
	ws.Close()
}
