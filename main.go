// Package main 是 PairLink 的命令行入口。
//
// PairLink 是配对式中继系统，不是 P2P NAT 穿透工具：同一二进制通过
// -relay / -agent / -client 切换三种模式，读取同一份 ini 中对应的配置段。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/lsong98sh/pairlink/internal/agent"
	"github.com/lsong98sh/pairlink/internal/client"
	"github.com/lsong98sh/pairlink/internal/config"
	"github.com/lsong98sh/pairlink/internal/logx"
	"github.com/lsong98sh/pairlink/internal/relay"
	"github.com/lsong98sh/pairlink/internal/service"
	"github.com/lsong98sh/pairlink/internal/version"
)

// Version 是当前版本号，-version 输出 "pairlink <Version>"。
//
// 单一来源是 internal/version.Version；保留本常量是为了 main_test 与调用方兼容。
const Version = version.Version

// usageText 返回命令行用法说明。
func usageText() string {
	return fmt.Sprintf(`PairLink %s —— 配对式中继系统（不是 P2P NAT 穿透工具，所有流量都经过中继服务器）

用法:
  pairlink -relay  -config relay.ini
  pairlink -agent  -config server.ini
  pairlink -client -config client.ini
  pairlink -version
  pairlink -relay -config relay.ini -service install|start|stop|uninstall   （仅 Windows）

参数:
  -relay          以 relay 模式运行（公网中继服务器）
  -agent          以 agent 模式运行（服务提供者侧）
  -client         以 client 模式运行（访问侧）
  -config <path>  ini 配置文件路径
  -service <op>   Windows 服务操作：install / start / stop / uninstall
                  （install 时会把 -config 解析为绝对路径写入服务参数）
  -version        打印版本号并退出

必须且只能指定一个模式参数；未指定时打印本帮助并以退出码 2 结束。
`, Version)
}

// main 是进程入口。
func main() {
	os.Exit(run(os.Args[1:]))
}

// run 解析命令行并执行，返回进程退出码。
func run(args []string) int {
	fs := flag.NewFlagSet("pairlink", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText()) }

	relayMode := fs.Bool("relay", false, "以 relay 模式运行")
	agentMode := fs.Bool("agent", false, "以 agent 模式运行")
	clientMode := fs.Bool("client", false, "以 client 模式运行")
	cfgPath := fs.String("config", "", "ini 配置文件路径")
	serviceCmd := fs.String("service", "", "Windows 服务操作：install|start|stop|uninstall")
	showVersion := fs.Bool("version", false, "打印版本号")

	if err := fs.Parse(args); err != nil {
		// flag 包已经打印了错误信息/帮助，这里只补 -h 的正常退出。
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fs.Usage()
		return 2
	}

	if *showVersion {
		fmt.Printf("pairlink %s\n", Version)
		return 0
	}

	mode, err := selectMode(*relayMode, *agentMode, *clientMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n\n", err)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "错误: 存在多余参数 %v\n\n", fs.Args())
		fs.Usage()
		return 2
	}

	if strings.TrimSpace(*serviceCmd) != "" {
		return handleService(*serviceCmd, mode, *cfgPath)
	}
	return runForeground(mode, *cfgPath)
}

// selectMode 从三个模式开关中选出唯一模式。
func selectMode(relayMode, agentMode, clientMode bool) (config.Mode, error) {
	var modes []config.Mode
	if relayMode {
		modes = append(modes, config.ModeRelay)
	}
	if agentMode {
		modes = append(modes, config.ModeAgent)
	}
	if clientMode {
		modes = append(modes, config.ModeClient)
	}
	switch len(modes) {
	case 0:
		return "", errors.New("必须指定一个模式：-relay 或 -agent 或 -client")
	case 1:
		return modes[0], nil
	default:
		return "", fmt.Errorf("只能指定一个模式，当前指定了 %d 个（-relay / -agent / -client）", len(modes))
	}
}

// loadConfig 加载并校验指定模式的配置。
func loadConfig(path string, mode config.Mode) (*config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("必须通过 -config 指定 ini 配置文件")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(mode); err != nil {
		return nil, err
	}
	return cfg, nil
}

// runForeground 以普通前台进程方式运行。
func runForeground(mode config.Mode, cfgPath string) int {
	cfg, err := loadConfig(cfgPath, mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		return 1
	}

	// Windows 上如果进程由 SCM 启动但命令行没有 -service（例如旧版服务参数），
	// 自动进入服务模式，避免在无控制台环境下直接运行。
	if runtime.GOOS == "windows" && service.IsWindowsService() {
		return serveAsWindowsService(mode, cfg)
	}

	logger := logx.New(logx.Config{
		Level:  cfg.Common.LogLevel,
		Format: cfg.Common.LogFormat,
		Mode:   string(mode),
	})
	logger.Info("PairLink 启动",
		"event", "start",
		"version", Version,
		"config", cfg.Path,
		"pid", os.Getpid(),
	)
	for _, w := range cfg.Warnings {
		logger.Warn("配置提示", "event", "config_warning", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = dispatch(ctx, mode, cfg)
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		logger.Info("PairLink 已退出", "event", "exit")
		return 0
	default:
		logger.Error("PairLink 异常退出", "event", "exit", "err", err)
		return 1
	}
}

// serveAsWindowsService 以 Windows 服务方式运行，日志落到 exe 同目录的 <service>.log。
func serveAsWindowsService(mode config.Mode, cfg *config.Config) int {
	name := service.Name(string(mode))
	logPath, err := service.LogPath(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		return 1
	}
	logger, closer, err := logx.NewFileLogger(logPath, logx.Config{
		Level:  cfg.Common.LogLevel,
		Format: cfg.Common.LogFormat,
		Mode:   string(mode),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		return 1
	}
	defer closer.Close()

	// Windows 服务没有控制台：把 stderr/stdout 一并指向日志文件，
	// 这样各包内用 logx.New（默认写 os.Stderr）创建的 logger 也能落盘。
	if f, ok := closer.(*os.File); ok {
		os.Stderr = f
		os.Stdout = f
	}

	logger.Info("服务模式启动",
		"event", "service_start",
		"service", name,
		"config", cfg.Path,
		"log", logPath,
		"version", Version,
	)
	for _, w := range cfg.Warnings {
		logger.Warn("配置提示", "event", "config_warning", "detail", w)
	}

	err = service.Run(name, func(ctx context.Context) error {
		return dispatch(ctx, mode, cfg)
	})
	if err != nil {
		logger.Error("服务异常退出", "event", "service_exit", "err", err)
		return 1
	}
	logger.Info("服务已停止", "event", "service_stop", "service", name)
	return 0
}

// handleService 处理 -service 子操作。
func handleService(cmd string, mode config.Mode, cfgPath string) int {
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	name := service.Name(string(mode))

	if runtime.GOOS != "windows" {
		fmt.Fprintf(os.Stderr, "错误: -service 仅支持 Windows，当前平台是 %s/%s；请直接用 -%s 前台运行\n",
			runtime.GOOS, runtime.GOARCH, mode)
		return 1
	}

	switch cmd {
	case "install":
		if strings.TrimSpace(cfgPath) == "" {
			fmt.Fprintln(os.Stderr, "错误: -service install 必须同时用 -config 指定 ini 配置文件")
			return 1
		}
		abs, err := filepath.Abs(filepath.FromSlash(cfgPath))
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: 解析配置文件路径 %q: %v\n", cfgPath, err)
			return 1
		}
		if _, err := os.Stat(abs); err != nil {
			fmt.Fprintf(os.Stderr, "错误: 配置文件 %q 不可访问: %v\n", abs, err)
			return 1
		}
		// 服务参数里必须写绝对路径：SCM 启动服务时工作目录不是 exe 目录。
		args := []string{"-" + string(mode), "-config", abs, "-service", "run"}
		display := fmt.Sprintf("PairLink %s (%s)", modeTitle(mode), name)
		if err := service.Install(name, display, args); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			return 1
		}
		fmt.Printf("已安装服务 %s（开机自动启动）\n  配置文件: %s\n  启动参数: %s\n", name, abs, strings.Join(args, " "))
		return 0

	case "uninstall":
		if err := service.Uninstall(name); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			return 1
		}
		fmt.Printf("已卸载服务 %s\n", name)
		return 0

	case "start":
		if err := service.Start(name); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			return 1
		}
		printServiceStatus(name)
		return 0

	case "stop":
		if err := service.Stop(name); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			return 1
		}
		printServiceStatus(name)
		return 0

	case "run":
		cfg, err := loadConfig(cfgPath, mode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			return 1
		}
		return serveAsWindowsService(mode, cfg)

	default:
		fmt.Fprintf(os.Stderr, "错误: 未知的 -service 操作 %q（可选 install|start|stop|uninstall|run）\n", cmd)
		return 2
	}
}

// printServiceStatus 打印服务状态。
func printServiceStatus(name string) {
	st, err := service.Query(name)
	if err != nil {
		fmt.Printf("服务 %s 状态查询失败: %v\n", name, err)
		return
	}
	if st.PID != 0 {
		fmt.Printf("服务 %s 状态: %s（PID %d）\n", name, st.State, st.PID)
		return
	}
	fmt.Printf("服务 %s 状态: %s\n", name, st.State)
}

// dispatch 调用对应模式的 Run。
func dispatch(ctx context.Context, mode config.Mode, cfg *config.Config) error {
	switch mode {
	case config.ModeRelay:
		return relay.Run(ctx, cfg)
	case config.ModeAgent:
		return agent.Run(ctx, cfg)
	case config.ModeClient:
		return client.Run(ctx, cfg)
	default:
		return fmt.Errorf("未知模式 %q", mode)
	}
}

// modeTitle 返回模式的中文/英文展示名，用于服务显示名。
func modeTitle(mode config.Mode) string {
	switch mode {
	case config.ModeRelay:
		return "Relay"
	case config.ModeAgent:
		return "Agent"
	case config.ModeClient:
		return "Client"
	default:
		return string(mode)
	}
}
