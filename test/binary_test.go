package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pairlinkExe 是 TestMain 构建出的真实二进制路径。
var pairlinkExe string

// repoRoot 是仓库根目录（test 包的父目录）。
var repoRoot string

func TestMain(m *testing.M) {
	root, err := filepath.Abs("..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析仓库根目录: %v\n", err)
		os.Exit(1)
	}
	repoRoot = root

	dir, err := os.MkdirTemp("", "pairlink-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时目录: %v\n", err)
		os.Exit(1)
	}
	exeName := "pairlink"
	if runtime.GOOS == "windows" {
		exeName = "pairlink.exe"
	}
	exe := filepath.Join(dir, exeName)
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "构建测试用 pairlink 二进制失败: %v\n%s\n", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	pairlinkExe = exe

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// runBin 运行真实二进制并返回退出码与合并输出。
func runBin(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(pairlinkExe, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("运行 %s %v: %v", pairlinkExe, args, err)
	return -1, ""
}

// TestBinaryVersionUsageAndConfigErrors 验收项：真实二进制的 -version 输出、
// 退出码 2 的参数错误、退出码 1 的配置错误。
func TestBinaryVersionUsageAndConfigErrors(t *testing.T) {
	guardGoroutines(t)

	code, out := runBin(t, "-version")
	if code != 0 {
		t.Fatalf("-version 退出码 = %d，期望 0（输出=%q）", code, out)
	}
	if got := strings.TrimSpace(out); got != "pairlink v0.1.0" {
		t.Fatalf("-version 输出 = %q，期望 %q", got, "pairlink v0.1.0")
	}
	t.Logf("-version: %s（退出码 0）", strings.TrimSpace(out))

	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "无模式", args: nil, want: 2},
		{name: "多模式", args: []string{"-relay", "-agent"}, want: 2},
		{name: "未知 flag", args: []string{"-not-a-flag"}, want: 2},
	}
	for _, tc := range cases {
		code, out := runBin(t, tc.args...)
		if code != tc.want {
			t.Fatalf("%s：退出码 = %d，期望 %d（输出=%q）", tc.name, code, tc.want, out)
		}
		if !strings.Contains(out, "用法:") && !strings.Contains(out, "flag provided but not defined") {
			t.Fatalf("%s：错误输出缺少用法说明: %q", tc.name, out)
		}
		t.Logf("%s：退出码 %d（符合预期）", tc.name, code)
	}

	// -relay -config 不存在 → 1，且有清晰错误。
	missing := filepath.Join(t.TempDir(), "does-not-exist.ini")
	code, out = runBin(t, "-relay", "-config", missing)
	if code != 1 {
		t.Fatalf("-relay -config 不存在：退出码 = %d，期望 1（输出=%q）", code, out)
	}
	if !strings.Contains(out, "错误:") || !strings.Contains(out, "读取配置文件") {
		t.Fatalf("-relay -config 不存在：错误信息不清晰: %q", out)
	}
	t.Logf("-relay -config 不存在：退出码 1，错误=%q", strings.Split(strings.TrimSpace(out), "\n")[0])

	// -agent -config 非法 ini → 1，且有清晰错误。
	badINI := writeINI(t, "bad.ini", "[server]\ntoken = x\nrelay_addr = 127.0.0.1:1\n")
	code, out = runBin(t, "-agent", "-config", badINI)
	if code != 1 {
		t.Fatalf("-agent -config 非法 ini：退出码 = %d，期望 1（输出=%q）", code, out)
	}
	if !strings.Contains(out, "错误:") || !strings.Contains(out, "至少要配置一个服务") {
		t.Fatalf("-agent -config 非法 ini：错误信息不清晰: %q", out)
	}
	t.Logf("-agent -config 非法 ini：退出码 1，错误=%q", strings.Split(strings.TrimSpace(out), "\n")[0])
}

// childProc 是一个真实 PairLink 子进程。
type childProc struct {
	name    string
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	err     error
}

func startChild(t *testing.T, name string, args ...string) *childProc {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("创建 %s 日志文件: %v", name, err)
	}
	cmd := exec.Command(pairlinkExe, args...)
	cmd.Dir = repoRoot
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		t.Fatalf("启动 %s 子进程: %v", name, err)
	}
	p := &childProc{name: name, cmd: cmd, logPath: logPath, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		_ = f.Close()
		close(p.done)
	}()
	t.Cleanup(func() { p.kill(t) })
	return p
}

func (p *childProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *childProc) kill(t *testing.T) {
	t.Helper()
	if p.exited() {
		return
	}
	// 按任务要求用 taskkill /PID 收尾（Windows）；其它平台回退 Process.Kill。
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(p.cmd.Process.Pid)).Run()
	}
	select {
	case <-p.done:
		return
	case <-time.After(2 * time.Second):
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Errorf("%s 子进程无法结束（pid=%d）", p.name, p.cmd.Process.Pid)
	}
}

func (p *childProc) logTail() string {
	f, err := os.Open(p.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 8192
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := st.Size() - max
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// waitChild 轮询等待条件，若子进程提前退出则打印日志尾部并失败。
func waitChild(t *testing.T, timeout time.Duration, desc string, procs []*childProc, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		for _, p := range procs {
			if p.exited() {
				t.Fatalf("%s: 子进程 %s 提前退出，err=%v\n--- %s 日志尾部 ---\n%s",
					desc, p.name, p.err, p.name, p.logTail())
			}
		}
		if time.Now().After(deadline) {
			var b strings.Builder
			for _, p := range procs {
				fmt.Fprintf(&b, "\n--- %s 日志尾部 ---\n%s", p.name, p.logTail())
			}
			t.Fatalf("%s: 等待超时（%s）%s", desc, timeout, b.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSubprocessTCPEcho 验收项：真实二进制三个子进程完成一次完整 TCP echo。
func TestSubprocessTCPEcho(t *testing.T) {
	guardGoroutines(t)

	echoAddr := startTCPEchoServer(t)

	relayManage := hostPort(freeTCPPort(t))
	relayCfg, relayPath := writeRelayINI(t, relaySpec{
		Tunnel:      hostPort(freeTCPPort(t)),
		TCP:         hostPort(freeTCPPort(t)),
		Manage:      relayManage,
		PairTimeout: 5 * time.Second,
		LogLevel:    "info",
	})
	relayProc := startChild(t, "relay", "-relay", "-config", relayPath)
	waitChild(t, 10*time.Second, "relay 子进程监听隧道端口", []*childProc{relayProc}, func() bool {
		conn, err := net.DialTimeout("tcp", relayCfg.Relay.TunnelListen, 300*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})
	waitChild(t, 10*time.Second, "relay 子进程管理页就绪", []*childProc{relayProc}, func() bool {
		return adminReachable(relayManage, "/")
	})

	_, agentPath := writeAgentINI(t, agentSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Protocol:  "tcp",
		Local:     echoAddr,
		LogLevel:  "info",
	})
	agentProc := startChild(t, "agent", "-agent", "-config", agentPath)
	waitChild(t, 10*time.Second, "agent 子进程注册服务", []*childProc{relayProc, agentProc}, func() bool {
		code, body, err := adminGet(relayManage, "/relay?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "服务注册表（1）") &&
			strings.Contains(body, "在线 agent（1）")
	})

	clientPort := freeTCPPort(t)
	clientCfg, clientPath := writeClientINI(t, clientSpec{
		RelayAddr: relayCfg.Relay.TunnelListen,
		Manage:    hostPort(freeTCPPort(t)),
		Services:  []clientService{{ID: "default", Listen: hostPort(clientPort)}},
		LogLevel:  "info",
	})
	clientProc := startChild(t, "client", "-client", "-config", clientPath)
	waitChild(t, 10*time.Second, "client 子进程本地监听就绪", []*childProc{relayProc, agentProc, clientProc}, func() bool {
		code, body, err := adminGet(clientCfg.Common.ManageListen, "/client?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, hostPort(clientPort))
	})

	conn, err := net.DialTimeout("tcp", hostPort(clientPort), 5*time.Second)
	if err != nil {
		t.Fatalf("连接 client 子进程本地端口: %v", err)
	}
	echoText(t, conn, "three-real-processes-echo")
	_ = conn.Close()

	waitChild(t, 8*time.Second, "relay 配对表回落", []*childProc{relayProc, agentProc, clientProc}, func() bool {
		code, body, err := adminGet(relayManage, "/relay?refresh=0")
		return err == nil && code == 200 && strings.Contains(body, "当前配对表（0）")
	})

	// 子进程日志应存在且非空。
	for _, p := range []*childProc{relayProc, agentProc, clientProc} {
		st, err := os.Stat(p.logPath)
		if err != nil || st.Size() == 0 {
			t.Fatalf("%s 子进程日志为空: %v", p.name, err)
		}
	}
	t.Logf("真实子进程三进程 TCP echo 成功（relay/agent/client），本地端口 %s", hostPort(clientPort))
}

// TestCrossCompile 验收项：纯 Go（无 CGO）跨平台编译通过。
func TestCrossCompile(t *testing.T) {
	guardGoroutines(t)

	targets := []struct {
		goos, goarch string
	}{
		{"linux", "amd64"},
		{"windows", "amd64"},
		{"windows", "arm64"},
	}
	outDir := t.TempDir()
	for _, tc := range targets {
		outName := fmt.Sprintf("pairlink-%s-%s", tc.goos, tc.goarch)
		if tc.goos == "windows" {
			outName += ".exe"
		}
		outPath := filepath.Join(outDir, outName)
		cmd := exec.Command("go", "build", "-o", outPath, ".")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+tc.goos,
			"GOARCH="+tc.goarch,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("GOOS=%s GOARCH=%s go build: %v\n%s", tc.goos, tc.goarch, err, out)
		}
		st, err := os.Stat(outPath)
		if err != nil || st.Size() == 0 {
			t.Fatalf("GOOS=%s GOARCH=%s 产物不存在或为空: %v", tc.goos, tc.goarch, err)
		}
		t.Logf("交叉编译通过: GOOS=%s GOARCH=%s（%d 字节）", tc.goos, tc.goarch, st.Size())
	}

	// 再按验收命令原样跑一次 ./...（覆盖全部包）。
	for _, env := range [][]string{
		{"GOOS=linux", "GOARCH=amd64"},
		{"GOOS=windows", "GOARCH=amd64"},
		{"GOOS=windows", "GOARCH=arm64"},
	} {
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), append([]string{"CGO_ENABLED=0"}, env...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v go build ./...: %v\n%s", env, err, out)
		}
	}
	t.Log("go build ./... 在 linux/amd64、windows/amd64、windows/arm64 下均通过")
}

// wslAvailable 检测本机是否有可用 WSL 发行版。
func wslAvailable() (bool, string) {
	if runtime.GOOS != "windows" {
		return false, "当前不是 Windows 平台"
	}
	cmd := exec.Command("wsl.exe", "-l", "-q")
	out, err := cmd.CombinedOutput()
	text := strings.ReplaceAll(string(out), "\x00", "")
	text = strings.TrimSpace(text)
	if err != nil {
		return false, fmt.Sprintf("wsl -l -q 退出码非 0: %v", err)
	}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	if len(names) == 0 {
		return false, "wsl -l -q 无输出"
	}
	return true, strings.Join(names, ", ")
}
