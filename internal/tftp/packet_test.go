package tftp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseRequest(t *testing.T) {
	// 典型的 PXE 客户端 RRQ：带 blksize / tsize / timeout / windowsize
	raw := []byte{0, 1}
	raw = append(raw, "ipxe.efi"...)
	raw = append(raw, 0)
	raw = append(raw, "octet"...)
	raw = append(raw, 0)
	raw = append(raw, "blksize"...)
	raw = append(raw, 0)
	raw = append(raw, "1468"...)
	raw = append(raw, 0)
	raw = append(raw, "tsize"...)
	raw = append(raw, 0)
	raw = append(raw, "0"...)
	raw = append(raw, 0)
	raw = append(raw, "timeout"...)
	raw = append(raw, 0)
	raw = append(raw, "5"...)
	raw = append(raw, 0)
	raw = append(raw, "windowsize"...)
	raw = append(raw, 0)
	raw = append(raw, "8"...)
	raw = append(raw, 0)

	req, err := parseRequest(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.Opcode != opRRQ {
		t.Errorf("操作码 = %d", req.Opcode)
	}
	if req.Filename != "ipxe.efi" {
		t.Errorf("文件名 = %q", req.Filename)
	}
	if req.Mode != "octet" {
		t.Errorf("模式 = %q", req.Mode)
	}
	if req.Options["blksize"] != "1468" || req.Options["tsize"] != "0" ||
		req.Options["timeout"] != "5" || req.Options["windowsize"] != "8" {
		t.Errorf("选项 = %v", req.Options)
	}
	if len(req.optionOrder) != 4 || req.optionOrder[0] != "blksize" {
		t.Errorf("选项顺序 = %v", req.optionOrder)
	}
}

func TestParseRequestErrors(t *testing.T) {
	if _, err := parseRequest([]byte{0, 1}); err == nil {
		t.Error("过短报文应报错")
	}
	if _, err := parseRequest([]byte{0, 3, 'a', 0, 'o', 0}); err == nil {
		t.Error("非 RRQ/WRQ 应报错")
	}
	// 只有文件名没有模式
	raw := append([]byte{0, 1}, "file"...)
	raw = append(raw, 0)
	if _, err := parseRequest(raw); err == nil {
		t.Error("缺少模式字段应报错")
	}
	// 空文件名
	raw = []byte{0, 1, 0}
	raw = append(raw, "octet"...)
	raw = append(raw, 0)
	if _, err := parseRequest(raw); err == nil {
		t.Error("空文件名应报错")
	}
	// 没有结尾 NUL 的文件名（客户端不规范）
	raw = append([]byte{0, 1}, "file"...)
	raw = append(raw, 0)
	raw = append(raw, "octet"...)
	req, err := parseRequest(raw)
	if err != nil {
		t.Fatalf("末尾缺 NUL 也应能解析: %v", err)
	}
	if req.Mode != "octet" {
		t.Errorf("模式 = %q", req.Mode)
	}
	// 模式大小写不敏感
	raw = []byte{0, 1}
	raw = append(raw, "f"...)
	raw = append(raw, 0)
	raw = append(raw, "OCTET"...)
	raw = append(raw, 0)
	req, err = parseRequest(raw)
	if err != nil || req.Mode != "octet" {
		t.Errorf("模式规范化失败: %v / %q", err, req.Mode)
	}
}

func TestMarshalAndParsePacket(t *testing.T) {
	// DATA
	b := marshalData(7, []byte("hello"))
	p, err := parsePacket(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.Opcode != opDATA || p.Block != 7 || !bytes.Equal(p.Data, []byte("hello")) {
		t.Errorf("DATA 解析 = %+v", p)
	}
	// ACK
	p, err = parsePacket(marshalACK(3))
	if err != nil || p.Opcode != opACK || p.Block != 3 {
		t.Errorf("ACK 解析 = %+v / %v", p, err)
	}
	// ERROR
	p, err = parsePacket(marshalError(ErrFileNotFound, ""))
	if err != nil || p.Opcode != opERROR || p.Code != ErrFileNotFound {
		t.Errorf("ERROR 解析 = %+v / %v", p, err)
	}
	if p.Msg != "File not found" {
		t.Errorf("ERROR 默认文案 = %q", p.Msg)
	}
	// OACK
	p, err = parsePacket(marshalOACK([][2]string{{"blksize", "1024"}, {"tsize", "9999"}}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Opcode != opOACK || len(p.Opts) != 2 || p.Opts[1][0] != "tsize" || p.Opts[1][1] != "9999" {
		t.Errorf("OACK 解析 = %+v", p)
	}
	// 校验操作码字段布局
	if binary.BigEndian.Uint16(marshalACK(1)[:2]) != opACK {
		t.Error("ACK 操作码编码错误")
	}
}

func TestParsePacketErrors(t *testing.T) {
	if _, err := parsePacket([]byte{0}); err == nil {
		t.Error("单字节报文应报错")
	}
	if _, err := parsePacket([]byte{0, 3, 0}); err == nil {
		t.Error("截断的 DATA 应报错")
	}
	if _, err := parsePacket([]byte{0, 4, 0}); err == nil {
		t.Error("截断的 ACK 应报错")
	}
	if _, err := parsePacket([]byte{0, 5, 0}); err == nil {
		t.Error("截断的 ERROR 应报错")
	}
	if _, err := parsePacket([]byte{0, 99}); err == nil {
		t.Error("未知操作码应报错")
	}
}

func TestErrText(t *testing.T) {
	if errText(ErrFileNotFound) != "File not found" {
		t.Error("错误 1 文案不对")
	}
	if errText(ErrAccessViolation) != "Access violation" {
		t.Error("错误 2 文案不对")
	}
	if errText(999) != "Undefined error" {
		t.Error("未知错误码文案不对")
	}
	// 未知错误码构造的报文应带默认文案
	p, err := parsePacket(marshalError(999, ""))
	if err != nil || p.Code != 999 || p.Msg != "Undefined error" {
		t.Errorf("未知错误码报文 = %+v / %v", p, err)
	}
}

func TestNegotiate(t *testing.T) {
	t.Run("没有选项", func(t *testing.T) {
		req := &request{Options: map[string]string{}}
		bs, to, ws, oack, err := negotiate(req, 1024, 1468)
		if err != nil {
			t.Fatal(err)
		}
		// RFC 1350：未协商时块大小固定 512
		if bs != 512 || to != 3 || ws != 1 || len(oack) != 0 {
			t.Errorf("结果 = %d/%d/%d/%v", bs, to, ws, oack)
		}
	})
	t.Run("全部协商", func(t *testing.T) {
		req := &request{
			Options:     map[string]string{"blksize": "1024", "tsize": "0", "timeout": "5", "windowsize": "4"},
			optionOrder: []string{"blksize", "tsize", "timeout", "windowsize"},
		}
		bs, to, ws, oack, err := negotiate(req, 123456, 1468)
		if err != nil {
			t.Fatal(err)
		}
		if bs != 1024 || to != 5 || ws != 4 {
			t.Errorf("协商结果 = %d/%d/%d", bs, to, ws)
		}
		if len(oack) != 4 {
			t.Fatalf("OACK 项数 = %d", len(oack))
		}
		want := map[string]string{"blksize": "1024", "tsize": "123456", "timeout": "5", "windowsize": "4"}
		for _, kv := range oack {
			if want[kv[0]] != kv[1] {
				t.Errorf("OACK %s = %q，期望 %q", kv[0], kv[1], want[kv[0]])
			}
		}
	})
	t.Run("blksize 取较小值", func(t *testing.T) {
		// 客户端要 8192，服务端上限 1468 → 取 1468
		req := &request{Options: map[string]string{"blksize": "8192"}, optionOrder: []string{"blksize"}}
		bs, _, _, oack, err := negotiate(req, 100, 1468)
		if err != nil {
			t.Fatal(err)
		}
		if bs != 1468 || oack[0][1] != "1468" {
			t.Errorf("blksize = %d / OACK %v", bs, oack)
		}
		// 客户端要 512，服务端上限 1468 → 取 512
		req = &request{Options: map[string]string{"blksize": "512"}, optionOrder: []string{"blksize"}}
		bs, _, _, _, _ = negotiate(req, 100, 1468)
		if bs != 512 {
			t.Errorf("blksize = %d，期望 512", bs)
		}
	})
	t.Run("非法值报错", func(t *testing.T) {
		for _, kv := range [][2]string{
			{"blksize", "4"}, {"blksize", "999999"}, {"blksize", "abc"},
			{"timeout", "0"}, {"timeout", "256"},
			{"windowsize", "0"},
		} {
			req := &request{Options: map[string]string{kv[0]: kv[1]}, optionOrder: []string{kv[0]}}
			if _, _, _, _, err := negotiate(req, 100, 1468); err == nil {
				t.Errorf("%s=%s 应报错", kv[0], kv[1])
			}
		}
	})
	t.Run("未知选项被忽略", func(t *testing.T) {
		req := &request{Options: map[string]string{"foobar": "1"}, optionOrder: []string{"foobar"}}
		_, _, _, oack, err := negotiate(req, 100, 1468)
		if err != nil {
			t.Fatal(err)
		}
		if len(oack) != 0 {
			t.Errorf("未知选项不应进 OACK: %v", oack)
		}
	})
}
