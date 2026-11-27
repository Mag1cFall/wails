//go:build windows

package edge

import (
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/wailsapp/wails/v3/internal/webview2/internal/w32"
	"golang.org/x/sys/windows"
)

func TestHandlerReferenceCounting(t *testing.T) {
	e := NewChromium()
	handler := newComRef(unsafe.Pointer(e.processFailed))
	unbalanced := unbalancedHandlerReleases.Load()
	if handler.addRef() != 1 || handler.addRef() != 2 {
		t.Fatal("AddRef did not count native references")
	}
	if handler.release() != 1 || handlerRefCount(unsafe.Pointer(e.processFailed)) != 1 {
		t.Fatal("Release did not drop one reference")
	}
	if handler.release() != 0 || handlerRefCount(unsafe.Pointer(e.processFailed)) != 0 {
		t.Fatal("final Release left the handler registered")
	}
	if handler.release() != 0 || unbalancedHandlerReleases.Load() != unbalanced+1 {
		t.Fatal("unbalanced Release was not reported")
	}
}

// hostWindow stands in for the window whose callbacks a Chromium holds. Tests
// prove it unreachable by its weak pointer becoming nil, which the runtime only
// promises for an object it does not batch into one allocation slot with
// others. It batches tiny pointer-free objects, so the pointer field keeps a
// window out of any batch: without it, one allocated just before an object
// that stays referenced is retained with it.
type hostWindow struct {
	events int
	_      *hostWindow
}

// besideLiveNeighbours allocates windows that nothing references, each
// followed at once by a small pointer-free object the caller keeps.
//
//go:noinline
func besideLiveNeighbours() ([]weak.Pointer[hostWindow], []*int64) {
	windows := make([]weak.Pointer[hostWindow], 0, 8)
	neighbours := make([]*int64, 0, 8)
	for i := 0; i < 8; i++ {
		windows = append(windows, weak.Make(&hostWindow{}))
		neighbours = append(neighbours, new(int64))
	}
	return windows, neighbours
}

// The lifetime tests prove a window unreachable by its weak pointer becoming
// nil. That proof needs a window the runtime frees on its own, never batched
// into one allocation slot with an object that is still referenced.
func TestHostWindowIsCollectedBesideLiveObjects(t *testing.T) {
	windows, neighbours := besideLiveNeighbours()
	defer runtime.KeepAlive(neighbours)
	for i, window := range windows {
		if !waitCollected(window) {
			t.Fatalf("window %d was retained by the live object allocated beside it", i)
		}
	}
}

// abandonWithHostCallbacks returns a handler that native code still references
// and the window whose callbacks its abandoned Chromium used to hold.
//
//go:noinline
func abandonWithHostCallbacks() (comRef, weak.Pointer[hostWindow]) {
	w := &hostWindow{}
	e := NewChromium()
	e.MessageCallback = func(string, *ICoreWebView2, *ICoreWebView2WebMessageReceivedEventArgs) { w.events++ }
	e.MessageWithAdditionalObjectsCallback = e.MessageCallback
	e.WebResourceRequestedCallback = func(*ICoreWebView2WebResourceRequest, *ICoreWebView2WebResourceRequestedEventArgs) { w.events++ }
	e.NavigationStartingCallback = func(*ICoreWebView2) { w.events++ }
	e.NavigationCompletedCallback = func(*ICoreWebView2, *ICoreWebView2NavigationCompletedEventArgs) { w.events++ }
	e.ProcessFailedCallback = func(*ICoreWebView2, *ICoreWebView2ProcessFailedEventArgs) { w.events++ }
	e.ContainsFullScreenElementChangedCallback = func(*ICoreWebView2, *ICoreWebView2ContainsFullScreenElementChangedEventArgs) { w.events++ }
	e.AcceleratorKeyCallback = func(uint) bool { w.events++; return false }
	e.CursorChangedCallback = func(HCURSOR, uint32) { w.events++ }
	e.SetErrorCallback(func(error) { w.events++ })
	handler := newComRef(unsafe.Pointer(e.processFailed))
	handler.addRef()
	e.ShuttingDown()
	return handler, weak.Make(w)
}

func TestAbandonedChromiumDoesNotRetainWindow(t *testing.T) {
	handler, window := abandonWithHostCallbacks()
	defer handler.release()
	if !waitCollected(window) {
		t.Fatal("a native reference to an abandoned Chromium retained its window")
	}
	if handler.refs() != 1 {
		t.Fatal("handler lost its native reference")
	}
}

func writeComOutPtr(dst, ptr uintptr) {
	rtlMoveMemory := windows.NewLazySystemDLL("kernel32.dll").NewProc("RtlMoveMemory")
	_, _, _ = rtlMoveMemory.Call(dst, uintptr(unsafe.Pointer(&ptr)), unsafe.Sizeof(ptr))
}

func TestSettingsExtensionMethodsReleaseCOMRefs(t *testing.T) {
	var (
		settingsRefs    int
		extRefs         int
		controller3Refs int
	)

	var extVtbl ICoreWebView2Settings9Vtbl
	extVtbl.Release = NewComProc(func(uintptr) uintptr {
		extRefs--
		return uintptr(extRefs)
	})
	extVtbl.PutIsGeneralAutofillEnabled = NewComProc(func(uintptr, uintptr) uintptr { return 0 })
	extVtbl.PutIsPasswordAutosaveEnabled = NewComProc(func(uintptr, uintptr) uintptr { return 0 })
	extVtbl.PutIsSwipeNavigationEnabled = NewComProc(func(uintptr, uintptr) uintptr { return 0 })
	extVtbl.GetIsSwipeNavigationEnabled = NewComProc(func(uintptr, uintptr) uintptr { return 0 })
	extVtbl.PutIsNonClientRegionSupportEnabled = NewComProc(func(uintptr, uintptr) uintptr { return 0 })
	extObj := ICoreWebView2Settings9{Vtbl: &extVtbl}

	var settingsVtbl _ICoreWebViewSettingsVtbl
	settingsVtbl.QueryInterface = NewComProc(func(_, _, out uintptr) uintptr {
		extRefs++
		writeComOutPtr(out, uintptr(unsafe.Pointer(&extObj)))
		return 0
	})
	settingsVtbl.Release = NewComProc(func(uintptr) uintptr {
		settingsRefs--
		return uintptr(settingsRefs)
	})
	settingsObj := ICoreWebViewSettings{vtbl: &settingsVtbl}

	var webviewVtbl iCoreWebView2Vtbl
	webviewVtbl.GetSettings = NewComProc(func(_, out uintptr) uintptr {
		settingsRefs++
		writeComOutPtr(out, uintptr(unsafe.Pointer(&settingsObj)))
		return 0
	})
	webviewObj := ICoreWebView2{vtbl: &webviewVtbl}

	var controller3Vtbl ICoreWebView2Controller3Vtbl
	controller3Vtbl.Release = NewComProc(func(uintptr) uintptr {
		controller3Refs--
		return uintptr(controller3Refs)
	})
	controller3Obj := ICoreWebView2Controller3{Vtbl: &controller3Vtbl}

	var controllerVtbl _ICoreWebView2ControllerVtbl
	controllerVtbl.QueryInterface = NewComProc(func(_, _, out uintptr) uintptr {
		controller3Refs++
		writeComOutPtr(out, uintptr(unsafe.Pointer(&controller3Obj)))
		return 0
	})
	controllerObj := ICoreWebView2Controller{vtbl: &controllerVtbl}

	e := &Chromium{
		webview:                &webviewObj,
		webview2RuntimeVersion: "140.0.0.0",
	}

	if err := e.PutIsGeneralAutofillEnabled(true); err != nil {
		t.Fatalf("PutIsGeneralAutofillEnabled: %v", err)
	}
	if err := e.PutIsPasswordAutosaveEnabled(true); err != nil {
		t.Fatalf("PutIsPasswordAutosaveEnabled: %v", err)
	}
	if err := e.PutIsSwipeNavigationEnabled(true); err != nil {
		t.Fatalf("PutIsSwipeNavigationEnabled: %v", err)
	}
	if _, err := e.GetIsSwipeNavigationEnabled(); err != nil {
		t.Fatalf("GetIsSwipeNavigationEnabled: %v", err)
	}
	if err := e.PutIsNonClientRegionSupportEnabled(true); err != nil {
		t.Fatalf("PutIsNonClientRegionSupportEnabled: %v", err)
	}
	c3 := controllerObj.GetICoreWebView2Controller3()
	if c3 == nil {
		t.Fatal("GetICoreWebView2Controller3 returned nil")
	}
	c3.Release()

	runtime.KeepAlive(extVtbl)
	runtime.KeepAlive(settingsVtbl)
	runtime.KeepAlive(webviewVtbl)
	runtime.KeepAlive(controller3Vtbl)
	runtime.KeepAlive(controllerVtbl)
	runtime.KeepAlive(extObj)
	runtime.KeepAlive(settingsObj)
	runtime.KeepAlive(webviewObj)
	runtime.KeepAlive(controller3Obj)
	runtime.KeepAlive(controllerObj)

	if settingsRefs != 0 {
		t.Fatalf("ICoreWebViewSettings ref count = %d, want 0", settingsRefs)
	}
	if extRefs != 0 {
		t.Fatalf("extended settings COM ref count = %d, want 0", extRefs)
	}
	if controller3Refs != 0 {
		t.Fatalf("ICoreWebView2Controller3 ref count = %d, want 0", controller3Refs)
	}
}

func TestPumpUntilInitedWMQuitSetsCancellationError(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	e := NewChromium()
	w32.User32PostQuitMessage.Call(0)

	if e.pumpUntilInited(5 * time.Second) {
		t.Fatal("pumpUntilInited reported ready after WM_QUIT")
	}
	if e.initErr == nil || !strings.Contains(e.initErr.Error(), "quitting") {
		t.Fatalf("initErr = %v, want quitting cancellation instead of timeout", e.initErr)
	}

	var msg w32.Msg
	gotQuit := false
	for {
		got, _, _ := w32.User32PeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, w32.PM_REMOVE)
		if got == 0 {
			break
		}
		if msg.Message == w32.WM_QUIT {
			gotQuit = true
		}
	}
	if !gotQuit {
		t.Fatal("pumpUntilInited did not preserve WM_QUIT in thread message queue")
	}
}
