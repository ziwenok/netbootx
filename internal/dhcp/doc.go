// Package dhcp 实现 NetBootX 的 DHCP / proxyDHCP 服务器。
//
// 支持三种工作模式：
//
//	full     —— 本机是局域网里唯一的 DHCP 服务器，负责分配 IP 地址并下发引导参数；
//	proxy    —— 局域网已有 DHCP 服务器，本机只作为 proxyDHCP 回答 PXE 相关选项
//	            （不分配 IP，yiaddr 恒为 0，不回 option 1/3/6/51），但 DISCOVER 和
//	            REQUEST 都要响应；
//	external —— 完全不监听 DHCP 端口，只保留 BootFile 查询能力，供外部 DHCP 服务器
//	            通告 next-server / bootfile 时使用。
//
// 报文编解码全部手工实现（BOOTP 固定头 + magic cookie + options TLV），
// 只依赖 Go 标准库。
package dhcp
