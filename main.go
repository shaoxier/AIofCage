package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shaoxier/AIofCage/internal/runtime"
	"github.com/shaoxier/AIofCage/internal/service"
	"github.com/shaoxier/AIofCage/internal/store"
	"github.com/shaoxier/AIofCage/internal/vault"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/src
var assets embed.FS

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--mcp" {
		if err := runControl(); err != nil {
			fmt.Fprintln(os.Stderr, "本机控制结束：", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Ai of Cage 启动失败:", err)
		os.Exit(1)
	}
}

// Quit states: the app runs, waits for its operations to stop before quitting, or quits now.
const (
	quitIdle int32 = iota
	quitStopping
	quitNow
	quitForced
)

// keepOpen runs when the window is closed or the app is quit; it reports whether to keep the app
// open. With operations in progress it asks first. On 停止并退出 the window stays while they stop
// (a VM start finishes first, so the VM is not left broken), then the app quits, so no build or
// download it started keeps running after it. Closing again meanwhile asks whether to wait.
func keepOpen(ctx context.Context, app *service.App, state *atomic.Int32) bool {
	switch state.Load() {
	case quitNow, quitForced:
		return false
	case quitStopping:
		choice, err := wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{Type: wailsruntime.QuestionDialog,
			Title:   "正在停止操作",
			Message: "停完后会自动退出。也可以立即退出：构建已经停止；正在启动的专用虚拟机会在后台启动完成。",
			Buttons: []string{"继续等待", "立即退出"}, DefaultButton: "继续等待", CancelButton: "继续等待"})
		if err == nil && choice == "立即退出" {
			state.Store(quitForced)
			return false
		}
		return true
	}
	running := app.RunningOperations()
	if len(running) == 0 {
		return false
	}
	choice, err := wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{Type: wailsruntime.QuestionDialog,
		Title:   "还有操作没有完成",
		Message: strings.Join(running, "、") + "还在进行。退出前会先停止它们；如果专用虚拟机正在启动，要等它启动完成才能退出，以免损坏虚拟机。",
		Buttons: []string{"停止并退出", "继续使用"}, DefaultButton: "继续使用", CancelButton: "继续使用"})
	if err != nil || choice != "停止并退出" {
		return true
	}
	state.Store(quitStopping)
	wailsruntime.EventsEmit(ctx, "app:quitting")
	go func() {
		// The longest step that cannot be interrupted is creating the VM (20 minutes at most).
		app.StopOperations(20 * time.Minute)
		if !state.CompareAndSwap(quitStopping, quitNow) {
			return // the user chose to quit at once
		}
		wailsruntime.Quit(ctx)
		// [NSApp stop:] ends the event loop only after the next event; without one, finish here.
		time.Sleep(5 * time.Second)
		app.Shutdown(5 * time.Second)
		app.CloseAllTerminals()
		os.Exit(0)
	}()
	return true
}

func run() error {
	frontend, err := fs.Sub(assets, "frontend/src")
	if err != nil {
		return err
	}
	dir, err := store.DefaultDir()
	if err != nil {
		return err
	}
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	keychain := vault.NewKeychain()
	// Without Docker/Colima the app still opens: provider management works and the environment
	// page explains what is missing.
	manager, managerErr := runtime.NewManager(st, keychain)
	var desktopContext context.Context
	var quitState atomic.Int32
	app := service.NewApp(st, keychain, manager, managerErr, func() (string, error) {
		if desktopContext == nil {
			return "", fmt.Errorf("桌面窗口尚未就绪")
		}
		return wailsruntime.OpenDirectoryDialog(desktopContext, wailsruntime.OpenDialogOptions{
			Title: "选择项目文件夹",
		})
	})
	return wails.Run(&options.App{
		Title: "Ai of Cage", Width: 1180, Height: 800, MinWidth: 900, MinHeight: 640,
		// 显式启用 macOS 窗口缩放；Wails 在 Mac 选项为 nil 时禁用绿色按钮。
		Mac: &mac.Options{TitleBar: mac.TitleBarDefault(), DisableZoom: false,
			DisableEscapeExitsFullscreen: true,
			Preferences:                  &mac.Preferences{TabFocusesLinks: mac.Enabled}},
		BackgroundColour: options.NewRGB(245, 246, 248),
		AssetServer:      &assetserver.Options{Assets: frontend},
		OnStartup: func(ctx context.Context) {
			desktopContext = ctx
			app.SetEmitter(func(event string, data any) { wailsruntime.EventsEmit(ctx, event, data) })
			app.StartMonitor(ctx)
		},
		OnBeforeClose: func(ctx context.Context) bool { return keepOpen(ctx, app, &quitState) },
		OnShutdown: func(context.Context) {
			wait := 30 * time.Second
			if quitState.Load() == quitForced {
				wait = 2 * time.Second // the user chose not to wait
			}
			app.Shutdown(wait)
			app.CloseAllTerminals()
		},
		Bind: []interface{}{app},
	})
}
