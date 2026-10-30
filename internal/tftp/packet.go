package tftp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// TFTP 操作码。
const (
	opRRQ   uint16 = 1
	opWRQ   uint16 = 2
	opDATA  uint16 = 3
	opACK   uint16 = 4
	opERROR uint16 = 5
	opOACK  uint16 = 6
)

// TFTP 错误码（RFC 1350）。
const (
	ErrNotDefined        uint16 = 0 // 未定义
	ErrFileNotFound      uint16 = 1 // 文件不存在
	ErrAccessViolation   uint16 = 2 // 访问越权 / 路径非法
	ErrDiskFull          uint16 = 3 // 磁盘满
	ErrIllegalOp         uint16 = 4 // 非法操作
	ErrUnknownTID        uint16 = 5 // 未知传输 ID
	ErrFileExists        uint16 = 6 // 文件已存在
	ErrNoSuchUser        uint16 = 7 // 用户不存在
	ErrOptionNegotiation uint16 = 8 // 选项协商失败
)

// errText 返回错误码的标准英文描述（协议字段，保持英文）。
func errText(code uint16) string {
	switch code {
	case ErrFileNotFound:
		return "File not found"
	case ErrAccessViolation:
		return "Access violation"
	case ErrDiskFull:
		return "Disk full or allocation exceeded"
	case ErrIllegalOp:
		return "Illegal TFTP operation"
	case ErrUnknownTID:
		return "Unknown transfer ID"
	case ErrFileExists:
		return "File already exists"
	case ErrNoSuchUser:
		return "No such user"
	case ErrOptionNegotiation:
		return "Option negotiation failed"
	}
	return "Undefined error"
}

// 默认参数。
const (
	defaultBlockSize = 1468
	maxBlockSize     = 65464 // RFC 7440：UDP 数据报上限 65467 - 4 字节头
	minBlockSize     = 8
	defaultTimeout   = 3
	maxTimeout       = 255
	maxWindowsize    = 65535
)

// request 是一个解析后的 RRQ / WRQ 请求。
type request struct {
	Opcode   uint16
	Filename string
	Mode     string
	Options  map[string]string
	// optionOrder 保留客户端请求选项的顺序，便于按原序回 OACK。
	optionOrder []string
}

// parseRequest 解析 RRQ / WRQ 报文。
func parseRequest(b []byte) (*request, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("tftp: 报文长度 %d 字节，至少需要 4 字节", len(b))
	}
	op := binary.BigEndian.Uint16(b[:2])
	if op != opRRQ && op != opWRQ {
		return nil, fmt.Errorf("tftp: 不是 RRQ/WRQ 报文（操作码 %d）", op)
	}
	rest := b[2:]
	fields := make([]string, 0, 8)
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			fields = append(fields, string(rest))
			rest = nil
			break
		}
		fields = append(fields, string(rest[:i]))
		rest = rest[i+1:]
	}
	if len(fields) < 2 {
		return nil, fmt.Errorf("tftp: RRQ 缺少文件名或模式字段")
	}
	r := &request{
		Opcode:   op,
		Filename: fields[0],
		Mode:     strings.ToLower(fields[1]),
		Options:  make(map[string]string),
	}
	if r.Filename == "" {
		return nil, fmt.Errorf("tftp: 文件名为空")
	}
	for i := 2; i+1 < len(fields); i += 2 {
		name := strings.ToLower(strings.TrimSpace(fields[i]))
		if name == "" {
			continue
		}
		if _, dup := r.Options[name]; !dup {
			r.optionOrder = append(r.optionOrder, name)
		}
		r.Options[name] = fields[i+1]
	}
	return r, nil
}

// marshalData 构造 DATA 报文。
func marshalData(block uint16, data []byte) []byte {
	b := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(b[0:2], opDATA)
	binary.BigEndian.PutUint16(b[2:4], block)
	copy(b[4:], data)
	return b
}

// marshalACK 构造 ACK 报文。
func marshalACK(block uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], opACK)
	binary.BigEndian.PutUint16(b[2:4], block)
	return b
}

// marshalError 构造 ERROR 报文。
func marshalError(code uint16, msg string) []byte {
	if msg == "" {
		msg = errText(code)
	}
	b := make([]byte, 4, 5+len(msg))
	binary.BigEndian.PutUint16(b[0:2], opERROR)
	binary.BigEndian.PutUint16(b[2:4], code)
	b = append(b, msg...)
	b = append(b, 0)
	return b
}

// marshalOACK 构造 OACK 报文。
func marshalOACK(opts [][2]string) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b[0:2], opOACK)
	for _, kv := range opts {
		b = append(b, kv[0]...)
		b = append(b, 0)
		b = append(b, kv[1]...)
		b = append(b, 0)
	}
	return b
}

// parsedPacket 是一个解析后的通用 TFTP 报文。
type parsedPacket struct {
	Opcode uint16
	Block  uint16
	Data   []byte
	Code   uint16
	Msg    string
	Opts   [][2]string
}

// parsePacket 解析任意 TFTP 报文。
func parsePacket(b []byte) (*parsedPacket, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("tftp: 报文长度 %d 字节，过短", len(b))
	}
	p := &parsedPacket{Opcode: binary.BigEndian.Uint16(b[:2])}
	switch p.Opcode {
	case opDATA:
		if len(b) < 4 {
			return nil, fmt.Errorf("tftp: DATA 报文长度 %d 字节，至少需要 4 字节", len(b))
		}
		p.Block = binary.BigEndian.Uint16(b[2:4])
		p.Data = b[4:]
	case opACK:
		if len(b) < 4 {
			return nil, fmt.Errorf("tftp: ACK 报文长度 %d 字节，至少需要 4 字节", len(b))
		}
		p.Block = binary.BigEndian.Uint16(b[2:4])
	case opERROR:
		if len(b) < 4 {
			return nil, fmt.Errorf("tftp: ERROR 报文长度 %d 字节，至少需要 4 字节", len(b))
		}
		p.Code = binary.BigEndian.Uint16(b[2:4])
		p.Msg = string(bytes.TrimRight(b[4:], "\x00"))
	case opOACK:
		rest := b[2:]
		for len(rest) > 0 {
			i := bytes.IndexByte(rest, 0)
			if i < 0 {
				break
			}
			name := string(rest[:i])
			rest = rest[i+1:]
			j := bytes.IndexByte(rest, 0)
			if j < 0 {
				break
			}
			p.Opts = append(p.Opts, [2]string{name, string(rest[:j])})
			rest = rest[j+1:]
		}
	case opRRQ, opWRQ:
		// 请求报文由 parseRequest 处理，这里只填操作码。
	default:
		return nil, fmt.Errorf("tftp: 未知操作码 %d", p.Opcode)
	}
	return p, nil
}

// negotiate 根据客户端请求的选项计算实际使用的参数，返回要回给客户端的 OACK 键值对。
//
// 返回的 opts 为空表示客户端没有请求任何可协商选项，直接进 DATA 传输。
func negotiate(req *request, fileSize int64, cfgBlockSize int) (blksize int, timeout int, windowsize int, oack [][2]string, err error) {
	// RFC 1350 规定未协商时块大小固定为 512 字节；
	// cfgBlockSize 只作为协商时的上限，不改变默认值。
	blksize = 512
	maxBlk := cfgBlockSize
	if maxBlk <= 0 || maxBlk > maxBlockSize {
		maxBlk = maxBlockSize
	}
	if maxBlk < minBlockSize {
		maxBlk = minBlockSize
	}
	timeout = defaultTimeout
	windowsize = 1

	for _, name := range req.optionOrder {
		val := strings.TrimSpace(req.Options[name])
		switch name {
		case "blksize":
			n, e := strconv.Atoi(val)
			if e != nil || n < minBlockSize || n > maxBlockSize {
				return 0, 0, 0, nil, fmt.Errorf("blksize 取值 %q 非法（允许 %d-%d）", val, minBlockSize, maxBlockSize)
			}
			// 取客户端请求值与服务端上限的较小者。
			blksize = n
			if blksize > maxBlk {
				blksize = maxBlk
			}
			oack = append(oack, [2]string{"blksize", strconv.Itoa(blksize)})
		case "tsize":
			oack = append(oack, [2]string{"tsize", strconv.FormatInt(fileSize, 10)})
		case "timeout":
			n, e := strconv.Atoi(val)
			if e != nil || n < 1 || n > maxTimeout {
				return 0, 0, 0, nil, fmt.Errorf("timeout 取值 %q 非法（允许 1-%d 秒）", val, maxTimeout)
			}
			timeout = n
			oack = append(oack, [2]string{"timeout", strconv.Itoa(timeout)})
		case "windowsize":
			n, e := strconv.Atoi(val)
			if e != nil || n < 1 || n > maxWindowsize {
				return 0, 0, 0, nil, fmt.Errorf("windowsize 取值 %q 非法（允许 1-%d）", val, maxWindowsize)
			}
			windowsize = n
			oack = append(oack, [2]string{"windowsize", strconv.Itoa(windowsize)})
		default:
			// 不认识的选项直接忽略（RFC 2347 要求服务端忽略未知选项）。
		}
	}
	return blksize, timeout, windowsize, oack, nil
}
