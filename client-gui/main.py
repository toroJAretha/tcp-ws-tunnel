import base64
import json
import subprocess
import threading
import tkinter as tk
from tkinter import ttk, scrolledtext
import os
import re
import signal
import sys

import win32crypt

DEFAULT_SERVER_URL = "wss://tunnel.tororincho.tech"
APP_NAME = "TunnelClient"


def _get_save_path():
    """AppData/Local/TunnelClient/settings.json のパスを返す。"""
    appdata = os.environ.get("LOCALAPPDATA", os.path.expanduser("~"))
    directory = os.path.join(appdata, "TororinchoApp", APP_NAME)
    os.makedirs(directory, exist_ok=True)
    return os.path.join(directory, "settings.json")


def _encrypt_password(password):
    """DPAPIでパスワードを暗号化し、Base64文字列で返す。"""
    encrypted = win32crypt.CryptProtectData(password.encode("utf-8"))
    return base64.b64encode(encrypted).decode("ascii")


def _decrypt_password(encrypted_b64):
    """Base64文字列をDPAPIで復号してパスワードを返す。"""
    encrypted = base64.b64decode(encrypted_b64)
    _, decrypted = win32crypt.CryptUnprotectData(encrypted)
    return decrypted.decode("utf-8")


def _load_settings():
    path = _get_save_path()
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except (FileNotFoundError, json.JSONDecodeError):
        return {}


def _save_settings(data):
    path = _get_save_path()
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)


class TunnelClientGUI:
    def __init__(self, root):
        self.root = root
        self.root.title("ぶりぶりトンネル")
        self.root.resizable(False, False)
        self.process = None
        self.running = False
        self.server_domain = ""

        self._build_ui()
        self._load_saved_inputs()
        self.root.protocol("WM_DELETE_WINDOW", self._on_close)

    def _build_ui(self):
        # 転送設定フレーム
        settings = ttk.LabelFrame(self.root, text="転送設定", padding=10)
        settings.grid(row=0, column=0, padx=10, pady=(10, 5), sticky="ew")

        ttk.Label(settings, text="ローカルアドレス:").grid(row=0, column=0, sticky="w")
        self.local_host = ttk.Entry(settings, width=25)
        self.local_host.grid(row=0, column=1, padx=(5, 0), pady=2)
        self.local_host.insert(0, "localhost")

        ttk.Label(settings, text="ポート番号:").grid(row=1, column=0, sticky="w")
        self.local_port = ttk.Entry(settings, width=25)
        self.local_port.grid(row=1, column=1, padx=(5, 0), pady=2)

        # 認証フレーム
        auth = ttk.LabelFrame(self.root, text="認証", padding=10)
        auth.grid(row=1, column=0, padx=10, pady=5, sticky="ew")

        ttk.Label(auth, text="ユーザーID:").grid(row=0, column=0, sticky="w")
        self.user_id = ttk.Entry(auth, width=25)
        self.user_id.grid(row=0, column=1, padx=(5, 0), pady=2)

        ttk.Label(auth, text="パスワード:").grid(row=1, column=0, sticky="w")
        self.password = ttk.Entry(auth, width=25, show="*")
        self.password.grid(row=1, column=1, padx=(5, 0), pady=2)

        self.save_password_var = tk.BooleanVar(value=False)
        self.save_password_check = ttk.Checkbutton(auth, text="パスワードを保存する", variable=self.save_password_var)
        self.save_password_check.grid(row=2, column=0, columnspan=2, sticky="w", pady=(5, 0))

        # ボタンフレーム
        btn_frame = ttk.Frame(self.root)
        btn_frame.grid(row=2, column=0, padx=10, pady=5)

        self.connect_btn = ttk.Button(btn_frame, text="接続", command=self._connect)
        self.connect_btn.grid(row=0, column=0, padx=5)

        self.disconnect_btn = ttk.Button(btn_frame, text="切断", command=self._disconnect, state="disabled")
        self.disconnect_btn.grid(row=0, column=1, padx=5)

        # ステータス
        self.status_var = tk.StringVar(value="未接続")
        self.status_label = ttk.Label(self.root, textvariable=self.status_var, foreground="gray")
        self.status_label.grid(row=3, column=0, padx=10, pady=(0, 5))

        # 共有URL表示フレーム
        share_frame = ttk.LabelFrame(self.root, text="共有アドレス", padding=10)
        share_frame.grid(row=4, column=0, padx=10, pady=(0, 5), sticky="ew")

        self.share_url_var = tk.StringVar(value="")
        self.share_url_entry = ttk.Entry(share_frame, textvariable=self.share_url_var, state="readonly", width=40, font=("Consolas", 11))
        self.share_url_entry.grid(row=0, column=0, padx=(0, 5))

        self.copy_btn = ttk.Button(share_frame, text="コピー", command=self._copy_share_url, state="disabled")
        self.copy_btn.grid(row=0, column=1)

        # ログ表示
        log_frame = ttk.LabelFrame(self.root, text="ログ", padding=5)
        log_frame.grid(row=5, column=0, padx=10, pady=(0, 10), sticky="ew")

        self.log_area = scrolledtext.ScrolledText(log_frame, width=55, height=10, state="disabled", font=("Meiryo", 9))
        self.log_area.pack()

    def _load_saved_inputs(self):
        settings = _load_settings()
        if settings.get("local_host"):
            self.local_host.delete(0, tk.END)
            self.local_host.insert(0, settings["local_host"])
        if settings.get("local_port"):
            self.local_port.insert(0, settings["local_port"])
        if settings.get("user_id"):
            self.user_id.insert(0, settings["user_id"])
        if settings.get("save_password"):
            self.save_password_var.set(True)
            if settings.get("password_encrypted"):
                try:
                    pw = _decrypt_password(settings["password_encrypted"])
                    self.password.insert(0, pw)
                except Exception:
                    pass

    def _save_inputs(self):
        settings = {
            "local_host": self.local_host.get().strip(),
            "local_port": self.local_port.get().strip(),
            "user_id": self.user_id.get().strip(),
            "save_password": self.save_password_var.get(),
        }
        if self.save_password_var.get():
            pw = self.password.get()
            if pw:
                settings["password_encrypted"] = _encrypt_password(pw)
        _save_settings(settings)

    def _log(self, message, max_lines=500):
        self.log_area.configure(state="normal")
        self.log_area.insert(tk.END, message + "\n")
        # ログ行数の上限を超えた場合、古い行を削除
        line_count = int(self.log_area.index("end-1c").split(".")[0])
        if line_count > max_lines:
            self.log_area.delete("1.0", f"{line_count - max_lines}.0")
        self.log_area.see(tk.END)
        self.log_area.configure(state="disabled")

    def _set_status(self, text, color):
        self.status_var.set(text)
        self.status_label.configure(foreground=color)

    def _set_share_url(self, port):
        url = f"{self.server_domain}:{port}"
        self.share_url_var.set(url)
        self.copy_btn.configure(state="normal")

    def _clear_share_url(self):
        self.share_url_var.set("")
        self.copy_btn.configure(state="disabled")

    def _copy_share_url(self):
        url = self.share_url_var.get()
        if url:
            self.root.clipboard_clear()
            self.root.clipboard_append(url)

    def _extract_domain(self, server_url):
        """wss://example.com → example.com"""
        domain = re.sub(r"^wss?://", "", server_url)
        domain = domain.rstrip("/")
        return domain

    def _find_executable(self):
        # PyInstallerで同梱された場合は _MEIPASS から取得
        if hasattr(sys, "_MEIPASS"):
            bundled = os.path.join(sys._MEIPASS, "tunnel-client.exe")
            if os.path.isfile(bundled):
                return bundled

        # 開発時はbuilds/から探す
        base_dir = os.path.dirname(os.path.abspath(__file__))
        project_dir = os.path.dirname(base_dir)
        candidates = [
            os.path.join(project_dir, "builds", "tunnel-client.exe"),
            os.path.join(project_dir, "tunnel-client.exe"),
        ]
        for path in candidates:
            if os.path.isfile(path):
                return path
        return None

    def _connect(self):
        host = self.local_host.get().strip() or "localhost"
        port = self.local_port.get().strip()
        user = self.user_id.get().strip()
        pw = self.password.get().strip()

        if not port:
            self._log("[ERROR] ポート番号を入力してください")
            return
        if not port.isdigit() or not (1 <= int(port) <= 65535):
            self._log("[ERROR] ポート番号は1〜65535の数値で入力してください")
            return
        if not user or not pw:
            self._log("[ERROR] ユーザーIDとパスワードを入力してください")
            return

        exe = self._find_executable()
        if not exe:
            self._log("[ERROR] tunnel-client.exe が見つかりません（builds/ に配置してください）")
            return

        # 入力情報を保存
        self._save_inputs()

        # 設定ファイルからサーバーURLを取得
        server_url = self._load_server_url()

        self.server_domain = self._extract_domain(server_url)
        local = f"{host}:{port}"

        self.connect_btn.configure(state="disabled")
        self.disconnect_btn.configure(state="normal")
        self._clear_share_url()
        self._set_status("接続中...", "orange")
        self._log(f"[GUI] 接続開始: {server_url} → {local}")

        cmd = [exe, "-server", server_url, "-local", local, "-user", user, "-password", pw]
        self.running = True
        threading.Thread(target=self._run_process, args=(cmd,), daemon=True).start()

    def _load_server_url(self):
        base_dir = os.path.dirname(os.path.abspath(__file__))
        config_path = os.path.join(base_dir, "config.json")
        try:
            with open(config_path, "r", encoding="utf-8") as f:
                config = json.load(f)
            url = config.get("server_url", "").strip()
            if url:
                return url
        except (FileNotFoundError, json.JSONDecodeError):
            pass
        return DEFAULT_SERVER_URL

    def _run_process(self, cmd):
        try:
            self.process = subprocess.Popen(
                cmd,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,  # Goのlogパッケージはstderrに出力するため必須
                text=True,
                creationflags=subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.CREATE_NO_WINDOW,
            )

            for line in self.process.stdout:
                line = line.rstrip()
                if line:
                    self.root.after(0, self._log, line)
                    if "assigned public port" in line:
                        match = re.search(r"assigned public port: (\d+)", line)
                        if match:
                            port = match.group(1)
                            self.root.after(0, self._set_share_url, port)
                        self.root.after(0, self._set_status, "接続済み", "green")
                    elif "authenticated" in line:
                        self.root.after(0, self._set_status, "認証成功 - 接続中...", "orange")
                    elif "authentication failed" in line.lower():
                        self.root.after(0, self._set_status, "認証失敗", "red")

            self.process.wait()
        except Exception as e:
            self.root.after(0, self._log, f"[ERROR] {e}")
        finally:
            self.running = False
            self.process = None
            self.root.after(0, self._on_process_exit)

    def _on_process_exit(self):
        self.connect_btn.configure(state="normal")
        self.disconnect_btn.configure(state="disabled")
        if self.status_var.get() != "認証失敗":
            self._set_status("切断済み", "gray")
            self._clear_share_url()
        self._log("[GUI] プロセス終了")

    def _stop_process(self):
        """プロセスにシグナルを送り、タイムアウト後に強制終了する。"""
        if not self.process:
            return
        try:
            os.kill(self.process.pid, signal.CTRL_BREAK_EVENT)
        except OSError:
            pass
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()

    def _disconnect(self):
        if self.process:
            self._log("[GUI] 切断中...")
            threading.Thread(target=self._stop_process, daemon=True).start()

    def _on_close(self):
        self._save_inputs()
        if self.process:
            self.root.withdraw()
            threading.Thread(target=self._close_after_stop, daemon=True).start()
        else:
            self.root.destroy()

    def _close_after_stop(self):
        self._stop_process()
        self.root.after(0, self.root.destroy)


def main():
    root = tk.Tk()
    TunnelClientGUI(root)
    root.mainloop()


if __name__ == "__main__":
    main()
