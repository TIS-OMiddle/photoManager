package organizer

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// exifTime 从 JPEG 文件头部解析 EXIF 拍摄时间。
// 轻量容错解析器：只提取日期时间标签，对其他损坏/非标准标签保持容忍，
// 避免第三方库因个别设备写出的不规范标签（如 count=0 的 ExifIFDPointer）
// 导致整体解析失败而错误降级。
// 流式读取：只读 EXIF 所在的 APP1 段（JPEG 规范单段上限 64KB），
// 其他段通过 Seek 跳过，不读取图像数据。
func exifTime(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()

	var hdr [2]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || hdr[0] != 0xFF || hdr[1] != 0xD8 {
		return time.Time{}, fmt.Errorf("不是 JPEG 文件")
	}
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return time.Time{}, fmt.Errorf("未找到 EXIF 段")
		}
		if hdr[0] != 0xFF {
			return time.Time{}, fmt.Errorf("JPEG 标记损坏")
		}
		marker := hdr[1]
		if marker == 0xFF { // 填充字节，继续读
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // SOS / EOI：元数据区结束
			return time.Time{}, fmt.Errorf("未找到 EXIF 段")
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) { // 无长度字段的独立标记
			continue
		}
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return time.Time{}, fmt.Errorf("JPEG 段长度损坏")
		}
		segLen := int(binary.BigEndian.Uint16(hdr[:]))
		if segLen < 2 {
			return time.Time{}, fmt.Errorf("JPEG 段长度损坏")
		}
		payloadLen := segLen - 2
		if marker == 0xE1 { // APP1
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(f, payload); err != nil {
				return time.Time{}, fmt.Errorf("JPEG 段读取失败")
			}
			if len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
				return parseExifDateTime(payload[6:])
			}
			continue // XMP 等非 Exif 的 APP1，继续找
		}
		if _, err := f.Seek(int64(payloadLen), io.SeekCurrent); err != nil {
			return time.Time{}, err
		}
	}
}

// tiffParser 封装 TIFF 结构的安全读取（所有访问带边界检查）。
type tiffParser struct {
	data []byte
	bo   binary.ByteOrder
}

func (p *tiffParser) u16(off int) uint16 {
	if off < 0 || off+2 > len(p.data) {
		return 0
	}
	return p.bo.Uint16(p.data[off:])
}

func (p *tiffParser) u32(off int) uint32 {
	if off < 0 || off+4 > len(p.data) {
		return 0
	}
	return p.bo.Uint32(p.data[off:])
}

// parseExifDateTime 从 TIFF 结构中提取拍摄时间。
// 优先 Exif 子 IFD 的 DateTimeOriginal(0x9003) / DateTimeDigitized(0x9004)，
// 其次 IFD0 的同类标签及 DateTime(0x0132)。
func parseExifDateTime(tiff []byte) (time.Time, error) {
	if len(tiff) < 8 {
		return time.Time{}, fmt.Errorf("EXIF 数据过短")
	}
	p := &tiffParser{data: tiff}
	switch string(tiff[:2]) {
	case "II":
		p.bo = binary.LittleEndian
	case "MM":
		p.bo = binary.BigEndian
	default:
		return time.Time{}, fmt.Errorf("未知的 EXIF 字节序")
	}
	if p.u16(2) != 42 {
		return time.Time{}, fmt.Errorf("EXIF 头无效")
	}
	ifd0 := int(p.u32(4))

	// ExifIFDPointer(0x8769) 指向子 IFD。
	// 容错：不校验该指针标签的 count 字段（部分手机写成 count=0），
	// 直接按 LONG 读取其值作为子 IFD 偏移。
	if entry, ok := p.findTagEntry(ifd0, 0x8769); ok {
		sub := int(p.u32(entry + 8))
		if t, err := p.dateTimeInIFD(sub, 0x9003, 0x9004); err == nil {
			return t, nil
		}
	}
	// 部分设备把时间标签直接写在 IFD0
	if t, err := p.dateTimeInIFD(ifd0, 0x9003, 0x9004, 0x0132); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("EXIF 中无有效时间标签")
}

// findTagEntry 在指定 IFD 中查找标签，返回条目起始偏移。
func (p *tiffParser) findTagEntry(ifdOff int, tag uint16) (int, bool) {
	if ifdOff < 0 || ifdOff+2 > len(p.data) {
		return 0, false
	}
	count := int(p.u16(ifdOff))
	if count > 512 { // 防止异常 count 导致越界扫描
		return 0, false
	}
	base := ifdOff + 2
	for i := 0; i < count; i++ {
		entry := base + i*12
		if entry+12 > len(p.data) {
			return 0, false
		}
		if p.u16(entry) == tag {
			return entry, true
		}
	}
	return 0, false
}

// dateTimeInIFD 在指定 IFD 中按优先级尝试多个时间标签。
func (p *tiffParser) dateTimeInIFD(ifdOff int, tags ...uint16) (time.Time, error) {
	for _, tag := range tags {
		entry, ok := p.findTagEntry(ifdOff, tag)
		if !ok {
			continue
		}
		s, ok := p.asciiValue(entry)
		if !ok {
			continue
		}
		if t, err := parseExifTime(s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无有效时间标签")
}

// asciiValue 读取 ASCII 类型（type=2）标签的字符串值。
// count<=4 时数据内联在条目内，否则为指向数据区的偏移。
func (p *tiffParser) asciiValue(entry int) (string, bool) {
	if p.u16(entry+2) != 2 {
		return "", false
	}
	cnt := int(p.u32(entry + 4))
	if cnt <= 0 || cnt > 64 {
		return "", false
	}
	var raw []byte
	if cnt <= 4 {
		raw = p.data[entry+8 : entry+8+cnt] // entry+12 已在 findTagEntry 中校验
	} else {
		off := int(p.u32(entry + 8))
		if off < 0 || off+cnt > len(p.data) {
			return "", false
		}
		raw = p.data[off : off+cnt]
	}
	return strings.TrimRight(string(raw), "\x00 "), true
}

// parseExifTime 解析 "2006:01:02 15:04:05" 格式，无时区信息按本地时间处理。
func parseExifTime(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local)
	if err != nil || t.IsZero() {
		return time.Time{}, fmt.Errorf("时间格式无效: %q", s)
	}
	return t, nil
}
