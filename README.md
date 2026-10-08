# photoManager 照片管理工具

按照片的拍摄/文件时间，把杂乱的照片目录自动整理成按日期分类的目录结构。提供命令行（CLI）和图形界面（GUI）两个版本，仅支持 Windows。

## 功能特性

- 按时间分类：支持 EXIF 拍摄时间、文件创建时间、文件修改时间，可自定义尝试顺序
- 自定义目录结构：如 `{yyyy}-{MM}-{dd}`、`{yyyy}/{MM}/{dd}` 等
- 三种处理模式：先模拟演练（dry-run）确认无误，再复制（copy）或移动（move）
- 同名文件策略：覆盖 / 跳过 / 自动重命名
- 多线程并行处理，支持上万张照片
- 支持常见图片（jpg、png、heic、cr2 等 RAW 格式）和视频（mp4、mov 等）格式

## 下载与安装

从 [Releases](../../releases) 页面下载最新版本：

- `photoManager.exe` —— 命令行版本
- `photoManager-gui.exe` —— 图形界面版本（双击即用）

也可以自行构建（需要 Go 1.24+）：

```powershell
# CLI
go build -o photoManager.exe .

# GUI
go build -ldflags="-H windowsgui" -o photoManager-gui.exe ./gui
```

## GUI 使用

双击 `photoManager-gui.exe`，依次填写：

1. **源目录 / 目标目录**：可手动输入或点"浏览..."选择
2. **目录结构模板**：默认 `{yyyy}/{MM}`，即按"年/月"两级目录归类
3. **文件处理方式**：建议先选 `dry-run 仅模拟`，确认日志中的归类结果无误后再改成 `copy` 或 `move`
4. **时间来源**：逗号分隔，按顺序尝试，默认 `exif,modify-time,create-time`
5. **同名文件处理策略**：目标位置已有同名文件时怎么办
6. 点击"开始整理"，日志区实时显示每个文件的归类结果，可用"复制日志"导出

## CLI 使用

```powershell
photoManager.exe -src "D:\照片" -dst "D:\整理后" -mode dry-run
```

### 参数说明

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-src` | （必填） | 源图片目录 |
| `-dst` | （必填） | 分类后的目标目录 |
| `-recursive` | `true` | 是否递归处理子目录 |
| `-template` | `{yyyy}-{MM}-{dd}` | 目录结构模板，`/` 表示子目录 |
| `-mode` | `dry-run` | 处理方式：`dry-run` 仅模拟 / `copy` 复制 / `move` 移动 |
| `-time-source` | `exif,create-time,modify-time` | 时间来源优先级，逗号分隔 |
| `-on-conflict` | `rename` | 同名文件策略：`overwrite` 覆盖 / `skip` 跳过 / `rename` 重命名 |
| `-workers` | `4` | 并发线程数 |

### 示例

先演练看看会怎么分类：

```powershell
photoManager.exe -src "D:\照片" -dst "D:\整理后" -mode dry-run
```

按"年/月"目录归类并复制（保留原目录不动）：

```powershell
photoManager.exe -src "D:\照片" -dst "D:\整理后" -template "{yyyy}/{MM}" -mode copy
```

只用文件修改时间做分类（适合手机截图等无 EXIF 的文件）：

```powershell
photoManager.exe -src "D:\截图" -dst "D:\整理后" -time-source "modify-time" -mode move
```

### 输出示例

```
D:\照片\IMG_001.jpg -> D:\整理后\2023-05-01\IMG_001.jpg
D:\照片\IMG_002.jpg -> D:\整理后\2023-05-02\IMG_002.jpg
警告: 无法获取时间信息，已跳过: D:\照片\corrupted.jpg
已处理 8/9，耗时 2.5s
```

## 目录模板变量

| 变量 | 含义 | 示例 |
|------|------|------|
| `{yyyy}` | 4 位年份 | 2023 |
| `{yy}` | 2 位年份 | 23 |
| `{MM}` | 2 位月份 | 05 |
| `{dd}` | 2 位日期 | 01 |
| `{HH}` | 2 位小时（24 小时制） | 14 |
| `{mm}` | 2 位分钟 | 30 |
| `{ss}` | 2 位秒 | 05 |

模板中的 `/` 表示子目录，例如 `{yyyy}/{MM}/{dd}` 会生成 `2023/05/01` 这样的三级目录。

## 时间来源说明

| 来源 | 说明 |
|------|------|
| `exif` | 照片 EXIF 中的拍摄时间（仅 jpg / tiff 类格式支持） |
| `create-time` | 文件创建时间 |
| `modify-time` | 文件修改时间 |

按给定顺序依次尝试，取第一个成功的时间。视频文件没有 EXIF，请确保列表中包含 `create-time` 或 `modify-time`。全部失败时该文件会被跳过并输出警告。

## 常见问题

**整理错了能恢复吗？**
`copy` 模式不改动原文件；`move` 模式会移动文件。强烈建议先用默认的 `dry-run` 模式确认分类结果，再执行实际操作。

**目标目录在源目录里面会不会重复处理？**
不会，程序会自动跳过源目录内的目标目录。

**为什么有些照片的时间不对？**
如果相机时区设置有误，EXIF 时间本身就不准，可以改用 `modify-time`。另外部分经过微信等软件转发的照片会丢失 EXIF 信息。

## 许可证

本项目基于 [MIT License](LICENSE) 开源。
