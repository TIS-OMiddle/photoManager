//go:build !windows

package organizer

import (
	"fmt"
	"io/fs"
	"time"
)

// creationTime 非 Windows 平台暂不支持获取创建时间，
// 返回错误使流程回退到下一个时间来源。
func creationTime(info fs.FileInfo) (time.Time, error) {
	return time.Time{}, fmt.Errorf("当前平台不支持获取文件创建时间")
}
