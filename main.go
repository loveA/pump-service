package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	saveInterval = 5 * time.Minute // 每5分钟保存一次
	recordsDir   = "./records"     // 视频保存目录
	jpegQuality  = 80              // JPEG质量
	targetFPS    = 10              // 目标帧率，推流端最大为 10fps
)

// DeviceStream 管理单个设备的视频流
type DeviceStream struct {
	mu             sync.RWMutex
	lastFrame      []byte
	frameBuffer    [][]byte       // 帧缓冲区
	recordStopCh   chan struct{}  // 停止记录的通道
	recordTickerCh chan time.Time // 定时器通道
	recordQuitOnce sync.Once      // 确保只停止一次
}

// VideoRecorder 管理视频保存的全局状态
type VideoRecorder struct {
	mu              sync.Mutex
	activeRecorders map[string]*DeviceRecorder
}

// DeviceRecorder 管理单个设备的视频记录
type DeviceRecorder struct {
	deviceID    string
	startTime   time.Time
	frameBuffer [][]byte
	mu          sync.Mutex
}

var (
	activeStreams sync.Map
	videoRecorder *VideoRecorder
)

func main() {
	// 初始化记录目录
	if err := initRecordsDir(); err != nil {
		log.Fatalf("初始化记录目录失败: %v", err)
	}

	// 初始化视频录制器
	videoRecorder = &VideoRecorder{
		activeRecorders: make(map[string]*DeviceRecorder),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/pump_push/", handlePush)
	mux.HandleFunc("/live.mjpeg", handlePull)

	server := &http.Server{
		Addr:              "0.0.0.0:19999",
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		// Disable WriteTimeout for long-lived MJPEG streaming connections
		WriteTimeout:   0,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	log.Println("[Pump] 多设备中转服务正在监听 0.0.0.0:19999 ...")
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "仅支持 POST 请求", http.StatusMethodNotAllowed)
		return
	}

	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[1] == "" {
		http.Error(w, "缺少 device_id", http.StatusBadRequest)
		return
	}
	deviceID := pathParts[1]

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读取数据失败", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	if len(bodyBytes) == 0 {
		http.Error(w, "数据为空", http.StatusBadRequest)
		return
	}

	actual, loaded := activeStreams.LoadOrStore(deviceID, &DeviceStream{
		recordStopCh: make(chan struct{}),
		frameBuffer:  make([][]byte, 0),
	})
	stream := actual.(*DeviceStream)

	if !loaded {
		go registerStreamToGo2RTC(deviceID)
		go startVideoRecording(deviceID, stream)
	}

	stream.mu.Lock()
	stream.lastFrame = make([]byte, len(bodyBytes))
	copy(stream.lastFrame, bodyBytes)
	// 添加帧到缓冲区
	frameCopy := make([]byte, len(bodyBytes))
	copy(frameCopy, bodyBytes)
	stream.frameBuffer = append(stream.frameBuffer, frameCopy)
	stream.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		http.Error(w, "缺少 device_id 参数", http.StatusBadRequest)
		return
	}

	actual, exists := activeStreams.Load(deviceID)
	if !exists {
		http.Error(w, "该设备未激活或尚未推流", http.StatusNotFound)
		return
	}
	stream := actual.(*DeviceStream)

	// 设置 MJPEG 响应头
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame_boundary")
	w.Header().Set("Cache-Control", "no-cache, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)

	// 核心优化 1: 在循环外预分配 Buffer，彻底消除循环内的 make() 操作
	// 512KB 足够容纳 1080P 或 720P 的高压缩 JPEG 帧
	buf := make([]byte, 512*1024)

	ticker := time.NewTicker(100 * time.Millisecond) // 10 FPS
	defer ticker.Stop()

	// 核心优化 2: 通过 Context 监听客户端断开，防止协程堆积
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done(): // 客户端已断开，立即退出循环并释放资源
			return
		case <-ticker.C:
			stream.mu.RLock()
			// 避免在循环内重新 make 内存，直接使用预分配的 buf
			frameSize := len(stream.lastFrame)
			if frameSize > len(buf) {
				// 如果图片异常巨大，动态扩容，防止程序崩溃
				buf = make([]byte, frameSize)
			}
			copy(buf, stream.lastFrame)
			stream.mu.RUnlock()

			if frameSize == 0 {
				continue
			}

			// 写入数据流
			_, err := fmt.Fprintf(w, "--frame_boundary\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", frameSize)
			if err != nil {
				return
			}
			_, err = w.Write(buf[:frameSize])
			if err != nil {
				return
			}
			_, err = w.Write([]byte("\r\n"))
			if err != nil {
				return
			}

			// 强制 Flush，确保低延迟
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func registerStreamToGo2RTC(deviceID string) {
	// 纯净的拼接字符串
	// apiURL := fmt.Sprintf("http://127.0.0.1:1984/api/streams?name=%s&src=http://127.0.0.1:19999/live.mjpeg?device_id=%s", deviceID, deviceID)
	apiURL := fmt.Sprintf("http://go2rtc:1984/api/streams?name=%s&src=http://pump-server:19999/live.mjpeg?device_id=%s", deviceID, deviceID)
	req, err := http.NewRequest(http.MethodPut, apiURL, nil)
	if err != nil {
		log.Printf("[Pump] 动态构建 Go2RTC 注册请求失败: %v", err)
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Pump] 尝试将流 %s 注册至 Go2RTC 失败: %v", deviceID, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		log.Printf("[Pump] 设备 %s 已自动注册至 Go2RTC！", deviceID)
	} else {
		log.Printf("[Pump] 动态注册流 %s 失败，Go2RTC 响应状态码: %d", deviceID, resp.StatusCode)
	}
}

// initRecordsDir 初始化视频保存目录
func initRecordsDir() error {
	if err := os.MkdirAll(recordsDir, 0755); err != nil {
		return fmt.Errorf("创建记录目录失败: %v", err)
	}
	log.Printf("[Video] 视频保存目录初始化完成: %s", recordsDir)
	return nil
}

// 1. 使用一个带缓冲的 Channel 作为“录制队列”
// 我们可以定义一个结构体来传递帧数据
type RecordTask struct {
	Frames    [][]byte
	Timestamp time.Time
}

// 2. startVideoRecording 只负责“切分”缓冲区，绝不执行编码
func startVideoRecording(deviceID string, stream *DeviceStream) {
	ticker := time.NewTicker(saveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stream.recordStopCh:
			return
		case <-ticker.C:
			stream.mu.Lock()
			if len(stream.frameBuffer) == 0 {
				stream.mu.Unlock()
				continue
			}

			// 【极致优化】：不拷贝切片内容，直接将旧的指针赋值给局部变量
			// 然后将原始变量指向一个新申请的空切片
			framesToSave := stream.frameBuffer
			stream.frameBuffer = make([][]byte, 0, 1000) // 预分配容量，避免后续 append 动态扩容
			stream.mu.Unlock()

			// 此时 framesToSave 仅是一个切片头，没有任何内存拷贝动作，耗时 < 1ms
			go saveVideoSegment(deviceID, framesToSave)
		}
	}
}

// saveVideoSegment 现在是纯粹的异步 I/O 逻辑
func saveVideoSegment(deviceID string, frames [][]byte) {
	filename := getRecordFilename(deviceID)
	filepath := filepath.Join(recordsDir, filename)
	start := time.Now()
	// FFmpeg 编码是耗时动作，现在运行在独立的 goroutine 中，
	// 即使运行 1 分钟，也不会阻塞 HTTP 推流的锁
	if err := encodeToMP4(filepath, frames); err != nil {
		log.Printf("[Video] 视频保存异常: %v", err)
		return
	}
	log.Printf("[Video] 设备 %s 编码耗时: %v 视频段已落地: %s", deviceID, time.Since(start), filename)
}

// encodeToMP4 将JPEG帧编码为MP4文件
func encodeToMP4(outputPath string, frames [][]byte) error {
	if len(frames) == 0 {
		return fmt.Errorf("帧列表为空")
	}

	// 检查ffmpeg是否存在
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("未找到 ffmpeg，请确保已安装: %v", err)
	}

	// 核心优化：
	// 1. nice -n 10: 降低 CPU 优先级
	// 2. taskset -c 0,1: 强制编码只使用前 2 个核心，留下 2,3 核给 HTTP 推流
	cmd := exec.Command("taskset", "-c", "0,1", "nice", "-n", "10", "ffmpeg",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-r", fmt.Sprintf("%d", targetFPS),
		"-i", "pipe:0",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "ultrafast",
		"-crf", "28", // 调高 CRF 减小压缩复杂度，减轻 CPU 压力
		"-y",
		outputPath,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建管道失败: %v", err)
	}

	// 使用缓冲区捕获输出
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 启动命令
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("启动ffmpeg失败: %v", err)
	}

	// 写入所有JPEG帧
	for _, frame := range frames {
		if _, err := stdin.Write(frame); err != nil {
			stdin.Close()
			cmd.Wait()
			return fmt.Errorf("写入帧数据失败: %v", err)
		}
	}
	stdin.Close()

	// 等待命令完成
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg编码失败: %v, stderr: %s", err, stderr.String())
	}

	return nil
}

// getRecordFilename 生成录制文件名 (device_id_时间戳.mp4)
func getRecordFilename(deviceID string) string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("%s_%s.mp4", deviceID, timestamp)
}
