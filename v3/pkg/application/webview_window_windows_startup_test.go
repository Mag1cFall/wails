//go:build windows && !server

package application

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

func setupStartupFailureTestApp(
	t *testing.T,
	className string,
	disableQuitOnClose bool,
	browserPath string,
	onCreateHWND func(w32.HWND),
	onError func(error),
) (*App, *windowsApp) {
	t.Helper()

	instance := w32.GetModuleHandle("")
	if className != "" {
		var wc w32.WNDCLASSEX
		wc.Size = uint32(unsafe.Sizeof(wc))
		wc.WndProc = syscall.NewCallback(func(hwnd w32.HWND, msg uint32, wParam, lParam uintptr) uintptr {
			if msg == w32.WM_NCCREATE && onCreateHWND != nil {
				onCreateHWND(hwnd)
			}
			return w32.DefWindowProc(hwnd, msg, wParam, lParam)
		})
		wc.Instance = instance
		wc.ClassName = w32.MustStringToUTF16Ptr(className)
		if atom := w32.RegisterClassEx(&wc); atom == 0 {
			t.Fatalf("RegisterClassEx failed: %v", syscall.GetLastError())
		}
		t.Cleanup(func() {
			user32 := syscall.NewLazyDLL("user32.dll")
			_, _, _ = user32.NewProc("UnregisterClassW").Call(
				uintptr(unsafe.Pointer(wc.ClassName)),
				uintptr(instance),
			)
		})
	}

	app := &App{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		options: Options{
			Windows: WindowsOptions{
				WndClass:                      className,
				DisableQuitOnLastWindowClosed: disableQuitOnClose,
				WebviewUserDataPath:           t.TempDir(),
				WebviewBrowserPath:            browserPath,
			},
			ErrorHandler: onError,
		},
		windows: make(map[uint]Window),
	}
	app.Window = newWindowManager(app)
	app.Screen = newScreenManager(app)
	native := &windowsApp{
		parent:       app,
		instance:     instance,
		mainThreadID: w32.GetCurrentThreadId(),
		windowMap:    make(map[w32.HWND]*windowsWebviewWindow),
	}
	app.impl = native
	globalApplication = app
	if err := native.processAndCacheScreens(); err != nil {
		t.Fatalf("processAndCacheScreens failed: %v", err)
	}
	return app, native
}

func createRealTestWindow(t *testing.T, className string) w32.HWND {
	t.Helper()
	hwnd := w32.CreateWindowEx(
		0,
		w32.MustStringToUTF16Ptr(className),
		w32.MustStringToUTF16Ptr("Live Window"),
		w32.WS_OVERLAPPEDWINDOW,
		w32.CW_USEDEFAULT,
		w32.CW_USEDEFAULT,
		200,
		200,
		0,
		0,
		w32.GetModuleHandle(""),
		nil,
	)
	if hwnd == 0 {
		t.Fatalf("CreateWindowEx for live window failed: %v", syscall.GetLastError())
	}
	t.Cleanup(func() {
		if w32.IsWindow(hwnd) {
			w32.DestroyWindow(hwnd)
		}
	})
	return hwnd
}

func newMenuWithHiddenSubmenu() (*Menu, *MenuItem) {
	menu := NewMenu()
	sharedItem := menu.Add("Action")
	hiddenSub := menu.AddSubmenu("HiddenSub")
	hiddenSub.Add("SubItem")
	// Hide the submenu item so Win32Menu tracks it in detachedSubmenus.
	menu.items[len(menu.items)-1].SetHidden(true)
	return menu, sharedItem
}

func TestCanSurviveStartupFailure(t *testing.T) {
	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	w := &windowsWebviewWindow{}

	globalApplication = &App{}
	if w.canSurviveStartupFailure() {
		t.Fatal("default single-window app should keep fatal startup behaviour")
	}

	globalApplication = &App{
		options: Options{Windows: WindowsOptions{DisableQuitOnLastWindowClosed: true}},
	}
	if !w.canSurviveStartupFailure() {
		t.Fatal("app with DisableQuitOnLastWindowClosed should survive startup failure")
	}

	globalApplication = &App{
		impl: &windowsApp{
			windowMap: map[w32.HWND]*windowsWebviewWindow{
				w32.HWND(1): {},
			},
		},
	}
	if !w.canSurviveStartupFailure() {
		t.Fatal("multi-window app with an existing live window should survive startup failure")
	}
}

func TestSetupChromiumConfigErrorSurvivableStartupDoesNotTerminate(t *testing.T) {
	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	var reported error
	globalApplication = &App{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		options: Options{
			Windows:      WindowsOptions{DisableQuitOnLastWindowClosed: true},
			ErrorHandler: func(err error) { reported = err },
		},
	}

	want := errors.New("settings failed")
	if got := (&windowsWebviewWindow{}).setupChromiumConfigError(false, want); got {
		t.Fatal("setupChromiumConfigError = true, want false")
	}
	if !errors.Is(reported, want) {
		t.Fatalf("reported error = %v, want %v", reported, want)
	}
}

func TestStartupEmbedFailureCleansUpWithoutExiting(t *testing.T) {
	tests := []struct {
		name               string
		disableQuitOnClose bool
		existingWindow     bool
	}{
		{name: "DisableQuitOnLastWindowClosed", disableQuitOnClose: true},
		{name: "another_live_window", existingWindow: true},
	}

	getGuiResources := syscall.NewLazyDLL("user32.dll").NewProc("GetGuiResources")
	getCurrentProcess := syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentProcess")
	process, _, _ := getCurrentProcess.Call()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			previous := globalApplication
			t.Cleanup(func() { globalApplication = previous })

			className := "WailsTestStartupFailure_" + tc.name
			missingBrowser := filepath.Join(t.TempDir(), "missing-webview2")

			var (
				createdHWND     w32.HWND
				hwndAliveOnErr  bool
				reported        error
				capturedMenu    w32.HMENU
				capturedSubmenu w32.HMENU
			)

			var existingHWND w32.HWND
			app, native := setupStartupFailureTestApp(
				t,
				className,
				tc.disableQuitOnClose,
				missingBrowser,
				func(hwnd w32.HWND) {
					if existingHWND == 0 || hwnd != existingHWND {
						createdHWND = hwnd
					}
				},
				func(err error) {
					reported = err
					if createdHWND != 0 {
						hwndAliveOnErr = w32.IsWindow(createdHWND)
					}
				},
			)
			if tc.existingWindow {
				existingHWND = createRealTestWindow(t, className)
				createdHWND = 0
				native.windowMap[existingHWND] = &windowsWebviewWindow{hwnd: existingHWND}
			}

			winMenu, _ := newMenuWithHiddenSubmenu()
			parent := NewWindow(WebviewWindowOptions{
				Title:  "Test Startup Failure",
				Hidden: true,
				Windows: WindowsWindow{
					Menu: winMenu,
				},
			})
			app.Window.Add(parent)
			w := newWindowImpl(parent)
			parent.impl = w

			// Capture the created HMENU and detached submenu handle during error callback,
			// before cleanupFailedStartup releases them.
			origErrHandler := app.options.ErrorHandler
			app.options.ErrorHandler = func(err error) {
				if w.menu != nil {
					capturedMenu = w.menu.menu
					for sub := range w.menu.detachedSubmenus {
						capturedSubmenu = sub
					}
				}
				origErrHandler(err)
			}

			cancellerCalled := false
			parent.addCancellationFunction(func() { cancellerCalled = true })

			w.run()

			if reported == nil {
				t.Fatal("expected ErrorHandler to receive WebView2 startup error")
			}
			if !strings.Contains(reported.Error(), "missing-webview2") && !strings.Contains(reported.Error(), "WebView2") {
				t.Fatalf("unexpected startup error: %v", reported)
			}
			if createdHWND == 0 {
				t.Fatal("expected CreateWindowEx to allocate a real HWND before Embed failed")
			}
			if !hwndAliveOnErr {
				t.Fatal("expected HWND to still exist when ErrorHandler ran")
			}
			if w.hwnd != 0 {
				t.Fatalf("failed startup leaked hwnd field %v", w.hwnd)
			}
			if w32.IsWindow(createdHWND) {
				t.Fatalf("failed startup did not destroy OS window %v", createdHWND)
			}
			if capturedMenu != 0 && w32.IsMenu(capturedMenu) {
				t.Fatalf("failed startup leaked root HMENU %v", capturedMenu)
			}
			if capturedSubmenu != 0 && w32.IsMenu(capturedSubmenu) {
				t.Fatalf("failed startup leaked detached submenu HMENU %v", capturedSubmenu)
			}
			if !parent.isDestroyed() {
				t.Fatal("parent window was not marked destroyed after failed startup")
			}
			if _, ok := app.Window.GetByID(parent.ID()); ok {
				t.Fatal("failed window was not removed from WindowManager")
			}
			if !cancellerCalled {
				t.Fatal("window cancellers were not invoked on failed startup cleanup")
			}
			if w.chromium != nil && w.chromium.IsReady() {
				t.Fatal("chromium should not report ready after failed startup")
			}
			if tc.existingWindow {
				if !w32.IsWindow(existingHWND) {
					t.Fatal("existing live OS window was destroyed")
				}
				if _, ok := native.windowMap[existingHWND]; !ok {
					t.Fatal("existing live window was unexpectedly removed from windowMap")
				}
			} else if len(native.windowMap) != 0 {
				t.Fatalf("failed window unexpectedly registered in windowMap: %d", len(native.windowMap))
			}

			// Methods invoked after a failed startup must remain safe no-ops without
			// leaking GDI brushes or mutating shared menu items.
			sharedMenu := NewMenu()
			sharedItem := sharedMenu.Add("KeepMe")
			sentinel := &windowsMenuItem{id: 42}
			sharedItem.impl = sentinel

			gdiBefore, _, _ := getGuiResources.Call(process, 0) // GR_GDIOBJECTS
			for i := 0; i < 20; i++ {
				parent.SetBackgroundColour(RGBA{Red: 10, Green: 20, Blue: 30, Alpha: 255})
			}
			gdiAfter, _, _ := getGuiResources.Call(process, 0)
			if gdiAfter != gdiBefore {
				t.Fatalf("SetBackgroundColour on failed window leaked GDI objects: before=%d after=%d", gdiBefore, gdiAfter)
			}

			parent.SetMenu(sharedMenu)
			if sharedItem.impl != sentinel {
				t.Fatal("SetMenu on failed window mutated shared menu item impl")
			}

			parent.SetTitle("ignored")
			parent.SetAlwaysOnTop(true)
			parent.Hide()
			parent.Show()
			parent.Focus()
			parent.Maximise()
			parent.UnMinimise()
			parent.Close()
		})
	}
}

func TestStartupCreateWindowFailureReleasesMenu(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	var (
		reported        error
		capturedMenu    w32.HMENU
		capturedSubmenu w32.HMENU
	)
	app, _ := setupStartupFailureTestApp(
		t,
		"", // skip registration; set unregistered class below
		true,
		filepath.Join(t.TempDir(), "missing-webview2"),
		nil,
		func(err error) { reported = err },
	)
	app.options.Windows.WndClass = "WailsUnregisteredStartupFailureClass"

	winMenu, _ := newMenuWithHiddenSubmenu()
	parent := NewWindow(WebviewWindowOptions{
		Title:  "CreateWindow Failure",
		Hidden: true,
		Windows: WindowsWindow{
			Menu: winMenu,
		},
	})
	app.Window.Add(parent)
	w := newWindowImpl(parent)
	parent.impl = w

	app.options.ErrorHandler = func(err error) {
		reported = err
		if w.menu != nil {
			capturedMenu = w.menu.menu
			for sub := range w.menu.detachedSubmenus {
				capturedSubmenu = sub
			}
		}
	}

	w.run()

	if reported == nil {
		t.Fatal("expected ErrorHandler to receive window creation error")
	}
	if capturedMenu == 0 {
		t.Fatal("expected menu to have been allocated before CreateWindowEx failed")
	}
	if w32.IsMenu(capturedMenu) {
		t.Fatalf("CreateWindowEx failure leaked root HMENU %v", capturedMenu)
	}
	if capturedSubmenu != 0 && w32.IsMenu(capturedSubmenu) {
		t.Fatalf("CreateWindowEx failure leaked detached submenu HMENU %v", capturedSubmenu)
	}
	if !parent.isDestroyed() {
		t.Fatal("parent window was not marked destroyed after CreateWindowEx failure")
	}
	if _, ok := app.Window.GetByID(parent.ID()); ok {
		t.Fatal("failed window remained in WindowManager")
	}
}

func TestStartupModalFailureReenablesParent(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	className := "WailsTestModalStartupFailure"
	app, native := setupStartupFailureTestApp(
		t,
		className,
		true,
		filepath.Join(t.TempDir(), "missing-webview2"),
		nil,
		nil,
	)

	parentHWND := createRealTestWindow(t, className)
	owner := NewWindow(WebviewWindowOptions{Title: "Owner"})
	ownerImpl := &windowsWebviewWindow{parent: owner, hwnd: parentHWND}
	owner.impl = ownerImpl
	native.windowMap[parentHWND] = ownerImpl

	modal := NewWindow(WebviewWindowOptions{Title: "Modal", Hidden: true})
	app.Window.Add(modal)
	modalImpl := newWindowImpl(modal)
	modal.impl = modalImpl

	disabledDuringSetup := false
	app.options.ErrorHandler = func(error) {
		ownerImpl.attachModal(modal)
		disabledDuringSetup = !w32.IsWindowEnabled(parentHWND)
	}

	modalImpl.run()

	if !disabledDuringSetup {
		t.Fatal("expected AttachModal to disable parent window before cleanup")
	}
	if !w32.IsWindow(parentHWND) {
		t.Fatal("parent window was unexpectedly destroyed")
	}
	if !w32.IsWindowEnabled(parentHWND) {
		t.Fatal("parent window remained disabled after modal startup failure cleanup")
	}
}

func TestWindowManagerAddOnCreateNoDeadlockOnStartupFailure(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	className := "WailsTestAddOnCreateNoDeadlock"
	app, _ := setupStartupFailureTestApp(
		t,
		className,
		true,
		filepath.Join(t.TempDir(), "missing-webview2"),
		nil,
		func(error) {},
	)

	app.Window.OnCreate(func(win Window) {
		win.Show()
	})

	parent := NewWindow(WebviewWindowOptions{Title: "OnCreate Failure", Hidden: true})

	timer := time.AfterFunc(10*time.Second, func() {
		panic("WindowManager.Add deadlocked on OnCreate -> Show -> cleanupFailedStartup -> Remove")
	})
	defer timer.Stop()

	app.Window.Add(parent)

	if !parent.isDestroyed() {
		t.Fatal("window created via Add+OnCreate was not cleaned up after startup failure")
	}
	if _, ok := app.Window.GetByID(parent.ID()); ok {
		t.Fatal("failed window remained in WindowManager")
	}
}

func TestStartupFailureWhenLastWindowClosesMidSetupDoesNotFatal(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	className := "WailsTestLastWindowClosesMidSetup"
	var (
		reported      error
		existingHWND  w32.HWND
		app           *App
		native        *windowsApp
	)
	app, native = setupStartupFailureTestApp(
		t,
		className,
		false, // default quit-on-last-window-closed enabled
		filepath.Join(t.TempDir(), "missing-webview2"),
		nil,
		func(err error) {
			reported = err
			// Simulate the last pre-existing window closing while setupChromium
			// was pumping messages: windowMap becomes empty and WM_QUIT is posted.
			native.unregisterWindow(&windowsWebviewWindow{hwnd: existingHWND})
		},
	)
	existingHWND = createRealTestWindow(t, className)
	native.windowMap[existingHWND] = &windowsWebviewWindow{hwnd: existingHWND}

	parent := NewWindow(WebviewWindowOptions{Title: "Second Window", Hidden: true})
	app.Window.Add(parent)
	w := newWindowImpl(parent)
	parent.impl = w

	// Must return cleanly without invoking globalApplication.fatal (os.Exit(1)).
	w.run()

	if reported == nil {
		t.Fatal("expected non-fatal ErrorHandler callback")
	}
	if !parent.isDestroyed() {
		t.Fatal("failed window was not cleaned up")
	}

	// Drain the queued WM_QUIT so it does not leak into subsequent tests, and verify
	// it was preserved for the outer message loop to exit cleanly.
	var msg w32.MSG
	gotQuit := false
	for w32.PeekMessage(&msg, 0, 0, 0, w32.PM_REMOVE) {
		if msg.Message == w32.WM_QUIT {
			gotQuit = true
		}
	}
	if !gotQuit {
		t.Fatal("expected WM_QUIT from closing the last live window to remain queued")
	}
}
