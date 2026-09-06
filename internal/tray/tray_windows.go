//go:build windows

package tray

import (
	"context"
	"log"
	"net/url"
	"strings"

	"fyne.io/systray"

	"github.com/omniapi/omni-api/internal/browser"
)

func run(ctx context.Context, options Options) {
	ready := func() {
		systray.SetIcon(icon())
		systray.SetTitle("OmniApi")
		systray.SetTooltip("OmniApi 网关")
		open := systray.AddMenuItem("打开配置", "在浏览器中打开配置界面")
		logs := systray.AddMenuItem("运行日志", "实时查看请求转发与错误详情")
		systray.AddSeparator()
		quit := systray.AddMenuItem("退出", "停止网关并退出")

		go func() {
			for {
				select {
				case <-open.ClickedCh:
					if err := browser.Open(options.ConsoleURL()); err != nil {
						log.Printf("could not open the configuration page: %v", err)
					}
				case <-quit.ClickedCh:
					systray.Quit()
					return
				case <-logs.ClickedCh:
					if target, err := url.Parse(options.ConsoleURL()); err == nil {
						target.Path = strings.TrimRight(target.Path, "/") + "/runtime-logs"
						if err := browser.Open(target.String()); err != nil {
							log.Printf("could not open runtime logs: %v", err)
						}
					}
				case <-ctx.Done():
					systray.Quit()
					return
				}
			}
		}()
	}
	systray.Run(ready, func() {})
}
