//go:build windows && !server && production && !devtools

package application

import "github.com/Mag1cFall/wails/v3/internal/webview2/pkg/edge"

func (w *windowsWebviewWindow) openDevTools() {}

func (w *windowsWebviewWindow) enableDevTools(settings *edge.ICoreWebViewSettings) error {
	return settings.PutAreDevToolsEnabled(false)
}
