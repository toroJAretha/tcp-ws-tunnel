# VPSサーバーセットアップ手順

Ubuntu 24.04 LTS を前提とした、tunnel-server の環境構築手順。
rootでSSHログインした状態から開始する。

## 1. システム更新

```bash
apt update && apt upgrade -y
```

## 2. 専用ユーザー作成

```bash
adduser tunnel-server-manageer
usermod -aG sudo tunnel-server-manageer
```

## 3. SSH鍵認証の設定（tunnel-server-manageerユーザー）

```bash
su - tunnel-server-manageer
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

以降は `tunnel-server-manageer` ユーザーでSSHログインして作業する。

## 5. SSH config設定（自宅PC側）

`~/.ssh/config`（Windows: `C:\Users\<ユーザー名>\.ssh\config`）に以下を追加する：

```
Host tunnel-server
    HostName <VPS-IP>
    User tunnel-server-manageer
    IdentityFile ~/.ssh/id_ed25519
```

以降は `ssh tunnel-server` で接続できる。

## 6. バイナリ配置

```bash
mkdir -p ~/tunnel-server
```

自宅PC側からバイナリをアップロードする：

```bash
scp tunnel-server tunnel-server:~/tunnel-server/
```

VPS側で実行権限を付与：

```bash
chmod +x ~/tunnel-server/tunnel-server
```

## 7. ファイアウォール設定

```bash
sudo ufw allow 22/tcp           # SSH
sudo ufw allow 80/tcp           # Caddy（証明書取得用）
sudo ufw allow 443/tcp          # Caddy（HTTPS/WSS）
sudo ufw allow 49152:49200/tcp  # クライアントが使う公開ポート範囲
sudo ufw enable
```

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
sudo tee /etc/caddy/Caddyfile > /dev/null << 'EOF'
<YOUR-DOMAIN> {
    reverse_proxy localhost:8080
}
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
User=tunnel-server-manageer
WorkingDirectory=/home/tunnel-server-manageer/tunnel-server
ExecStart=/home/tunnel-server-manageer/tunnel-server/tunnel-server -control :8080
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable tunnel-server
sudo systemctl start tunnel-server
```

## 10. 動作確認

```bash
# ステータス確認
sudo systemctl status tunnel-server

# ログ確認（リアルタイム）
sudo journalctl -u tunnel-server -f
```

## 11. 自宅PCからの接続テスト

自宅PC側で以下を実行：

```bash
tunnel-client.exe -server wss://<YOUR-DOMAIN> -public 49152 -local localhost:49152
```

VPS側のログに `tunnel client connected` と表示されれば成功。
