// GUI 版本入口：基于 Gio 的照片管理工具界面。
// 与命令行版 main.go 共用 organizer 核心逻辑，仅负责参数收集与日志展示。
package main

import (
	"fmt"
	"image/color"
	"io"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"gioui.org/x/explorer"

	"photomanager/organizer"
)

type ui struct {
	win *app.Window
	th  *material.Theme
	exp *explorer.Explorer

	srcEdit, dstEdit, tmplEdit, workersEdit widget.Editor
	srcBtn, dstBtn, runBtn, copyBtn         widget.Clickable
	recursiveChk                            widget.Bool
	modeEnum                                widget.Enum
	conflictEnum                            widget.Enum
	tsEXIF, tsCreate, tsModify              widget.Bool

	logList     widget.List
	logLines    []string // 全量日志（仅 UI 线程写入）
	logVersion  int
	logSnapshot []string // 渲染用快照
	snapVersion int

	logCh   chan string
	mu      sync.Mutex
	running bool
}

func main() {
	go func() {
		w := new(app.Window)
		w.Option(app.Title("照片管理工具"), app.Size(unit.Dp(900), unit.Dp(720)))
		if err := run(w); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}

// newTheme 构造主题，并加载系统中文字体以支持中文显示。
func newTheme() *material.Theme {
	th := material.NewTheme()
	collection := gofont.Collection()
	// Windows 常见中文字体：微软雅黑 / 黑体
	for _, p := range []string{`C:\Windows\Fonts\msyh.ttc`, `C:\Windows\Fonts\simhei.ttf`} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		faces, err := opentype.ParseCollection(data)
		if err != nil {
			continue
		}
		collection = append(collection, faces...)
		break
	}
	th.Shaper = text.NewShaper(text.WithCollection(collection))
	return th
}

func run(w *app.Window) error {
	u := &ui{win: w, th: newTheme(), exp: explorer.NewExplorer(w)}
	u.srcEdit.SingleLine = true
	u.dstEdit.SingleLine = true
	u.tmplEdit.SingleLine = true
	u.tmplEdit.SetText("{yyyy}/{MM}")
	u.workersEdit.SingleLine = true
	u.workersEdit.SetText("4")
	u.recursiveChk.Value = true
	u.modeEnum.Value = string(organizer.ModeDryRun)
	u.conflictEnum.Value = string(organizer.ConflictOverwrite)
	u.tsEXIF.Value = true
	u.tsCreate.Value = true
	u.tsModify.Value = true
	u.logList.Axis = layout.Vertical
	u.logList.ScrollToEnd = true // 在底部时自动跟随；用户上翻后不再强制吸底
	u.logCh = make(chan string, 1024)

	var ops op.Ops
	for {
		e := w.Event()
		u.exp.ListenEvents(e)
		switch e := e.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			if u.srcBtn.Clicked(gtx) {
				go u.chooseFolder(&u.srcEdit)
			}
			if u.dstBtn.Clicked(gtx) {
				go u.chooseFolder(&u.dstEdit)
			}
			if u.runBtn.Clicked(gtx) && !u.isRunning() {
				u.clearLogs() // 每次开始整理前清空上次日志
				if opts, err := u.buildOptions(); err != nil {
					u.appendLog("错误: " + err.Error())
				} else {
					go u.run(opts)
				}
			}
			if u.copyBtn.Clicked(gtx) {
				gtx.Execute(clipboard.WriteCmd{
					Type: "application/text",
					Data: io.NopCloser(strings.NewReader(u.allLogs())),
				})
			}
			u.drainLogs()
			u.Layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (u *ui) isRunning() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

func (u *ui) setRunning(v bool) {
	u.mu.Lock()
	u.running = v
	u.mu.Unlock()
	u.win.Invalidate()
}

func (u *ui) appendLog(s string) {
	u.logCh <- s
	u.win.Invalidate()
}

// drainLogs 在 UI 线程批量接收日志，避免竞态；真正的渲染由 logArea 懒加载完成。
func (u *ui) drainLogs() {
	for {
		select {
		case s := <-u.logCh:
			u.mu.Lock()
			u.logLines = append(u.logLines, s)
			u.logVersion++
			u.mu.Unlock()
		default:
			return
		}
	}
}

// clearLogs 清空日志（UI 线程调用）。
func (u *ui) clearLogs() {
	u.mu.Lock()
	u.logLines = nil
	u.logVersion++
	u.mu.Unlock()
}

// allLogs 返回完整日志文本（用于复制到剪贴板）。
func (u *ui) allLogs() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.logLines, "\n")
}

// chooseFolder 弹出系统目录选择框，结果填入指定输入框。
func (u *ui) chooseFolder(ed *widget.Editor) {
	dir, err := u.exp.ChooseFolder()
	if err != nil {
		if err != explorer.ErrUserDecline {
			u.appendLog("错误: 选择目录失败: " + err.Error())
		}
		return
	}
	ed.SetText(dir)
	u.win.Invalidate()
}

// buildOptions 从 UI 状态收集 organizer.Options（在 UI 线程调用）。
func (u *ui) buildOptions() (organizer.Options, error) {
	src := strings.TrimSpace(u.srcEdit.Text())
	dst := strings.TrimSpace(u.dstEdit.Text())
	if src == "" || dst == "" {
		return organizer.Options{}, fmt.Errorf("源目录和目标目录不能为空")
	}
	workers, err := strconv.Atoi(strings.TrimSpace(u.workersEdit.Text()))
	if err != nil || workers < 1 {
		workers = 4
	}
	var sources []organizer.TimeSource
	if u.tsEXIF.Value {
		sources = append(sources, organizer.TimeSourceEXIF)
	}
	if u.tsCreate.Value {
		sources = append(sources, organizer.TimeSourceCreateTime)
	}
	if u.tsModify.Value {
		sources = append(sources, organizer.TimeSourceModifyTime)
	}
	if len(sources) == 0 {
		return organizer.Options{}, fmt.Errorf("请至少选择一种时间来源")
	}
	tmpl := strings.TrimSpace(u.tmplEdit.Text())
	if tmpl == "" {
		tmpl = "{yyyy}-{MM}-{dd}"
	}
	return organizer.Options{
		SrcDir:      src,
		DstDir:      dst,
		DirTemplate: tmpl,
		Recursive:   u.recursiveChk.Value,
		Mode:        organizer.FileMode(u.modeEnum.Value),
		TimeSources: sources,
		OnConflict:  organizer.ConflictStrategy(u.conflictEnum.Value),
		Workers:     workers,
	}, nil
}

// run 在后台 goroutine 中执行整理任务，结果写入日志区。
func (u *ui) run(opts organizer.Options) {
	u.setRunning(true)
	defer u.setRunning(false)
	u.appendLog("开始处理...")
	res, err := organizer.Organize(opts)
	if err != nil {
		u.appendLog("错误: " + err.Error())
		return
	}
	for _, f := range res.Files {
		u.appendLog(f.Src + " -> " + f.Dst)
	}
	for _, w := range res.Warnings {
		u.appendLog("警告: " + w)
	}
	u.appendLog(fmt.Sprintf("已处理 %d/%d，耗时 %.1fs", res.Processed, res.Total, res.Duration.Seconds()))
}

var borderColor = color.NRGBA{R: 0xaa, G: 0xaa, B: 0xaa, A: 0xff}

// editorBox 为输入框绘制边框。
func editorBox(th *material.Theme, ed *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		b := widget.Border{Color: borderColor, Width: unit.Dp(1), CornerRadius: unit.Dp(4)}
		e := material.Editor(th, ed, hint)
		return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(6)).Layout(gtx, e.Layout)
		})
	}
}

// pathRow 渲染“标签 + 输入框 + 浏览按钮”的路径选择行。
func (u *ui) pathRow(label string, ed *widget.Editor, btn *widget.Clickable) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(material.Label(u.th, u.th.TextSize, label).Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Flexed(1, editorBox(u.th, ed, "请选择或输入目录路径")),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(material.Button(u.th, btn, "浏览...").Layout),
				)
			}),
		)
	}
}

func (u *ui) Layout(gtx layout.Context) layout.Dimensions {
	return layout.UniformInset(unit.Dp(12)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(material.H6(u.th, "照片管理工具").Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
			layout.Rigid(u.pathRow("源目录", &u.srcEdit, &u.srcBtn)),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(u.pathRow("目标目录", &u.dstEdit, &u.dstBtn)),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(material.CheckBox(u.th, &u.recursiveChk, "递归处理子目录").Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

			layout.Rigid(material.Label(u.th, u.th.TextSize, `目录结构模板（如 {yyyy}-{MM}-{dd}/{HH}-{mm}-{ss}）`).Layout),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(editorBox(u.th, &u.tmplEdit, "{yyyy}-{MM}-{dd}")),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

			layout.Rigid(material.Label(u.th, u.th.TextSize, "文件处理方式").Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(material.RadioButton(u.th, &u.modeEnum, string(organizer.ModeDryRun), "dry-run 仅模拟").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.RadioButton(u.th, &u.modeEnum, string(organizer.ModeCopy), "copy 复制").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.RadioButton(u.th, &u.modeEnum, string(organizer.ModeMove), "move 移动").Layout),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

			layout.Rigid(material.Label(u.th, u.th.TextSize, "时间来源（按顺序尝试，至少选一项）").Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(material.CheckBox(u.th, &u.tsEXIF, "EXIF 拍摄时间").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.CheckBox(u.th, &u.tsCreate, "创建时间").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.CheckBox(u.th, &u.tsModify, "修改时间").Layout),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

			layout.Rigid(material.Label(u.th, u.th.TextSize, "同名文件处理策略").Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{}.Layout(gtx,
					layout.Rigid(material.RadioButton(u.th, &u.conflictEnum, string(organizer.ConflictOverwrite), "overwrite 覆盖").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.RadioButton(u.th, &u.conflictEnum, string(organizer.ConflictSkip), "skip 跳过").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.RadioButton(u.th, &u.conflictEnum, string(organizer.ConflictRename), "rename 重命名").Layout),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),

			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(material.Label(u.th, u.th.TextSize, "并发线程数").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Max.X = gtx.Dp(unit.Dp(120))
						return editorBox(u.th, &u.workersEdit, "4")(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(24)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						label := "开始整理"
						b := material.Button(u.th, &u.runBtn, label)
						if u.isRunning() {
							b.Text = "处理中..."
							b.Background = color.NRGBA{R: 0x99, G: 0x99, B: 0x99, A: 0xff}
						}
						return b.Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),

			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(material.Label(u.th, u.th.TextSize, "日志").Layout),
					layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
					layout.Rigid(material.Button(u.th, &u.copyBtn, "复制日志").Layout),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Flexed(1, u.logArea),
		)
	})
}

// logArea 渲染日志区域：layout.List 虚拟化渲染，仅绘制可见行，
// 上万条日志也不会卡顿；通过“复制日志”按钮导出全文。
func (u *ui) logArea(gtx layout.Context) layout.Dimensions {
	u.mu.Lock()
	if u.logVersion != u.snapVersion {
		u.logSnapshot = slices.Clone(u.logLines)
		u.snapVersion = u.logVersion
	}
	lines := u.logSnapshot
	u.mu.Unlock()
	b := widget.Border{Color: borderColor, Width: unit.Dp(1), CornerRadius: unit.Dp(4)}
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(6)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.List(u.th, &u.logList)
			return l.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
				lbl := material.Body2(u.th, lines[i])
				lbl.TextSize = unit.Sp(13)
				return lbl.Layout(gtx)
			})
		})
	})
}
