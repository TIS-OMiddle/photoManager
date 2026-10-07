// Package organizer 提供照片整理的核心逻辑，独立于 CLI，
// 接收参数结构体并返回结果结构体，可直接被后续 UI 调用。
package organizer

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileMode 文件处理方式
type FileMode string

const (
	ModeDryRun FileMode = "dry-run" // 仅模拟，不实际移动或删除文件
	ModeCopy   FileMode = "copy"    // 复制文件到目标目录
	ModeMove   FileMode = "move"    // 移动文件到目标目录
)

// TimeSource 照片时间信息来源
type TimeSource string

const (
	TimeSourceEXIF       TimeSource = "exif"        // 从 EXIF 拍摄时间获取
	TimeSourceCreateTime TimeSource = "create-time" // 从文件创建时间获取
	TimeSourceModifyTime TimeSource = "modify-time" // 从文件修改时间获取
)

// ConflictStrategy 目标路径存在同名文件时的处理策略
type ConflictStrategy string

const (
	ConflictOverwrite ConflictStrategy = "overwrite" // 覆盖已存在的文件
	ConflictSkip      ConflictStrategy = "skip"      // 跳过
	ConflictRename    ConflictStrategy = "rename"    // 重命名为 {filename}_{timestamp}.{ext}
)

// Options 整理参数
type Options struct {
	SrcDir      string           // 源图片目录
	DstDir      string           // 分类后的目标目录
	DirTemplate string           // 目录结构模板，如 "{yyyy}-{MM}-{dd}/{HH}-{mm}-{ss}"
	Recursive   bool             // 是否递归处理子目录
	Mode        FileMode         // 文件处理方式
	TimeSources []TimeSource     // 时间来源优先级，按顺序尝试
	OnConflict  ConflictStrategy // 同名文件处理策略
	Workers     int              // 并发线程数，默认 4
}

// FileStatus 单个文件的处理状态
type FileStatus string

const (
	StatusPlanned FileStatus = "planned" // dry-run 拟处理
	StatusCopied  FileStatus = "copied"
	StatusMoved   FileStatus = "moved"
)

// FileResult 单个成功处理的文件
type FileResult struct {
	Src    string
	Dst    string
	Status FileStatus
}

// Result 整理结果
type Result struct {
	Total     int           // 发现的图片文件总数
	Processed int           // 成功处理（或拟处理）数量
	Files     []FileResult  // 成功处理的文件列表
	Warnings  []string      // 警告信息（如跳过、失败的文件）
	Duration  time.Duration // 总耗时
}

// 支持的图片及视频扩展名
var supportedExts = map[string]bool{
	// 图片
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".bmp": true, ".webp": true, ".tif": true, ".tiff": true,
	".heic": true, ".heif": true, ".dng": true,
	".cr2": true, ".nef": true, ".arw": true, ".orf": true, ".rw2": true,
	// 视频（无 EXIF，时间取文件创建/修改时间）
	".mp4": true, ".mov": true, ".m4v": true, ".avi": true,
	".mkv": true, ".wmv": true, ".flv": true, ".webm": true,
	".mpg": true, ".mpeg": true, ".3gp": true, ".mts": true, ".m2ts": true,
}

// 可能包含 EXIF 信息的扩展名（避免对 png 等格式做无效的 EXIF 读取）
var exifExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".tif": true, ".tiff": true,
}

// Organize 执行照片整理，为核心逻辑入口。
func Organize(opts Options) (*Result, error) {
	start := time.Now()
	if err := opts.normalizeAndValidate(); err != nil {
		return nil, err
	}

	absSrc, err := filepath.Abs(opts.SrcDir)
	if err != nil {
		return nil, fmt.Errorf("解析源路径失败: %w", err)
	}
	absDst, err := filepath.Abs(opts.DstDir)
	if err != nil {
		return nil, fmt.Errorf("解析目标路径失败: %w", err)
	}

	jobs, err := collectFiles(absSrc, absDst, opts.Recursive)
	if err != nil {
		return nil, fmt.Errorf("扫描源目录失败: %w", err)
	}

	res := &Result{Total: len(jobs)}
	if len(jobs) == 0 {
		res.Duration = time.Since(start)
		return res, nil
	}

	alloc := newDstAllocator(opts.OnConflict)

	jobCh := make(chan fileJob)
	outCh := make(chan outcome)

	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256*1024) // 每个 worker 复用复制缓冲区
			for j := range jobCh {
				outCh <- processFile(j, absDst, &opts, alloc, buf)
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
	}()
	go func() {
		wg.Wait()
		close(outCh)
	}()

	for o := range outCh {
		if o.warn != "" {
			res.Warnings = append(res.Warnings, o.warn)
			continue
		}
		res.Files = append(res.Files, *o.fr)
		res.Processed++
	}

	// 排序保证输出稳定
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Src < res.Files[j].Src })
	sort.Strings(res.Warnings)
	res.Duration = time.Since(start)
	return res, nil
}

func (o *Options) normalizeAndValidate() error {
	if o.SrcDir == "" || o.DstDir == "" {
		return fmt.Errorf("源路径和目标路径不能为空")
	}
	fi, err := os.Stat(o.SrcDir)
	if err != nil {
		return fmt.Errorf("源路径不可访问: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("源路径不是目录: %s", o.SrcDir)
	}
	if o.DirTemplate == "" {
		o.DirTemplate = "{yyyy}-{MM}-{dd}"
	}
	switch o.Mode {
	case ModeDryRun, ModeCopy, ModeMove:
	default:
		return fmt.Errorf("未知的文件处理方式: %q（可选 dry-run/copy/move）", o.Mode)
	}
	switch o.OnConflict {
	case ConflictOverwrite, ConflictSkip, ConflictRename:
	default:
		return fmt.Errorf("未知的同名文件处理策略: %q（可选 overwrite/skip/rename）", o.OnConflict)
	}
	if len(o.TimeSources) == 0 {
		o.TimeSources = []TimeSource{TimeSourceEXIF, TimeSourceCreateTime, TimeSourceModifyTime}
	}
	for _, s := range o.TimeSources {
		switch s {
		case TimeSourceEXIF, TimeSourceCreateTime, TimeSourceModifyTime:
		default:
			return fmt.Errorf("未知的时间来源: %q（可选 exif/create-time/modify-time）", s)
		}
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	return nil
}

// fileJob 携带遍历时已获取的 FileInfo，避免后续重复 stat。
type fileJob struct {
	path string
	info fs.FileInfo
}

// collectFiles 遍历源目录收集图片文件。
// 若目标目录位于源目录内，遍历时跳过目标目录，避免重复处理已整理的文件。
func collectFiles(absSrc, absDst string, recursive bool) ([]fileJob, error) {
	var files []fileJob
	err := filepath.WalkDir(absSrc, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != absSrc {
				if !recursive {
					return filepath.SkipDir
				}
				if samePath(path, absDst) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !supportedExts[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 信息获取失败的文件由后续处理阶段报告
		}
		files = append(files, fileJob{path: path, info: info})
		return nil
	})
	return files, err
}

// outcome 单个文件的处理结果：成功时 fr 非空，失败/跳过时 warn 非空。
type outcome struct {
	fr   *FileResult
	warn string
}

// processFile 处理单个文件：获取时间 -> 计算目标路径 -> 冲突处理 -> 复制/移动。
func processFile(j fileJob, absDst string, opts *Options, alloc *dstAllocator, buf []byte) outcome {
	fail := func(format string, args ...any) outcome {
		return outcome{warn: fmt.Sprintf(format, args...)}
	}
	ok := func(status FileStatus, dst string) outcome {
		return outcome{fr: &FileResult{Src: j.path, Dst: dst, Status: status}}
	}

	t, err := detectTime(j, opts.TimeSources)
	if err != nil {
		return fail("无法获取时间信息，已跳过: %s", j.path)
	}

	sub := renderTemplate(opts.DirTemplate, t)
	dst := filepath.Join(absDst, filepath.FromSlash(sub), filepath.Base(j.path))

	if samePath(j.path, dst) {
		return fail("源路径与目标路径相同，已跳过: %s", j.path)
	}

	finalDst, proceed := alloc.Allocate(dst)
	if !proceed {
		return fail("目标文件已存在，已跳过: %s -> %s", j.path, dst)
	}

	switch opts.Mode {
	case ModeDryRun:
		return ok(StatusPlanned, finalDst)
	case ModeCopy, ModeMove:
		if err := alloc.ensureDir(filepath.Dir(finalDst)); err != nil {
			return fail("创建目录失败 %s: %v", filepath.Dir(finalDst), err)
		}
		if opts.Mode == ModeCopy {
			if err := copyFile(j.path, finalDst, j.info, buf); err != nil {
				return fail("复制失败 %s: %v", j.path, err)
			}
			return ok(StatusCopied, finalDst)
		}
		if err := moveFile(j.path, finalDst, j.info, buf); err != nil {
			return fail("移动失败 %s: %v", j.path, err)
		}
		return ok(StatusMoved, finalDst)
	}
	return fail("未知的处理方式: %s", opts.Mode)
}

// detectTime 按用户给定顺序尝试各时间来源，全部失败则返回错误。
func detectTime(j fileJob, sources []TimeSource) (time.Time, error) {
	var lastErr error
	for _, s := range sources {
		var t time.Time
		var err error
		switch s {
		case TimeSourceEXIF:
			// 仅对可能含 EXIF 的格式读取文件头，减少无效 I/O
			if exifExts[strings.ToLower(filepath.Ext(j.path))] {
				t, err = exifTime(j.path)
			} else {
				err = fmt.Errorf("该格式不含 EXIF")
			}
		case TimeSourceCreateTime:
			t, err = creationTime(j.info)
		case TimeSourceModifyTime:
			t = j.info.ModTime()
		}
		if err == nil && !t.IsZero() {
			return t, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无可用时间来源")
	}
	return time.Time{}, lastErr
}

var templateVarRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// renderTemplate 将模板中的 {yyyy} {yy} {MM} {dd} {HH} {mm} {ss} 替换为实际时间，
// 未知变量原样保留。
func renderTemplate(tmpl string, t time.Time) string {
	return templateVarRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		switch m[1 : len(m)-1] {
		case "yyyy":
			return fmt.Sprintf("%04d", t.Year())
		case "yy":
			return fmt.Sprintf("%02d", t.Year()%100)
		case "MM":
			return fmt.Sprintf("%02d", int(t.Month()))
		case "dd":
			return fmt.Sprintf("%02d", t.Day())
		case "HH":
			return fmt.Sprintf("%02d", t.Hour())
		case "mm":
			return fmt.Sprintf("%02d", t.Minute())
		case "ss":
			return fmt.Sprintf("%02d", t.Second())
		default:
			return m
		}
	})
}

// dstAllocator 负责目标路径冲突处理与目录创建缓存，并发安全。
type dstAllocator struct {
	mu       sync.Mutex
	claimed  map[string]struct{}            // 本次运行已占用的目标路径（含磁盘上已存在）
	dirLists map[string]map[string]struct{} // 目标目录现有文件清单缓存（目录 -> 文件名集合）
	dirs     sync.Map                       // 已创建的目录缓存（MkdirAll 幂等，无需锁）
	strategy ConflictStrategy
}

func newDstAllocator(s ConflictStrategy) *dstAllocator {
	return &dstAllocator{
		claimed:  make(map[string]struct{}),
		dirLists: make(map[string]map[string]struct{}),
		strategy: s,
	}
}

// Allocate 根据冲突策略返回最终可用的目标路径；proceed=false 表示跳过。
func (a *dstAllocator) Allocate(dst string) (final string, proceed bool) {
	// overwrite 策略无需关心磁盘上是否已存在同名文件，省去一次 Stat
	if a.strategy == ConflictOverwrite {
		a.mu.Lock()
		a.claimed[dst] = struct{}{}
		a.mu.Unlock()
		return dst, true
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.takenLocked(dst) {
		a.claimed[dst] = struct{}{}
		return dst, true
	}
	switch a.strategy {
	case ConflictSkip:
		return "", false
	case ConflictRename:
		ext := filepath.Ext(dst)
		base := strings.TrimSuffix(dst, ext)
		ts := time.Now().Unix()
		cand := fmt.Sprintf("%s_%d%s", base, ts, ext)
		for i := 1; a.takenLocked(cand); i++ {
			cand = fmt.Sprintf("%s_%d_%d%s", base, ts, i, ext)
		}
		a.claimed[cand] = struct{}{}
		return cand, true
	}
	return "", false
}

// takenLocked 判断路径是否已被本次运行占用或已存在于磁盘。
// 磁盘存在性通过"目录清单缓存"判断：每个目录只 ReadDir 一次，
// 避免对海量目标文件逐个 Stat（在慢速磁盘上开销显著）。
func (a *dstAllocator) takenLocked(p string) bool {
	if _, ok := a.claimed[p]; ok {
		return true
	}
	dir := filepath.Dir(p)
	names, ok := a.dirLists[dir]
	if !ok {
		names = make(map[string]struct{})
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				names[cacheKey(e.Name())] = struct{}{}
			}
		}
		a.dirLists[dir] = names
	}
	_, ok = names[cacheKey(filepath.Base(p))]
	return ok
}

// cacheKey 统一文件名比较形式（Windows 文件系统不区分大小写）。
func cacheKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

// ensureDir 创建目录，带缓存避免重复系统调用。
// MkdirAll 本身幂等且并发安全，缓存用 sync.Map 即可，不占互斥锁。
func (a *dstAllocator) ensureDir(dir string) error {
	if _, ok := a.dirs.Load(dir); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	a.dirs.Store(dir, struct{}{})
	return nil
}

// copyFile 复制文件并保留修改时间。
func copyFile(src, dst string, info fs.FileInfo, buf []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(out, in, buf)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	// 保留原修改时间
	_ = os.Chtimes(dst, time.Now(), info.ModTime())
	return nil
}

// moveFile 移动文件；跨盘时回退为复制+删除。
func moveFile(src, dst string, info fs.FileInfo, buf []byte) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst, info, buf); err != nil {
		return err
	}
	return os.Remove(src)
}

// samePath 判断两个路径是否指向同一文件（Windows 不区分大小写）。
func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
