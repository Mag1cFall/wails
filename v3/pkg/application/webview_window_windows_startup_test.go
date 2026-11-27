//go:build windows && !server

package application

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	app.KeyBinding = newKeyBindingManager(app)
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
				createdHWND    w32.HWND
				reported       error
				menuBuiltEarly bool
				destroyedOnErr bool
				gdiErrBefore   uintptr
				gdiErrAfter    uintptr
				existingHWND   w32.HWND
				w              *windowsWebviewWindow
				parent         *WebviewWindow
			)

			app, native := setupStartupFailureTestApp(
				t,
				className,
				tc.disableQuitOnClose,
				missingBrowser,
				func(hwnd w32.HWND) {
					if existingHWND == 0 || hwnd != existingHWND {
						createdHWND = hwnd
						if w != nil && w.menu != nil {
							menuBuiltEarly = true
						}
					}
				},
				func(err error) {
					reported = err
					destroyedOnErr = parent.isDestroyed() && (createdHWND == 0 || !w32.IsWindow(createdHWND))
					// Calling mutators inside ErrorHandler must not leak GDI objects because
					// cleanup runs before ErrorHandler is invoked.
					gdiErrBefore, _, _ = getGuiResources.Call(process, 0)
					for i := 0; i < 10; i++ {
						parent.SetBackgroundColour(RGBA{Red: 1, Green: 2, Blue: 3, Alpha: 255})
					}
					gdiErrAfter, _, _ = getGuiResources.Call(process, 0)
				},
			)
			if tc.existingWindow {
				existingHWND = createRealTestWindow(t, className)
				createdHWND = 0
				native.windowMap[existingHWND] = &windowsWebviewWindow{hwnd: existingHWND}
			}

			winMenu, _ := newMenuWithHiddenSubmenu()
			parent = NewWindow(WebviewWindowOptions{
				Title:  "Test Startup Failure",
				Hidden: true,
				Windows: WindowsWindow{
					Menu: winMenu,
				},
			})
			app.Window.Add(parent)
			w = newWindowImpl(parent)
			parent.impl = w

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
			if menuBuiltEarly || w.menu != nil || winMenu.impl != nil {
				t.Fatal("menu should not be built before WebView2 setup succeeds")
			}
			if !destroyedOnErr {
				t.Fatal("expected failed window to be cleaned up before ErrorHandler ran")
			}
			if w.hwnd != 0 {
				t.Fatalf("failed startup leaked hwnd field %v", w.hwnd)
			}
			if w32.IsWindow(createdHWND) {
				t.Fatalf("failed startup did not destroy OS window %v", createdHWND)
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
			if gdiErrAfter != gdiErrBefore {
				t.Fatalf("ErrorHandler SetBackgroundColour leaked GDI objects: before=%d after=%d", gdiErrBefore, gdiErrAfter)
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

			gdiBefore, _, _ := getGuiResources.Call(process, 0)
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

func TestStartupErrorHandlerPanicStillCleansUp(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	className := "WailsTestStartupErrorHandlerPanic"
	var createdHWND w32.HWND
	app, _ := setupStartupFailureTestApp(
		t,
		className,
		true,
		filepath.Join(t.TempDir(), "missing-webview2"),
		func(hwnd w32.HWND) { createdHWND = hwnd },
		func(error) { panic("error handler panic") },
	)

	parent := NewWindow(WebviewWindowOptions{Title: "Panic Handler", Hidden: true})
	app.Window.Add(parent)
	w := newWindowImpl(parent)
	parent.impl = w

	recovered := false
	func() {
		defer func() {
			if recover() != nil {
				recovered = true
			}
		}()
		w.run()
	}()

	if !recovered {
		t.Fatal("expected ErrorHandler panic to propagate")
	}
	if createdHWND == 0 || w32.IsWindow(createdHWND) {
		t.Fatalf("HWND %v was not destroyed when ErrorHandler panicked", createdHWND)
	}
	if !parent.isDestroyed() {
		t.Fatal("parent window was not marked destroyed when ErrorHandler panicked")
	}
	if _, ok := app.Window.GetByID(parent.ID()); ok {
		t.Fatal("failed window remained in WindowManager when ErrorHandler panicked")
	}
}

func encodeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestStartupFailurePreservesSharedMenuBitmapsTrayAndKeyBindings(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	getObjectType := syscall.NewLazyDLL("gdi32.dll").NewProc("GetObjectType")

	className := "WailsTestStartupSharedAndPrivateMenu"
	app, native := setupStartupFailureTestApp(
		t,
		className,
		true,
		filepath.Join(t.TempDir(), "missing-webview2"),
		nil,
		func(error) {},
	)
	app.running = true

	// Case 1: Shared menu between a live window (with submenu + runtime SetBitmap)
	// and a second window whose startup fails.
	liveHWND := createRealTestWindow(t, className)
	liveParent := NewWindow(WebviewWindowOptions{Title: "Live"})
	liveWin := &windowsWebviewWindow{parent: liveParent, hwnd: liveHWND}
	liveParent.impl = liveWin
	native.windowMap[liveHWND] = liveWin

	sharedMenu := NewMenu()
	sharedItem := sharedMenu.Add("before")
	sharedSub := sharedMenu.AddSubmenu("Sub")
	sharedSubItem := sharedSub.Add("SubItem")
	liveWin.setMenu(sharedMenu)
	if liveWin.menu == nil {
		t.Fatal("live window failed to install shared menu")
	}
	t.Cleanup(func() {
		liveWin.removeMenu()
	})
	sharedItem.SetBitmap(encodeTestPNG(t))
	liveImpl, ok := sharedItem.impl.(*windowsMenuItem)
	if !ok || liveImpl.bitmap == 0 {
		t.Fatal("shared menu item has no native impl or bitmap")
	}
	liveBitmap := liveImpl.bitmap
	subImpl, ok := sharedSubItem.impl.(*windowsMenuItem)
	if !ok || !w32.IsMenu(subImpl.hMenu) {
		t.Fatal("shared submenu item has no valid parent HMENU")
	}

	failingShared := NewWindow(WebviewWindowOptions{
		Title:   "Failing Shared",
		Hidden:  true,
		Windows: WindowsWindow{Menu: sharedMenu},
	})
	app.Window.Add(failingShared)
	failingSharedImpl := newWindowImpl(failingShared)
	failingShared.impl = failingSharedImpl
	failingSharedImpl.run()

	if sharedItem.impl != liveImpl {
		t.Fatal("failed startup mutated shared MenuItem.impl of the live window")
	}
	if !w32.IsMenu(subImpl.hMenu) {
		t.Fatal("failed startup destroyed live window's shared submenu HMENU")
	}
	if objType, _, _ := getObjectType.Call(uintptr(liveBitmap)); objType != 7 {
		t.Fatalf("failed startup destroyed live window's HBITMAP: GetObjectType=%d, want 7", objType)
	}
	sharedItem.SetEnabled(false)
	info := w32.MENUITEMINFO{FMask: w32.MIIM_STATE}
	info.CbSize = uint32(unsafe.Sizeof(info))
	if !w32.GetMenuItemInfo(liveWin.menu.menu, uint32(liveImpl.id), false, &info) {
		t.Fatal("live window menu item became unreadable after sibling startup failure")
	}
	if info.FState&w32.MFS_DISABLED == 0 {
		t.Fatal("shared MenuItem.SetEnabled(false) did not reach the live window menu")
	}

	// Case 2: Shared tray menu (windowsMenu built via Update) keeps its HMENU intact.
	trayMenu := NewMenu()
	traySub := trayMenu.AddSubmenu("TraySub")
	traySubItem := traySub.Add("TrayAction")
	trayMenu.Update()
	traySubImpl, ok := traySubItem.impl.(*windowsMenuItem)
	if !ok || !w32.IsMenu(traySubImpl.hMenu) {
		t.Fatal("tray submenu HMENU was not created")
	}
	t.Cleanup(func() {
		if wm, ok := trayMenu.impl.(*windowsMenu); ok && wm != nil {
			wm.freeBitmaps()
			wm.detachedSubmenus.destroyAll()
			if wm.hMenu != 0 {
				w32.DestroyMenu(wm.hMenu)
			}
		}
	})

	failingTrayWin := NewWindow(WebviewWindowOptions{
		Title:   "Failing Tray Shared",
		Hidden:  true,
		Windows: WindowsWindow{Menu: trayMenu},
	})
	app.Window.Add(failingTrayWin)
	failingTrayImpl := newWindowImpl(failingTrayWin)
	failingTrayWin.impl = failingTrayImpl
	failingTrayImpl.run()

	if !w32.IsMenu(traySubImpl.hMenu) {
		t.Fatal("failed startup invalidated shared tray submenu HMENU")
	}

	// Case 3: Existing app-level accelerator (Ctrl+M) must stay intact when a failing
	// window defines a private menu with the same accelerator (even with invalid PNG).
	appMenu := NewMenu()
	existingItem := appMenu.Add("ExistingAppAction").SetAccelerator("Ctrl+M").OnClick(func(*Context) {})
	appMenu.Update()
	t.Cleanup(func() {
		if wm, ok := appMenu.impl.(*windowsMenu); ok && wm != nil {
			wm.freeBitmaps()
			wm.detachedSubmenus.destroyAll()
			if wm.hMenu != 0 {
				w32.DestroyMenu(wm.hMenu)
			}
		}
	})
	accelKey := existingItem.accelerator.String()
	app.keyBindingsLock.RLock()
	beforeBinding := app.KeyBinding.menuBindings[accelKey]
	app.keyBindingsLock.RUnlock()
	if beforeBinding != existingItem {
		t.Fatalf("expected app-level binding for %q to be registered before test", accelKey)
	}

	privateMenu := NewMenu()
	privateItem := privateMenu.Add("PrivateAction").SetAccelerator("Ctrl+M").OnClick(func(*Context) {})
	privateItem.SetBitmap([]byte("not-a-png"))

	failingPrivate := NewWindow(WebviewWindowOptions{
		Title:   "Failing Private",
		Hidden:  true,
		Windows: WindowsWindow{Menu: privateMenu},
	})
	app.Window.Add(failingPrivate)
	failingPrivateImpl := newWindowImpl(failingPrivate)
	failingPrivate.impl = failingPrivateImpl
	failingPrivateImpl.run()

	if privateMenu.impl != nil {
		t.Fatal("failed startup unexpectedly built windowsMenu for private menu")
	}
	app.keyBindingsLock.RLock()
	afterBinding := app.KeyBinding.menuBindings[accelKey]
	app.keyBindingsLock.RUnlock()
	if afterBinding != existingItem {
		t.Fatalf("failed startup clobbered pre-existing global binding %q: got %v, want %v", accelKey, afterBinding, existingItem)
	}
}

func TestStartupCleanupVisibilityTimeoutNoRace(t *testing.T) {
	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	app := &App{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		windows: make(map[uint]Window),
	}
	app.Window = newWindowManager(app)
	globalApplication = app

	for i := 0; i < 20; i++ {
		parent := NewWindow(WebviewWindowOptions{Title: "TimerRace"})
		app.Window.Add(parent)
		w := newWindowImpl(parent)
		parent.impl = w

		fired := make(chan struct{})
		w.visibilityTimeoutMu.Lock()
		var timer *time.Timer
		timer = time.AfterFunc(time.Millisecond, func() {
			close(fired)
			w.visibilityTimeoutMu.Lock()
			if w.visibilityTimeout == timer {
				w.visibilityTimeout = nil
			}
			w.visibilityTimeoutMu.Unlock()
		})
		w.visibilityTimeout = timer
		w.visibilityTimeoutMu.Unlock()

		<-fired
		w.cleanupFailedStartup()
	}
}

func TestStartupCreateWindowFailureReleasesMenu(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	var reported error
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

	w.run()

	if reported == nil {
		t.Fatal("expected ErrorHandler to receive window creation error")
	}
	if w.menu != nil {
		t.Fatal("CreateWindowEx failure left w.menu non-nil")
	}
	if !parent.isDestroyed() {
		t.Fatal("parent window was not marked destroyed after CreateWindowEx failure")
	}
	if _, ok := app.Window.GetByID(parent.ID()); ok {
		t.Fatal("failed window remained in WindowManager")
	}
}

func TestStartupModalFailureParentEnableSemantics(t *testing.T) {
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
		func(error) {},
	)

	// Subcase 1: Single modal fails during startup -> parent must be re-enabled.
	parentHWND := createRealTestWindow(t, className)
	modalHWND := createRealTestWindow(t, className)
	owner := NewWindow(WebviewWindowOptions{Title: "Owner"})
	ownerImpl := &windowsWebviewWindow{parent: owner, hwnd: parentHWND}
	owner.impl = ownerImpl
	native.windowMap[parentHWND] = ownerImpl

	modal := NewWindow(WebviewWindowOptions{Title: "Modal1"})
	app.Window.Add(modal)
	modalImpl := newWindowImpl(modal)
	modalImpl.hwnd = modalHWND
	modal.impl = modalImpl

	ownerImpl.attachModal(modal)
	if w32.IsWindowEnabled(parentHWND) {
		t.Fatal("expected AttachModal to disable parent window")
	}
	modalImpl.cleanupFailedStartup()

	if !w32.IsWindow(parentHWND) || !w32.IsWindowEnabled(parentHWND) {
		t.Fatal("parent window was not re-enabled after sole modal startup failure")
	}

	// Subcase 2: Existing live modal owns parent; second modal fails -> parent stays disabled.
	liveModalHWND := createRealTestWindow(t, className)
	liveModal := NewWindow(WebviewWindowOptions{Title: "LiveModal"})
	liveModalImpl := &windowsWebviewWindow{parent: liveModal, hwnd: liveModalHWND}
	liveModal.impl = liveModalImpl
	native.windowMap[liveModalHWND] = liveModalImpl
	ownerImpl.attachModal(liveModal)

	secondModalHWND := createRealTestWindow(t, className)
	secondModal := NewWindow(WebviewWindowOptions{Title: "SecondModal"})
	app.Window.Add(secondModal)
	secondModalImpl := newWindowImpl(secondModal)
	secondModalImpl.hwnd = secondModalHWND
	secondModal.impl = secondModalImpl
	ownerImpl.attachModal(secondModal)

	secondModalImpl.cleanupFailedStartup()
	if w32.IsWindowEnabled(parentHWND) {
		t.Fatal("failing second modal prematurely re-enabled parent while first modal is still live")
	}
}

func TestWindowManagerAddOnCreateNoDeadlockAndNoRace(t *testing.T) {
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

	// Concurrent OnCreate + Add must be race-free under -race.
	raceApp := &App{windows: make(map[uint]Window)}
	raceApp.Window = newWindowManager(raceApp)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			raceApp.Window.OnCreate(func(Window) {})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			raceApp.Window.Add(NewWindow(WebviewWindowOptions{Title: "Race"}))
		}
	}()
	wg.Wait()
}

func TestStartupFailureWhenLastWindowClosesMidSetupDoesNotFatal(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	previous := globalApplication
	t.Cleanup(func() { globalApplication = previous })

	className := "WailsTestLastWindowClosesMidSetup"
	var (
		reported     error
		existingHWND w32.HWND
		app          *App
		native       *windowsApp
	)
	app, native = setupStartupFailureTestApp(
		t,
		className,
		false, // default quit-on-last-window-closed enabled
		filepath.Join(t.TempDir(), "missing-webview2"),
		func(hwnd w32.HWND) {
			if existingHWND != 0 && hwnd != existingHWND {
				// Simulate the last pre-existing window closing after allowNonFatal
				// was snapshotted, before setupChromium completes: windowMap empties
				// and WM_QUIT is posted.
				native.unregisterWindow(&windowsWebviewWindow{hwnd: existingHWND})
			}
		},
		func(err error) { reported = err },
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
