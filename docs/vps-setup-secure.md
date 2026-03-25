# VPSサーバーセットアップ手順（セキュア版 / Caddy + TLS）

Ubuntu 24.04 LTS を前提とした、tunnel-server の環境構築手順。
Caddy をリバースプロキシとして使用し、WebSocket 通信を TLS（wss://）で暗号化する。
ドメインが必要。rootでSSHログインした状態から開始する。

> IP直接接続の簡易構成は [vps-setup-ip.md](vps-setup-ip.md) を参照すること。

## 1. システム更新

```bash
apt update && apt upgrade -y
```

## 2. 専用ユーザー作成

```bash
adduser tunnel-server-manager
usermod -aG sudo tunnel-server-manager
```

## 3. SSH鍵認証の設定（tunnel-server-managerユーザー）

```bash
su - tunnel-server-manager
mkdir -p ~/.ssh && chmod 700 ~/.ssh
vi ~/.ssh/authorized_keys   # 自宅PCの公開鍵を貼り付ける
chmod 600 ~/.ssh/authorized_keys
exit
```

## 4. SSHセキュリティ強化(rootユーザー操作)

```bash
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin no/' /etc/ssh/sshd_config
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
systemctl restart ssh
```

以降は `tunnel-server-manager` ユーザーでSSHログインして作業する。

## 5. SSH config設定（自宅PC側）

`~/.ssh/config`（Windows: `C:\Users\<ユーザー名>\.ssh\config`）に以下を追加する：

```
Host tunnel-server
    HostName <VPS-IP>
    User tunnel-server-manager
    IdentityFile ~/.ssh/id_ed25519
```

以降は `ssh tunnel-server` で接続できる。

## 6. バイナリ配置

```bash
mkdir -p ~/tunnel-server
```

自宅PC側からバイナリをアップロードする：

```bash
scp builds/tunnel-server tunnel-server:~/tunnel-server/
scp builds/tunnel-auth tunnel-server:~/tunnel-server/
```

VPS側で実行権限を付与：

```bash
chmod +x ~/tunnel-server/tunnel-server
chmod +x ~/tunnel-server/tunnel-auth
```

## 7. ファイアウォール設定

公開ポートはtunnel-serverがiptablesで動的に管理するため、ufwでの範囲許可は不要。

```bash
sudo ufw allow 22/tcp           # SSH
sudo ufw allow 80/tcp           # Caddy（証明書取得用）
sudo ufw allow 443/tcp          # Caddy（HTTPS/WSS）
sudo ufw enable
```

tunnel-serverにiptables操作権限を付与する：

```bash
sudo setcap cap_net_admin+ep /home/tunnel-server-manager/tunnel-server/tunnel-server
```

> **注意**: バイナリを更新するたびにsetcapの再実行が必要。

## 7.5. 認証設定

### JWT秘密鍵の生成

tunnel-serverとtunnel-authで共有するHMAC秘密鍵を設定する。

```bash
openssl rand -hex 32
```

生成した値を環境変数ファイルに保存する：

```bash
cat > ~/tunnel-server/.tunnel-env << 'EOF'
AUTH_SECRET=<上で生成した秘密鍵>
EOF
chmod 600 ~/tunnel-server/.tunnel-env
```

### ユーザー登録

`tunnel-auth add-user` コマンドでユーザーを追加する。
パスワードはbcryptでハッシュ化されて `users.json` に保存される。

```bash
cd ~/tunnel-server
./tunnel-auth add-user -user <ユーザーID> -password <パスワード>
```


複数ユーザーを追加する場合はコマンドを繰り返す：

```bash
./tunnel-auth add-user -user user1 -password pass1
./tunnel-auth add-user -user user2 -password pass2
```

登録済みユーザーのパスワードを変更する場合は、同じユーザーIDで再度実行する。

> **注意**: `users.json` のパーミッションを確認すること。
> ```bash
> chmod 600 ~/tunnel-server/users.json
> ```

## 8. Caddyのインストールと設定

Caddyをリバースプロキシとして使用し、WebSocket通信をTLS（wss://）で暗号化する。

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update
sudo apt install caddy
```

Caddyfileを作成する：

`<YOUR-DOMAIN>`を取得したドメインに差し替えること。
DNS設定で `<YOUR-DOMAIN>` のAレコードをVPSのIPアドレスに向けておくこと。
Caddyが自動的にLet's Encrypt証明書を取得・更新する。

```bash
sudo tee /etc/caddy/tunnel.caddy > /dev/null << 'EOF'
<YOUR-DOMAIN> {
    handle /token {
        reverse_proxy localhost:8081
    }
    handle {
        reverse_proxy localhost:8080
    }
}
EOF
```

```bash
sudo tee /etc/caddy/Caddyfile > /dev/null << 'EOF'
    import /etc/caddy/tunnel.caddy
EOF
```

Caddyを再起動して設定を反映：

```bash
sudo systemctl restart caddy
```

## 9. systemdサービス化

自動起動・異常時の自動再起動を設定する。

```bash
sudo tee /etc/systemd/system/tunnel-server.service > /dev/null << 'EOF'
[Unit]
Description=TCP Tunnel Server
After=network.target

[Service]
Type=simple
User=tunnel-server-manager
WorkingDirectory=/home/tunnel-server-manager/tunnel-server
EnvironmentFile=/home/tunnel-server-manager/tunnel-server/.tunnel-env
ExecStart=/home/tunnel-server-manager/tunnel-server/tunnel-server -control :8080 -port-min 49152 -port-max 49200 -secret ${AUTH_SECRET} -max-per-user 1
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable tunnel-server
sudo systemctl start tunnel-server
```

### 認証APIサービス

```bash
sudo tee /etc/systemd/system/tunnel-auth.service > /dev/null << 'EOF'
[Unit]
Description=Tunnel Auth API
After=network.target

[Service]
Type=simple
User=tunnel-server-manager
WorkingDirectory=/home/tunnel-server-manager/tunnel-server
EnvironmentFile=/home/tunnel-server-manager/tunnel-server/.tunnel-env
ExecStart=/home/tunnel-server-manager/tunnel-server/tunnel-auth -addr :8081 -secret ${AUTH_SECRET} -users /home/tunnel-server-manager/tunnel-server/users.json -expiry 24h
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable tunnel-auth
sudo systemctl start tunnel-auth
```

## 10. 動作確認

```bash
# ステータス確認
sudo systemctl status tunnel-server
sudo systemctl status tunnel-auth

# ログ確認（リアルタイム）
sudo journalctl -u tunnel-server -f
sudo journalctl -u tunnel-auth -f
```

## 11. 自宅PCからの接続テスト

自宅PC側で以下を実行：

```bash
tunnel-client.exe -server wss://<YOUR-DOMAIN> -local localhost:25565 -user <認証ユーザーID> -password <認証パスワード>
```

クライアントが自動的に `https://<YOUR-DOMAIN>/token` へID/PWを送信してJWTを取得し、トンネル接続を開始する。

VPS側のログに `tunnel client connected` と `assigned port` が表示されれば成功。
クライアント側のログに表示されるポート番号を外部ユーザーに共有する。
