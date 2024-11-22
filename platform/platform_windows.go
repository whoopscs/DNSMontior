//go:build windows
// +build windows

package platform

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/0xrawsec/golang-etw/etw"
)

const (
	// Microsoft-Windows-DNS-Client Provider GUID
	dnsProviderGUID = "{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}"

	// 进程访问权限
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	PROCESS_QUERY_INFORMATION         = 0x0400
)

// Windows API 函数声明
var (
	modkernel32                       = syscall.NewLazyDLL("kernel32.dll")
	modpsapi                          = syscall.NewLazyDLL("psapi.dll")
	procQueryFullProcessImageNameW    = modkernel32.NewProc("QueryFullProcessImageNameW")
	procGetProcessImageFileNameW      = modpsapi.NewProc("GetProcessImageFileNameW")
)

// DNS查询状态码映射
var statusMap = map[int]string{
	0:    "succeeded",
	123:  "query name error",
	1460: "query timeout",
	9003: "DNS name does not exist",
	9501: "query record not found",
}

// DNS查询类型映射
var queryTypes = map[int]string{
	1:  "A",
	2:  "NS",
	5:  "CNAME",
	6:  "SOA",
	12: "PTR",
	15: "MX",
	16: "TXT",
	28: "AAAA",
	33: "SRV",
}

type Config struct {
	// 事件ID白名单，为空则不过滤
	EventIDWhitelist []uint16
	// 域名黑名单，为空则不过滤
	DomainBlacklist []string
}

// 配置事件白名单ID和域名黑名单
var config = Config{
	// DNS查询事件ID：3008【已完成的查询】，3011【DNS服务器响应】，3018【缓存查询响应】，3020【索引查询响应】
	EventIDWhitelist: []uint16{3008, 3018, 3020},
	DomainBlacklist: []string{
		"localhost",
	},
}

// 检查事件ID是否在白名单中
func isEventIDAllowed(eventID uint16, whitelist []uint16) bool {
	if len(whitelist) == 0 {
		return true
	}
	for _, id := range whitelist {
		if eventID == id {
			return true
		}
	}
	return false
}

// 检查域名是否在黑名单中
func isDomainBlocked(domain string, blacklist []string) bool {
	if len(blacklist) == 0 {
		return false
	}
	domain = strings.ToLower(domain)
	for _, blocked := range blacklist {
		if strings.Contains(domain, strings.ToLower(blocked)) {
			return true
		}
	}
	return false
}

// 获取进程路径
func getProcessPath(processHandle syscall.Handle) string {
	// 创建缓冲区来存储路径信息
	buffer := make([]uint16, syscall.MAX_PATH)
	size := uint32(len(buffer))

	// 尝试调用 QueryFullProcessImageNameW
	ret, _, err := procQueryFullProcessImageNameW.Call(
		uintptr(processHandle),
		uintptr(0),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret != 0 {
		return syscall.UTF16ToString(buffer[:size])
	}

	// 如果失败，尝试调用 GetProcessImageFileNameW
	ret, _, err = procGetProcessImageFileNameW.Call(
		uintptr(processHandle),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(size),
	)
	if ret != 0 {
		return syscall.UTF16ToString(buffer[:size])
	}

	log.Printf("无法获取进程路径, 错误: %v", err)
	// 如果都失败，返回空字符串
	return ""
}

// 获取进程名
func getProcessName(processPath string) string {
	for i := len(processPath) - 1; i >= 0; i-- {
		if processPath[i] == '\\' {
			return processPath[i+1:]
		}
	}
	return processPath
}

// 获取进程信息
func getProcessInfo(pid uint32) (name, path string) {
	// 使用 PROCESS_QUERY_LIMITED_INFORMATION 权限
	handle, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		log.Printf("无法打开进程 %d: %v", pid, err)
		// 返回默认值或空值
		return fmt.Sprintf("PID: %d", pid), ""
	}
	defer syscall.CloseHandle(handle)

	// 获取路径信息
	path = getProcessPath(handle)
	if path == "" {
		return fmt.Sprintf("PID: %d", pid), ""
	}

	// 获取进程名称
	name = getProcessName(path)
	return name, path
}

// 获取DNS查询类型的字符串表示
func getDNSQueryType(queryType interface{}) string {
	switch t := queryType.(type) {
	case float64:
		if name, ok := queryTypes[int(t)]; ok {
			return name
		}
		return fmt.Sprintf("UNKNOWN(%d)", int(t))
	case int:
		if name, ok := queryTypes[t]; ok {
			return name
		}
		return fmt.Sprintf("UNKNOWN(%d)", t)
	case string:
		tInt, err := strconv.Atoi(t)
		if err != nil {
			return fmt.Sprintf("UNKNOWN(%s)", t)
		}
		if name, ok := queryTypes[tInt]; ok {
			return name
		}
		return fmt.Sprintf("UNKNOWN(%d)", tInt)
	default:
		return fmt.Sprintf("%v", queryType)
	}
}

// 获取DNS查询状态的字符串表示
func getDNSStatus(status interface{}) string {
	switch s := status.(type) {
	case float64:
		if statusStr, ok := statusMap[int(s)]; ok {
			return statusStr
		}
		return fmt.Sprintf("ERROR(%d)", int(s))
	case int:
		if statusStr, ok := statusMap[s]; ok {
			return statusStr
		}
		return fmt.Sprintf("ERROR(%d)", s)
	case string:
		sInt, err := strconv.Atoi(s)
		if err != nil {
			return ""
		}
		if statusStr, ok := statusMap[sInt]; ok {
			return statusStr
		}
		return fmt.Sprintf("ERROR(%d)", sInt)
	default:
		return fmt.Sprintf("%v", status)
	}
}

// 格式化为北京时间
func formatTimeAsBeijing(t time.Time, format string) string {
	// 设置时区为北京
	const beijingTimeZone = "Asia/Shanghai"
	loc, err := time.LoadLocation(beijingTimeZone)
	if err != nil {
		// 如果加载时区失败，使用 UTC 作为默认值
		fmt.Println("Failed to load Beijing time zone, using UTC as default:", err)
		return t.Format(format)
	}

	// 将时间转换为北京时间
	beijingTime := t.In(loc)
	return beijingTime.Format(format)
}
func DNSMonitorImpl() {
	// 创建实时会话
	session := etw.NewRealTimeSession("DNSMonitor")
	defer session.Stop()

	// 解析并启用 DNS Provider
	dnsProvider := etw.MustParseProvider(dnsProviderGUID)
	if err := session.EnableProvider(dnsProvider); err != nil {
		log.Fatalf("启用 Provider 失败: %v", err)
	}
	fmt.Println("DNS Provider 启用成功")

	// 创建消费者并启动异步监听
	ctx, cancel := context.WithCancel(context.Background())
	consumer := etw.NewRealTimeConsumer(ctx)
	defer cancel()
	defer consumer.Stop()

	// 将消费者与会话关联
	consumer.FromSessions(session)

	// 处理事件
	go func() {
		for evt := range consumer.Events {
			handleProcessEvent(evt)
		}
	}()

	// 启动消费者
	errChan := make(chan error, 1)
	go func() {
		if err := consumer.Start(); err != nil {
			errChan <- fmt.Errorf("DNS事件消费者启动失败: %v", err)
		}
	}()

	<-ctx.Done()
}

func handleProcessEvent(evt *etw.Event) {
	if evt.System.Provider.Guid == dnsProviderGUID {
		// 过滤白名单事件
		if !isEventIDAllowed(evt.System.EventID, config.EventIDWhitelist) {
			return
		}

		queryName, hasQuery := evt.EventData["QueryName"]
		if !hasQuery {
			return
		}

		// 过滤黑名单域名
		if isDomainBlocked(fmt.Sprintf("%v", queryName), config.DomainBlacklist) {
			return
		}

		processId := evt.System.Execution.ProcessID
		threadId := evt.System.Execution.ThreadID
		processName, processPath := getProcessInfo(processId)

		status := ""
		if r, ok := evt.EventData["QueryStatus"]; ok {
			status = getDNSStatus(r)
		}
		if r, ok := evt.EventData["Status"]; ok {
			status = getDNSStatus(r)
		}

		queryType := getDNSQueryType(evt.EventData["QueryType"])
		result := evt.EventData["QueryResults"]
		timestamp := formatTimeAsBeijing(evt.System.TimeCreated.SystemTime, "2006-01-02 03:04:05.000")

		// 控制台输出
		fmt.Printf("\n检测到DNS查询:\n")
		fmt.Printf("时间: %s\n", timestamp)
		fmt.Printf("查询域名: %s\n", queryName)
		fmt.Printf("查询类型: %s\n", queryType)
		fmt.Printf("查询状态: %s\n", status)
		fmt.Printf("查询结果: %s\n", result)
		fmt.Printf("进程ID: %d\n", processId)
		fmt.Printf("线程ID: %d\n", threadId)
		fmt.Printf("进程名: %s\n", processName)
		fmt.Printf("进程路径: %s\n", processPath)
		fmt.Printf("事件ID: %d\n", evt.System.EventID)
		fmt.Println("------------------------")

		// 调试用：打印完整事件数据
		//if data, err := json.MarshalIndent(evt, "", "  "); err == nil {
		//	fmt.Printf("调试信息 - 完整事件数据:\n%s\n", string(data))
		//}
	}
}