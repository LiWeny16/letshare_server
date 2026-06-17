package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

type FileHook struct {
	logDir     string
	maxEntries int
	writeFile  *os.File
	mu         sync.Mutex
	appendOnly bool // true: 只追加, 不在每次写入时读取整个文件
}

var (
	fileHook *FileHook
	once     sync.Once
)

// Init 初始化日志系统
func Init(level string, maxEntries int) {
	once.Do(func() {
		logLevel, err := logrus.ParseLevel(level)
		if err != nil {
			logLevel = logrus.InfoLevel
		}
		logrus.SetLevel(logLevel)

		logrus.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})

		logDir := "logs"
		if err := os.MkdirAll(logDir, 0755); err != nil {
			logrus.WithError(err).Error("创建日志目录失败")
			return
		}

		// 打开日志文件 (追加模式)
		logFile := filepath.Join(logDir, "errors.log")
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			logrus.WithError(err).Error("打开日志文件失败")
			return
		}

		fileHook = &FileHook{
			logDir:     logDir,
			maxEntries: maxEntries,
			writeFile:  f,
			appendOnly: true,
		}

		// 启动时截断旧日志
		fileHook.truncateIfNeeded()

		logrus.AddHook(fileHook)

		logrus.WithFields(logrus.Fields{
			"level":       level,
			"max_entries": maxEntries,
			"log_dir":     logDir,
			"mode":        "append-only",
		}).Info("日志系统已初始化")
	})
}

// Fire 实现 logrus.Hook 接口 — 追加写入, 不读取全文件
func (hook *FileHook) Fire(entry *logrus.Entry) error {
	if entry.Level > logrus.WarnLevel {
		return nil
	}

	hook.mu.Lock()
	defer hook.mu.Unlock()

	logEntry := LogEntry{
		Timestamp: entry.Time,
		Level:     entry.Level.String(),
		Message:   entry.Message,
		Fields:    make(map[string]interface{}),
	}
	for k, v := range entry.Data {
		logEntry.Fields[k] = v
	}

	data, err := json.Marshal(logEntry)
	if err != nil {
		return fmt.Errorf("序列化日志失败: %w", err)
	}

	// 纯追加写入 — O(1) 操作
	if _, err := hook.writeFile.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入日志失败: %w", err)
	}

	return nil
}

// truncateIfNeeded 在启动时裁剪日志到 maxEntries 条
func (hook *FileHook) truncateIfNeeded() {
	filename := filepath.Join(hook.logDir, "errors.log")
	data, err := os.ReadFile(filename)
	if err != nil || len(data) == 0 {
		return
	}

	var logs []LogEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var le LogEntry
		if json.Unmarshal([]byte(line), &le) == nil {
			logs = append(logs, le)
		}
	}

	if len(logs) <= hook.maxEntries {
		return
	}

	// 保留最新的
	logs = logs[len(logs)-hook.maxEntries:]

	f, err := os.Create(filename)
	if err != nil {
		return
	}
	defer f.Close()

	for _, le := range logs {
		if b, err := json.Marshal(le); err == nil {
			f.Write(append(b, '\n'))
		}
	}
}

// Levels 实现 logrus.Hook 接口
func (hook *FileHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
	}
}

// CleanupLogs 定期裁剪日志 (每 30 秒由维护任务调用)
func CleanupLogs() {
	if fileHook == nil {
		return
	}

	fileHook.mu.Lock()
	defer fileHook.mu.Unlock()

	filename := filepath.Join(fileHook.logDir, "errors.log")

	data, err := os.ReadFile(filename)
	if err != nil || len(data) == 0 {
		return
	}

	var logs []LogEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var le LogEntry
		if json.Unmarshal([]byte(line), &le) == nil {
			logs = append(logs, le)
		}
	}

	if len(logs) <= fileHook.maxEntries {
		return
	}

	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})
	logs = logs[:fileHook.maxEntries]

	f, err := os.Create(filename)
	if err != nil {
		return
	}
	defer f.Close()

	// 重新打开追加文件句柄
	fileHook.writeFile.Close()
	fileHook.writeFile = f

	for _, le := range logs {
		if b, err := json.Marshal(le); err == nil {
			f.Write(append(b, '\n'))
		}
	}

	// 重新以追加模式打开
	newF, _ := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if newF != nil {
		fileHook.writeFile = newF
	}
}

// GetErrorLogs 获取错误日志 (用于监控/调试)
func GetErrorLogs(limit int) ([]LogEntry, error) {
	if fileHook == nil {
		return nil, fmt.Errorf("日志系统未初始化")
	}

	fileHook.mu.Lock()
	defer fileHook.mu.Unlock()

	filename := filepath.Join(fileHook.logDir, "errors.log")
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var logs []LogEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var le LogEntry
		if json.Unmarshal([]byte(line), &le) == nil {
			logs = append(logs, le)
		}
	}

	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})

	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}

	return logs, nil
}
