# Port Tunnel - ポート転送ツール

HTTP(WebSocket)を使ったTCPトンネリングツール。ルーターのポート開放なしで任意のTCPサービスを公開

## 仕組み

```
[外部クライアント] → [公開サーバー:<ランダムポート>] ⟷ WebSocket ⟷ [自宅PC] → [localhost:<PORT>]
```

1台のサーバー(VPS)で複数のクライアントを同時にホストできる。各クライアントにはサーバーがランダムなポートを自動割り当てする。

```
クライアントA (自宅PC-A) ⟷ WebSocket ⟷ [VPS :59382] ← 外部ユーザー
クライアントB (自宅PC-B) ⟷ WebSocket ⟷ [VPS :51847] ← 外部ユーザー
クライアントC (自宅PC-C) ⟷ WebSocket ⟷ [VPS :63291] ← 外部ユーザー
```

1. クライアント(自宅)がサーバー(VPS)にWebSocketで接続
2. サーバーがランダムなポートを割り当て、クライアントに通知
3. 外部クライアントがサーバーの該当ポートに接続
4. サーバーがWebSocket経由でクライアントにトラフィックを転送
5. クライアントがローカルサービスにトラフィックを転送

## ビルド

```bash
# サーバー側（Linux向け）

# bash 
GOOS=linux GOARCH=amd64 go build -o builds/tunnel-server ./cmd/server
GOOS=linux GOARCH=amd64 go build -o builds/tunnel-auth ./cmd/auth
# powershell
$env:GOOS="linux";
$env:GOARCH="amd64";
go build -o builds/tunnel-server ./cmd/server;  
go build -o builds/tunnel-auth ./cmd/auth; $env:GOOS="";
$env:GOARCH=""

# クライアント（Windows向け）
go build -o builds/tunnel-client.exe ./cmd/client
```

## 使い方

### サーバー側（VPS）

```bash
./tunnel-server -control :8080
```

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `-control` | `:8080` | クライアントが接続するHTTP/WSアドレス |
| `-port-min` | `49152` | ランダムポート割り当て範囲の下限 |
| `-port-max` | `65535` | ランダムポート割り当て範囲の上限 |
| `-secret` | (なし) | JWT署名検証用のHMAC秘密鍵（未設定の場合は認証なし） |
| `-max-per-user` | `1` | 1ユーザーあたりの最大トンネル数（0で無制限） |

サーバーは1プロセスで複数クライアントに対応する。公開ポートはクライアント接続時にランダムに割り当てられる。

### クライアント側（自宅）

```bash
tunnel-client.exe -server wss://<YOUR-DOMAIN> -local localhost:25565
```

| フラグ | デフォルト | 説明 |
|--------|-----------|------|
| `-server` | (必須) | サーバーのWebSocket URL |
| `-local` | `localhost:25565` | 転送先のローカルアドレス |

接続するとサーバーから割り当てられたポート番号がログに表示される。このポート番号を外部ユーザーに共有する。

### 例：複数サービスを1台のVPSで公開

1. VPSでサーバーを起動：
   ```bash
   ./tunnel-server -control :8080
   ```

2. ユーザーAがMinecraftサーバーを公開：
   ```bash
   tunnel-client.exe -server wss://tunnel.example.com -local localhost:25565
   # ログ出力: [client] assigned public port: 59382
   ```

3. ユーザーBがWebサーバーを公開：
   ```bash
   tunnel-client.exe -server wss://tunnel.example.com -local localhost:8000
   # ログ出力: [client] assigned public port: 51847
   ```

4. 外部クライアントはそれぞれ `tunnel.example.com:59382`、`tunnel.example.com:51847` で接続

## セキュリティ

- Caddy + TLSによるWebSocket通信の暗号化（wss://）
- JWT認証（認証APIでトークン発行、HMAC-SHA256署名検証）
- データチャネルのセッション紐付け（他ユーザーの接続乗っ取り防止）
- 1ユーザーあたりのトンネル数制限（リソース枯渇防止）
- bcryptによるパスワードハッシュ化（サーバー側）
- DPAPIによるパスワード暗号化（クライアントGUI側）
- ランダムポート割り当てによるポート推測の困難化
- レート制限によるポートスキャン対策
- エラーコードのみ返却（内部情報の非公開）

## エラーコード

| コード | 意味 |
|--------|------|
| `E1001` | トークン未指定 |
| `E1002` | トークン無効・期限切れ |
| `E1003` | アクセス権限なし |
| `E2001` | トンネル数の上限超過 |
| `E3001` | リクエストパラメータ不正 |
| `E3002` | リソースが見つからない |
| `E5001` | ポート割り当て失敗 |

## セットアップ

- [VPSセットアップ手順（セキュア版 / Caddy + TLS）](docs/vps-setup-secure.md)
- [VPSセットアップ手順（IP直接接続版）](docs/vps-setup-ip.md)

## 特徴

- WebSocket経由のTCPトンネリング
- 1台のVPSサーバーで複数クライアントをホスト
- ランダムポート自動割り当て
- 自動再接続（指数バックオフ）
- Ping/Pongによるキープアライブ
- 接続タイムアウト管理
- シグナルによるグレースフルシャットダウン
