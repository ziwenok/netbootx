package boot_test

import (
	"fmt"

	"github.com/wupengcheng/netbootx/internal/boot"
	"github.com/wupengcheng/netbootx/internal/model"
)

// ExampleScript 展示一台 x86_64 UEFI 客户机拿到的完整菜单脚本。
//
// 这个例子同时充当「生成结果长什么样」的活文档：godoc 里直接可见。
func ExampleScript() {
	client := &model.Client{
		MAC:      "52:54:00:12:34:56",
		Hostname: "pxe-test",
		Arch:     model.ArchUEFI64,
	}
	settings := model.DefaultSettings()
	settings.ServerIP = "192.168.123.10"
	settings.HTTPPort = 16000
	settings.BootMenuTimeout = 30
	settings.DefaultISO = "ubuntu2404"

	isos := []model.ISO{
		{
			ID: "ubuntu2404", Name: "ubuntu-24.04.2-desktop-amd64.iso",
			Size: 3113851289, Family: model.FamilyLinux,
			OSName: "Ubuntu 24.04.2 LTS", OSArch: model.ArchUEFI64, Bootable: true,
			Boot: model.BootSpec{
				Method:     "kernel",
				Kernel:     "/casper/vmlinuz",
				Initrd:     []string{"/casper/initrd"},
				KernelArgs: []string{"boot=casper", "iso-scan/filename={iso}", "quiet", "splash", "---"},
			},
		},
	}
	fmt.Println(boot.Script(client, isos, settings, "http://192.168.123.10:16000"))
	// Output:
	// #!ipxe
	// # NetBootX iPXE 启动脚本（由服务端自动生成，请勿手工编辑）
	// # 客户机: 52:54:00:12:34:56 / pxe-test / UEFI x86_64
	// # 服务端: http://192.168.123.10:16000
	// dhcp || goto netbootx_failed
	// set base http://192.168.123.10:16000
	// menu NetBootX 启动菜单 - 52:54:00:12:34:56
	// item --gap --  Linux
	// item iso_ubuntu2404 1) Ubuntu 24.04.2 LTS - ubuntu-24.04.2-desktop-amd64.iso (2.9GB)
	// choose --timeout 30000 --default iso_ubuntu2404 target || goto netbootx_cancelled
	// goto ${target}
	// :iso_ubuntu2404
	// kernel ${base}/iso/ubuntu2404/casper/vmlinuz boot=casper iso-scan/filename=http://192.168.123.10:16000/iso/ubuntu2404/ubuntu-24.04.2-desktop-amd64.iso quiet splash ---
	// initrd ${base}/iso/ubuntu2404/casper/initrd
	// boot || goto netbootx_failed
	//
	// :netbootx_failed
	// echo 引导失败，5 秒后返回启动菜单
	// sleep 5
	// dhcp || goto netbootx_cancelled
	// chain http://192.168.123.10:16000/boot/menu.ipxe?mac=52:54:00:12:34:56
	//
	// :netbootx_cancelled
	// echo 已取消启动，进入 iPXE 命令行
	// shell
}
