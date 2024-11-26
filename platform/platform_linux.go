//go:build linux
// +build linux

package platform

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel dns_bpf bpf/dnsfilter.c -- -I. -O2 -g -Wall -Werror -D__TARGET_ARCH_x86

// DNS查询类型映射
var dnsTypeMap = map[uint16]string{
	1:  "A",
	2:  "NS",
	5:  "CNAME",
	28: "AAAA",
}

// 网络协议映射
var protocolMap = map[uint16]string{
	6:  "TCP",
	17: "UDP",
}

// DNS查询信息
type DNSInfo struct {
	QueryName string
	QueryType uint16
}

// 进程信息
type ProcessInfo struct {
	Name string
	Path string
}

// 输出格式定义
const outputFormat = "%-19s  %-6d  %-15s  %-40s  %-4s  %-6s  %s\n"

// 获取北京时间
func getBeijingTime() time.Time {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return time.Now().In(loc)
}

// 获取进程信息
func getProcessInfo(pid uint32) ProcessInfo {
	info := ProcessInfo{
		Name: "unknown",
		Path: "unknown",
	}

	// 获取进程名
	if commBytes, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		info.Name = strings.TrimSpace(string(commBytes))
	}

	// 获取进程路径
	if exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		info.Path = exePath
	} else if cmdlineBytes, err := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		args := strings.Split(string(cmdlineBytes), "\x00")
		if len(args) > 0 && args[0] != "" {
			info.Path = args[0]
		}
	}

	return info
}

// 解析DNS数据包
func parseDNSPacket(data []byte) *DNSInfo {
	if len(data) < 12 {
		return nil
	}

	// 检查是否是查询包（QR=0）
	flags := binary.BigEndian.Uint16(data[2:4])
	if (flags & 0x8000) != 0 {
		return nil
	}

	offset := 12
	var queryName []byte

	// 解析域名
	for offset < len(data) {
		length := int(data[offset])
		if length == 0 {
			break
		}
		if length > 63 || offset+1+length > len(data) {
			return nil
		}
		if len(queryName) > 0 {
			queryName = append(queryName, '.')
		}
		queryName = append(queryName, data[offset+1:offset+1+length]...)
		offset += length + 1
	}

	// 确保有足够的数据读取类型
	if offset+5 > len(data) {
		return nil
	}

	offset++
	queryType := binary.BigEndian.Uint16(data[offset:])

	if len(queryName) == 0 {
		return nil
	}

	return &DNSInfo{
		QueryName: string(queryName),
		QueryType: queryType,
	}
}

// DNSMonitorImpl 实现 DNS 监控功能
func DNSMonitorImpl() {
	// 检查 root 权限
	if os.Geteuid() != 0 {
		log.Fatal("必须以 root 权限运行此程序")
	}

	// 允许当前进程锁定内存以使用 eBPF 资源
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("移除内存锁限制失败: %v", err)
	}

	// 加载 eBPF 程序
	spec, err := loadDns_bpf()
	if err != nil {
		log.Fatalf("加载 eBPF spec 失败: %v", err)
	}

	var objs struct {
		TraceUdpSendmsg *ebpf.Program `ebpf:"trace_udp_sendmsg"`
		TraceTcpSendmsg *ebpf.Program `ebpf:"trace_tcp_sendmsg"`
		Events          *ebpf.Map     `ebpf:"events"`
	}

	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("加载 eBPF 对象失败: %v", err)
	}
	defer objs.Events.Close()
	defer objs.TraceUdpSendmsg.Close()
	defer objs.TraceTcpSendmsg.Close()

	// 附加 kprobes
	kprobes := []struct {
		name    string
		program *ebpf.Program
	}{
		{"udp_sendmsg", objs.TraceUdpSendmsg},
		{"tcp_sendmsg", objs.TraceTcpSendmsg},
	}

	var kps []link.Link
	for _, kp := range kprobes {
		probe, err := link.Kprobe(kp.name, kp.program, nil)
		if err != nil {
			log.Fatalf("附加 kprobe %s 失败: %v", kp.name, err)
		}
		kps = append(kps, probe)
	}
	defer func() {
		for _, kp := range kps {
			kp.Close()
		}
	}()

	// 创建 ring buffer 读取器
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("创建 ring buffer 读取器失败: %v", err)
	}
	defer rd.Close()

	fmt.Println("DNS 查询监控启动...")

	// 读取事件
	go func() {
		// 定义与 C 结构体完全匹配的事件结构
		var event struct {
			Timestamp uint64
			PID       uint32
			TGID      uint32
			UID       uint32
			GID       uint32
			Ifindex   uint32
			Comm      [64]byte
			Sport     uint16
			Dport     uint16
			Saddr     uint32
			Daddr     uint32
			Protocol  uint16
			PktLen    uint16
			PktData   [512]byte
		}

		for {
			record, err := rd.Read()
			if err != nil {
				if err == ringbuf.ErrClosed {
					fmt.Println("Ring buffer 已关闭")
					return
				}
				continue
			}

			if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
				continue
			}

			if event.PktLen > 0 {
				dnsInfo := parseDNSPacket(event.PktData[:event.PktLen])
				if dnsInfo != nil {
					procInfo := getProcessInfo(event.PID)

					// 获取协议名称
					proto := "UNK"
					if p, ok := protocolMap[event.Protocol]; ok {
						proto = p
					}

					// 获取查询类型
					qtype := fmt.Sprintf("TYPE%d", dnsInfo.QueryType)
					if t, ok := dnsTypeMap[dnsInfo.QueryType]; ok {
						qtype = t
					}

					fmt.Printf(outputFormat,
						getBeijingTime().Format("2006-01-02 15:04:05"),
						event.PID,
						procInfo.Name,
						procInfo.Path,
						proto,
						qtype,
						dnsInfo.QueryName,
					)
				}
			}
		}
	}()

	// 保持程序运行
	select {}
}
