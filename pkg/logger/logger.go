package logger

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
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
	mutex      sync.Mutex
}

var (
	fileHook *FileHook
	once     sync.Once
)

// Init 初始化日志系统
func Init(level string, maxEntries int) {
	once.Do(func() {
		// 设置日志级别
		logLevel, err := logrus.ParseLevel(level)
		if err != nil {
			logLevel = logrus.InfoLevel
		}
		logrus.SetLevel(logLevel)
		
		// 设置日志格式
		logrus.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339,
		})
		
		// 创建日志目录
		logDir := "logs"
		if err := os.MkdirAll(logDir, 0755); err != nil {
			logrus.WithError(err).Error("创建日志目录失败")
			return
		}
		
		// 创建文件hook
		fileHook = &FileHook{
			logDir:     logDir,
			maxEntries: maxEntries,
		}
		
		// 添加hook到logrus
		logrus.AddHook(fileHook)
		
		logrus.WithFields(logrus.Fields{
			"level":       level,
			"max_entries": maxEntries,
			"log_dir":     logDir,
		}).Info("日志系统已初始化")
	})
}

// Fire 实现logrus.Hook接口
func (hook *FileHook) Fire(entry *logrus.Entry) error {
	// 只记录错误和警告日志到文件
	if entry.Level > logrus.WarnLevel {
		return nil
	}
	
	hook.mutex.Lock()
	defer hook.mutex.Unlock()
	
	logEntry := LogEntry{
		Timestamp: entry.Time,
		Level:     entry.Level.String(),
		Message:   entry.Message,
		Fields:    make(map[string]interface{}),
	}
	
	// 复制字段
	for k, v := range entry.Data {
		logEntry.Fields[k] = v
	}
	
	// 写入文件
	return hook.writeToFile(logEntry)
}

// Levels 返回此hook关心的日志级别
func (hook *FileHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
	}
}

// writeToFile 写入日志到文件
func (hook *FileHook) writeToFile(entry LogEntry) error {
	filename := filepath.Join(hook.logDir, "errors.log")
	
	// 读取现有日志
	var logs []LogEntry
	if data, err := ioutil.ReadFile(filename); err == nil {
		// 尝试解析每一行JSON
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var logEntry LogEntry
			if err := json.Unmarshal([]byte(line), &logEntry); err == nil {
				logs = append(logs, logEntry)
			}
		}
	}
	
	// 添加新日志
	logs = append(logs, entry)
	
	// 如果超过最大条目数，删除最旧的
	if len(logs) > hook.maxEntries {
		logs = logs[len(logs)-hook.maxEntries:]
	}
	
	// 写回文件
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("创建日志文件失败: %w", err)
	}
	defer file.Close()
	
	for _, log := range logs {
		if data, err := json.Marshal(log); err == nil {
			file.WriteString(string(data) + "\n")
		}
	}
	
	return nil
}

// CleanupLogs 清理日志（由维护任务调用）
func CleanupLogs() {
	if fileHook == nil {
		return
	}
	
	fileHook.mutex.Lock()
	defer fileHook.mutex.Unlock()
	
	filename := filepath.Join(fileHook.logDir, "errors.log")
	
	// 读取现有日志
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}
	
	var logs []LogEntry
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var logEntry LogEntry
		if err := json.Unmarshal([]byte(line), &logEntry); err == nil {
			logs = append(logs, logEntry)
		}
	}
	
	// 按时间排序
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})
	
	// 保留最新的日志条目
	if len(logs) > fileHook.maxEntries {
		logs = logs[:fileHook.maxEntries]
	}
	
	// 写回文件
	file, err := os.Create(filename)
	if err != nil {
		return
	}
	defer file.Close()
	
	for _, log := range logs {
		if data, err := json.Marshal(log); err == nil {
			file.WriteString(string(data) + "\n")
		}
	}
}

// GetErrorLogs 获取错误日志（用于监控）
func GetErrorLogs(limit int) ([]LogEntry, error) {
	if fileHook == nil {
		return nil, fmt.Errorf("日志系统未初始化")
	}
	
	filename := filepath.Join(fileHook.logDir, "errors.log")
	
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	
	var logs []LogEntry
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var logEntry LogEntry
		if err := json.Unmarshal([]byte(line), &logEntry); err == nil {
			logs = append(logs, logEntry)
		}
	}
	
	// 按时间排序，最新的在前
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].Timestamp.After(logs[j].Timestamp)
	})
	
	// 限制返回数量
	if limit > 0 && len(logs) > limit {
		logs = logs[:limit]
	}
	
	return logs, nil
} 