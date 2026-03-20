# Port Tunnel - ポート転送ツール

HTTP(WebSocket)を使ったTCPトンネリングツール。ルーターのポート開放なしで任意のTCPサービスを公開

## 仕組み

```
[外部クライアント] → [公開サーバー:<PORT>] ⟷ WebSocket ⟷ [自宅PC] → [localhost:<PORT>]
```

1台のサーバー(VPS)で複数のクライアントを同時にホストできる。各クライアントはそれぞれ異なる公開ポートを使用する。

```
クライアントA (自宅PC-A) ⟷ WebSocket ⟷ [VPS :49152] ← 外部ユーザー
クライアントB (自宅PC-B) ⟷ WebSocket ⟷ [VPS :49153] ← 外部ユーザー
クライアントC (自宅PC-C) ⟷ WebSocket ⟷ [VPS :49154] ← 外部ユーザー
```

1. クライアント(自宅)がサーバー(VPS)にWebSocketで接続し、公開ポートを登録
2. 外部クライアントがサーバーの該当ポートに接続
3. サーバーがWebSocket経由でクライアントにトラフィックを転送
4. クライアントがローカルサービスにトラフィックを転送

## ビルド

```bash
# サーバー側
go build -o tunnel-server ./cmd/server

# クライアント
go build -o tunnel-client ./cmd/client
```

## 使い方

### サーバー側（VPS）

```bash
./tunnel-server -control :8080
```

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `-control` | `:8080` | クライアントが接続するHTTP/WSアドレス |

サーバーは1プロセスで複数クライアントに対応する。公開ポートはクライアント接続時に動的に割り当てられる。

### クライアント側（自宅）

```bash
./tunnel-client -server ws://your-vps:8080 -public <PORT> -local localhost:<PORT>
```

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `-server` | (必須) | サーバーのWebSocket URL |
| `-public` | (必須) | サーバー側で公開するポート番号 |
| `-local` | `localhost:49152` | 転送先のローカルアドレス |

### 例：複数サービスを1台のVPSで公開

1. VPSでサーバーを起動：
   ```bash
   ./tunnel-server -control :8080
   ```

2. ユーザーAがMinecraftサーバーを公開（ポート49152）：
   ```bash
   ./tunnel-client -server ws://vps-ip:8080 -public 49152 -local localhost:49152
   ```

3. ユーザーBがWebサーバーを公開（ポート49153）：
   ```bash
   ./tunnel-client -server ws://vps-ip:8080 -public 49153 -local localhost:49153
   ```

4. 外部クライアントはそれぞれ `vps-ip:49152`、`vps-ip:49153` で接続

## 特徴

- WebSocket経由のTCPトンネリング
- 1台のVPSサーバーで複数クライアントをホスト
- クライアント接続時に公開ポートを動的割り当て
- 自動再接続（指数バックオフ）
- Ping/Pongによるキープアライブ
- 接続タイムアウト管理
- シグナルによるグレースフルシャットダウン
