//go:build windows

// newapi 桌面版：一个可执行文件里同时跑转换服务和控制台窗口。
//
// 服务直接在本进程内启动（不拉子进程），窗口用系统自带的 WebView2 加载
// 控制台页面，所以退出时不会有后台进程残留，也没有端口争抢的问题。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"

	"github.com/zhangjl/newapi-box/internal/admin"
	"github.com/zhangjl/newapi-box/internal/config"
	"github.com/zhangjl/newapi-box/internal/proxy"
)

// 桌面版固定绑回环：控制台不对外暴露，要对外服务请用命令行版。
const listen = "127.0.0.1:18888"

const consoleURL = "http://127.0.0.1:18888/"

var startedAt = time.Now()

// trace 打印启动各阶段耗时。设置 NEWAPI_TRACE=1 后运行，输出走 stderr。
func trace(format string, args ...any) {
	if os.Getenv("NEWAPI_TRACE") == "" {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "[newapi-box %5dms] %s\n",
		time.Since(startedAt).Milliseconds(), fmt.Sprintf(format, args...))
}

func main() {
	// 窗口、消息循环和托盘都必须在同一个线程上，先把自己钉住。
	runtime.LockOSThread()

	// Go 侧内存约束：这个服务本身就轻，堆上限给 128MB 防异常膨胀；
	// GC 触发点收紧到 50%，用少量 CPU 换更低的常驻内存。
	debug.SetMemoryLimit(128 << 20)
	debug.SetGCPercent(50)

	// 单实例互斥：双开会导致两套 WebView2/Chromium 进程，内存直接翻倍。
	// 端口冲突要等服务起来才发现，互斥锁能在开窗口之前就拦住。
	if !acquireSingleInstance() {
		alert("newapi box 已经在运行了（请查看托盘图标）")
		os.Exit(0)
	}

	// WebView2 会读取系统代理设置。控制台是回环地址，走代理只会白等，
	// 所以明确告诉内核别用代理。
	disableProxyForWebView()

	if err := run(); err != nil {
		alert(fmt.Sprintf("newapi box 启动失败：\n\n%v", err))
		os.Exit(1)
	}
}

// acquireSingleInstance 用命名互斥体保证进程唯一。返回 false 表示已有实例。
func acquireSingleInstance() bool {
	const errorAlreadyExists = 183
	name, _ := syscall.UTF16PtrFromString("Local\\newapi-box-singleton")
	handle, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return true // 拿不到句柄就放行，别因为锁失败挡住启动
	}
	if err.(syscall.Errno) == errorAlreadyExists {
		_, _, _ = procCloseHandle.Call(handle)
		return false
	}
	return true
}

var (
	procCreateMutexW = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateMutexW")
	procCloseHandle  = syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
)

// disableProxyForWebView 在创建 WebView2 环境之前设置附加参数。
// 已有设置的话保留并追加，不覆盖用户的配置。
//
// 这些参数由 WebView2 运行时自己读取（WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS
// 是官方支持的入口），所以不必改库。控制台是回环上的纯静态页，用不到
// Chromium 的扩展、同步、组件更新和后台联网，关掉它们能缩短首屏时间。
func disableProxyForWebView() {
	args := []string{
		"--no-proxy-server", // 系统装了代理时，走代理访问 127.0.0.1 只会白等
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--disable-extensions",
		"--disable-default-apps",
		"--no-first-run",
		// 省内存：单页控制台一个渲染进程足够，缓存也压到最小
		"--renderer-process-limit=1",
		"--disk-cache-size=8388608",
		"--media-cache-size=8388608",
		"--mute-audio",
		// 静态控制台用不上硬件加速，关掉 GPU 进程的大块显存/着色器开销，
		// 渲染回退到 CPU 合成（页面无动画，无感知）
		"--disable-gpu",
		"--disable-features=Translate,MediaRouter,DialMediaRouteProvider,BackgroundFetch,BackgroundSync",
	}
	joined := strings.Join(args, " ")
	if existing := strings.TrimSpace(os.Getenv(webviewArgsEnv)); existing != "" {
		joined = existing + " " + joined
	}
	os.Setenv(webviewArgsEnv, joined)
	trace("WebView2 参数: %s", joined)
}

const webviewArgsEnv = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"

// alert 用系统对话框报错：这个进程没有控制台窗口，写 stderr 用户是看不到的。
func alert(message string) {
	text, err := syscall.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	caption, _ := syscall.UTF16PtrFromString("newapi box")
	const mbIconError = 0x00000010
	proc := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	_, _, _ = proc.Call(0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(caption)),
		mbIconError)
}

func run() error {
	configPath := beside("config.json")

	// 服务启动和窗口初始化用的是两套互不相干的资源，同时开工。
	// WebView2 的初始化通常比服务慢得多，串行做等于白白多等一段。
	type boot struct {
		server *http.Server
		err    error
	}
	ready := make(chan boot, 1)
	go func() {
		server, err := startServer(configPath)
		ready <- boot{server, err}
	}()

	trace("初始化 WebView2 …")
	window := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "newapi box",
			Width:  1180,
			Height: 820,
			Center: true,
		},
	})
	if window == nil {
		return errors.New("无法初始化 WebView2。请确认已安装 Microsoft Edge WebView2 运行时")
	}
	defer window.Destroy()
	trace("窗口就绪")

	booted := <-ready
	if booted.err != nil {
		return booted.err
	}
	trace("服务就绪")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = booted.server.Shutdown(ctx)
	}()

	if !waitForPort(5 * time.Second) {
		return fmt.Errorf("服务没能在 %s 上就绪，可能端口被占用", listen)
	}

	// 关窗口默认收进托盘，不退出程序。
	trayIcon := newTray(uintptr(window.Window()), window)
	defer trayIcon.remove()

	trace("加载控制台")
	window.Navigate(consoleURL)

	trace("进入消息循环")
	window.Run() // 窗口隐藏时依然阻塞在这里，托盘菜单里的退出才会让它返回
	trace("退出")
	return nil
}

// startServer 组装转换服务，逻辑与命令行版一致，只是绑到了回环地址。
func startServer(configPath string) (*http.Server, error) {
	cfg, _, err := config.LoadOrInit(configPath)
	if err != nil {
		return nil, err
	}
	cfg.Listen = listen

	store := config.NewStore(configPath, cfg)
	relay, err := proxy.New(store)
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           traceRequests(admin.New(relay)),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败（是否已有实例在运行？）：%w", listen, err)
	}

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(os.Stderr, "newapi-box: %v\n", serveErr)
		}
	}()
	return server, nil
}

// traceRequests 记录控制台开始取数据的时间。页面一加载就会请求这两个接口，
// 所以这个时间点等于"界面真正能用"的时刻，是衡量启动体验的准确标尺。
func traceRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/api/") {
			trace("页面已开始取数 %s", r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func waitForPort(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", listen, 300*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// beside 把相对路径解析到可执行文件所在目录，整个文件夹可以直接拷走。
func beside(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(exe), name)
}
