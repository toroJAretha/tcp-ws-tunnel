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
```

VPS側で実行権限を付与：

```bash
chmod +x ~/tunnel-server/tunnel-server
```

## 7. ファイアウォール設定

公開ポートはtunnel-serverがiptablesで動的に管理するため、ufwでの範囲許可は不要。

```bash
sudo ufw allow 22/tcp           # SSH
sudo ufw allow 8080/tcp         # 制御チャネル（WebSocket）
sudo ufw enable
```

tunnel-serverにiptables操作権限を付与する：

```bash
sudo setcap cap_net_admin+ep /home/tunnel-server-manager/tunnel-server/tunnel-server
```

> **注意**: バイナリを更新するたびにsetcapの再実行が必要。

## 8. systemdサービス化

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
ExecStart=/home/tunnel-server-manager/tunnel-server/tunnel-server -control :8080 -port-min 49152 -port-max 65535
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
tunnel-client.exe -server ws://<VPS-IP>:8080 -local localhost:25565
```

VPS側のログに `tunnel client connected` と `assigned port` が表示されれば成功。
クライアント側のログに表示されるポート番号を外部ユーザーに共有する。
