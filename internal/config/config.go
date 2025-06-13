package config

import (
	"log"
	"os"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Mode   string `mapstructure:"mode"`
	Server Server `mapstructure:"server"`
	JWT    JWT    `mapstructure:"jwt"`
	CORS   CORS   `mapstructure:"cors"`
	Log    Log    `mapstructure:"log"`
	WebSocket WebSocket `mapstructure:"websocket"`
}

type Server struct {
	Port string `mapstructure:"port"`
}

type JWT struct {
	Secret          string `mapstructure:"secret"`
	ExpirationHours int    `mapstructure:"expiration_hours"`
}

type CORS struct {
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

type Log struct {
	Level      string `mapstructure:"level"`
	MaxEntries int    `mapstructure:"max_entries"`
}

type WebSocket struct {
	MaxRoomUsers int `mapstructure:"max_room_users"`
}

func Load() *Config {
	// 确定运行模式
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "local" // 默认本地调试模式
	}

	viper.SetConfigName(mode)
	viper.SetConfigType("yaml")
	viper.AddConfigPath("./configs")

	// 设置环境变量前缀
	viper.SetEnvPrefix("LETSHARE")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// 设置默认值
	setDefaults()

	if err := viper.ReadInConfig(); err != nil {
		log.Printf("配置文件读取失败: %v，使用默认配置", err)
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		log.Fatalf("配置解析失败: %v", err)
	}

	cfg.Mode = mode
	return &cfg
}

func setDefaults() {
	viper.SetDefault("server.port", "8080")
	viper.SetDefault("jwt.secret", "letshare-jwt-secret-key-2024")
	viper.SetDefault("jwt.expiration_hours", 720) // 30天
	viper.SetDefault("cors.allowed_origins", []string{
		"https://letshare.fun",
		"https://www.letshare.fun",
		"https://cdn.letshare.fun",
		"http://localhost:3000",
		"http://localhost:5173",
	})
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.max_entries", 200)
	viper.SetDefault("websocket.max_room_users", 50)
} 