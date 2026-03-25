package server

import (
	"fmt"
	"log"
	"os/exec"
	"os/user"
)

// firewall はiptablesのカスタムチェーン「TUNNEL」を操作して、
// 使用中のポートのみを外部に公開する。
// 未使用ポートへのパケットはDROPされ、外部からは存在が見えない。

const chainName = "TUNNEL"

// checkFirewallAvailable はiptablesの実行可否を確認する。
func checkFirewallAvailable() error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}
	if u.Uid != "0" {
		return fmt.Errorf("root privileges required (current uid: %s)", u.Uid)
	}

	if _, err := exec.LookPath("iptables"); err != nil {
		return fmt.Errorf("iptables not found: %w", err)
	}

	return nil
}

// initFirewall はTUNNELチェーンを作成し、INPUTチェーンから参照する。
// サーバー起動時に1回だけ呼び出す。
func initFirewall(portMin, portMax int) error {
	if err := checkFirewallAvailable(); err != nil {
		return fmt.Errorf("firewall unavailable: %w", err)
	}

	// チェーンが既に存在する場合はフラッシュ
	exec.Command("iptables", "-N", chainName).Run()
	if err := exec.Command("iptables", "-F", chainName).Run(); err != nil {
		return fmt.Errorf("failed to flush chain %s: %w", chainName, err)
	}

	// ポート範囲宛のパケットをTUNNELチェーンに転送するルールを追加
	// 重複防止のため既存ルールを先に削除
	jumpRule := []string{"-p", "tcp", "--dport", fmt.Sprintf("%d:%d", portMin, portMax), "-j", chainName}
	exec.Command("iptables", append([]string{"-D", "INPUT"}, jumpRule...)...).Run()
	if err := exec.Command("iptables", append([]string{"-I", "INPUT"}, jumpRule...)...).Run(); err != nil {
		cleanupFirewall(portMin, portMax)
		return fmt.Errorf("failed to add INPUT jump rule: %w", err)
	}

	// TUNNELチェーンの末尾でDROP（許可されていないポートはすべて破棄）
	if err := exec.Command("iptables", "-A", chainName, "-j", "DROP").Run(); err != nil {
		cleanupFirewall(portMin, portMax)
		return fmt.Errorf("failed to add DROP rule: %w", err)
	}

	log.Printf("[firewall] initialized chain %s for port range %d-%d", chainName, portMin, portMax)
	return nil
}

// openPort は指定ポートへのTCP接続を許可するルールをTUNNELチェーンに追加する。
func openPort(port int) error {
	err := exec.Command("iptables", "-I", chainName, "1",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT").Run()
	if err != nil {
		return fmt.Errorf("failed to open port %d: %w", port, err)
	}
	log.Printf("[firewall] opened port %d", port)
	return nil
}

// closePort は指定ポートへのACCEPTルールをTUNNELチェーンから削除する。
func closePort(port int) error {
	err := exec.Command("iptables", "-D", chainName,
		"-p", "tcp", "--dport", fmt.Sprintf("%d", port), "-j", "ACCEPT").Run()
	if err != nil {
		return fmt.Errorf("failed to close port %d: %w", port, err)
	}
	log.Printf("[firewall] closed port %d", port)
	return nil
}

// cleanupFirewall はTUNNELチェーンを削除してiptablesを元の状態に戻す。
// サーバー終了時に呼び出す。
func cleanupFirewall(portMin, portMax int) {
	jumpRule := []string{"-p", "tcp", "--dport", fmt.Sprintf("%d:%d", portMin, portMax), "-j", chainName}
	exec.Command("iptables", append([]string{"-D", "INPUT"}, jumpRule...)...).Run()
	exec.Command("iptables", "-F", chainName).Run()
	exec.Command("iptables", "-X", chainName).Run()
	log.Printf("[firewall] cleaned up chain %s", chainName)
}
