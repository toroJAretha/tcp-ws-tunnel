# VPSサーバーセットアップ手順（IP直接接続版）

Ubuntu 24.04 LTS を前提とした、tunnel-server の環境構築手順。
ドメインやTLSを使わず、IPアドレスで直接接続する簡易構成。
rootでSSHログインした状態から開始する。

> **注意**: この構成ではWebSocket通信が平文（ws://）のため、経路上での盗聴・改ざんのリスクがある。
> セキュアな構成が必要な場合は [vps-setup-secure.md](vps-setup-secure.md) を参照すること。

## 1. システム更新

```bash
apt update && apt upgrade -y
```

## 2. 専用ユーザー作成

```bash
adduser tunnel-sever-manageer
usermod -aG sudo tunnel-sever-manageer
```

## 3. SSH鍵認証の設定（tunnel-sever-manageerユーザー）

```bash
su - tunnel-sever-manageer
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

以降は `tunnel-sever-manageer` ユーザーでSSHログインして作業する。

## 5. SSH config設定（自宅PC側）

`~/.ssh/config`（Windows: `C:\Users\<ユーザー名>\.ssh\config`）に以下を追加する：

```
Host tunnel-server
    HostName <VPS-IP>
    User tunnel-sever-manageer
    IdentityFile ~/.ssh/id_ed25519
```

以降は `ssh tunnel-server` で接続できる。

## 6. バイナリ配置

```bash
mkdir -p ~/tunnel-sever
```

自宅PC側からバイナリをアップロードする：

```bash
scp tunnel-server tunnel-server:~/tunnel-sever/
```

VPS側で実行権限を付与：

```bash
chmod +x ~/tunnel-sever/tunnel-server
```

## 7. ファイアウォール設定

```bash
sudo ufw allow 22/tcp           # SSH
sudo ufw allow 8080/tcp         # 制御チャネル（WebSocket）
sudo ufw allow 49152:49200/tcp  # クライアントが使う公開ポート範囲
sudo ufw enable
```

## 8. systemdサービス化

自動起動・異常時の自動再起動を設定する。

```bash
sudo tee /etc/systemd/system/tunnel-server.service > /dev/null << 'EOF'
[Unit]
Description=TCP Tunnel Server
After=network.target

[Service]
Type=simple
User=tunnel-sever-manageer
WorkingDirectory=/home/tunnel-sever-manageer/tunnel-sever
ExecStart=/home/tunnel-sever-manageer/tunnel-sever/tunnel-server -control :8080
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable tunnel-server
sudo systemctl start tunnel-server
```

## 9. 動作確認

```bash
# ステータス確認
sudo systemctl status tunnel-server

# ログ確認（リアルタイム）
sudo journalctl -u tunnel-server -f
```

## 10. 自宅PCからの接続テスト

自宅PC側で以下を実行：

```bash
tunnel-client.exe -server ws://<VPS-IP>:8080 -public 49152 -local localhost:49152
```

VPS側のログに `tunnel client connected` と表示されれば成功。
