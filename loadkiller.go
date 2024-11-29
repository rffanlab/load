package main

import (
    "flag"
    "fmt"
    "os"
    "os/signal"
    "runtime"
    "sync"
    "sync/atomic"
    "syscall"
    "time"
)

type PIDController struct {
    kp, ki, kd float64
    setpoint   float64
    integral   float64
    lastError  float64
}

func NewPIDController(kp, ki, kd, setpoint float64) *PIDController {
    return &PIDController{
        kp:       kp,
        ki:       ki,
        kd:       kd,
        setpoint: setpoint,
    }
}

func (pid *PIDController) Calculate(input float64) float64 {
    error := pid.setpoint - input
    pid.integral += error
    derivative := error - pid.lastError
    output := pid.kp*error + pid.ki*pid.integral + pid.kd*derivative
    pid.lastError = error
    return output
}

type ResourceController struct {
    targetCPUPercent float64
    targetMemoryMB   int
    numCPUs         int
    stopChan        chan struct{}
    wg              sync.WaitGroup
    cpuUsage        atomic.Value
    pid             *PIDController
    workload        int64
}

func NewResourceController(cpuPercent float64, memoryMB, numCPUs int) *ResourceController {
    rc := &ResourceController{
        targetCPUPercent: cpuPercent,
        targetMemoryMB:   memoryMB,
        numCPUs:         numCPUs,
        stopChan:        make(chan struct{}),
        workload:        1000,
    }
    rc.cpuUsage.Store(0.0)
    // 调整 PID 参数以适应系统级 CPU 控制
    rc.pid = NewPIDController(0.05, 0.005, 0.001, cpuPercent)
    return rc
}

func (rc *ResourceController) Start() {
    fmt.Printf("Starting resource controller (Target Total CPU: %.1f%%, Memory: %dMB)\n",
        rc.targetCPUPercent, rc.targetMemoryMB)

    // 启动多个 CPU 工作协程，每个核心启动多个协程以更均匀地分配负载
    workersPerCPU := 4
    rc.wg.Add(2 + rc.numCPUs*workersPerCPU) // monitor + memory + CPU workers
    
    // 为每个 CPU 核心启动多个工作协程
    for i := 0; i < rc.numCPUs; i++ {
        for j := 0; j < workersPerCPU; j++ {
            go rc.cpuWorker(i)
        }
    }
    
    go rc.controlMemory()
    go rc.monitor()
}

func (rc *ResourceController) Stop() {
    close(rc.stopChan)
    rc.wg.Wait()
    fmt.Println("Resource controller stopped")
}

func getCPUUsage() float64 {
    rusage := new(syscall.Rusage)
    syscall.Getrusage(syscall.RUSAGE_SELF, rusage)
    
    // 获取所有 CPU 时间
    var stats syscall.Statfs_t
    syscall.Statfs("/", &stats)
    
    time.Sleep(100 * time.Millisecond)
    
    rusageAfter := new(syscall.Rusage)
    syscall.Getrusage(syscall.RUSAGE_SELF, rusageAfter)
    
    userTimeDiff := float64(rusageAfter.Utime.Sec-rusage.Utime.Sec) + 
                    float64(rusageAfter.Utime.Usec-rusage.Utime.Usec)/1e6
    sysTimeDiff := float64(rusageAfter.Stime.Sec-rusage.Stime.Sec) + 
                   float64(rusageAfter.Stime.Usec-rusage.Stime.Usec)/1e6
    
    totalCPUTime := userTimeDiff + sysTimeDiff
    return (totalCPUTime / 0.1) * 100.0 // 0.1 是测量间隔（100ms）
}

func (rc *ResourceController) cpuWorker(cpuIndex int) {
    defer rc.wg.Done()
    
    // 设置 CPU 亲和性（如果支持）
    if runtime.GOOS == "linux" {
        // 注意：在实际代码中需要添加 CPU 亲和性设置
        // 这里仅作为示意
    }

    for {
        select {
        case <-rc.stopChan:
            return
        default:
            workload := atomic.LoadInt64(&rc.workload)
            for i := int64(0); i < workload; i++ {
                _ = i * i
            }
            // 添加短暂休眠以允许其他协程执行
            time.Sleep(time.Microsecond)
        }
    }
}

func (rc *ResourceController) monitor() {
    defer rc.wg.Done()
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-rc.stopChan:
            return
        case <-ticker.C:
            currentUsage := getCPUUsage()
            rc.cpuUsage.Store(currentUsage)
            
            // 根据整体 CPU 使用率调整工作负载
            adjustment := rc.pid.Calculate(currentUsage)
            newWorkload := float64(atomic.LoadInt64(&rc.workload)) * (1 + adjustment/100)
            
            // 限制工作负载范围
            if newWorkload < 100 {
                newWorkload = 100
            } else if newWorkload > 1000000 {
                newWorkload = 1000000
            }
            
            atomic.StoreInt64(&rc.workload, int64(newWorkload))
            
            fmt.Printf("\rTotal CPU Usage: %.1f%% (Target: %.1f%%) Workload: %d   ",
                currentUsage, rc.targetCPUPercent, int64(newWorkload))
        }
    }
}

func (rc *ResourceController) controlMemory() {
    defer rc.wg.Done()

    memoryBytes := rc.targetMemoryMB * 1024 * 1024
    data := make([]byte, memoryBytes)

    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-rc.stopChan:
            return
        case <-ticker.C:
            for i := 0; i < len(data); i += 4096 {
                data[i] = byte(i)
            }
        }
    }
}

func main() {
    cpuPercent := flag.Float64("cpu", 30.0, "Target total CPU usage percentage")
    memoryMB := flag.Int("mem", 500, "Target memory usage in MB")
    numCPUs := flag.Int("cores", runtime.NumCPU(), "Number of CPU cores to use")
    flag.Parse()

    if *cpuPercent <= 0 || *cpuPercent > 100 {
        fmt.Println("CPU usage must be between 0 and 100")
        os.Exit(1)
    }
    if *memoryMB <= 0 {
        fmt.Println("Memory usage must be positive")
        os.Exit(1)
    }
    if *numCPUs <= 0 || *numCPUs > runtime.NumCPU() {
        fmt.Printf("Invalid number of CPU cores. Must be between 1 and %d\n", runtime.NumCPU())
        os.Exit(1)
    }

    // 设置可用的 CPU 核心数
    runtime.GOMAXPROCS(*numCPUs)

    // 创建控制器
    controller := NewResourceController(*cpuPercent, *memoryMB, *numCPUs)

    // 设置信号处理
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

    // 启动控制器
    controller.Start()

    // 等待中断信号
    <-sigChan
    fmt.Println("\nReceived interrupt signal, shutting down...")
    controller.Stop()
}