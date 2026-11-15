//go:build windows

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestE2EWindowsService 验收项（task.md §10）：Windows 服务 install/start/stop/
// uninstall，服务名为 pairlink-<mode>。
//
// 本机以非管理员运行：断言 -service install 给出可读错误而不是崩溃或静默成功。
// 如果测试进程是管理员，则完整执行 install → start → RUNNING → stop → uninstall。
func TestE2EWindowsService(t *testing.T) {
	// 先验证服务名参数错误（不依赖管理员）。
	code, out := runBin(t, "-relay", "-service", "no-such-op")
	if code != 2 {
		t.Fatalf("-service no-such-op 退出码 = %d，期望 2（输出=%q）", code, out)
	}
	if !strings.Contains(out, "未知的 -service 操作") {
		t.Fatalf("-service no-such-op 错误信息不清晰: %q", out)
	}

	relayCfg, relayPath := writeRelayINI(t, relaySpec{
		Tunnel: hostPort(freeTCPPort(t)),
		TCP:    hostPort(freeTCPPort(t)),
		Manage: hostPort(freeTCPPort(t)),
	})
	_ = relayCfg

	code, out = runBin(t, "-relay", "-config", relayPath, "-service", "install")
	if !windows.GetCurrentProcessToken().IsElevated() {
		if code != 1 {
			t.Fatalf("非管理员 -service install：退出码 = %d，期望 1（输出=%q）", code, out)
		}
		lower := strings.ToLower(out)
		readable := strings.Contains(out, "错误:") &&
			(strings.Contains(out, "管理员") || strings.Contains(lower, "scm") ||
				strings.Contains(lower, "access is denied") || strings.Contains(out, "拒绝访问"))
		if !readable {
			t.Fatalf("非管理员 -service install 错误不可读: %q", out)
		}
		if strings.Contains(lower, "panic") {
			t.Fatalf("非管理员 -service install 出现 panic: %q", out)
		}
		t.Logf("非管理员环境：pairlink-relay -service install 按预期失败（退出码 1）: %s",
			strings.TrimSpace(out))
		t.Log("Windows 服务完整生命周期需要管理员权限，本机未执行 install→start→stop→uninstall")
		return
	}

	// 管理员分支：完整生命周期。
	if code != 0 {
		t.Fatalf("管理员 -service install：退出码 = %d，期望 0（输出=%q）", code, out)
	}
	t.Cleanup(func() {
		_, _ = runBin(t, "-relay", "-config", relayPath, "-service", "stop")
		_, _ = runBin(t, "-relay", "-config", relayPath, "-service", "uninstall")
	})
	name := "pairlink-relay"
	if !strings.Contains(out, name) {
		t.Fatalf("install 输出未包含服务名 %s: %q", name, out)
	}

	code, out = runBin(t, "-relay", "-config", relayPath, "-service", "start")
	if code != 0 {
		t.Fatalf("-service start：退出码 = %d，期望 0（输出=%q）", code, out)
	}
	if !strings.Contains(strings.ToLower(out), "running") {
		t.Fatalf("-service start 输出未显示 running: %q", out)
	}
	scOut, _ := exec.Command("sc.exe", "query", name).CombinedOutput()
	if !strings.Contains(strings.ToUpper(string(scOut)), "RUNNING") && !strings.Contains(string(scOut), "运行") {
		t.Fatalf("sc query %s 未显示 RUNNING: %s", name, scOut)
	}
	t.Logf("sc query：%s", strings.TrimSpace(string(scOut)))

	// 日志按日期分文件（v0.5.0 起）：<base>-YYYY-MM-DD.log，取启动当天的本地日期。
	logPath := filepath.Join(filepath.Dir(pairlinkExe), name+"-"+time.Now().Format("2006-01-02")+".log")
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), "服务模式启动") {
			t.Logf("服务日志已生成: %s（%d 字节）", logPath, len(data))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("服务日志 %s 未生成或内容不完整: %v", logPath, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	code, out = runBin(t, "-relay", "-config", relayPath, "-service", "stop")
	if code != 0 {
		t.Fatalf("-service stop：退出码 = %d，期望 0（输出=%q）", code, out)
	}
	if !strings.Contains(strings.ToLower(out), "stopped") {
		t.Fatalf("-service stop 输出未显示 stopped: %q", out)
	}

	code, out = runBin(t, "-relay", "-config", relayPath, "-service", "uninstall")
	if code != 0 {
		t.Fatalf("-service uninstall：退出码 = %d，期望 0（输出=%q）", code, out)
	}
	if scOut, err := exec.Command("sc.exe", "query", name).CombinedOutput(); err == nil {
		t.Fatalf("uninstall 后 sc query 仍成功: %s", scOut)
	}
	t.Log("管理员环境：install → start(RUNNING) → stop → uninstall 全部通过")
}
