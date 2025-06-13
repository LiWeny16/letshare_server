package service

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

type RoomService struct {
	namePattern *regexp.Regexp
}

func NewRoomService() *RoomService {
	// 与前端完全一致的正则表达式：[\u4e00-\u9fa5a-zA-Z0-9 _-]+
	// 在Go中需要使用\p{Han}表示中文字符，或者直接使用Unicode范围
	pattern := regexp.MustCompile(`^[\p{Han}a-zA-Z0-9 _-]+$`)
	return &RoomService{
		namePattern: pattern,
	}
}

// ValidateRoomName 验证房间名（与前端tools.ts中的validateRoomName完全一致）
func (r *RoomService) ValidateRoomName(name string) (bool, string) {
	// 检查长度（按字符数，不是字节数）
	charCount := utf8.RuneCountInString(name)
	
	if charCount < 2 {
		return false, "房间名太短啦，至少两个字符"
	}
	
	if charCount > 12 {
		return false, "房间名最多 12 个字符"
	}
	
	// 检查字符规则：中文、字母、数字、空格、下划线、中划线
	if !r.namePattern.MatchString(name) {
		return false, "房间名只能包含中文、字母、数字、空格、下划线和中划线"
	}
	
	return true, ""
}

// SanitizeRoomName 清理房间名（移除前后空格）
func (r *RoomService) SanitizeRoomName(name string) string {
	// 移除前后空格，但保留中间的空格
	result := ""
	runes := []rune(name)
	
	// 找到第一个非空格字符
	start := 0
	for start < len(runes) && runes[start] == ' ' {
		start++
	}
	
	// 找到最后一个非空格字符
	end := len(runes) - 1
	for end >= start && runes[end] == ' ' {
		end--
	}
	
	if start <= end {
		result = string(runes[start:end+1])
	}
	
	return result
}

// GenerateRoomID 生成房间ID（用于内部存储）
func (r *RoomService) GenerateRoomID(name string) string {
	// 清理并转换为小写，用于内部键值
	sanitized := r.SanitizeRoomName(name)
	return fmt.Sprintf("room:%s", sanitized)
} 