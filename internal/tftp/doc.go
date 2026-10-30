// Package tftp 实现 NetBootX 的 TFTP 服务器（RFC 1350 + 选项协商扩展）。
//
// 只支持读请求（RRQ），不支持写入；每个请求都会新建一个临时 UDP 端口回包，
// 主套接字只负责收 RRQ —— 这是 TFTP 协议的要求（客户端用 TID 校验对端端口）。
//
// 支持的扩展：
//   - RFC 2347 选项协商框架
//   - RFC 2348 blksize
//   - RFC 2349 tsize / timeout
//   - RFC 7440 windowsize
//
// 安全：路径穿越（..）一律拒绝；文件不存在返回 ERROR 1，越权访问返回 ERROR 2。
package tftp
