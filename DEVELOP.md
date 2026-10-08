# 开发文档（供 AI / 开发者维护使用）

本文档记录 photoManager 的设计需求与当前实现细节，供后续开发（尤其是 AI 辅助开发）对齐上下文。
面向最终用户的使用说明见 [README.md](README.md)。

## 一、原始需求与设计目标

这是一个照片管理工具：接收一个照片目录，根据照片的时间元数据进行分类整理。

技术栈为 Go。核心约束：**实际逻辑函数独立于入口实现**，接收一个参数结构体并返回结果结构体，以便 CLI 和 UI 都直接调用同一套核心逻辑。

主要功能参数：

1. **源路径**：图片目录，可选是否递归处理子目录。
2. **目标路径**：分类后的图片目录。
3. **目录结构模板**：如 `{yyyy}-{MM}-{dd}`、`{yyyy}-{MM}-{dd}/{HH}-{mm}-{ss}`，`{}` 内变量替换为实际时间，`/` 表示目录分隔。
4. **文件处理方式**：
   - `dry-run`：仅模拟，输出所有拟移动的文件路径，不做实际变更。
   - `copy`：复制文件到目标目录。
   - `move`：移动文件到目标目录。
5. **时间来源**（用于渲染模板），按用户自定义顺序尝试：
   - `exif`：照片 EXIF 拍摄时间。
   - `create-time`：文件创建时间（如平台支持）。
   - `modify-time`：文件修改时间。
   - 所有来源都取不到时，跳过该文件并输出警告。
6. **同名文件处理策略**：
   - `overwrite`：覆盖已存在的文件。
   - `skip`：跳过。
   - `rename`：重命名为 `{filename}_{timestamp}.{ext}`。

输出要求：只打印必要信息（每个文件的 `源 -> 目标` 映射、警告），最后打印 `已处理 成功数/总数，耗时 X.Xs`。

性能要求：默认 4 线程并行；需支撑上万文件场景，做好缓存、减少文件操作次数、仅读取必要信息。

## 二、项目结构

```
photoManager/
├── main.go                        # CLI 入口：解析 flag -> 调 organizer.Organize -> 打印结果
├── gui/main.go                    # GUI 入口（Gio）：收集参数 -> 调 organizer.Organize -> 展示日志
├── organizer/
│   ├── organizer.go               # 核心逻辑：扫描、并发处理、模板渲染、冲突处理、复制/移动
│   ├── exif.go                    # 自研轻量 EXIF 拍摄时间解析器（仅 JPEG/TIFF 头部）
│   ├── filetime_windows.go        # Windows 创建时间（syscall.Win32FileAttributeData）
│   └── filetime_other.go          # 非 Windows：创建时间不支持，返回错误以回退下一来源
├── build.sh                       # 本地构建 CLI: go build -o photoManager.exe .
├── build-gui.sh                   # 本地构建 GUI: go build -ldflags="-H windowsgui" -o photoManager-gui.exe ./gui
└── .github/workflows/release.yml  # CI：windows-latest 构建双产物，打 v* tag 时发布 Release
```

## 三、核心包 organizer

### 3.1 对外契约

入口为 `Organize(opts Options) (*Result, error)`：

- `Options`：SrcDir / DstDir / DirTemplate / Recursive / Mode / TimeSources / OnConflict / Workers。
  `normalizeAndValidate` 负责填默认值（模板默认 `{yyyy}-{MM}-{dd}`，时间来源默认 `exif,create-time,modify-time`，Workers 默认 4）并校验枚举值，非法值直接报错。
- `Result`：Total（扫描到的文件总数）/ Processed（成功或拟处理数）/ Files（`[]FileResult{Src,Dst,Status}`，按 Src 排序保证输出稳定）/ Warnings（排序后的字符串列表）/ Duration。
- 枚举类型 `FileMode` / `TimeSource` / `ConflictStrategy` 均为 string 别名，常量与需求文档的取值一致。

### 3.2 支持的扩展名

`supportedExts` 含常见图片（jpg/jpeg/png/gif/bmp/webp/tif/tiff/heic/heif/dng/cr2/nef/arw/orf/rw2）和视频（mp4/mov/avi/mkv 等）。
`exifExts` 仅 jpg/jpeg/tif/tiff —— 只有这些格式会真正读取 EXIF，其他格式直接跳过 EXIF 来源，避免无效 I/O。视频因此只能靠 create-time / modify-time 归类。

### 3.3 并发模型

经典 worker pool：

- `collectFiles` 用 `filepath.WalkDir` 扫描，**遍历时即通过 `d.Info()` 拿到 `fs.FileInfo` 存入 `fileJob`**，后续阶段不再重复 stat。
- 若目标目录位于源目录内，遍历时 `SkipDir` 跳过，避免把已整理的文件再次处理。
- N 个 worker 从 `jobCh` 消费，结果写入 `outCh`；主 goroutine 汇总。每个 worker 持有独立的 256KB 复制缓冲区复用。
- 汇总后对 Files/Warnings 排序，保证多次运行输出一致。

### 3.4 单文件处理流程（processFile）

1. `detectTime` 按 TimeSources 顺序尝试，返回第一个成功且非零值的时间；全部失败 -> 警告跳过。
2. `renderTemplate` 渲染目录模板（见 3.5），拼出目标路径。
3. 源路径与目标路径相同（`samePath`，Windows 下大小写不敏感）-> 警告跳过。
4. `alloc.Allocate(dst)` 按冲突策略决定最终路径（见 3.6）。
5. dry-run 直接返回 planned；copy/move 先 `ensureDir`（带缓存的 MkdirAll）再执行。
   - `copyFile`：`io.CopyBuffer` + 保留原修改时间（`os.Chtimes`）。
   - `moveFile`：优先 `os.Rename`，跨盘失败时回退 copy + remove。

### 3.5 模板渲染

正则 `\{([a-zA-Z]+)\}` 匹配变量，支持：`yyyy`（4 位年）、`yy`（2 位年）、`MM`、`dd`、`HH`、`mm`、`ss`（均补零 2 位）。未知变量原样保留。`/` 通过 `filepath.FromSlash` 转平台分隔符。

### 3.6 冲突处理：dstAllocator（并发安全）

关键性能设计：**不逐文件 Stat 目标路径**，而是：

- `claimed`：本次运行已占用的目标路径集合。
- `dirLists`：目标目录现有文件清单缓存（目录 -> 文件名集合），每个目录只 `os.ReadDir` 一次；Windows 下文件名统一转小写比较（`cacheKey`）。
- `dirs`：已创建目录缓存（`sync.Map`，MkdirAll 幂等不占主锁）。

各策略行为：

- `overwrite`：不做磁盘检查（省去清单读取），直接占用返回。
- `skip`：已占用/已存在 -> proceed=false。
- `rename`：追加 `_<unix时间戳>`，冲突时再追加 `_1/_2/...` 序号。

注意 rename 的时间戳取的是**处理时刻** `time.Now().Unix()`，同一秒内多个同名文件靠序号区分。

### 3.7 EXIF 解析（exif.go）

自研轻量解析器，**有意不用第三方库**：

- 动机：部分设备会写出不规范标签（如 count=0 的 ExifIFDPointer），第三方库遇错会整体失败导致错误降级到文件时间；自研解析器只提取时间标签，对其他损坏保持容忍。
- 流式读取：校验 SOI(0xFFD8) 后逐段扫描，**只完整读 APP1(0xFFE1) 段**（JPEG 单段上限 64KB），其余段用 `Seek` 跳过，不读图像数据。
- TIFF 解析：支持 II/MM 字节序，全部访问带边界检查；IFD 条目数上限 512 防越界。
- 时间标签优先级：Exif 子 IFD 的 DateTimeOriginal(0x9003) / DateTimeDigitized(0x9004) -> IFD0 的 0x9003 / 0x9004 / DateTime(0x0132)。
- 容错细节：ExifIFDPointer(0x8769) 不校验 count 字段，直接按 LONG 读值。
- 时间格式 `2006:01:02 15:04:05`，EXIF 无时区信息，**按本地时区（time.Local）解析**。

### 3.8 平台差异

`creationTime` 用 build tag 分平台实现：Windows 从 `Win32FileAttributeData.CreationTime` 取；其他平台返回错误，由 `detectTime` 自动回退到下一来源。当前产物只面向 Windows。

## 四、入口层

### 4.1 CLI（main.go）

flag 参数与 Options 一一对应：`-src`、`-dst`（必填）、`-recursive`（默认 true）、`-template`、`-mode`（默认 dry-run）、`-time-source`（逗号分隔字符串，解析为 `[]TimeSource`）、`-on-conflict`（默认 rename）、`-workers`（默认 4）。输出：`Src -> Dst` 逐行、警告、最后汇总行。

### 4.2 GUI（gui/main.go）

基于 Gio（`gioui.org` + `gioui.org/x/explorer`），Material 主题：

- 与 CLI 共用 organizer，UI 只做参数收集（`buildOptions`）与日志展示。
- 中文字体：优先加载 `C:\Windows\Fonts\msyh.ttc`（微软雅黑），失败尝试 `simhei.ttf`，叠加在 gofont 之上。
- 时间来源是单行输入框，逗号分隔自定义顺序（默认 `exif,modify-time,create-time`）。
- 整理在后台 goroutine 执行，日志经带缓冲 channel（1024）回 UI 线程，`drainLogs` 批量消费避免竞态；运行期间按钮置灰防重入，每次开始整理前清空上次日志。
- 日志区用 `layout.List` 虚拟化渲染（仅绘制可见行，上万条不卡），`logSnapshot` 版本化快照隔离读写；`ScrollToEnd=true` 在底部时自动跟随，用户上翻后不强制吸底。"复制日志"按钮导出全文到剪贴板。
- 目录选择用 `gioui.org/x/explorer` 的 `ChooseFolder`。

## 五、构建与发布

- 本地：`build.sh` / `build-gui.sh`。GUI 必须带 `-ldflags="-H windowsgui"` 隐藏控制台窗口。
- CI（`.github/workflows/release.yml`）：`windows-latest` + Go 1.26.3，打 `v*` tag 时构建双产物（`-trimpath -ldflags="-s -w"`）并通过 `softprops/action-gh-release` 发布 Release；平时可手动 `workflow_dispatch` 构建 artifact。
- 纯 Go 依赖，无 CGO，Windows 交叉/原生编译均无额外依赖。

## 六、已知取舍 / 注意事项

- 视频文件无 EXIF 解析，只能依赖文件时间。
- 非 Windows 平台 `create-time` 恒失败（回退机制兜底）。
- EXIF 时间按本地时区处理，跨时区拍摄的照片不做过渡修正。
- rename 策略的时间戳精度为秒，同秒多文件靠递增序号保证唯一。
- dry-run 模式下 rename 冲突探测基于运行开始时的目录快照缓存，运行期间外部对目标目录的改动不可见。

## 备忘
https://todo.sr.ht/~eliasnaur/gio/710?__goaway_challenge=meta-refresh&__goaway_id=5c39a5d7abeff4a27806346690da6a68&__goaway_referer=https%3A%2F%2Ftodo.sr.ht%2F~eliasnaur%2Fgio%3Fsearch%3Deditor
Windows在DPI缩放下（比如4K+150%）会导致Editor拖拽选区、滚动条拖拽异常，属于GIO内部bug，目前未修复