package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"photomanager/organizer"
)

func main() {
	src := flag.String("src", "", "源图片目录（必填）")
	dst := flag.String("dst", "", "分类后的目标目录（必填）")
	recursive := flag.Bool("recursive", true, "是否递归处理子目录")
	tmpl := flag.String("template", "{yyyy}-{MM}-{dd}", `目录结构模板，如 "{yyyy}-{MM}-{dd}/{HH}-{mm}-{ss}"`)
	mode := flag.String("mode", "dry-run", "文件处理方式: dry-run | copy | move")
	timeSource := flag.String("time-source", "exif,create-time,modify-time", "时间来源优先级，逗号分隔: exif, create-time, modify-time")
	conflict := flag.String("on-conflict", "rename", "目标存在同名文件时的策略: overwrite | skip | rename")
	workers := flag.Int("workers", 4, "并发线程数")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "错误: -src 和 -dst 为必填参数")
		flag.Usage()
		os.Exit(2)
	}

	var sources []organizer.TimeSource
	for _, s := range strings.Split(*timeSource, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			sources = append(sources, organizer.TimeSource(s))
		}
	}

	res, err := organizer.Organize(organizer.Options{
		SrcDir:      *src,
		DstDir:      *dst,
		DirTemplate: *tmpl,
		Recursive:   *recursive,
		Mode:        organizer.FileMode(*mode),
		TimeSources: sources,
		OnConflict:  organizer.ConflictStrategy(*conflict),
		Workers:     *workers,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}

	for _, f := range res.Files {
		fmt.Printf("%s -> %s\n", f.Src, f.Dst)
	}
	for _, w := range res.Warnings {
		fmt.Println("警告:", w)
	}
	fmt.Printf("已处理 %d/%d，耗时 %.1fs\n", res.Processed, res.Total, res.Duration.Seconds())
}
