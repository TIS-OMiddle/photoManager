//go:build windows

package organizer

import (
	"fmt"
	"io/fs"
	"syscall"
	"time"
)

// creationTime 从 FileInfo 中提取 Windows 文件创建时间。
func creationTime(info fs.FileInfo) (time.Time, error) {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, fmt.Errorf("无法获取文件创建时间")
	}
	return time.Unix(0, d.CreationTime.Nanoseconds()), nil
}
